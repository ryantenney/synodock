package dsm

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the NAS to talk to and how to log in to it.
type Config struct {
	Name            string // the [nas.NAME] profile; "" for the unnamed one
	URL             string
	User            string
	Keychain        string
	Password        string // SYNODOCK_PASSWORD, for the unnamed NAS only
	PasswordCommand string
	OTPCommand      string
	IdleExit        *time.Duration // idle_exit: how long serve waits idle before exiting; nil if unset
	idleExit        string
}

// Host is the NAS's host name, which keys its saved credentials.
func (c *Config) Host() string {
	if u, err := url.Parse(c.URL); err == nil {
		return u.Hostname()
	}
	return ""
}

// Selection is what the command line names: -H/--host or --nas (not both).
type Selection struct {
	Host string
	NAS  string
}

type settings struct {
	URL             string `toml:"url"`
	User            string `toml:"user"`
	Keychain        string `toml:"keychain"`
	PasswordCommand string `toml:"password_command"`
	OTPCommand      string `toml:"otp_command"`
	IdleExit        string `toml:"idle_exit"`
}

type configFile struct {
	Default         string              `toml:"default"`
	URL             string              `toml:"url"`
	User            string              `toml:"user"`
	Keychain        string              `toml:"keychain"`
	PasswordCommand string              `toml:"password_command"`
	OTPCommand      string              `toml:"otp_command"`
	IdleExit        string              `toml:"idle_exit"`
	NAS             map[string]settings `toml:"nas"`
}

func ConfigPath() string { return filepath.Join(ConfigDir, "config.toml") }

func readConfigFile() (*configFile, error) {
	var f configFile
	if _, err := os.Stat(ConfigPath()); err == nil {
		if _, err := toml.DecodeFile(ConfigPath(), &f); err != nil {
			return nil, Errorf("%s: %v", ConfigPath(), err)
		}
	}
	return &f, nil
}

// LoadConfig picks the NAS as the docker CLI picks its endpoint, first match wins:
//
//	-H/--host URL          a NAS by address (https://user@nas:5001), like docker -H
//	--nas NAME             a [nas.NAME] profile, like docker --context
//	SYNODOCK_URL           the address from the environment, like DOCKER_HOST
//	SYNODOCK_NAS           a profile from the environment, like DOCKER_CONTEXT
//	default = "NAME"       the profile `synodock use` set, like docker context use
//	url, user              the unnamed NAS at the top of config.toml
//
// A profile is self-contained, as a docker context is: SYNODOCK_USER,
// SYNODOCK_PASSWORD and the other SYNODOCK_* settings apply only to a NAS picked
// by address or to the unnamed one.
func LoadConfig(sel Selection) (*Config, error) {
	c, _, err := Resolve(sel)
	return c, err
}

// Resolve is LoadConfig, also saying what picked the NAS: "--host", "--nas",
// "SYNODOCK_URL", "SYNODOCK_NAS", "default", or "" for the unnamed NAS.
func Resolve(sel Selection) (*Config, string, error) {
	f, err := readConfigFile()
	if err != nil {
		return nil, "", err
	}
	c, chosenBy, err := f.resolve(sel)
	if err != nil {
		return nil, "", err
	}
	if c.Name == "" && c.URL == "" && len(f.NAS) > 0 {
		return nil, "", Errorf("no NAS picked: pass --nas NAME, or run synodock use NAME (%s has %s)",
			ConfigPath(), strings.Join(f.names(), ", "))
	}
	return c, chosenBy, c.check()
}

// resolve returns the NAS chosen and what chose it.
func (f *configFile) resolve(sel Selection) (*Config, string, error) {
	switch {
	case sel.Host != "" && sel.NAS != "":
		return nil, "", Errorf("--host and --nas can't be used together")
	case sel.Host != "":
		c, err := byAddress(sel.Host)
		return c, "--host", err
	case sel.NAS != "":
		c, err := f.profile(sel.NAS, "--nas")
		return c, "--nas", err
	case os.Getenv("SYNODOCK_URL") != "":
		return f.unnamed(), "SYNODOCK_URL", nil
	case os.Getenv("SYNODOCK_NAS") != "":
		c, err := f.profile(os.Getenv("SYNODOCK_NAS"), "SYNODOCK_NAS")
		return c, "SYNODOCK_NAS", err
	case f.Default != "":
		c, err := f.profile(f.Default, "default in "+ConfigPath())
		return c, "default", err
	}
	return f.unnamed(), "", nil
}

func byAddress(address string) (*Config, error) {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, Errorf("--host wants the NAS's DSM address, like https://user@nas.example.com:5001")
	}
	c := envSettings()
	if u.User != nil {
		c.User = u.User.Username()
	}
	u.User = nil
	c.URL = u.String()
	return c, nil
}

func envSettings() *Config {
	return &Config{
		User:            os.Getenv("SYNODOCK_USER"),
		Keychain:        firstOf(os.Getenv("SYNODOCK_KEYCHAIN"), "synodock"),
		Password:        os.Getenv("SYNODOCK_PASSWORD"),
		PasswordCommand: os.Getenv("SYNODOCK_PASSWORD_COMMAND"),
		OTPCommand:      os.Getenv("SYNODOCK_OTP_COMMAND"),
		idleExit:        os.Getenv("SYNODOCK_IDLE_EXIT"),
	}
}

