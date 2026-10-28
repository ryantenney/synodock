package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/ryantenney/synodock/internal/dsm"
)

// projectActions maps a project command to the compose action Container Manager runs.
// up and down are compose's build and clean, Container Manager's names for them.
var projectActions = map[string]string{
	"project up":      "build",
	"project down":    "clean",
	"project start":   "start",
	"project stop":    "stop",
	"project restart": "restart",
}

// projectCommand runs one project command and returns the exit code. Compose's own
// exit code is the command's, for the ones that run compose.
func projectCommand(client *dsm.Client, o options) (int, error) {
	switch o.cmd {
	case "project ls":
		return 0, projectList(client)
	case "project config":
		return 0, projectConfig(client, o)
	case "project ps":
		return 0, projectPS(client, o.name)
	case "project create":
		return 0, projectCreate(client, o)
	case "project update":
		return 0, projectUpdate(client, o)
	case "project logs":
		return 0, projectLogs(client, o.name)
	case "project rm":
		return 0, projectRemove(client, o)
	}
	return projectRun(client, o.name, projectActions[o.cmd])
}

func projectList(client *dsm.Client) error {
	projects, err := client.Projects()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tPATH")
	for _, p := range projects {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.Status, p.Path)
	}
	return tw.Flush()
}

func projectConfig(client *dsm.Client, o options) error {
	p, err := client.ProjectByName(o.name)
	if err != nil {
		return err
	}
	if o.outFile != "" {
		return os.WriteFile(o.outFile, []byte(p.Content), 0o644)
	}
	fmt.Print(p.Content)
	return nil
}

func projectPS(client *dsm.Client, name string) error {
	p, err := client.ProjectByName(name)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tHEALTH\tIMAGE")
	for i := range p.Containers {
		c := &p.Containers[i]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.BareName(), c.State.Status, c.Health(), c.Config.Image)
	}
	return tw.Flush()
}

func projectCreate(client *dsm.Client, o options) error {
	content, err := os.ReadFile(o.file)
	if err != nil {
		return err
	}
	share := o.sharePath
	if share == "" {
		share = "/docker/" + o.name
	}
	p, err := client.CreateProject(o.name, share, string(content), func(folder string) {
		fmt.Fprintf(os.Stderr, "created %s\n", folder)
	})
	if err != nil {
		return err
	}
	fmt.Printf("created project %s at %s (%s); start it with: synodock project up %s\n", p.Name, p.Path, p.Status, p.Name)
	return nil
}

// projectUpdate replaces a stopped project's compose file, backing up the old one first.
// Container Manager's UI allows edits only while a project is stopped, and the API
// allows them either way, so the rule is kept here.
func projectUpdate(client *dsm.Client, o options) error {
	p, err := client.ProjectByName(o.name)
	if err != nil {
		return err
	}
	if p.Running() {
		return dsm.Errorf("%s is %s; Container Manager only allows compose edits while a project is stopped "+
			"(run: synodock project down %s)", p.Name, p.Status, p.Name)
	}
	content, err := os.ReadFile(o.file)
	if err != nil {
		return err
	}
	local, err := client.BackupProject(p, p.Content)
	if local != "" {
		fmt.Fprintf(os.Stderr, "backed up current compose to %s and %s/backups\n", local, p.SharePath)
	}
	if err != nil {
		return err
	}
	if err := client.ProjectUpdate(p.ID, string(content)); err != nil {
		return err
	}
	fmt.Printf("updated %s compose\n", p.Name)
	return nil
}

// projectRun runs a compose action on a project. Up refuses a running project: compose
// would recreate its containers in place, and Container Manager then lists them under
// temporary names. Up on a stopped project also resyncs names that are already stale,
// which is the fix. Down can't touch stale names, so it warns about them.
func projectRun(client *dsm.Client, name, action string) (int, error) {
	p, err := client.ProjectByName(name)
	if err != nil {
		return 0, err
	}
	stale, err := client.StaleNames(p)
	if err != nil {
		return 0, err
	}
	switch {
	case action == "build" && p.Running():
		return 0, dsm.Errorf("%s is %s; take it down first (synodock project down %s): up on a running "+
			"project recreates its containers in place", p.Name, p.Status, p.Name)
	case action == "build" && len(stale) > 0:
		fmt.Fprintf(os.Stderr, "resyncing container names for %s\n", strings.Join(stale, ", "))
	case action == "clean" && len(stale) > 0:
		fmt.Fprintf(os.Stderr, "warning: %s is listed under temporary names; down won't touch it, "+
			"so run up once first to resync\n", strings.Join(stale, ", "))
	}
	return client.ProjectStream(p.ID, action, func(line string) { fmt.Println(line) })
}

func projectLogs(client *dsm.Client, name string) error {
	p, err := client.ProjectByName(name)
	if err != nil {
		return err
	}
	out, err := client.ProjectLog(p.ID)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// projectRemove deletes a project with its containers and data. It refuses a running
// project, and asks for the name typed back unless -f, since nothing brings the data back.
func projectRemove(client *dsm.Client, o options) error {
	p, err := client.ProjectByName(o.name)
	if err != nil {
		return err
	}
	if p.Running() {
		return dsm.Errorf("%s is %s; take it down first (synodock project down %s)", p.Name, p.Status, p.Name)
	}
	if !o.force {
		if err := confirmTyped(fmt.Sprintf("This deletes %s with its containers and all its data.", p.Name), p.Name); err != nil {
			return err
		}
	}
	if err := client.ProjectDelete(p.ID); err != nil {
		return err
	}
	fmt.Printf("deleted project %s\n", p.Name)
	return nil
}
