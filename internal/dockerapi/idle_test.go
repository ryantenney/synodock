package dockerapi

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ryantenney/synodock/internal/dsm"
)

// With IdleExit, serve stays up while a request is open and exits once none has
// been for that long, leaving the service manager's socket in place.
func TestIdleExit(t *testing.T) {
	dir, err := os.MkdirTemp("", "idle") // short: a socket path fits in 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	socket := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false) // launchd's or systemd's, not ours
	client := dsm.NewClient(context.Background(), &dsm.Config{URL: "https://nas.invalid", User: "u"})
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), client, ServeOptions{Socket: socket, Listener: l,
			IdleExit: 400 * time.Millisecond, Context: "synology"})
	}()

	// A docker load whose upload never finishes: open well past the idle time.
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("POST /images/load HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\npartial"))
	time.Sleep(1500 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("exited with a request open: %v", err)
	default:
	}
	conn.Close()

	httpc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	if resp, err := httpc.Get("http://x/_ping"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("didn't exit when idle")
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("the socket was removed: %v", err)
	}
}

// After macOS refused the local network, serve exits as soon as no request is
// open, idle timeout or not, so the next connection starts a process that may
// have the permission by then.
func TestExitAfterLocalNetworkDenied(t *testing.T) {
	s := &server{onDemand: true, lastDone: time.Now()}
	srv := &http.Server{}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = srv.Serve(l); close(done) }()
	go s.exitWhenIdle(srv, 0)
	release := s.busy() // a request still open
	s.denied.Store(true)
	select {
	case <-done:
		t.Fatal("exited with a request open")
	case <-time.After(1500 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("didn't exit after the refusal")
	}
}
