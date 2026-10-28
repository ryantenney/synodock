package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ryantenney/synodock/internal/dsm"
)

// deployNAS is a fake DSM with state, enough for project deploy: one project, its
// compose file and containers, the image list, and FileStation folders. Uploads land in
// folders, an import reads the uploaded tar's manifest, a build runs the compose file,
// and deleting an image follows Docker: every tag goes, and an image in use is refused.
type deployNAS struct {
	t  *testing.T
	mu sync.Mutex

	project    string // name; its id is "id-" + name
	share      string // /docker/<name>
	status     string
	compose    string
	containers map[string]string // container name -> image ref it runs
	failWith   int               // a build's containers exit with this, when non-zero

	images   map[string]string // tag -> full image id
	untagged map[string]string // full image id -> its repository, once its last tag moved
	files    map[string][]byte // NAS path -> content
	calls    []string          // API.method, in order, for checking the sequence
	deleted  []string          // image ids deleted
}

func newDeployNAS(t *testing.T, project, compose string, images map[string]string) *deployNAS {
	n := &deployNAS{t: t, project: project, share: "/docker/" + project, status: "RUNNING", compose: compose,
		images: map[string]string{}, untagged: map[string]string{}, files: map[string][]byte{}, containers: map[string]string{}}
	for tag, id := range images {
		n.images[tag] = fullID(id)
	}
	n.build() // it's running before the deploy
	return n
}

func fullID(short string) string { return "sha256:" + short + strings.Repeat("0", 64-len(short)) }

