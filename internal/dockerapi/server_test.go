package dockerapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"

	"github.com/ryantenney/synodock/internal/dsm"
)

const containerID = "4f1c2b3a5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"

// fakeDSM answers the web API calls serve makes, for one container "web", and
// runs execs from /docker/ws under dash on a pty.
type fakeDSM struct {
	stopped atomic.Bool
	bin     string
	mu      sync.Mutex
	execs   map[string]string
}

func (f *fakeDSM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/docker/ws" {
		f.terminal(w, r)
		return
	}
	_ = r.ParseForm()
	api, method := r.Form.Get("api"), r.Form.Get("method")
	var data any
	switch api + "." + method {
	case "SYNO.API.Auth.login":
		data = map[string]any{"sid": "sid", "synotoken": "token"}
	case "SYNO.API.Auth.logout":
	case "SYNO.Docker.Container.list":
		// Docker's keys after DSM's, differing only in case: they mustn't win.
		status, running := "running", "true"
		if f.stopped.Load() {
			status, running = "exited", "false"
		}
		data = json.RawMessage(`{"containers": [{"name": "web", "id": "` + containerID + `", "status": "` + status + `",
			"image": "nginx:latest", "created": 1700000000, "Labels": {"tier": "front"},
			"State": {"Status": "` + status + `", "Running": ` + running + `, "StartedAt": "2026-10-26T10:00:00.123456789Z"},
			"NetworkSettings": {"Networks": {"bridge": {}}},
			"Name": "/web", "Id": "other", "Status": "Up 2 hours", "Created": "2026-10-26T10:00:00Z", "Image": "sha256:ff"}]}`)
	case "SYNO.Docker.Container.get":
		data = map[string]any{"details": map[string]any{"Id": containerID, "Name": "/web",
			"Config": map[string]any{"Tty": false}, "Mounts": []any{}, "Big": 12345678901234567}}
	case "SYNO.Docker.Container.Log.get":
		data = map[string]any{"total": 2, "logs": []any{ // newest first
			map[string]string{"created": "2026-10-26T10:00:02Z", "stream": "stderr", "text": "oops"},
			map[string]string{"created": "2026-10-26T10:00:01Z", "stream": "stdout", "text": "hello"},
		}}
	case "SYNO.Docker.Image.list":
		data = map[string]any{"images": []any{map[string]any{"id": "sha256:" + strings.Repeat("ab", 32),
			"repository": "nginx", "tags": []string{"latest"}, "created": 1700000000, "size": 187654321}}}
	case "SYNO.Docker.Container.get_process":
		data = json.RawMessage(`{"processes": [{"command": "nginx: master process", "cpu": 0.3499999940395355,
			"memory": 2936832, "memoryPercent": 0.0087328330406555403, "pid": "28444", "start": "00:50"}], "total": 1}`)
	case "SYNO.Docker.Container.stats":
		data = map[string]any{containerID: map[string]any{"cpu_stats": map[string]any{"system_cpu_usage": 100},
			"memory_stats": map[string]any{"usage": 1 << 20, "limit": 1 << 30}}}
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": map[string]int{"code": 103}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func (f *fakeDSM) terminal(w http.ResponseWriter, r *http.Request) {
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	var wmu sync.Mutex
	send := func(v ...any) {
		wmu.Lock()
		defer wmu.Unlock()
		_ = ws.WriteJSON(v)
	}
	var ptmx *os.File
	for {
		var msg []json.RawMessage
		if ws.ReadJSON(&msg) != nil {
			return
		}
		var event, arg string
		_ = json.Unmarshal(msg[0], &event)
		if len(msg) > 1 {
			_ = json.Unmarshal(msg[1], &arg)
		}
		switch event {
		case "enter":
			send("enter", true)
			send("list", []string{"web", "web"})
		case "create":
			b := make([]byte, 32)
			_, _ = rand.Read(b)
			id := hex.EncodeToString(b)
			f.mu.Lock()
			f.execs[id] = arg
			f.mu.Unlock()
			send("create", true)
			send("list", []string{"web", "web", id, "sh"})
		case "attach":
			f.mu.Lock()
			cmd := exec.Command(filepath.Join(f.bin, "sh"), "-c", f.execs[arg])
			f.mu.Unlock()
			cmd.Env = append(os.Environ(), "PATH="+f.bin+":"+os.Getenv("PATH"))
			if ptmx, err = pty.Start(cmd); err != nil {
				return
			}
			send("attach", 1503)
			go func() {
				buf := make([]byte, 4096)
				for {
					n, err := ptmx.Read(buf)
					if n > 0 {
						send("data", string(buf[:n]))
					}
					if err != nil {
						break
					}
				}
				_ = cmd.Wait()
				send("close", 1501)
			}()
		case "data":
			if ptmx != nil {
				_, _ = ptmx.Write([]byte(arg))
			}
		case "resize":
			var rows, cols uint16
			_ = json.Unmarshal(msg[1], &rows)
			_ = json.Unmarshal(msg[2], &cols)
			if ptmx != nil {
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
			}
		}
	}
}

