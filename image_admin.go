package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/ryantenney/synodock/internal/dsm"
)

// imageSave writes an image to a NAS folder as a tar, named by Container Manager.
func imageSave(client *dsm.Client, o options) error {
	repo, tag := splitRef(o.name)
	if err := client.ImageExport(repo, tag, o.folder); err != nil {
		return err
	}
	fmt.Printf("saved %s:%s to %s\n", repo, tag, o.folder)
	return nil
}

// splitRef is repo and tag from REPO[:TAG], latest when there's no tag.
func splitRef(ref string) (string, string) {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// imagePrune lists the images no container uses, then deletes them after the word
// prune is typed back. Container Manager takes no list: it removes whatever is unused.
func imagePrune(client *dsm.Client, o options) error {
	images, err := client.Images()
	if err != nil {
		return err
	}
	containers, err := client.Containers()
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for _, c := range containers {
		used[dsm.ShortID(c.ImageID)] = true
	}
	var unused []string
	for _, img := range images {
		if used[dsm.ShortID(img.ID)] {
			continue
		}
		name := "<none>"
		if len(img.Tags) > 0 {
			name = img.Repository + ":" + img.Tags[0]
		}
		unused = append(unused, fmt.Sprintf("%s %s", dsm.ShortID(img.ID), name))
	}
	if len(unused) == 0 {
		fmt.Println("no unused images")
		return nil
	}
	fmt.Fprintln(os.Stderr, "unused images:")
	for _, u := range unused {
		fmt.Fprintln(os.Stderr, "  "+u)
	}
	if !o.force {
		if err := confirmTyped(fmt.Sprintf("This deletes %d unused images.", len(unused)), "prune"); err != nil {
			return err
		}
	}
	if err := client.ImagePrune(); err != nil {
		return err
	}
	fmt.Printf("pruned %d unused images\n", len(unused))
	return nil
}
