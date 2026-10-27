package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const Manager = "launchd"

// Label is the launchd job's name.
func Label(name string) string {
	if name == "" {
		return "com.github.ryantenney.synodock"
	}
	return "com.github.ryantenney.synodock." + name
}

func plistPath(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", Label(name)+".plist")
}

// LogPath is where serve's messages go.
func LogPath(name string) string {
	home, _ := os.UserHomeDir()
	file := "synodock.log"
	if name != "" {
		file = "synodock-" + name + ".log"
	}
	return filepath.Join(home, "Library", "Logs", file)
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// plist is the agent: no RunAtLoad or KeepAlive, so launchd starts serve only
// when something connects to the socket, and again after it exits idle.
func plist(s Spec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + esc(Label(s.Name)) + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range append([]string{s.Exe}, s.Args...) {
		b.WriteString("\t\t<string>" + esc(a) + "</string>\n")
	}
	b.WriteString(`	</array>
	<key>Sockets</key>
	<dict>
		<key>Listeners</key>
		<dict>
			<key>SockPathName</key>
			<string>` + esc(s.Socket) + `</string>
			<key>SockPathMode</key>
			<integer>384</integer>
		</dict>
	</dict>
	<key>EnvironmentVariables</key>
	<dict>
`)
	for _, k := range sortedKeys(s.Env) {
		b.WriteString("\t\t<key>" + esc(k) + "</key>\n\t\t<string>" + esc(s.Env[k]) + "</string>\n")
	}
	b.WriteString(`	</dict>
	<key>StandardErrorPath</key>
	<string>` + esc(LogPath(s.Name)) + `</string>
	<key>ProcessType</key>
	<string>Background</string>
</dict>
</plist>
`)
	return b.String()
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("launchctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Install writes the agent and loads it, replacing one already there.
func Install(s Spec) ([]string, error) {
	var wait time.Duration
	if _, err := os.Stat(plistPath(s.Name)); err == nil { // a previous install: stop it, and its serve
		_, _ = launchctl("bootout", domain()+"/"+Label(s.Name))
		wait = stopWait
	}
	if err := prepareSocket(s.Socket, wait); err != nil {
		return nil, err
	}
	path := plistPath(s.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(LogPath(s.Name)), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(plist(s)), 0o644); err != nil {
		return nil, err
	}
	if _, err := launchctl("bootstrap", domain(), path); err != nil {
		return nil, err
	}
	return []string{path}, nil
}

// Uninstall unloads the agent and removes it and its socket.
func Uninstall(name string, socket string) ([]string, error) {
	path := plistPath(name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("no service for this NAS (%s doesn't exist)", path)
	}
	_, _ = launchctl("bootout", domain()+"/"+Label(name))
	if err := os.Remove(path); err != nil {
		return nil, err
	}
	_ = os.Remove(socket)
	return []string{path}, nil
}

var (
	stateLine = regexp.MustCompile(`(?m)^\s*state = (.+)$`)
	pidLine   = regexp.MustCompile(`(?m)^\s*pid = (\d+)$`)
)

// Status says whether the agent is loaded and serve running.
func Status(name string) (string, error) {
	if _, err := os.Stat(plistPath(name)); os.IsNotExist(err) {
		return "not installed", nil
	}
	out, err := launchctl("print", domain()+"/"+Label(name))
	if err != nil {
		return "installed (" + plistPath(name) + ") but not loaded", nil
	}
	state := "loaded"
	if m := stateLine.FindStringSubmatch(out); m != nil {
		state = "loaded, " + m[1]
	}
	if m := pidLine.FindStringSubmatch(out); m != nil {
		state += ", pid " + m[1]
	} else {
		state += "; starts on the next connection"
	}
	return state + "; log " + LogPath(name), nil
}
