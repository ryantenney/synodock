package service

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A reinstall waits for the previous serve to finish exiting; anything still
// serving after that is refused, and a stale socket file is cleared.
func TestPrepareSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "svc") // short: a socket path fits in 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")

	old, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	go func() { time.Sleep(300 * time.Millisecond); old.Close() }() // the old serve exiting
	if err := prepareSocket(path, 2*time.Second); err != nil {
		t.Fatalf("didn't wait for the old serve: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale socket left: %v", err)
	}

	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := prepareSocket(path, 300*time.Millisecond); err == nil {
		t.Fatal("took over a socket something is serving on")
	}
	if err := prepareSocket(path, 0); err == nil {
		t.Fatal("took over a socket something is serving on (no wait)")
	}
}
