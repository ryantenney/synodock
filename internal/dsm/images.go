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
	"strings"
	"time"
)

// ImageTar is what a `docker save` tar holds, read from its manifest: the tags it
// loads, and the short id of the image (its config blob's digest).
type ImageTar struct {
	Tags    []string
	ShortID string
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
		var out ImageTar
		for _, entry := range manifest {
			out.Tags = append(out.Tags, entry.RepoTags...)
		}
		if len(manifest) > 0 {
			config := manifest[0].Config
			config = config[strings.LastIndex(config, "/")+1:]
			out.ShortID = ShortID(strings.TrimSuffix(config, ".json"))
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
		if names[prev] {
			if err := c.FSDelete(folder + "/" + prev); err != nil {
				return "", err
			}
		}
		if err := c.FSRename(folder+"/"+name, prev); err != nil {
			return "", err
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
