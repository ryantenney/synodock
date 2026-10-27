package terminal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
}

func (f *fakeTerminal) command(command string) *exec.Cmd {
	if image := os.Getenv("SYNODOCK_TEST_IMAGE"); image != "" {
		return exec.Command("docker", "run", "--rm", "-i", "-t", "-v", f.data+":"+f.data, image, "sh", "-c", command)
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
				_, _ = ptmx.Write([]byte(text))
			}
		}
	}
}

func fakeClient(t *testing.T) (*dsm.Client, string) { return fakeClientWith(t, false) }

func fakeClientWith(t *testing.T, reorder bool) (*dsm.Client, string) {
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
	srv := httptest.NewServer(&fakeTerminal{execs: map[string]string{}, bin: bin, data: data, reorder: reorder})
	t.Cleanup(srv.Close)
	return dsm.NewClient(context.Background(), &dsm.Config{URL: srv.URL, User: "test"}), data
}

func runFramed(t *testing.T, client *dsm.Client, argv []string, o Options) ([]byte, int) {
	t.Helper()
	s, err := Open(context.Background(), client, "ctr", CommandFor(argv, o), 24, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var out bytes.Buffer
	code, err := PumpFramed(context.Background(), s, func(b []byte) error { out.Write(b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), code
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
	if _, err := PumpFramed(context.Background(), s, func(b []byte) error {
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
	if _, err := PumpFramed(ctx, s, func([]byte) error { return nil }); err != context.DeadlineExceeded {
		t.Fatalf("got %v", err)
	}
}
