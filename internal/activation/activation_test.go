//go:build unix

package activation

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSystemdListeners hands a listening socket to a child as systemd does (fd
// 3, LISTEN_FDS=1, LISTEN_PID = the child's pid) and has it accept a connection.
func TestSystemdListeners(t *testing.T) {
	if os.Getenv("ACTIVATION_CHILD") == "1" {
		ls, err := systemdListeners()
		if err != nil || len(ls) != 1 {
			fmt.Println("child:", ls, err)
			os.Exit(1)
		}
		if os.Getenv("LISTEN_FDS") != "" {
			fmt.Println("child: LISTEN_FDS left set")
			os.Exit(1)
		}
		conn, err := ls[0].Accept()
		if err != nil {
			fmt.Println("child:", err)
			os.Exit(1)
		}
		fmt.Fprintln(conn, "hello from the child")
		conn.Close()
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "act") // short: a socket path fits in 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false) // the socket outlives us, as a service manager's does
	f, err := l.(*net.UnixListener).File()
	if err != nil {
		t.Fatal(err)
	}
	// sh's $$ is the pid exec hands on to the test binary.
	cmd := exec.Command("sh", "-c", `LISTEN_PID=$$ LISTEN_FDS=1 exec "$0" -test.run=TestSystemdListeners`, os.Args[0])
	cmd.Env = append(os.Environ(), "ACTIVATION_CHILD=1")
	cmd.ExtraFiles = []*os.File{f} // fd 3
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	l.Close() // the child holds the socket now
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(conn).ReadString('\n')
	conn.Close()
	if err := cmd.Wait(); err != nil || line != "hello from the child\n" {
		t.Fatalf("got %q, %v: %s", line, err, out.String())
	}
}

func TestNoActivation(t *testing.T) {
	t.Setenv("LISTEN_PID", "1")
	t.Setenv("LISTEN_FDS", "1")
	if ls, err := systemdListeners(); ls != nil || err != nil {
		t.Fatalf("took another process's sockets: %v %v", ls, err)
	}
	if ls, err := Listeners("Listeners"); ls != nil || err != nil {
		t.Fatalf("outside a service manager: %v %v", ls, err)
	}
}