func TestDockerCLI(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("no docker CLI")
	}
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("no dash to run the framing script")
	}
	bin := t.TempDir()
	if err := os.Symlink(dash, filepath.Join(bin, "sh")); err != nil {
		t.Fatal(err)
	}
	fake := &fakeDSM{bin: bin, execs: map[string]string{}}
	nas := httptest.NewServer(fake)
	defer nas.Close()
	t.Setenv("SYNODOCK_GZIP", "1")

	dir, err := os.MkdirTemp("", "sd") // short: a socket path has to fit in 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	client := dsm.NewClient(ctx, &dsm.Config{URL: nas.URL, User: "test", Password: "x"})
	if err := client.Login(""); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, client, ServeOptions{Socket: socket, Version: "test", Context: "synology"})
	}()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", info, err)
	}

	run := func(stdin string, args ...string) (string, string, int) {
		cmd := exec.Command(docker, append([]string{"-H", "unix://" + socket}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		}
		return stdout.String(), stderr.String(), code
	}
	check := func(name string, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}

	out, errOut, _ := run("", "ps", "--format", "{{.Names}} {{.Image}} {{.Labels}}")
	check("ps", out, "web nginx:latest tier=front\n")
	if errOut != "" {
		t.Errorf("ps stderr: %s", errOut)
	}
	out, _, _ = run("", "inspect", "--format", "{{.Name}} {{.Big}}", "web")
	check("inspect", out, "/web 12345678901234567\n")
	out, errOut, _ = run("", "logs", "web")
	check("logs stdout", out, "hello\n")
	check("logs stderr", errOut, "oops\n")
	out, _, _ = run("", "images", "--format", "{{.Repository}}:{{.Tag}} {{.ID}}")
	check("images", out, "nginx:latest "+strings.Repeat("ab", 6)+"\n")
	out, _, _ = run("", "stats", "--no-stream", "--format", "{{.Name}} {{.MemUsage}}", "web")
	check("stats", out, "web 1MiB / 1GiB\n")
	out, _, _ = run("", "top", "web")
	check("top", strings.Join(strings.Fields(out), " "), "PID %CPU %MEM RSS STIME CMD 28444 0.3 0.0 2868 00:50 nginx: master process")
	_, errOut, code := run("", "top", "web", "aux")
	if code == 0 || !strings.Contains(errOut, "ps options") {
		t.Errorf("top aux: exit %d, %q", code, errOut)
	}
	out, _, code = run("", "exec", "web", "sh", "-c", "echo a=b c; exit 3")
	check("exec", out, "a=b c\n")
	if code != 3 {
		t.Errorf("exec exit %d, want 3", code)
	}
	out, _, code = run("hello\n", "exec", "-i", "web", "cat")
	check("exec -i", out, "hello\n")
	if code != 0 {
		t.Errorf("exec -i exit %d", code)
	}
	out, _, code = run("", "exec", "-e", "X=1", "-w", "/tmp", "web", "sh", "-c", `echo "$X $(pwd)"`)
	if !strings.HasSuffix(out, "/tmp\n") || !strings.HasPrefix(out, "1 ") || code != 0 {
		t.Errorf("exec -e -w: %q, exit %d", out, code)
	}
	// -it: the CLI on a terminal of its own, 30x100.
	cmd := exec.Command(docker, "-H", "unix://"+socket, "exec", "-it", "web", "sh", "-c", "stty size; exit 4")
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}
	ttyOut, _ := io.ReadAll(tty)
	_ = cmd.Wait()
	if !strings.Contains(string(ttyOut), "30 100") || cmd.ProcessState.ExitCode() != 4 {
		t.Errorf("exec -it: %q, exit %d", ttyOut, cmd.ProcessState.ExitCode())
	}
	_, errOut, code = run("", "run", "alpine")
	if code == 0 || !strings.Contains(errOut, "not supported by the DSM adapter") {
		t.Errorf("run: exit %d, %q", code, errOut)
	}

	// events: the container stops while docker events watches.
	events := exec.Command(docker, "-H", "unix://"+socket, "events", "--format", "{{.Type}} {{.Action}} {{.Actor.Attributes.name}}")
	eventsOut, err := events.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var eventsErr strings.Builder
	events.Stderr = &eventsErr
	if err := events.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	fake.stopped.Store(true)
	lines := make(chan string)
	go func() {
		scanner := bufio.NewScanner(eventsOut)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	var seen []string
	timeout := time.After(8 * time.Second)
collect:
	for len(seen) < 2 {
		select {
		case line, ok := <-lines:
			if !ok {
				break collect
			}
			seen = append(seen, line)
		case <-timeout:
			break collect
		}
	}
	_ = events.Process.Kill()
	_ = events.Wait()
	check("events", strings.Join(seen, "|")+eventsErr.String(), "container die web|container stop web")

	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("socket left behind: %v", err)
	}
}

func TestEventsBetween(t *testing.T) {
	old := &snapshot{
		containers: []snapContainer{{"a", "web", "nginx", "running", "t1"}, {"b", "db", "pg", "running", "t1"}},
		images:     []snapImage{{"i1", []string{"nginx:latest"}}},
	}
	cur := &snapshot{
		containers: []snapContainer{{"a", "web", "nginx", "running", "t2"}, {"c", "new", "redis", "running", "t1"}},
		images:     []snapImage{{"i1", []string{"nginx:1"}}},
	}
	var got []string
	for _, e := range eventsBetween(old, cur) {
		got = append(got, e.Type+" "+e.Action+" "+e.Actor.ID)
	}
	want := "container die a|container start a|container restart a|container create c|container start c|" +
		"container destroy b|image tag i1|image untag i1"
	if strings.Join(got, "|") != want {
		t.Fatalf("got %s", strings.Join(got, "|"))
	}
}
