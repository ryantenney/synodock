package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/ryantenney/synodock/internal/dsm"
)

// deployPull is one registry image a deploy pulls: its repo and tag, the name the NAS
// gives it, and the id the registry says it has.
type deployPull struct {
	repo, tag, target, remote string
	current                   bool // the NAS already has this exact image
}

// deployTar is a docker save tar to load for a deploy, read once up front.
type deployTar struct {
	path    string
	name    string
	size    int64
	info    dsm.ImageTar
	skipped bool // the NAS already has this exact image on every tag it carries
}

// projectDeploy takes a project down, loads what's new, and brings it back up. Everything
// that can fail (reading tars, checking space and the compose file, uploading and loading)
// happens while the project is still up, so the downtime is only clean, compose update,
// build and the health wait. Old images are deleted only once the project is healthy.
//
// Not here yet, against dsm.py: pulls from the homelab registry (which checks digests
// against that registry's own API) and single-service deploys.
func projectDeploy(client *dsm.Client, o options) (int, error) {
	p, err := client.ProjectByName(o.name)
	if err != nil {
		return 0, err
	}
	current := p.Content
	target := current
	if o.file != "" {
		data, err := os.ReadFile(o.file)
		if err != nil {
			return 0, err
		}
		target = string(data)
	}
	composeChanged := target != current
	folder := strings.TrimSuffix(o.folder, "/")
	var reg *dsm.RegistryAuth
	if len(o.pulls) > 0 {
		if reg, err = client.Registry(o.registry, o.user); err != nil {
			return 0, err
		}
	}
	if folder == "" {
		folder = p.SharePath + "/images"
	}
	images := composeImages(target)
	inCompose := map[string]bool{}
	for _, img := range images {
		inCompose[img] = true
	}

	// --- preflight: nothing changes here ---
	currentIDs, err := client.ImageIDsByTag()
	if err != nil {
		return 0, err
	}
	var tars []deployTar
	var problems []string
	replaced := map[string]string{} // tag -> the image id it had, for cleanup
	var need int64
	for _, path := range o.images {
		info, err := dsm.ReadImageTar(path)
		if err != nil {
			return 0, err
		}
		st, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		t := deployTar{path: path, name: filepath.Base(path), size: st.Size(), info: info}
		t.skipped = len(info.Tags) > 0
		for _, tag := range info.Tags {
			if dsm.ShortID(currentIDs[tag]) != info.ShortID {
				t.skipped = false
			}
		}
		if t.skipped {
			tars = append(tars, t)
			continue
		}
		for _, tag := range info.Tags {
			if !inCompose[tag] {
				problems = append(problems, fmt.Sprintf("the compose file doesn't use %s from %s; loading it would change nothing",
					tag, t.name))
			}
			if id, ok := currentIDs[tag]; ok {
				replaced[tag] = id
			}
		}
		need += st.Size() * 3 // tar plus its extracted layers, with headroom
		tars = append(tars, t)
	}
	var uploads []deployTar
	for _, t := range tars {
		if !t.skipped {
			uploads = append(uploads, t)
		}
	}
	free := int64(0)
	if len(uploads) > 0 {
		if free, err = client.ShareFreeBytes(folder); err != nil {
			return 0, err
		}
	}
	// Registry images: each REF is checked against the registry, and pulled onto the NAS
	// under the name the NAS gives it, host/repo:tag. Its image id must match the registry's.
	var pulls []deployPull
	for _, ref := range o.pulls {
		repo, tag := splitRef(ref)
		target := reg.Host + "/" + repo + ":" + tag
		remote, err := dsm.RegistryImageID(reg.URL, repo, tag, reg.User, reg.Password)
		if err != nil {
			return 0, err
		}
		if remote == "" {
			problems = append(problems, fmt.Sprintf("%s:%s is not in %s (push it first)", repo, tag, reg.URL))
			continue
		}
		if !inCompose[target] {
			problems = append(problems, fmt.Sprintf("the compose file doesn't use %s; switch that service's image to it "+
				"(pass the edited file with --compose)", target))
		}
		pl := deployPull{repo: repo, tag: tag, target: target, remote: remote}
		if dsm.ShortID(currentIDs[target]) == remote {
			pl.current = true
		} else if id, ok := currentIDs[target]; ok {
			replaced[target] = id
		}
		pulls = append(pulls, pl)
	}
	stale, err := client.StaleNames(p)
	if err != nil {
		return 0, err
	}
	expected, err := composeContainerNames(target, p.Name)
	if err != nil {
		return 0, err
	}
	if len(stale) > 0 {
		problems = append(problems, fmt.Sprintf("Container Manager lists %s under temporary names; run synodock project up %s "+
			"once to resync before deploying", strings.Join(stale, ", "), p.Name))
	}
	if len(uploads) > 0 && free < need {
		problems = append(problems, fmt.Sprintf("not enough free space: %.1f GB free, about %.1f GB wanted",
			float64(free)/1e9, float64(need)/1e9))
	}
	var pullNow []deployPull
	for _, pl := range pulls {
		if !pl.current {
			pullNow = append(pullNow, pl)
		}
	}
	if len(uploads) == 0 && len(pullNow) == 0 && !composeChanged && len(problems) == 0 {
		problems = append(problems, "nothing to deploy: no new image and the compose file is unchanged")
	}

	fmt.Printf("deploy %s (%s)\n", p.Name, p.Status)
	for _, t := range tars {
		if t.skipped {
			fmt.Printf("  skip   %s: %s already is this image (%s)\n", t.name, strings.Join(t.info.Tags, ", "), t.info.ShortID)
		}
	}
	for _, t := range uploads {
		fmt.Printf("  image  %s (%.0f MB) -> %s  tags=%s\n", t.name, float64(t.size)/1e6, folder, strings.Join(t.info.Tags, ", "))
	}
	for _, pl := range pulls {
		if pl.current {
			fmt.Printf("  skip   %s: already the registry's image (%s)\n", pl.target, pl.remote)
		} else {
			fmt.Printf("  pull   %s (%s) from %s\n", pl.target, pl.remote, reg.URL)
		}
	}
	oldAction := "deleted after healthy"
	if o.keepOld {
		oldAction = "kept"
	}
	for tag, id := range replaced {
		fmt.Printf("  old    %s %s (%s)\n", tag, dsm.ShortID(id), oldAction)
	}
	if composeChanged {
		fmt.Println("  compose changed (the old file is backed up first)")
	} else {
		fmt.Println("  compose unchanged")
	}
	steps := "clean (DOWN) -> "
	if composeChanged {
		steps = "clean (DOWN) -> compose update -> "
	}
	var before []string
	if len(uploads) > 0 {
		before = append(before, "upload + load")
	}
	if len(pullNow) > 0 {
		before = append(before, "pull")
	}
	if len(before) > 0 {
		steps = strings.Join(before, " + ") + " (still up) -> " + steps
	}
	fmt.Printf("  steps  %sbuild -> wait healthy for %s\n", steps, strings.Join(expected, ", "))
	for _, msg := range problems {
		fmt.Printf("  BLOCKED: %s\n", msg)
	}
	if len(problems) > 0 {
		return 1, nil
	}
	if o.dryRun {
		return 0, nil
	}
	if !o.yes {
		if err := confirmYes(fmt.Sprintf("take %s down and deploy?", p.Name)); err != nil {
			return 1, err
		}
	}

	// --- while still up ---
	for _, t := range uploads {
		fmt.Fprintf(os.Stderr, "uploading %s\n", t.name)
		if _, err := client.ImageUpload(t.path, folder); err != nil {
			return 1, err
		}
	}
	for _, t := range uploads {
		fmt.Fprintf(os.Stderr, "loading %s\n", t.name)
		expect := map[string]string{}
		for _, tag := range t.info.Tags {
			expect[tag] = t.info.ShortID
		}
		if err := client.ImageImport(folder+"/"+t.name, expect, 30*time.Minute); err != nil {
			return 1, err
		}
	}
	// A load can report success without moving a tag; check every tag now names the tar's
	// image, as the pulls below do, before anything goes down.
	if len(uploads) > 0 {
		got, err := client.ImageIDsByTag()
		if err != nil {
			return 1, err
		}
		for _, t := range uploads {
			for _, tag := range t.info.Tags {
				if dsm.ShortID(got[tag]) != t.info.ShortID {
					return 1, dsm.Errorf("after loading %s, %s is %s, expected %s; the project is still up",
						t.name, tag, dsm.ShortID(got[tag]), t.info.ShortID)
				}
			}
		}
	}

	for _, pl := range pullNow {
		fmt.Fprintf(os.Stderr, "pulling %s\n", pl.target)
		if err := client.ImagePull(reg.URL, pl.repo, pl.tag, reg.User, reg.Password, time.Hour); err != nil {
			return 1, err
		}
		got, err := client.ImageIDsByTag()
		if err != nil {
			return 1, err
		}
		if dsm.ShortID(got[pl.target]) != pl.remote {
			return 1, dsm.Errorf("after pulling, %s is %s, expected %s", pl.target, dsm.ShortID(got[pl.target]), pl.remote)
		}
	}

	// --- downtime ---
	if err := downtime(client, p, target, composeChanged, expected, o.timeout); err != nil {
		status := "unknown (couldn't list projects)"
		if now, lerr := client.ProjectListed(p.ID); lerr == nil {
			status = now.Status
		}
		fmt.Fprintf(os.Stderr, "synodock: deploy stopped: %v\n  %s is now %s; compose backups are in %s/backups; "+
			"old images were left in place\n", err, p.Name, status, p.SharePath)
		return 1, nil
	}
	fmt.Printf("%s is up and healthy\n", p.Name)

	// --- after ---
	newIDs, err := client.ImageIDsByTag()
	if err != nil {
		return 0, err
	}
	for tag, id := range replaced {
		if o.keepOld || newIDs[tag] == id {
			continue
		}
		if err := client.ImageDeleteID(id); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not delete old image %s: %v\n", dsm.ShortID(id), err)
			continue
		}
		fmt.Printf("deleted old image %s %s\n", tag, dsm.ShortID(id))
	}
	return 0, nil
}

