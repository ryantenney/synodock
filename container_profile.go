package main

import (
	"fmt"
	"os"

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
	fmt.Printf("updated container %s from %s\n", o.name, o.file)
	return nil
}
