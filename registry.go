package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/ryantenney/synodock/internal/dsm"
)

// registryCommand runs one registry command and returns the exit code.
func registryCommand(client *dsm.Client, o options) (int, error) {
	switch o.cmd {
	case "registry ls":
		return 0, registryList(client)
	case "registry add":
		return 0, registryAdd(client, o)
	case "registry rm":
		return 0, registryRemove(client, o)
	case "registry use":
		return 0, client.RegistryUse(o.name)
	case "registry search":
		return 0, registrySearch(client, o)
	case "registry tags":
		return 0, registryTags(client, o)
	}
	return 0, nil
}

func registryList(client *dsm.Client) error {
	regs, using, err := client.RegistryList()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tURL\tUSER")
	for _, r := range regs {
		mark := "  "
		if r.Name == using {
			mark = "* "
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%s\n", mark, r.Name, r.URL, dash(r.Username))
	}
	return tw.Flush()
}

func registryAdd(client *dsm.Client, o options) error {
	password := ""
	if o.user != "" {
		password = os.Getenv(dsm.RegistryPasswordEnv)
		if password == "" {
			return dsm.Errorf("set %s to the password for %s", dsm.RegistryPasswordEnv, o.user)
		}
	}
	if err := client.RegistryAdd(o.name, o.registry, o.user, password); err != nil {
		return err
	}
	fmt.Printf("added registry %s (%s)\n", o.name, o.registry)
	return nil
}

// registryRemove deletes a registry. The one in use is refused; use another first.
func registryRemove(client *dsm.Client, o options) error {
	regs, using, err := client.RegistryList()
	if err != nil {
		return err
	}
	if using == o.name {
		return dsm.Errorf("%s is the registry in use; run synodock registry use NAME for another first", o.name)
	}
	found := false
	for _, r := range regs {
		found = found || r.Name == o.name
	}
	if !found {
		return dsm.Errorf("no registry named %q", o.name)
	}
	if !o.force {
		if err := confirmTyped("This removes registry "+o.name+" from Container Manager.", o.name); err != nil {
			return err
		}
	}
	if err := client.RegistryRemove(o.name); err != nil {
		return err
	}
	fmt.Printf("removed registry %s\n", o.name)
	return nil
}

func registrySearch(client *dsm.Client, o options) error {
	results, err := client.RegistrySearch(o.name, o.tail)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tOFFICIAL\tDESCRIPTION")
	for _, r := range results {
		official := ""
		if r.IsOfficial {
			official = "[OK]"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, official, r.Description)
	}
	return tw.Flush()
}

func registryTags(client *dsm.Client, o options) error {
	tags, err := client.RegistryTags(o.name)
	if err != nil {
		return err
	}
	for _, t := range tags {
		fmt.Println(t)
	}
	return nil
}
