package terminal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"

	"github.com/ryantenney/synodock/internal/dsm"
)

// fakeTerminal plays Container Manager's /docker/ws: "create" makes an exec of
// `sh -c <command>` (sh's splitting stands in for DSM's), started on a pty at
// "attach", each pty read sent as one "data" message.
//
// It runs under dash, as in a Debian image (macOS's /bin/sh is bash 3.2, which
// can't parse a case inside $(...)), or with SYNODOCK_TEST_IMAGE=alpine, say, in a
// throwaway container of that image, under its own sh, dd, od and awk.
type fakeTerminal struct {
	mu      sync.Mutex
	execs   map[string]string
	bin     string // a directory whose sh is dash
	data    string // shared with the container
	reorder bool   // swap consecutive pty reads, as the relay can
	mangle  bool   // lose, swap and damage input records (synodock-agent's stdin)
	inputs  int    // input records seen, for mangle
	held    string // an input record mangle holds back, to go after the next
}

func (f *fakeTerminal) command(command string) *exec.Cmd {
	if image := os.Getenv("SYNODOCK_TEST_IMAGE"); image != "" {
		args := []string{"run", "--rm", "-i", "-t", "-v", f.data + ":" + f.data}
		if agent := os.Getenv("SYNODOCK_TEST_AGENT"); agent != "" { // synodock-agent, for the image's arch
			args = append(args, "-v", agent+":"+AgentPath+":ro")
		}
		return exec.Command("docker", append(args, image, "sh", "-c", command)...)
	}
	cmd := exec.Command(filepath.Join(f.bin, "sh"), "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+f.bin+":"+os.Getenv("PATH"))
	return cmd
}

func (f *fakeTerminal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		var event string
		_ = json.Unmarshal(msg[0], &event)
		switch event {
		case "enter":
			send("enter", true)
			var role string
			_ = json.Unmarshal(msg[2], &role)
			if role == "monitor" {
				send("list", []string{"ctr", "ctr"})
			}
		case "create":
			var command string
			_ = json.Unmarshal(msg[1], &command)
			b := make([]byte, 32)
			_, _ = rand.Read(b)
			id := hex.EncodeToString(b)
			f.mu.Lock()
			f.execs[id] = command
			f.mu.Unlock()
			send("create", true)
			send("list", []string{"ctr", "ctr", id, "sh"})
		case "attach":
			var id string
			_ = json.Unmarshal(msg[1], &id)
			f.mu.Lock()
			command := f.execs[id]
			f.mu.Unlock()
			cmd := f.command(command)
			ptmx, err = pty.Start(cmd)
			if err != nil {
				send("attach", attachFailed)
				return
			}
			send("attach", attachOK)
			reads := make(chan string)
			go func() {
				buf := make([]byte, 4096)
				for {
					n, err := ptmx.Read(buf)
					if n > 0 {
						reads <- string(buf[:n])
					}
					if err != nil {
						close(reads)
						return
					}
				}
			}()
			go func() {
				var held []string // reorder: a read waits briefly, to go out after the next
				for {
					if len(held) == 0 {
						read, ok := <-reads
						if !ok {
							break
						}
						if f.reorder {
							held = []string{read}
						} else {
							send("data", read)
						}
						continue
					}
					select {
					case read, ok := <-reads:
						if ok {
							send("data", read)
						}
						send("data", held[0])
						held = nil
						if !ok {
							break
						}
						continue
					case <-time.After(20 * time.Millisecond):
						send("data", held[0])
						held = nil
						continue
					}
					break
				}
				_ = cmd.Wait()
				send("close", closeEnded)
			}()
		case "resize":
			var rows, cols uint16
			_ = json.Unmarshal(msg[1], &rows)
			_ = json.Unmarshal(msg[2], &cols)
			if ptmx != nil {
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
			}
		case "data":
			var text string
			_ = json.Unmarshal(msg[1], &text)
			if ptmx != nil {
				for _, t := range f.mangleInput(text) {
					_, _ = ptmx.Write([]byte(t))
				}
			}
		}
	}
}

