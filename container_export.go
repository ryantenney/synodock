package main

import (
	"fmt"
	"os"

	"github.com/ryantenney/synodock/internal/dsm"
)

// containerExport writes a container's settings. They include its environment variables,
// which is where secrets go, so the file is private to this user.
func containerExport(client *dsm.Client, o options) error {
	data, name, err := client.ContainerSettings(o.name)
	if err != nil {
		return err
	}
	if o.output == "" {
		_, err := os.Stdout.Write(data)
		return err
	}
	if err := writePrivate(o.output, data); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes, the NAS calls it %s); it includes environment variables, which may be secrets\n",
		o.output, len(data), name)
	return nil
}