// downtime is clean, the compose update, build, and the health wait.
func downtime(client *dsm.Client, p *dsm.Project, target string, composeChanged bool, expected []string, timeout time.Duration) error {
	if code, err := client.ProjectStream(p.ID, "clean", printLine); err != nil || code != 0 {
		return fmt.Errorf("clean failed (exit %d)%s", code, errSuffix(err))
	}
	if composeChanged {
		if _, err := client.BackupProject(p, p.Content); err != nil {
			return err
		}
		if err := client.ProjectUpdate(p.ID, target); err != nil {
			return err
		}
		fmt.Println("updated compose")
	}
	if code, err := client.ProjectStream(p.ID, "build", printLine); err != nil || code != 0 {
		return fmt.Errorf("build failed (exit %d)%s", code, errSuffix(err))
	}
	return waitHealthy(client, p.ID, timeout, expected)
}

func printLine(line string) { fmt.Println(line) }

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}

// waitHealthy waits until every expected container is listed, running, and healthy if it
// has a check. A one-shot container that exited 0 counts as done. Container Manager's list
// trails Docker by a while after a create, so a missing name isn't an error yet.
func waitHealthy(client *dsm.Client, id string, timeout time.Duration, expected []string) error {
	deadline := time.Now().Add(timeout)
	for {
		p, err := client.ProjectByName(id)
		if err != nil {
			return err
		}
		listed := map[string]*dsm.ProjectContainer{}
		for i := range p.Containers {
			listed[p.Containers[i].BareName()] = &p.Containers[i]
		}
		var pending []string
		for _, name := range expected {
			c, ok := listed[name]
			if !ok {
				pending = append(pending, name+"=missing")
				continue
			}
			ready, status, err := containerReady(c)
			if err != nil {
				return fmt.Errorf("%s %v", name, err)
			}
			if !ready {
				pending = append(pending, name+"="+status)
			}
		}
		if len(pending) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not healthy after %s: %s", timeout, strings.Join(pending, ", "))
		}
		fmt.Fprintf(os.Stderr, "  waiting: %s\n", strings.Join(pending, ", "))
		time.Sleep(10 * time.Second)
	}
}

