// Package service installs synodock serve as a per-user service the system
// starts on demand: a launchd agent with a Sockets entry on macOS, a systemd
// socket unit and service on Linux. The service manager holds the socket, starts
// serve at the first connection, and serve exits again when idle.
//
// Author: Ryan Tenney
package service

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Spec is one service: synodock serve for one NAS.
type Spec struct {
	Name   string            // the [nas.NAME]; "" for the unnamed NAS
	Exe    string            // synodock, by absolute path
	Args   []string          // after Exe
	Socket string            // where the service manager listens
	Env    map[string]string // PATH and the like: services don't get a login shell's
}

// stopWait is how long a reinstall gives the previous install's serve to exit.
var stopWait = 10 * time.Second

// prepareSocket makes the socket's directory and clears a stale socket file,
// refusing if something is serving on it. After stopping a previous install,
// wait gives its serve time to finish exiting.
func prepareSocket(path string, wait time.Duration) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	for deadline := time.Now().Add(wait); ; time.Sleep(100 * time.Millisecond) {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			return fmt.Errorf("something is already serving on %s (a synodock serve still running?); stop it first", path)
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
