package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlist(t *testing.T) {
	text := plist(Spec{Name: "home", Exe: "/opt/synodock", Args: []string{"--nas", "home", "serve"},
		Socket: "/Users/x/.cache/synodock/home.sock", Env: map[string]string{"PATH": "/usr/bin:/a&b"}})
	path := filepath.Join(t.TempDir(), "x.plist")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("%s\n%s", out, text)
	}
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"Label":"com.github.ryantenney.synodock.home"`, `"SockPathMode":384`,
		`"ProgramArguments":["\/opt\/synodock","--nas","home","serve"]`, `"PATH":"\/usr\/bin:\/a&b"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(string(out), "KeepAlive") || strings.Contains(string(out), "RunAtLoad") {
		t.Error("the agent would run without a connection")
	}
}
