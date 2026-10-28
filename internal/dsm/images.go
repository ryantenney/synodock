package dsm

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ImageTar is what a `docker save` tar holds, read from its manifest: the tags it
// loads, and for each tag the short id of its image (its config blob's digest). A tar
// can hold several images (docker save a b), so the id is per tag.
type ImageTar struct {
	Tags    []string
	IDs     map[string]string // tag -> short id
	ShortID string            // the first image's, for messages
}

// ReadImageTar reads the manifest of a `docker save` tar, plain or gzipped.
func ReadImageTar(path string) (ImageTar, error) {
	f, err := os.Open(path)
	if err != nil {
		return ImageTar{}, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var r io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return ImageTar{}, err
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return ImageTar{}, Errorf("%s has no manifest.json: not a docker save tar", path)
		}
		if err != nil {
			return ImageTar{}, Errorf("%s is not a docker save tar: %v", path, err)
		}
		if hdr.Name != "manifest.json" && hdr.Name != "./manifest.json" {
			continue
		}
		var manifest []struct {
			RepoTags []string `json:"RepoTags"`
			Config   string   `json:"Config"`
		}
		if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
			return ImageTar{}, fmt.Errorf("%s: manifest.json: %w", path, err)
		}
		out := ImageTar{IDs: map[string]string{}}
		for i, entry := range manifest {
			config := entry.Config[strings.LastIndex(entry.Config, "/")+1:]
			id := ShortID(strings.TrimSuffix(config, ".json"))
			if i == 0 {
				out.ShortID = id
			}
			for _, tag := range entry.RepoTags {
				out.Tags = append(out.Tags, tag)
				out.IDs[tag] = id
			}
		}
		return out, nil
	}
}

// FSRename renames a NAS file or folder in place.
func (c *Client) FSRename(path, newName string) error {
	_, err := c.Call("SYNO.FileStation.Rename", "rename", 2, Params{"path": JS([]string{path}), "name": JS([]string{newName})})
	return err
}

// ImageUpload copies a tar into a NAS folder. An existing tar of the same name moves to
// <stem>.prev.tar first, replacing the last .prev: a load moves the tag, so that file is
// the way back. It returns the name the old tar was moved to, "" if there wasn't one.
func (c *Client) ImageUpload(tarPath, folder string) (string, error) {
	name := filepath.Base(tarPath)
	prev := strings.TrimSuffix(name, ".tar") + ".prev.tar"
	names, err := c.FSNames(folder)
	if err != nil {
		return "", err
	}
	moved := ""
	if names[name] {
		// Move the current tar aside first, so if anything fails the rollback copy (the
		// current tar, or the old .prev) is still there.
		aside := name + ".rotating"
		if err := c.FSRename(folder+"/"+name, aside); err != nil {
			return "", err
		}
		if names[prev] {
			if err := c.FSDelete(folder + "/" + prev); err != nil {
				return "", Errorf("%v; the current tar is kept as %s/%s", err, folder, aside)
			}
		}
		if err := c.FSRename(folder+"/"+aside, prev); err != nil {
			return "", Errorf("%v; the current tar is kept as %s/%s", err, folder, aside)
		}
		moved = folder + "/" + prev
	}
	return moved, c.FSPut(tarPath, folder, true, true)
}

// ImagePull pulls repo:tag from a registry onto the NAS, where it lands as host/repo:tag.
// Container Manager reports a failed pull only as an error code with the message stripped;
// its log has the message, which this returns when there is one.
func (c *Client) ImagePull(registryURL, repo, tag, user, password string, wait time.Duration) error {
	var task struct {
		TaskID string `json:"task_id"`
	}
	if err := c.CallInto(&task, "SYNO.Docker.Image", "pull_start", 1, Params{
		"repository": JS(repo), "tag": JS(tag), "registry": JS(registryURL),
		"username": JS(user), "password": JS(password)}); err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		var status struct {
			Finished bool `json:"finished"`
		}
		if err := c.CallInto(&status, "SYNO.Docker.Image", "pull_status", 1, Params{"task_id": JS(task.TaskID)}); err != nil {
			if msg, _ := c.lastDockerError(); msg != "" {
				return Errorf("pull of %s:%s failed: %s", repo, tag, msg)
			}
			return Errorf("pull of %s:%s failed: %v", repo, tag, err)
		}
		if status.Finished {
			return nil
		}
	}
	return Errorf("pull of %s:%s did not finish within %s", repo, tag, wait)
}

// lastDockerError is the newest error in Container Manager's log, "" if none.
func (c *Client) lastDockerError() (string, error) {
	var data struct {
		Logs []struct {
			Level string `json:"level"`
			Event string `json:"event"`
		} `json:"logs"`
	}
	err := c.CallInto(&data, "SYNO.Docker.Log", "list", 1, Params{"action": JS("load"), "offset": "0", "limit": "5",
		"sort_by": JS("time"), "sort_dir": JS("DESC"), "loglevel": JS(""), "filter_content": JS(""),
		"datefrom": "0", "dateto": "0"})
	if err != nil {
		return "", err
	}
	for _, e := range data.Logs {
		if e.Level == "err" {
			return e.Event, nil
		}
	}
	return "", nil
}

// ImageExport saves repo:tag as a docker save tar in a NAS folder, as Container Manager's
// Export does. The folder must exist.
func (c *Client) ImageExport(repo, tag, folder string) error {
	_, err := c.Call("SYNO.Docker.Image", "export", 1, Params{"repo": JS(repo), "tag": JS(tag), "path": JS(folder)})
	return err
}

// ImagePrune deletes the images no container uses, as Container Manager's Remove Unused
// Images does.
func (c *Client) ImagePrune() error {
	_, err := c.Call("SYNO.Docker.Image", "prune", 1, nil)
	return err
}

// ImageUpgrade pulls a newer version of repository:latest, as the UI's Upgrade does, and
// waits for it to finish. It returns when the NAS reports the task finished.
func (c *Client) ImageUpgrade(repository string, wait time.Duration) error {
	var task struct {
		TaskID string `json:"task_id"`
	}
	if err := c.CallInto(&task, "SYNO.Docker.Image", "upgrade_start", 1, Params{"repository": JS(repository)}); err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		var status struct {
			Finished bool `json:"finished"`
		}
		if err := c.CallInto(&status, "SYNO.Docker.Image", "upgrade_status", 1, Params{"task_id": JS(task.TaskID)}); err != nil {
			return Errorf("upgrade of %s failed: %v", repository, err)
		}
		if status.Finished {
			return nil
		}
	}
	return Errorf("upgrade of %s did not finish within %s", repository, wait)
}

// CheckTags says whether every tag now names the image it should (tag -> short id). A
// load or import can report success without moving a tag, so callers check afterwards.
func (c *Client) CheckTags(want map[string]string) error {
	now, err := c.ImageIDsByTag()
	if err != nil {
		return err
	}
	tags := make([]string, 0, len(want))
	for tag := range want {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		if got := ShortID(now[tag]); got != want[tag] {
			if got == "" {
				got = "missing"
			}
			return Errorf("%s is %s, expected %s", tag, got, want[tag])
		}
	}
	return nil
}
