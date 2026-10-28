package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/ryantenney/synodock/internal/dsm"
)

// networkCommand runs one network command and returns the exit code.
func networkCommand(client *dsm.Client, o options) (int, error) {
	switch o.cmd {
	case "network ls":
		return 0, networkList(client)
	case "network inspect":
		return 0, networkInspect(client, o.name)
	case "network create":
		return 0, networkCreate(client, o)
	case "network rm":
		return 0, networkRemove(client, o)
	}
	return 0, nil
}

func networkList(client *dsm.Client) error {
	nets, err := client.Networks()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDRIVER\tSUBNET\tGATEWAY\tCONTAINERS")
	for _, n := range nets {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", n.Name, n.Driver, dash(n.Subnet), dash(n.Gateway), len(n.Containers))
	}
	return tw.Flush()
}

func networkInspect(client *dsm.Client, name string) error {
	n, err := client.Network(name)
	if err != nil {
		return err
	}
	fmt.Printf("name:       %s\ndriver:     %s\nsubnet:     %s\ngateway:    %s\niprange:    %s\nipv6:       %t\n",
		n.Name, n.Driver, dash(n.Subnet), dash(n.Gateway), dash(n.IPRange), n.EnableIPv6)
	fmt.Printf("containers: %s\n", dash(strings.Join(n.Containers, ", ")))
	return nil
}

func networkCreate(client *dsm.Client, o options) error {
	if o.subnet == "" || o.gateway == "" {
		return dsm.Errorf("network create needs --subnet and --gateway: Container Manager's fixed-address networks have both")
	}
	if err := client.NetworkCreate(o.name, o.subnet, o.gateway, o.iprange, o.noMasquerade); err != nil {
		return err
	}
	fmt.Printf("created network %s (%s)\n", o.name, o.subnet)
	return nil
}

// networkRemove deletes a network. Container Manager refuses one with containers on it,
// and synodock says so first.
func networkRemove(client *dsm.Client, o options) error {
	n, err := client.Network(o.name)
	if err != nil {
		return err
	}
	if len(n.Containers) > 0 {
		return dsm.Errorf("%s has containers on it (%s); a network can only go once they've left",
			n.Name, strings.Join(n.Containers, ", "))
	}
	if !o.force {
		if err := confirmTyped("This deletes network "+n.Name+".", n.Name); err != nil {
			return err
		}
	}
	if err := client.NetworkRemove(*n); err != nil {
		return err
	}
	fmt.Printf("deleted network %s\n", n.Name)
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