// unnamed is the top-level url and user, each setting overridable from the environment.
func (f *configFile) unnamed() *Config {
	c := envSettings()
	c.URL = firstOf(os.Getenv("SYNODOCK_URL"), f.URL)
	c.User = firstOf(c.User, f.User)
	c.Keychain = firstOf(os.Getenv("SYNODOCK_KEYCHAIN"), f.Keychain, "synodock")
	c.PasswordCommand = firstOf(c.PasswordCommand, f.PasswordCommand)
	c.OTPCommand = firstOf(c.OTPCommand, f.OTPCommand)
	c.idleExit = firstOf(c.idleExit, f.IdleExit)
	return c
}

func (f *configFile) profile(name, chosenBy string) (*Config, error) {
	p, ok := f.NAS[name]
	if !ok {
		known := f.names()
		if len(known) == 0 {
			return nil, Errorf("no NAS named %q (%s): %s has no [nas.NAME] sections", name, chosenBy, ConfigPath())
		}
		return nil, Errorf("no NAS named %q (%s); %s has %s", name, chosenBy, ConfigPath(), strings.Join(known, ", "))
	}
	return &Config{Name: name, URL: p.URL, User: p.User, Keychain: firstOf(p.Keychain, "synodock"),
		PasswordCommand: p.PasswordCommand, OTPCommand: p.OTPCommand, idleExit: p.IdleExit}, nil
}

func (f *configFile) names() []string {
	var names []string
	for name := range f.NAS {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (c *Config) check() error {
	if c.idleExit != "" {
		d, err := time.ParseDuration(c.idleExit)
		if err != nil || d < 0 {
			return Errorf("idle_exit = %q: want a duration like \"15m\" or \"1h\" (\"0\" never exits)", c.idleExit)
		}
		c.IdleExit = &d
	}
	where := "SYNODOCK_URL or url in " + ConfigPath()
	if c.Name != "" {
		where = fmt.Sprintf("url in [nas.%s] in %s", c.Name, ConfigPath())
	}
	if c.URL == "" {
		return Errorf("no NAS address: set %s (e.g. https://nas.example.com:5001)", where)
	}
	if c.User == "" {
		where = "SYNODOCK_USER or user in " + ConfigPath()
		if c.Name != "" {
			where = fmt.Sprintf("user in [nas.%s]", c.Name)
		}
		return Errorf("no DSM account: set %s", where)
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return nil
}

// Profile is a NAS for `synodock use` to list.
type Profile struct {
	Name, URL, User string
	Active          bool
}

// Profiles lists the [nas.NAME] sections (and the unnamed NAS, if there is one),
// marking the one a command with no -H or --nas would use, and says what chose it.
func Profiles() ([]Profile, string, error) {
	f, err := readConfigFile()
	if err != nil {
		return nil, "", err
	}
	active, chosenBy, err := f.resolve(Selection{})
	if err != nil {
		return nil, "", err
	}
	var out []Profile
	if f.URL != "" || chosenBy == "SYNODOCK_URL" {
		u := f.unnamed()
		out = append(out, Profile{URL: u.URL, User: u.User, Active: active.Name == ""})
	}
	for _, name := range f.names() {
		p := f.NAS[name]
		out = append(out, Profile{Name: name, URL: p.URL, User: p.User, Active: active.Name == name})
	}
	return out, chosenBy, nil
}

var profileName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

var defaultLine = regexp.MustCompile(`^\s*default\s*=`)

// SetDefault makes name the NAS commands use when nothing else picks one ("" for
// the unnamed one), editing default = in config.toml and leaving the rest as is.
func SetDefault(name string) error {
	f, err := readConfigFile()
	if err != nil {
		return err
	}
	if name != "" {
		if !profileName.MatchString(name) {
			return Errorf("%q isn't a NAS name (letters, digits, - and _)", name)
		}
		if _, err := f.profile(name, "synodock use"); err != nil {
			return err
		}
	} else if f.URL == "" {
		return Errorf("%s has no unnamed NAS (url at the top) to go back to", ConfigPath())
	}
	data, err := os.ReadFile(ConfigPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var lines []string
	if len(data) > 0 {
		lines = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
	var out []string
	done := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !done && defaultLine.MatchString(line) {
			if name != "" {
				out = append(out, "default = "+strconv.Quote(name))
			}
			done = true
			continue
		}
		if !done && strings.HasPrefix(trimmed, "[") { // top-level keys go before any table
			if name != "" {
				out = append(out, "default = "+strconv.Quote(name), "")
			}
			done = true
		}
		out = append(out, line)
	}
	if !done && name != "" {
		out = append(out, "default = "+strconv.Quote(name))
	}
	return os.WriteFile(ConfigPath(), []byte(strings.Join(out, "\n")+"\n"), 0o600)
}