// mangleInput is what the pty gets for a data message: with mangle, of input records
// (they start with the mark) every 7th is lost, every 5th goes after the one following
// it, and every 11th has a byte changed.
func (f *fakeTerminal) mangleInput(text string) []string {
	if !f.mangle || !strings.HasPrefix(text, "\x1e") {
		return []string{text}
	}
	f.inputs++
	switch n := f.inputs; {
	case n%7 == 0:
		return nil
	case n%5 == 0 && f.held == "":
		f.held = text
		return nil
	case n%11 == 0:
		b := []byte(text)
		b[len(b)/2] ^= 1
		text = string(b)
	}
	out := []string{text}
	if f.held != "" {
		out, f.held = append(out, f.held), ""
	}
	return out
}

func fakeClient(t *testing.T) (*dsm.Client, string) { return fakeClientWith(t, false) }

func fakeClientWith(t *testing.T, reorder bool) (*dsm.Client, string) {
	return fakeClientFor(t, &fakeTerminal{reorder: reorder})
}

func fakeClientFor(t *testing.T, f *fakeTerminal) (*dsm.Client, string) {
	t.Helper()
	bin := t.TempDir()
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("no dash to run the framing script")
	}
	if err := os.Symlink(dash, filepath.Join(bin, "sh")); err != nil {
		t.Fatal(err)
	}
	data, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.execs, f.bin, f.data = map[string]string{}, bin, data
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return dsm.NewClient(context.Background(), &dsm.Config{URL: srv.URL, User: "test"}), data
}

func runFramed(t *testing.T, client *dsm.Client, argv []string, o Options) ([]byte, int) {
	t.Helper()
	out, _, code := runFramedStreams(t, client, argv, o)
	return out, code
}

