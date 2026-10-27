package dsm

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Project is a Container Manager project: a compose file and the folder it lives in.
// Content and Containers come only from Project (get); List leaves them empty.
type Project struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Path       string             `json:"path"`
	SharePath  string             `json:"share_path"`
	Status     string             `json:"status"`
	Content    string             `json:"content"`
	Containers []ProjectContainer `json:"containers"`
}

// Running is true when any of the project's containers is up: WARNING is some of them.
func (p *Project) Running() bool { return p.Status == "RUNNING" || p.Status == "WARNING" }

// ProjectContainer is one of a project's containers, as docker inspect reports it. Only
// the fields project ps shows are decoded, so the config's environment is never kept.
type ProjectContainer struct {
	Name  string `json:"Name"`
	State struct {
		Status   string `json:"Status"`
		ExitCode int    `json:"ExitCode"`
		Health   *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
	HostConfig struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

// Health is the container's health check status, "" when it has none.
func (pc *ProjectContainer) Health() string {
	if pc.State.Health == nil {
		return ""
	}
	return pc.State.Health.Status
}

// BareName is the container's name without docker's leading slash.
func (pc *ProjectContainer) BareName() string { return strings.TrimPrefix(pc.Name, "/") }

// Projects lists the Container Manager projects, sorted by name.
func (c *Client) Projects() ([]Project, error) {
	var data map[string]Project
	if err := c.CallInto(&data, "SYNO.Docker.Project", "list", 1, nil); err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(data))
	for _, p := range data {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ProjectByName is the project called name (or with that id), with its compose file and
// containers.
func (c *Client) ProjectByName(nameOrID string) (*Project, error) {
	projects, err := c.Projects()
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.Name == nameOrID || p.ID == nameOrID {
			var full Project
			if err := c.CallInto(&full, "SYNO.Docker.Project", "get", 1, Params{"id": JS(p.ID)}); err != nil {
				return nil, err
			}
			return &full, nil
		}
	}
	return nil, Errorf("no project named %q", nameOrID)
}

// ProjectUpdate replaces a project's compose file. Container Manager accepts this for a
// running project too; callers keep its rule and check first.
func (c *Client) ProjectUpdate(id, content string) error {
	_, err := c.Call("SYNO.Docker.Project", "update", 1, Params{"id": JS(id), "content": JS(content)})
	return err
}

// ProjectCreate makes a project from its compose file, with its folder at sharePath.
func (c *Client) ProjectCreate(name, sharePath, content string) (*Project, error) {
	var p Project
	err := c.CallInto(&p, "SYNO.Docker.Project", "create", 1, Params{
		"name": JS(name), "share_path": JS(sharePath), "content": JS(content), "enable_service_portal": "false"})
	return &p, err
}

// ProjectStream runs a compose action (build, clean, start, stop, restart) and passes
// each line of its output to out. It returns compose's exit code.
func (c *Client) ProjectStream(id, action string, out func(string)) (int, error) {
	params := url.Values{"api": {"SYNO.Docker.Project"}, "method": {action + "_stream"}, "version": {"1"}, "id": {JS(id)}}
	resp, err := c.request(c.ctx, params, nil, "", 0, true)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	code, exited := 0, false
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if line == "" {
			continue
		}
		text := line
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			text = strings.TrimSpace(rest)
		}
		var parsed any
		if json.Unmarshal([]byte(text), &parsed) == nil {
			switch v := parsed.(type) {
			case string:
				text = v
			case map[string]any:
				if success, ok := v["success"].(bool); ok && !success {
					return 0, Errorf("%s failed: %v", action, v["error"])
				}
			}
		}
		if m := exitCodeRE.FindStringSubmatch(text); m != nil {
			code, _ = strconv.Atoi(m[1])
			exited = true
		}
		out(strings.TrimRight(text, "\n"))
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if !exited {
		return 0, Errorf("%s: stream ended without an exit code", action)
	}
	return code, nil
}

var exitCodeRE = regexp.MustCompile(`Exit Code:\s*(-?\d+)`)

// StaleNames are the project's containers that Container Manager no longer lists under
// their own names. After a recreate while running, it lists them as "<12hex>_name", and
// stop and clean then silently do nothing until a build refreshes them.
func (c *Client) StaleNames(p *Project) ([]string, error) {
	containers, err := c.Containers()
	if err != nil {
		return nil, err
	}
	listed := make(map[string]bool, len(containers))
	for _, x := range containers {
		listed[x.Name] = true
	}
	var stale []string
	for i := range p.Containers {
		if name := p.Containers[i].BareName(); !listed[name] {
			stale = append(stale, name)
		}
	}
	return stale, nil
}

// bindFolder matches a compose bind mount relative to the project: ./data:/var/lib/x.
var bindFolder = regexp.MustCompile(`(?m)^\s*-\s*['"]?\./([^/:'"]+)[^:]*:`)

// CreateProject makes the project's folder at sharePath, and the folders its compose
// file binds relative to it (Container Manager creates neither, and a missing one fails
// the build), then the project. created is called for each folder it makes.
func (c *Client) CreateProject(name, sharePath, content string, created func(string)) (*Project, error) {
	projects, err := c.Projects()
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.Name == name {
			return nil, Errorf("project %s already exists", name)
		}
	}
	existing, err := c.FSNames(sharePath)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		if err := c.FSPutBytes(nil, ".keep", sharePath, true, true); err != nil {
			return nil, err
		}
		if existing, err = c.FSNames(sharePath); err != nil {
			return nil, err
		}
	}
	for _, m := range bindFolder.FindAllStringSubmatch(content, -1) {
		folder := m[1]
		if existing[folder] {
			continue
		}
		if err := c.FSMkdir(sharePath, folder); err != nil {
			return nil, err
		}
		existing[folder] = true
		created(sharePath + "/" + folder)
	}
	return c.ProjectCreate(name, sharePath, content)
}

