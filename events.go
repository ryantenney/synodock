package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/ryantenney/synodock/internal/dsm"
)

// eventsCommand runs one events command and returns the exit code.
func eventsCommand(client *dsm.Client, o options) (int, error) {
	switch o.cmd {
	case "events ls":
		return 0, eventsList(client, o)
	case "events clear":
		return 0, eventsClear(client, o)
	}
	return 0, nil
}

// eventLevels maps the short names on the command line to the log's own.
var eventLevels = map[string]string{"": "", "err": "error", "warn": "warning", "info": "information"}

func eventsList(client *dsm.Client, o options) error {
	level, ok := eventLevels[o.level]
	if !ok {
		return dsm.Errorf("--level is err, warn or info, not %q", o.level)
	}
	page, err := client.Events(o.tail, level, o.keyword)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d events (%d errors, %d warnings, %d info)\n",
		page.Total, page.ErrorCount, page.WarnCount, page.InfoCount)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "TIME\tLEVEL\tUSER\tEVENT")
	for _, e := range page.Events {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Time, e.Level, dash(e.User), strings.TrimSpace(e.Event))
	}
	return tw.Flush()
}

// eventsClear empties the log. It can't be undone, so it asks for the word clear.
func eventsClear(client *dsm.Client, o options) error {
	if !o.force {
		if err := confirmTyped("This clears all of Container Manager's log events; there's no undo.", "clear"); err != nil {
			return err
		}
	}
	if err := client.EventsClear(); err != nil {
		return err
	}
	fmt.Println("cleared the event log")
	return nil
}