// confirmYes asks y/N on the terminal; without one, the deploy refuses unless -y.
func confirmYes(question string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return dsm.Errorf("not a terminal; pass -y to deploy without asking")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	var answer string
	fmt.Fscanln(os.Stdin, &answer)
	if strings.ToLower(strings.TrimSpace(answer)) != "y" {
		return dsm.Errorf("aborted")
	}
	return nil
}

// containerReady says whether one container is done for the health wait: running and
// healthy (if it has a check), or exited 0 where Docker won't restart it (no policy, or
// on-failure). One that exited non-zero with no restart policy is an error, since it won't
// come back. Otherwise status is what to show while waiting.
func containerReady(c *dsm.ProjectContainer) (ready bool, status string, err error) {
	policy := c.HostConfig.RestartPolicy.Name
	if c.State.Status == "exited" {
		if c.State.ExitCode == 0 && (policy == "" || policy == "no" || policy == "on-failure") {
			return true, "", nil
		}
		if policy == "" || policy == "no" {
			return false, "", fmt.Errorf("exited with status %d and won't restart", c.State.ExitCode)
		}
	}
	if c.State.Status == "running" && (c.Health() == "" || c.Health() == "healthy") {
		return true, "", nil
	}
	if h := c.Health(); h != "" && c.State.Status == "running" {
		return false, h, nil
	}
	return false, c.State.Status, nil
}