// runFramedStreams is runFramed with stdout and stderr apart.
func runFramedStreams(t *testing.T, client *dsm.Client, argv []string, o Options) ([]byte, []byte, int) {
	t.Helper()
	s, err := Open(context.Background(), client, "ctr", CommandFor(argv, o), 24, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var out, errOut bytes.Buffer
	code, err := PumpFramed(context.Background(), s, func(stream int, b []byte) error {
		if stream == Stderr {
			errOut.Write(b)
		} else {
			out.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), errOut.Bytes(), code
}

// runFramedInput is runFramedStreams with stdin, and PumpFramedInput's error.
func runFramedInput(t *testing.T, client *dsm.Client, argv []string, o Options, stdin io.Reader) ([]byte, []byte, int, error) {
	t.Helper()
	o.FramedStdin = true
	s, err := Open(context.Background(), client, "ctr", CommandFor(argv, o), 24, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var out, errOut bytes.Buffer
	code, err := PumpFramedInput(context.Background(), s, stdin, func(stream int, b []byte) error {
		if stream == Stderr {
			errOut.Write(b)
		} else {
			out.Write(b)
		}
		return nil
	})
	return out.Bytes(), errOut.Bytes(), code, err
}

// Without synodock-agent, stdin can't go framed: the script stops before the command.
func TestFramedStdinNeedsTheAgent(t *testing.T) {
	if os.Getenv("SYNODOCK_TEST_AGENT") != "" {
		t.Skip("an agent is mounted")
	}
	client, dir := fakeClient(t)
	marker := filepath.Join(dir, "ran")
	_, _, _, err := runFramedInput(t, client, []string{"touch", marker}, Options{}, strings.NewReader("x"))
	if !errors.Is(err, ErrNoAgent) {
		t.Fatalf("err %v, want ErrNoAgent", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command ran")
	}
}

func TestFramedThroughPty(t *testing.T) {
	client, dir := fakeClient(t)
	data := make([]byte, 200000)
	_, _ = rand.Read(data)
	path := filepath.Join(dir, "random")
	if err := os.WriteFile(path, append(data, bytes.Repeat([]byte("text\n"), 20000)...), 0o600); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(path)
	for _, gz := range []bool{false, true} {
		out, code := runFramed(t, client, []string{"sh", "-c", "echo a=b c; exit 7"}, Options{Gzip: gz})
		if string(out) != "a=b c\n" || code != 7 {
			t.Errorf("gzip=%v: got %q, status %d", gz, out, code)
		}
		out, code = runFramed(t, client, []string{"cat", path}, Options{Gzip: gz})
		if !bytes.Equal(out, want) || code != 0 {
			t.Errorf("gzip=%v: cat gave %d bytes (want %d), status %d", gz, len(out), len(want), code)
		}
	}
}

// The relay delivering a record's pieces out of order: framing puts them back.
func TestFramedReordered(t *testing.T) {
	client, _ := fakeClientWith(t, true)
	var want strings.Builder
	for i := 1; i <= 20000; i++ {
		fmt.Fprintln(&want, i)
	}
	for _, gz := range []bool{false, true} {
		out, code := runFramed(t, client, []string{"seq", "20000"}, Options{Gzip: gz})
		if string(out) != want.String() || code != 0 {
			t.Errorf("gzip=%v: %d bytes, status %d", gz, len(out), code)
		}
	}
}

func TestFramedSlowOutputIsLive(t *testing.T) {
	client, _ := fakeClient(t)
	s, err := Open(context.Background(), client, "ctr",
		CommandFor([]string{"sh", "-c", "echo one; sleep 2; echo two"}, Options{Gzip: true}), 24, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Now()
	var firstAt time.Duration
	var out bytes.Buffer
	if _, err := PumpFramed(context.Background(), s, func(_ int, b []byte) error {
		if firstAt == 0 {
			firstAt = time.Since(start)
		}
		out.Write(b)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "one\ntwo\n" {
		t.Fatalf("got %q", out.String())
	}
	if total := time.Since(start); firstAt > total-time.Second {
		t.Fatalf("first output came at %s of %s: held back", firstAt, total)
	}
}

func TestDetach(t *testing.T) {
	client, _ := fakeClient(t)
	out, code := runFramed(t, client, []string{"nosuchcommand"}, Options{Detach: true})
	if code != 127 || !strings.Contains(string(out), "not found") {
		t.Fatalf("got %q, status %d", out, code)
	}
	start := time.Now()
	if _, code := runFramed(t, client, []string{"sleep", "5"}, Options{Detach: true}); code != 0 || time.Since(start) > 3*time.Second {
		t.Fatalf("status %d after %s", code, time.Since(start))
	}
}

func TestUnframedStdin(t *testing.T) {
	client, _ := fakeClient(t)
	s, err := Open(context.Background(), client, "ctr",
		CommandFor([]string{"sh", "-c", "cat; exit 3"}, Options{Unframed: true, Stdin: true}), 24, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var out strings.Builder
	code, known, err := PumpUnframed(context.Background(), s, func(t string) error { out.WriteString(t); return nil },
		strings.NewReader("hello\nworld"), true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello\nworld" || !known || code != 3 {
		t.Fatalf("got %q, known=%v, code %d", out.String(), known, code)
	}
}

func TestInterruptDeletesExec(t *testing.T) {
	client, _ := fakeClient(t)
	s, err := Open(context.Background(), client, "ctr", CommandFor([]string{"sleep", "30"}, Options{}), 24, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := PumpFramed(ctx, s, func(int, []byte) error { return nil }); err != context.DeadlineExceeded {
		t.Fatalf("got %v", err)
	}
}

func TestFramedKeepsStderrApart(t *testing.T) {
	client, dir := fakeClient(t)
	data := make([]byte, 150000)
	_, _ = rand.Read(data)
	path := filepath.Join(dir, "random")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Both streams at once and at full speed: lines interleave in the container, so any
	// mixing between them, or any loss or reordering within one, shows up.
	script := `i=0; while [ $i -lt 3000 ]; do echo out$i; echo err$i >&2; i=$((i+1)); done
cat "$1" & cat "$1" >&2 & wait; exit 3`
	var wantOut, wantErr bytes.Buffer
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&wantOut, "out%d\n", i)
		fmt.Fprintf(&wantErr, "err%d\n", i)
	}
	wantOut.Write(data)
	wantErr.Write(data)
	for _, gz := range []bool{false, true} {
		out, errOut, code := runFramedStreams(t, client, []string{"sh", "-c", script, "sh", path}, Options{Gzip: gz})
		if code != 3 {
			t.Errorf("gzip=%v: exit %d, want 3", gz, code)
		}
		if !bytes.Equal(out, wantOut.Bytes()) {
			t.Errorf("gzip=%v: stdout differs (%d bytes, want %d)", gz, len(out), wantOut.Len())
		}
		if !bytes.Equal(errOut, wantErr.Bytes()) {
			t.Errorf("gzip=%v: stderr differs (%d bytes, want %d)", gz, len(errOut), wantErr.Len())
		}
	}
}

func TestUser(t *testing.T) {
	client, _ := fakeClient(t)
	if os.Getenv("SYNODOCK_TEST_IMAGE") == "" {
		if os.Geteuid() == 0 {
			t.Skip("running as root here: the refusal can't be seen")
		}
		// Not root, as these tests run outside a container: -u refuses, and says why.
		_, errOut, code := runFramedStreams(t, client, []string{"id", "-un"}, Options{User: "nobody"})
		if code != 126 || !strings.Contains(string(errOut), "needs the exec to run as root") {
			t.Errorf("exit %d, stderr %q", code, errOut)
		}
		return
	}
	// In the image (as root): by name, by uid, and as uid:gid.
	for _, tc := range []struct{ user, want string }{
		{"nobody", "nobody"},
		{"65534", "65534"},
		{"65534:65534", "65534 65534"},
	} {
		argv := []string{"sh", "-c", `case $1 in *:*) echo "$(id -u) $(id -g)";; *[!0-9]*) id -un;; *) id -u;; esac`, "sh", tc.user}
		out, errOut, code := runFramedStreams(t, client, argv, Options{User: tc.user})
		if code == 127 && strings.Contains(string(errOut), "USER:GROUP needs setpriv") {
			continue // an image with only su (busybox): refused, and said why
		}
		if code != 0 || strings.TrimSpace(string(out)) != tc.want {
			t.Errorf("-u %s: exit %d, stdout %q, stderr %q; want %q", tc.user, code, out, errOut, tc.want)
		}
	}
}

// With synodock-agent mounted, it runs the command (its own message for a missing
// command, not sh's); without it, the script does.
func TestAgentRunsTheCommand(t *testing.T) {
	if os.Getenv("SYNODOCK_TEST_AGENT") == "" {
		t.Skip("set SYNODOCK_TEST_AGENT (and SYNODOCK_TEST_IMAGE) to a synodock-agent build")
	}
	client, _ := fakeClient(t)
	_, errOut, code := runFramedStreams(t, client, []string{"no-such-command"}, Options{Gzip: true})
	if code != 127 || !strings.HasPrefix(string(errOut), "synodock exec: no-such-command:") {
		t.Errorf("exit %d, stderr %q: not the agent", code, errOut)
	}
	for _, gz := range []bool{true, false} {
		out, errOut, code := runFramedStreams(t, client, []string{"sh", "-c", "seq 20000; seq 3 >&2; exit 4"}, Options{Gzip: gz})
		if code != 4 || !strings.HasSuffix(string(out), "19999\n20000\n") || string(errOut) != "1\n2\n3\n" {
			t.Errorf("gzip %v: exit %d, %d bytes of output, stderr %q", gz, code, len(out), errOut)
		}
	}
	// The script's switches stay out of the command's environment.
	out, _, _ := runFramedStreams(t, client, []string{"env"}, Options{Gzip: true})
	if strings.Contains(string(out), "SD_") {
		t.Errorf("env: %q", out)
	}
}

// Sent with the exec (no mount), synodock-agent runs the command and leaves nothing in
// /tmp; without base64 the script runs it as before.
func TestAgentSentWithTheExec(t *testing.T) {
	if os.Getenv("SYNODOCK_TEST_IMAGE") == "" || os.Getenv("SYNODOCK_TEST_AGENT") != "" {
		t.Skip("set SYNODOCK_TEST_IMAGE (and not SYNODOCK_TEST_AGENT, which mounts one)")
	}
	arch := runtime.GOARCH // Docker runs images for the host's arch
	client, _ := fakeClient(t)
	_, errOut, code := runFramedStreams(t, client, []string{"no-such-command"}, Options{Gzip: true, AgentArch: arch})
	if code != 127 || !strings.HasPrefix(string(errOut), "synodock exec: no-such-command:") {
		t.Errorf("exit %d, stderr %q: not the agent", code, errOut)
	}
	out, _, code := runFramedStreams(t, client, []string{"sh", "-c", "seq 20000; ls -A /tmp; env | grep -c ^SD_; exit 3"},
		Options{Gzip: true, AgentArch: arch})
	if code != 3 || !strings.HasSuffix(string(out), "19999\n20000\n0\n") {
		t.Errorf("exit %d, output ends %q", code, out[max(0, len(out)-40):])
	}
	// Without base64 (PATH without it), the script.
	_, errOut, code = runFramedStreams(t, client, []string{"sh", "-c", "PATH=/nowhere exec no-such-command"},
		Options{AgentArch: arch})
	if code != 127 || strings.HasPrefix(string(errOut), "synodock exec:") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

// Stdin through synodock-agent: whole and in order, paced to the command, ending with
// EOF; and a command that stops reading early doesn't hang it.
func TestAgentTakesStdin(t *testing.T) {
	o := Options{Gzip: true}
	switch {
	case os.Getenv("SYNODOCK_TEST_AGENT") != "":
	case os.Getenv("SYNODOCK_TEST_IMAGE") != "":
		o.AgentArch = runtime.GOARCH
	default:
		t.Skip("set SYNODOCK_TEST_IMAGE (and optionally SYNODOCK_TEST_AGENT)")
	}
	client, _ := fakeClient(t)
	data := make([]byte, 1<<20)
	_, _ = rand.Read(data)
	sum := sha256.Sum256(data)
	out, errOut, code, err := runFramedInput(t, client, []string{"sh", "-c", "sha256sum; exit 5"}, o, bytes.NewReader(data))
	if err != nil || code != 5 || !strings.HasPrefix(string(out), hex.EncodeToString(sum[:])) {
		t.Errorf("sha256sum: err %v, exit %d, out %q, stderr %q", err, code, out, errOut)
	}
	// A slow reader: everything still arrives.
	out, _, code, err = runFramedInput(t, client, []string{"sh", "-c", "sleep 1; wc -c"}, o, bytes.NewReader(data[:200000]))
	if err != nil || code != 0 || strings.TrimSpace(string(out)) != "200000" {
		t.Errorf("slow reader: err %v, exit %d, out %q", err, code, out)
	}
	// No input at all.
	out, _, code, err = runFramedInput(t, client, []string{"wc", "-c"}, o, strings.NewReader(""))
	if err != nil || code != 0 || strings.TrimSpace(string(out)) != "0" {
		t.Errorf("empty: err %v, exit %d, out %q", err, code, out)
	}
	// Input lost, reordered and damaged on the way: sent again, still whole.
	mangled, _ := fakeClientFor(t, &fakeTerminal{mangle: true})
	out, _, code, err = runFramedInput(t, mangled, []string{"sha256sum"}, o, bytes.NewReader(data[:300000]))
	part := sha256.Sum256(data[:300000])
	if err != nil || code != 0 || !strings.HasPrefix(string(out), hex.EncodeToString(part[:])) {
		t.Errorf("mangled: err %v, exit %d, out %q", err, code, out)
	}
	// Stops reading after 10 bytes.
	out, _, code, err = runFramedInput(t, client, []string{"head", "-c", "10"}, o, bytes.NewReader(data))
	if err != nil || code != 0 || !bytes.Equal(out, data[:10]) {
		t.Errorf("head: err %v, exit %d, %d bytes", err, code, len(out))
	}
}