// services parses the compose file into container name -> image ref.
func (n *deployNAS) services() map[string]string {
	var file struct {
		Services map[string]struct {
			Image         string `yaml:"image"`
			ContainerName string `yaml:"container_name"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(n.compose), &file); err != nil {
		n.t.Fatalf("fake NAS: compose: %v", err)
	}
	out := map[string]string{}
	for name, svc := range file.Services {
		cname := svc.ContainerName
		if cname == "" {
			cname = n.project + "-" + name + "-1"
		}
		out[cname] = svc.Image
	}
	return out
}

func (n *deployNAS) build() {
	n.containers = n.services()
	n.status = "RUNNING"
}

func (n *deployNAS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	params := r.URL.Query()
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			n.t.Errorf("fake NAS: multipart: %v", err)
		}
		for k, v := range r.MultipartForm.Value {
			params[k] = v
		}
	} else {
		_ = r.ParseForm()
		for k, v := range r.PostForm {
			params[k] = v
		}
	}
	str := func(key string) string { // the web API's JSON-quoted strings
		var s string
		if err := json.Unmarshal([]byte(params.Get(key)), &s); err != nil {
			return params.Get(key)
		}
		return s
	}
	list := func(key string) []string {
		var s []string
		_ = json.Unmarshal([]byte(params.Get(key)), &s)
		return s
	}
	call := params.Get("api") + "." + params.Get("method")
	n.calls = append(n.calls, call)
	var data any
	switch call {
	case "SYNO.API.Auth.login":
		data = map[string]any{"sid": "sid", "synotoken": "token"}
	case "SYNO.API.Auth.logout":
	case "SYNO.Docker.Project.list":
		data = map[string]any{"id-" + n.project: n.projectJSON(false)}
	case "SYNO.Docker.Project.get":
		data = n.projectJSON(true)
	case "SYNO.Docker.Project.update":
		n.compose = str("content")
	case "SYNO.Docker.Project.clean_stream", "SYNO.Docker.Project.build_stream":
		if strings.HasPrefix(call, "SYNO.Docker.Project.clean") {
			n.containers, n.status = map[string]string{}, "STOPPED"
		} else {
			n.build()
		}
		fmt.Fprintf(w, "data: %q\n\ndata: %q\n\n", "Container "+n.project+" done", "Exit Code: 0")
		return
	case "SYNO.Docker.Container.list":
		var cs []map[string]any
		for name := range n.containers {
			cs = append(cs, map[string]any{"name": name, "status": "running"})
		}
		data = map[string]any{"containers": cs}
	case "SYNO.Docker.Image.list":
		byID := map[string][]string{}
		for tag, id := range n.images {
			byID[id] = append(byID[id], tag)
		}
		var imgs []map[string]any
		for id, tags := range byID {
			repo := tags[0][:strings.LastIndex(tags[0], ":")]
			var short []string
			for _, tag := range tags {
				short = append(short, tag[strings.LastIndex(tag, ":")+1:])
			}
			imgs = append(imgs, map[string]any{"id": id, "repository": repo, "tags": short})
		}
		for id, repo := range n.untagged { // as the real list shows them
			imgs = append(imgs, map[string]any{"id": id, "repository": repo, "tags": []string{"<none>"}})
		}
		data = map[string]any{"images": imgs}
	case "SYNO.Docker.Image.import":
		n.importTar(str("path"))
	case "SYNO.Docker.Image.delete":
		var which []map[string]string
		_ = json.Unmarshal([]byte(params.Get("images")), &which)
		for _, img := range which {
			if err := n.deleteImage(img["identity"]); err != "" {
				n.fail(w, 1202)
				return
			}
		}
	case "SYNO.FileStation.List.list_share":
		data = map[string]any{"shares": []any{map[string]any{"path": "/docker",
			"additional": map[string]any{"volume_status": map[string]any{"freespace": int64(100e9)}}}}}
	case "SYNO.FileStation.List.list":
		folder := str("folder_path")
		var files []map[string]string
		for p := range n.files {
			if path.Dir(p) == folder {
				files = append(files, map[string]string{"name": path.Base(p)})
			}
		}
		if files == nil {
			n.fail(w, 408)
			return
		}
		data = map[string]any{"files": files}
	case "SYNO.FileStation.Upload.upload":
		f, hdr, err := r.FormFile("file")
		if err != nil {
			n.t.Errorf("fake NAS: upload: %v", err)
			n.fail(w, 101)
			return
		}
		body, _ := io.ReadAll(f)
		n.files[params.Get("path")+"/"+hdr.Filename] = body
	case "SYNO.FileStation.Rename.rename":
		from, to := list("path")[0], list("name")[0]
		n.files[path.Dir(from)+"/"+to] = n.files[from]
		delete(n.files, from)
	case "SYNO.FileStation.Delete.delete":
		delete(n.files, list("path")[0])
	default:
		n.t.Errorf("fake NAS: unexpected call %s", call)
		n.fail(w, 103)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func (n *deployNAS) fail(w http.ResponseWriter, code any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": map[string]any{"code": code}})
}

func (n *deployNAS) projectJSON(full bool) map[string]any {
	p := map[string]any{"id": "id-" + n.project, "name": n.project, "path": "/volume1" + n.share,
		"share_path": n.share, "status": n.status}
	if full {
		var cs []map[string]any
		for name, ref := range n.containers {
			state := map[string]any{"Status": "running"}
			if n.failWith != 0 {
				state = map[string]any{"Status": "exited", "ExitCode": n.failWith}
			}
			cs = append(cs, map[string]any{"Name": "/" + name, "State": state, "Config": map[string]any{"Image": ref},
				"HostConfig": map[string]any{"RestartPolicy": map[string]any{"Name": "no"}}})
		}
		p["content"], p["containers"] = n.compose, cs
	}
	return p
}

// importTar loads an uploaded docker save tar: each tag moves to its image's id.
func (n *deployNAS) importTar(nasPath string) {
	tmp := filepath.Join(n.t.TempDir(), "import.tar")
	if err := os.WriteFile(tmp, n.files[nasPath], 0o600); err != nil {
		n.t.Fatal(err)
	}
	info, err := dsm.ReadImageTar(tmp)
	if err != nil {
		n.t.Errorf("fake NAS: import %s: %v", nasPath, err)
		return
	}
	for tag, id := range info.IDs {
		old := n.images[tag]
		n.images[tag] = fullID(id)
		if old != "" && old != n.images[tag] && !n.tagged(old) {
			n.untagged[old] = tag[:strings.LastIndex(tag, ":")]
		}
	}
}

// deleteImage deletes by short id as Docker does: every tag on it goes, and an image a
// container runs is refused.
func (n *deployNAS) deleteImage(short string) string {
	var id string
	for _, full := range n.images {
		if strings.HasPrefix(strings.TrimPrefix(full, "sha256:"), short) {
			id = full
		}
	}
	for full := range n.untagged {
		if strings.HasPrefix(strings.TrimPrefix(full, "sha256:"), short) {
			id = full
		}
	}
	if id == "" {
		return "no such image"
	}
	delete(n.untagged, id)
	for _, ref := range n.containers {
		if n.images[ref] == id {
			return "in use"
		}
	}
	for tag, full := range n.images {
		if full == id {
			delete(n.images, tag)
		}
	}
	n.deleted = append(n.deleted, dsm.ShortID(id))
	return ""
}

// saveTar writes a docker save-style tar whose tags all name one image.
func saveTarFor(t *testing.T, id string, tags ...string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	manifest, _ := json.Marshal([]map[string]any{{"Config": "blobs/sha256/" + strings.TrimPrefix(fullID(id), "sha256:"), "RepoTags": tags}})
	_ = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifest))})
	_, _ = tw.Write(manifest)
	_ = tw.Close()
	p := filepath.Join(t.TempDir(), strings.NewReplacer("/", "-", ":", "-").Replace(tags[0])+".tar")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func deployClient(t *testing.T, nas *deployNAS) *dsm.Client {
	t.Helper()
	srv := httptest.NewServer(nas)
	t.Cleanup(srv.Close)
	oldCache := dsm.CacheDir
	dsm.CacheDir = t.TempDir() // the local compose backup
	t.Cleanup(func() { dsm.CacheDir = oldCache })
	client := dsm.NewClient(context.Background(), &dsm.Config{URL: srv.URL, User: "test", Password: "x"})
	if err := client.Login(""); err != nil {
		t.Fatal(err)
	}
	return client
}

// quietly runs f with stdout and stderr discarded: deploy narrates every step.
func quietly(t *testing.T, f func()) {
	t.Helper()
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()
	out, errOut := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devnull, devnull
	defer func() { os.Stdout, os.Stderr = out, errOut }()
	f()
}

const appCompose = "services:\n  app:\n    image: omni/app:latest\n    container_name: app\n"

func (n *deployNAS) indexOf(call string) int {
	for i, c := range n.calls {
		if c == call {
			return i
		}
	}
	return -1
}

func TestDeployLoadsReplacesAndCleansUp(t *testing.T) {
	nas := newDeployNAS(t, "web", appCompose, map[string]string{"omni/app:latest": "aaaaaaaaaaaa"})
	client := deployClient(t, nas)
	tarPath := saveTarFor(t, "bbbbbbbbbbbb", "omni/app:latest")
	nas.files["/docker/web/images/"+filepath.Base(tarPath)] = []byte("an older tar") // rotated to .prev

	var code int
	var err error
	quietly(t, func() {
		code, err = projectDeploy(client, options{name: "web", images: []string{tarPath}, yes: true, timeout: time.Minute})
	})
	if err != nil || code != 0 {
		t.Fatalf("deploy: code %d, err %v", code, err)
	}
	// Loaded while up, then down and up again; the compose didn't change, so no update.
	imp, clean, build := nas.indexOf("SYNO.Docker.Image.import"), nas.indexOf("SYNO.Docker.Project.clean_stream"), nas.indexOf("SYNO.Docker.Project.build_stream")
	if imp < 0 || clean < imp || build < clean {
		t.Errorf("order: import %d, clean %d, build %d in %v", imp, clean, build, nas.calls)
	}
	if nas.indexOf("SYNO.Docker.Project.update") >= 0 {
		t.Error("compose updated though it didn't change")
	}
	if got := nas.images["omni/app:latest"]; got != fullID("bbbbbbbbbbbb") {
		t.Errorf("omni/app:latest is %s", got)
	}
	if fmt.Sprint(nas.deleted) != "[aaaaaaaaaaaa]" {
		t.Errorf("deleted %v, want the replaced image", nas.deleted)
	}
	if string(nas.files["/docker/web/images/"+strings.TrimSuffix(filepath.Base(tarPath), ".tar")+".prev.tar"]) != "an older tar" {
		t.Error("the older tar wasn't kept as .prev.tar")
	}
}

func TestDeployKeepsAnOldImageAnotherTagHolds(t *testing.T) {
	// app:latest and app:1.4 share the old image; replacing latest mustn't take 1.4.
	nas := newDeployNAS(t, "web", appCompose, map[string]string{"omni/app:latest": "aaaaaaaaaaaa", "omni/app:1.4": "aaaaaaaaaaaa"})
	client := deployClient(t, nas)
	tarPath := saveTarFor(t, "bbbbbbbbbbbb", "omni/app:latest")
	quietly(t, func() {
		_, _ = projectDeploy(client, options{name: "web", images: []string{tarPath}, yes: true, timeout: time.Minute})
	})
	if len(nas.deleted) != 0 {
		t.Errorf("deleted %v; omni/app:1.4 still holds that image", nas.deleted)
	}
	if nas.images["omni/app:1.4"] != fullID("aaaaaaaaaaaa") {
		t.Error("omni/app:1.4 lost its image")
	}
}

func TestDeployComposeChangeBacksUpBeforeGoingDown(t *testing.T) {
	nas := newDeployNAS(t, "web", appCompose, map[string]string{"omni/app:latest": "aaaaaaaaaaaa", "omni/app:2": "cccccccccccc"})
	client := deployClient(t, nas)
	newCompose := filepath.Join(t.TempDir(), "compose.yml")
	_ = os.WriteFile(newCompose, []byte(strings.Replace(appCompose, "omni/app:latest", "omni/app:2", 1)), 0o600)
	var code int
	quietly(t, func() {
		code, _ = projectDeploy(client, options{name: "web", file: newCompose, yes: true, timeout: time.Minute})
	})
	if code != 0 {
		t.Fatalf("deploy: code %d", code)
	}
	var backup string
	for p := range nas.files {
		if strings.HasPrefix(p, "/docker/web/backups/") {
			backup = p
		}
	}
	uploadIdx := nas.indexOf("SYNO.FileStation.Upload.upload")
	if backup == "" || string(nas.files[backup]) != appCompose {
		t.Errorf("no backup of the old compose on the NAS (files %v)", keys(nas.files))
	}
	if clean := nas.indexOf("SYNO.Docker.Project.clean_stream"); uploadIdx < 0 || uploadIdx > clean {
		t.Errorf("backup at %d, clean at %d: the backup has to come first", uploadIdx, clean)
	}
	if !strings.Contains(nas.compose, "omni/app:2") || nas.containers["app"] != "omni/app:2" {
		t.Errorf("the new compose isn't running: %q %v", nas.compose, nas.containers)
	}
}

func TestDeployFailureLeavesOldImages(t *testing.T) {
	nas := newDeployNAS(t, "web", appCompose, map[string]string{"omni/app:latest": "aaaaaaaaaaaa"})
	client := deployClient(t, nas)
	tarPath := saveTarFor(t, "bbbbbbbbbbbb", "omni/app:latest")
	nas.failWith = 3 // the new containers exit at once
	var code int
	start := time.Now()
	quietly(t, func() {
		code, _ = projectDeploy(client, options{name: "web", images: []string{tarPath}, yes: true, timeout: time.Minute})
	})
	if code != 1 {
		t.Errorf("code %d, want 1", code)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("waited on a container that won't restart")
	}
	if len(nas.deleted) != 0 {
		t.Errorf("deleted %v after a failed deploy", nas.deleted)
	}
}

func TestDeployBlocksBeforeChangingAnything(t *testing.T) {
	nas := newDeployNAS(t, "web", appCompose, map[string]string{"omni/app:latest": "aaaaaaaaaaaa"})
	client := deployClient(t, nas)
	unused := saveTarFor(t, "dddddddddddd", "omni/other:1") // the compose doesn't run it
	for _, o := range []options{
		{name: "web", images: []string{unused}, yes: true},                                              // blocked
		{name: "web", dryRun: true, images: []string{saveTarFor(t, "bbbbbbbbbbbb", "omni/app:latest")}}, // plan only
	} {
		nas.calls = nil
		quietly(t, func() { _, _ = projectDeploy(client, o) })
		for _, c := range nas.calls {
			if !strings.HasSuffix(c, ".list") && !strings.HasSuffix(c, ".get") && !strings.HasSuffix(c, ".list_share") {
				t.Errorf("%+v: made a changing call %s", o, c)
			}
		}
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (n *deployNAS) tagged(id string) bool {
	for _, full := range n.images {
		if full == id {
			return true
		}
	}
	return false
}
