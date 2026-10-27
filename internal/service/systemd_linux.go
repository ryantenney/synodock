package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const Manager = "systemd"

// Label is the systemd units' name, without .socket or .service.
func Label(name string) string {
	if name == "" {
		return "synodock"
	}
	return "synodock-" + name
}

func unitDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user")
}

// LogPath is where serve's messages go: the journal.
func LogPath(name string) string { return "journalctl --user -u " + Label(name) + ".service" }

// quote is systemd's quoting for an ExecStart= or Environment= word.
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", "$$", "%", "%%")
	return `"` + r.Replace(s) + `"`
}

func units(s Spec) (socket, service string) {
	what := "synodock Docker API"
	if s.Name != "" {
		what += " for " + s.Name
	}
	socket = fmt.Sprintf(`[Unit]
Description=%s (socket)

[Socket]
ListenStream=%s
SocketMode=0600
DirectoryMode=0700

[Install]
WantedBy=sockets.target
`, what, s.Socket)
	var exec []string
	for _, a := range append([]string{s.Exe}, s.Args...) {
		exec = append(exec, quote(a))
	}
	var env strings.Builder
	for _, k := range sortedKeys(s.Env) {
		env.WriteString("Environment=" + quote(k+"="+s.Env[k]) + "\n")
	}
	service = fmt.Sprintf(`[Unit]
Description=%s
Requires=%s.socket

[Service]
ExecStart=%s
%s`, what, Label(s.Name), strings.Join(exec, " "), env.String())
	return socket, service
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func paths(name string) (string, string) {
	return filepath.Join(unitDir(), Label(name)+".socket"), filepath.Join(unitDir(), Label(name)+".service")
}

// Install writes the units, enables the socket and starts listening, replacing
// an install already there (and stopping its serve, to pick up a new binary).
func Install(s Spec) ([]string, error) {
	var wait time.Duration
	if socketPath, _ := paths(s.Name); fileExists(socketPath) { // a previous install: stop it, and its serve
		_ = systemctl("stop", Label(s.Name)+".socket", Label(s.Name)+".service")
		wait = stopWait
	}
	if err := prepareSocket(s.Socket, wait); err != nil {
		return nil, err
	}
	socketUnit, serviceUnit := units(s)
	socketPath, servicePath := paths(s.Name)
	if err := os.MkdirAll(unitDir(), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(socketPath, []byte(socketUnit), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(servicePath, []byte(serviceUnit), 0o644); err != nil {
		return nil, err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return nil, err
	}
	if err := systemctl("enable", "--now", Label(s.Name)+".socket"); err != nil {
		return nil, err
	}
	return []string{socketPath, servicePath}, nil
}

// Uninstall stops and removes the units and the socket.
func Uninstall(name string, socket string) ([]string, error) {
	socketPath, servicePath := paths(name)
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("no service for this NAS (%s doesn't exist)", socketPath)
	}
	_ = systemctl("disable", "--now", Label(name)+".socket")
	_ = systemctl("stop", Label(name)+".service")
	for _, p := range []string{socketPath, servicePath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	_ = systemctl("daemon-reload")
	_ = os.Remove(socket)
	return []string{socketPath, servicePath}, nil
}

// Status says whether the socket is listening and serve running.
func Status(name string) (string, error) {
	socketPath, _ := paths(name)
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		return "not installed", nil
	}
	state := func(unit string) string {
		out, _ := exec.Command("systemctl", "--user", "is-active", unit).Output()
		return strings.TrimSpace(string(out))
	}
	return fmt.Sprintf("socket %s, serve %s; log: %s", state(Label(name)+".socket"), state(Label(name)+".service"),
		LogPath(name)), nil
}
