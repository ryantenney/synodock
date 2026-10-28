package dsm

import (
	"context"
	"strings"
	"testing"
)

const registryConfig = `
[nas.home]
url = "https://home:5001"
user = "docker"
registry = "https://registry.example.com/"
registry_user = "ryan"
registry_password_command = "printf from-command"

[nas.bare]
url = "https://bare:5001"
user = "docker"
`

func registryFor(t *testing.T, nas, url, user string) (*RegistryAuth, error) {
	t.Helper()
	return NewClient(context.Background(), load(t, Selection{NAS: nas})).Registry(url, user)
}

func TestRegistryFromProfile(t *testing.T) {
	withConfig(t, registryConfig)
	r, err := registryFor(t, "home", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != "https://registry.example.com" || r.Host != "registry.example.com" || r.User != "ryan" || r.Password != "from-command" {
		t.Errorf("got %+v", r)
	}
}

func TestRegistryFlagsAndEnvWin(t *testing.T) {
	withConfig(t, registryConfig)
	t.Setenv(RegistryPasswordEnv, "from-env")
	r, err := registryFor(t, "home", "http://127.0.0.1:5050", "other")
	if err != nil {
		t.Fatal(err)
	}
	// The flags beat the profile, the port stays in the host (the NAS names images
	// 127.0.0.1:5050/repo), and the environment beats registry_password_command.
	if r.URL != "http://127.0.0.1:5050" || r.Host != "127.0.0.1:5050" || r.User != "other" || r.Password != "from-env" {
		t.Errorf("got %+v", r)
	}
}

func TestRegistryErrors(t *testing.T) {
	withConfig(t, registryConfig)
	for _, tc := range []struct {
		nas, url, user, want string
	}{
		{"bare", "", "", "no registry"},
		{"bare", "https://r.example", "", "no registry"},
		{"bare", "registry.example.com", "u", "want its URL"}, // no scheme
		{"bare", "https://r.example", "u", "no password"},
	} {
		_, err := registryFor(t, tc.nas, tc.url, tc.user)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err %v, want %q", tc, err, tc.want)
		}
	}
}
