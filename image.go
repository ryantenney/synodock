package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ryantenney/synodock/internal/dsm"
)

// registryPasswordEnv holds the password for image pull, which takes the account's
// password as its own argument, never from a keychain. Step 5 (registries) will own this.
const registryPasswordEnv = "SYNODOCK_REGISTRY_PASSWORD"

// imageCommand runs one image command and returns the exit code.
func imageCommand(client *dsm.Client, o options) (int, error) {
	switch o.cmd {
	case "image ls":
		return 0, imageList(client)
	case "image rm":
		return 0, imageRemove(client, o)
	case "image import":
		return 0, client.ImageImport(o.file, nil, 30*time.Minute)
	case "image load":
		return 0, imageLoad(client, o)
	case "image pull":
		return 0, imagePull(client, o)
	}
	return 0, nil
}

func imageList(client *dsm.Client) error {
	images, err := client.Images()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tTAG\tIMAGE ID\tSIZE")
	for _, img := range images {
		id := dsm.ShortID(img.ID)
		size := humanSize(img.Size.String())
		if len(img.Tags) == 0 {
			fmt.Fprintf(tw, "%s\t<none>\t%s\t%s\n", "<none>", id, size)
			continue
		}
		for _, tag := range img.Tags {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", img.Repository, tag, id, size)
		}
	}
	return tw.Flush()
}

func humanSize(bytes string) string {
	var n float64
	fmt.Sscan(bytes, &n)
	return fmt.Sprintf("%.1f MB", n/1e6)
}

func imageRemove(client *dsm.Client, o options) error {
	if !o.force {
		if err := confirmTyped("This deletes image "+o.name+".", o.name); err != nil {
			return err
		}
	}
	if err := client.ImageDeleteID(o.name); err != nil {
		return err
	}
	fmt.Printf("deleted image %s\n", o.name)
	return nil
}

// imageLoad copies a docker save tar to a NAS folder and imports it. The previous tar of
// that name is kept as <stem>.prev.tar, since a load moves the tag away from the old image.
// It waits for the import the way Container Manager's own does, if the request drops.
func imageLoad(client *dsm.Client, o options) error {
	info, err := dsm.ReadImageTar(o.file)
	if err != nil {
		return err
	}
	if len(info.Tags) == 0 {
		return dsm.Errorf("%s has no tags to load", o.file)
	}
	fmt.Fprintf(os.Stderr, "uploading %s -> %s\n", o.file, o.folder)
	moved, err := client.ImageUpload(o.file, o.folder)
	if err != nil {
		return err
	}
	if moved != "" {
		fmt.Fprintf(os.Stderr, "kept previous as %s\n", moved)
	}
	expect := map[string]string{}
	for _, tag := range info.Tags {
		expect[tag] = info.ShortID
	}
	nasPath := o.folder + "/" + filepath.Base(o.file)
	fmt.Fprintf(os.Stderr, "importing %s\n", nasPath)
	if err := client.ImageImport(nasPath, expect, 30*time.Minute); err != nil {
		return err
	}
	fmt.Printf("loaded %s\n", strings.Join(info.Tags, ", "))
	return nil
}

// imagePull pulls REPO[:TAG] from a registry onto the NAS. The image lands as host/REPO:TAG,
// the registry's host, so that's what the compose file has to name.
func imagePull(client *dsm.Client, o options) error {
	repo, tag, found := strings.Cut(o.name, ":")
	if !found || strings.Contains(tag, "/") {
		repo, tag = o.name, "latest"
	}
	if o.registry == "" || o.user == "" {
		return dsm.Errorf("image pull needs --registry and --user")
	}
	password := os.Getenv(registryPasswordEnv)
	if password == "" {
		return dsm.Errorf("set %s to the registry password for %s", registryPasswordEnv, o.user)
	}
	fmt.Fprintf(os.Stderr, "pulling %s:%s from %s\n", repo, tag, o.registry)
	if err := client.ImagePull(o.registry, repo, tag, o.user, password, time.Hour); err != nil {
		return err
	}
	u, err := url.Parse(o.registry)
	if err != nil {
		return err
	}
	fmt.Printf("pulled %s/%s:%s\n", u.Host, repo, tag)
	return nil
}
