package dsm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testConfig = `# my NAS
url = "https://top:5001/"
user = "topuser"
password_command = "echo top"

[nas.home]
url = "https://home:5001"
user = "docker"
otp_command = "echo 123456"

[nas.office]
url = "https://office:5001"
user = "svc"
keychain = "work"
`

func withConfig(t *testing.T, text string) {
	t.Helper()
	old := ConfigDir
	t.Cleanup(func() { ConfigDir = old })
	ConfigDir = t.TempDir()
	if text != "" {
		if err := os.WriteFile(ConfigPath(), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, env := range []string{"SYNODOCK_URL", "SYNODOCK_USER", "SYNODOCK_NAS", "SYNODOCK_KEYCHAIN",
		"SYNODOCK_PASSWORD", "SYNODOCK_PASSWORD_COMMAND", "SYNODOCK_OTP_COMMAND"} {
		t.Setenv(env, "")
	}
}

func load(t *testing.T, sel Selection) *Config {
	t.Helper()
	c, err := LoadConfig(sel)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSelectionOrder(t *testing.T) {
	withConfig(t, testConfig)
	c := load(t, Selection{})
	if c.Name != "" || c.URL != "https://top:5001" || c.User != "topuser" || c.PasswordCommand != "echo top" {
		t.Fatalf("unnamed: %+v", c)
	}
	if err := SetDefault("home"); err != nil {
		t.Fatal(err)
	}
	if c := load(t, Selection{}); c.Name != "home" || c.OTPCommand != "echo 123456" || c.Keychain != "synodock" {
		t.Fatalf("default: %+v", c)
	}
	t.Setenv("SYNODOCK_NAS", "office")
	if c := load(t, Selection{}); c.Name != "office" || c.Keychain != "work" {
		t.Fatalf("SYNODOCK_NAS: %+v", c)
	}
	t.Setenv("SYNODOCK_URL", "https://env:5001")
	t.Setenv("SYNODOCK_PASSWORD", "pw")
	if c := load(t, Selection{}); c.Name != "" || c.URL != "https://env:5001" || c.User != "topuser" || c.Password != "pw" {
		t.Fatalf("SYNODOCK_URL: %+v", c)
	}
	// A profile is self-contained: the environment's password doesn't go to it.
	if c := load(t, Selection{NAS: "home"}); c.Name != "home" || c.Password != "" || c.URL != "https://home:5001" {
		t.Fatalf("--nas: %+v", c)
	}
	if c := load(t, Selection{Host: "https://admin@adhoc:5001/"}); c.URL != "https://adhoc:5001" || c.User != "admin" || c.Password != "pw" {
		t.Fatalf("--host: %+v", c)
	}
	if _, err := LoadConfig(Selection{Host: "https://x", NAS: "home"}); err == nil {
		t.Fatal("--host and --nas together")
	}
	if _, err := LoadConfig(Selection{NAS: "nope"}); err == nil || !strings.Contains(err.Error(), "home, office") {
		t.Fatalf("unknown NAS: %v", err)
	}
	if _, err := LoadConfig(Selection{Host: "nas:5001"}); err == nil {
		t.Fatal("an address without a scheme was taken")
	}
}

func TestSetDefaultEditsInPlace(t *testing.T) {
	withConfig(t, testConfig)
	if err := SetDefault("office"); err != nil {
		t.Fatal(err)
	}
	if err := SetDefault("home"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(ConfigPath())
	text := string(data)
	if strings.Count(text, "default =") != 1 || !strings.Contains(text, `default = "home"`) ||
		!strings.HasPrefix(text, "# my NAS\n") || !strings.Contains(text, "[nas.office]\nurl") {
		t.Fatalf("got:\n%s", text)
	}
	if strings.Index(text, "default =") > strings.Index(text, "[nas.") {
		t.Fatalf("default ended up inside a table:\n%s", text)
	}
	if err := SetDefault(""); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(ConfigPath()); strings.Contains(string(data), "default") {
		t.Fatalf("unnamed left a default:\n%s", data)
	}
	if err := SetDefault("nope"); err == nil {
		t.Fatal("unknown NAS accepted")
	}
	profiles, _, err := Profiles()
	if err != nil || len(profiles) != 3 || !profiles[0].Active || profiles[1].Name != "home" {
		t.Fatalf("profiles: %+v %v", profiles, err)
	}
}

func TestEnvOnly(t *testing.T) {
	withConfig(t, "")
	t.Setenv("SYNODOCK_URL", "https://e:5001")
	t.Setenv("SYNODOCK_USER", "u")
	if c := load(t, Selection{}); c.URL != "https://e:5001" || c.User != "u" || c.Keychain != "synodock" {
		t.Fatalf("got %+v", c)
	}
	t.Setenv("SYNODOCK_URL", "")
	if _, err := LoadConfig(Selection{}); err == nil || !strings.Contains(err.Error(), "no NAS address") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(ConfigDir, "config.toml")); err == nil {
		t.Fatal("loading created config.toml")
	}
}

func TestProfilesWithoutDefault(t *testing.T) {
	withConfig(t, "[nas.home]\nurl = \"https://home\"\nuser = \"u\"\n")
	if _, err := LoadConfig(Selection{}); err == nil || !strings.Contains(err.Error(), "synodock use NAME") {
		t.Fatalf("got %v", err)
	}
}

func TestIdleExit(t *testing.T) {
	withConfig(t, "idle_exit = \"5m\"\n\n[nas.home]\nurl = \"https://home\"\nuser = \"u\"\nidle_exit = \"0\"\n\n"+
		"[nas.office]\nurl = \"https://office\"\nuser = \"u\"\n")
	c := load(t, Selection{NAS: "home"})
	if c.IdleExit == nil || *c.IdleExit != 0 {
		t.Fatalf("home: %v", c.IdleExit)
	}
	if c := load(t, Selection{NAS: "office"}); c.IdleExit != nil {
		t.Fatalf("office: %v (a profile doesn't take the top level's)", *c.IdleExit)
	}
	t.Setenv("SYNODOCK_URL", "https://env")
	t.Setenv("SYNODOCK_USER", "u")
	if c := load(t, Selection{}); c.IdleExit == nil || *c.IdleExit != 5*time.Minute {
		t.Fatalf("unnamed: %v", c.IdleExit)
	}
	t.Setenv("SYNODOCK_IDLE_EXIT", "soon")
	if _, err := LoadConfig(Selection{}); err == nil || !strings.Contains(err.Error(), "idle_exit") {
		t.Fatalf("bad duration: %v", err)
	}
}
