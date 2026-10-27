package dockerapi

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// DockerContext creates (or points again) a docker context at the socket, so
// `docker --context NAME` reaches this NAS.
func DockerContext(name, socket, description string) error {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("no docker CLI to create the context with")
	}
	action := "create"
	if exec.Command(docker, "context", "inspect", name).Run() == nil {
		action = "update"
	}
	var stderr bytes.Buffer
	cmd := exec.Command(docker, "context", action, name, "--description", description,
		"--docker", "host=unix://"+socket)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker context %s %s: %v: %s", action, name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
