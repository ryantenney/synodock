package dockerapi

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDockerContext(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker CLI")
	}
	t.Setenv("DOCKER_CONFIG", t.TempDir()) // not the user's contexts
	host := func() string {
		out, err := exec.Command("docker", "context", "inspect", "synology-home", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if err := DockerContext("synology-home", "/tmp/a.sock", "synodock: test"); err != nil {
		t.Fatal(err)
	}
	if h := host(); h != "unix:///tmp/a.sock" {
		t.Fatalf("created: %s", h)
	}
	if err := DockerContext("synology-home", "/tmp/b.sock", "synodock: test"); err != nil {
		t.Fatal(err)
	}
	if h := host(); h != "unix:///tmp/b.sock" {
		t.Fatalf("updated: %s", h)
	}
}
