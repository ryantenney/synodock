package main

import (
	"testing"

	"github.com/ryantenney/synodock/internal/dsm"
)

func container(status string, exit int, policy, health string) *dsm.ProjectContainer {
	var c dsm.ProjectContainer
	c.State.Status, c.State.ExitCode, c.HostConfig.RestartPolicy.Name = status, exit, policy
	if health != "" {
		c.State.Health = &struct {
			Status string `json:"Status"`
		}{health}
	}
	return &c
}

func TestContainerReady(t *testing.T) {
	for _, tc := range []struct {
		name          string
		c             *dsm.ProjectContainer
		ready, failed bool
		status        string
	}{
		{"running, no check", container("running", 0, "unless-stopped", ""), true, false, ""},
		{"running, healthy", container("running", 0, "always", "healthy"), true, false, ""},
		{"running, starting", container("running", 0, "always", "starting"), false, false, "starting"},
		{"running, unhealthy", container("running", 0, "always", "unhealthy"), false, false, "unhealthy"},
		{"one-shot done", container("exited", 0, "no", ""), true, false, ""},
		{"one-shot done, no policy", container("exited", 0, "", ""), true, false, ""},
		{"on-failure done", container("exited", 0, "on-failure", ""), true, false, ""},
		{"one-shot failed", container("exited", 3, "no", ""), false, true, ""},
		{"on-failure failed, will restart", container("exited", 1, "on-failure", ""), false, false, "exited"},
		{"always exited, will restart", container("exited", 0, "always", ""), false, false, "exited"},
		{"restarting", container("restarting", 0, "always", ""), false, false, "restarting"},
		{"created, not started", container("created", 0, "no", ""), false, false, "created"},
	} {
		ready, status, err := containerReady(tc.c)
		if ready != tc.ready || (err != nil) != tc.failed || status != tc.status {
			t.Errorf("%s: ready=%v status=%q err=%v; want ready=%v status=%q failed=%v",
				tc.name, ready, status, err, tc.ready, tc.status, tc.failed)
		}
	}
}
