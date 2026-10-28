package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ryantenney/synodock/internal/dsm"
)

// containerCreate makes a container from a profile file, the JSON that `container export`
// writes. The file is checked here first, so a missing field is reported without a call.
func containerCreate(client *dsm.Client, o options) error {
	profile, err := os.ReadFile(o.file)
	if err != nil {
		return err
	}
	if err := dsm.ValidateProfile(profile); err != nil {
		return err
	}
	if err := client.ContainerCreate(profile, o.start); err != nil {
		return err
	}
	fmt.Printf("created container from %s", o.file)
	if o.start {
		fmt.Print(", and started it")
	}
	fmt.Println()
	return nil
}

// containerUpdate applies a profile file to a container.
func containerUpdate(client *dsm.Client, o options) error {
	profile, err := os.ReadFile(o.file)
	if err != nil {
		return err
	}
	if err := dsm.ValidateProfile(profile); err != nil {
		return err
	}
	if err := client.ContainerUpdate(o.name, profile); err != nil {
		return err
	}
	// Container Manager's settings for an existing container cover the name, CPU priority,
	// memory and auto-restart, and nothing else: the command is kept whatever the profile
	// says. Read the settings back, so a command that didn't take is an error, not a success.
	var want struct {
		Name  string `json:"name"`
		CmdV2 string `json:"cmd_v2"`
	}
	if err := json.Unmarshal(profile, &want); err != nil {
		return err
	}
	// The profile's name can rename it, so read it back by that name.
	stored, _, err := client.ContainerSettings(want.Name)
	if err != nil {
		return err
	}
	var got struct {
		CmdV2 string `json:"cmd_v2"`
	}
	if err := json.Unmarshal(stored, &got); err != nil {
		return err
	}
	if want.CmdV2 != "" && strings.TrimSpace(want.CmdV2) != strings.TrimSpace(got.CmdV2) {
		return dsm.Errorf("updated %s, but its command is still %q: Container Manager doesn't change the command of an existing container. "+
			"To run a different command, make a new container with create --profile (its profile can come from export)", o.name, got.CmdV2)
	}
	fmt.Printf("updated container %s from %s\n", o.name, o.file)
	return nil
}
