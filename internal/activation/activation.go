//go:build unix

// Package activation picks up a listening socket a service manager opened for
// us (launchd's Sockets on macOS, systemd's socket units on Linux), so synodock
// serve can start when something first connects.
//
// Author: Ryan Tenney
package activation

import (
	"net"
	"os"
	"strconv"
	"syscall"
)

const listenFDsStart = 3 // SD_LISTEN_FDS_START

// systemdListeners are the sockets systemd passed (LISTEN_FDS from fd 3 on),
// or nil when it didn't start us. The variables are cleared so a child process
// doesn't take them for its own.
func systemdListeners() ([]net.Listener, error) {
	pid, _ := strconv.Atoi(os.Getenv("LISTEN_PID"))
	n, _ := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if pid != os.Getpid() || n <= 0 {
		return nil, nil
	}
	for _, env := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		os.Unsetenv(env)
	}
	var listeners []net.Listener
	for fd := listenFDsStart; fd < listenFDsStart+n; fd++ {
		syscall.CloseOnExec(fd)
		f := os.NewFile(uintptr(fd), "LISTEN_FD_"+strconv.Itoa(fd))
		l, err := net.FileListener(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, l)
	}
	return listeners, nil
}