// BackupProject saves a project's compose file before it's replaced: locally under the
// cache directory, and in <share>/backups on the NAS. It returns the local path.
func (c *Client) BackupProject(p *Project, content string) (string, error) {
	stamp := time.Now().Format("20060102-150405")
	dir := filepath.Join(CacheDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	local := filepath.Join(dir, p.Name+"-"+stamp+".yml")
	if err := os.WriteFile(local, []byte(content), 0o600); err != nil {
		return "", err
	}
	// overwrite=false: a second backup in the same second fails rather than replacing one.
	if err := c.FSPutBytes([]byte(content), "docker-compose-"+stamp+".yml", p.SharePath+"/backups", true, false); err != nil {
		return local, err
	}
	return local, nil
}

// FSNames is the entries of a NAS folder by name. A folder that doesn't exist (code 408)
// has none.
func (c *Client) FSNames(folder string) (map[string]bool, error) {
	var data struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	err := c.CallInto(&data, "SYNO.FileStation.List", "list", 2, Params{
		"folder_path": JS(folder), "additional": JS([]string{"size", "time"})})
	names := map[string]bool{}
	if e, ok := err.(*Error); ok && e.Code == 408 {
		return names, nil
	}
	if err != nil {
		return nil, err
	}
	for _, f := range data.Files {
		names[f.Name] = true
	}
	return names, nil
}

// FSMkdir makes a folder called name inside folder.
func (c *Client) FSMkdir(folder, name string) error {
	_, err := c.Call("SYNO.FileStation.CreateFolder", "create", 2, Params{
		"folder_path": JS([]string{folder}), "name": JS([]string{name})})
	return err
}

// ProjectLog is the compose output from the project's last action, as Container Manager
// keeps it.
func (c *Client) ProjectLog(id string) (string, error) {
	var out string
	err := c.CallInto(&out, "SYNO.Docker.Project", "log", 1, Params{"id": JS(id)})
	return out, err
}

// ProjectDelete removes a project and its containers. Container Manager's own confirmation
// warns that the project's data goes too; the API takes no option to keep any of it.
func (c *Client) ProjectDelete(id string) error {
	_, err := c.Call("SYNO.Docker.Project", "delete", 1, Params{"id": JS(id)})
	return err
}
