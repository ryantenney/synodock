package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
// With --service, only that service's container is replaced (serviceDeploy).
func projectDeploy(client *dsm.Client, o options) (int, error) {
	if o.service != "" {
		return serviceDeploy(client, o)
	}
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
		if reg, err = client.Registry(o.registry, o.user, o.registryPull); err != nil {
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
			if dsm.ShortID(currentIDs[tag]) != info.IDs[tag] {
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
		target := reg.PullHost + "/" + repo + ":" + tag
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
			fmt.Printf("  skip   %s: %s already loaded\n", t.name, strings.Join(t.info.Tags, ", "))
		}
	}
	for _, t := range uploads {
		fmt.Printf("  image  %s (%.0f MB) -> %s  tags=%s\n", t.name, float64(t.size)/1e6, folder, strings.Join(t.info.Tags, ", "))
	}
	for _, pl := range pulls {
		if pl.current {
			fmt.Printf("  skip   %s: already the registry's image (%s)\n", pl.target, pl.remote)
		} else {
			fmt.Printf("  pull   %s (%s) from %s\n", pl.target, pl.remote, reg.PullURL)
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
		if err := client.ImageImport(folder+"/"+t.name, t.info.IDs, 30*time.Minute); err != nil {
			return 1, err
		}
	}
	// A load can report success without moving a tag; check every tag now names the tar's
	// image, as the pulls below do, before anything goes down.
	for _, t := range uploads {
		if err := client.CheckTags(t.info.IDs); err != nil {
			return 1, dsm.Errorf("after loading %s: %v; the project is still up", t.name, err)
		}
	}

	for _, pl := range pullNow {
		fmt.Fprintf(os.Stderr, "pulling %s\n", pl.target)
		if err := client.ImagePull(reg.PullURL, pl.repo, pl.tag, reg.User, reg.Password, time.Hour); err != nil {
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
	if !o.keepOld {
		deleteReplaced(client, replaced, newIDs)
	}
	return 0, nil
}

// downtime is clean, the compose update, build, and the health wait.
func downtime(client *dsm.Client, p *dsm.Project, target string, composeChanged bool, expected []string, timeout time.Duration) error {
	// The backup needs no downtime, so it goes first: if it fails, nothing is down yet.
	if composeChanged {
		if _, err := client.BackupProject(p, p.Content); err != nil {
			return fmt.Errorf("backing up the compose file: %w (nothing was taken down)", err)
		}
	}
	if code, err := client.ProjectStream(p.ID, "clean", printLine); err != nil || code != 0 {
		return fmt.Errorf("clean failed (exit %d)%s", code, errSuffix(err))
	}
	if composeChanged {
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
// pollEvery is how often the health wait looks again, and unhealthyGrace how long a
// container may stay unhealthy before the wait gives up on it: a health check can fail a
// few times while a service starts and still recover, so one unhealthy look isn't enough.
var (
	pollEvery      = 10 * time.Second
	unhealthyGrace = 2 * time.Minute
)

func waitHealthy(client *dsm.Client, id string, timeout time.Duration, expected []string) error {
	deadline := time.Now().Add(timeout)
	unhealthySince := map[string]time.Time{}
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
			if status != "unhealthy" {
				delete(unhealthySince, name)
			} else if since, ok := unhealthySince[name]; !ok {
				unhealthySince[name] = time.Now()
			} else if time.Since(since) >= unhealthyGrace {
				return fmt.Errorf("%s has been unhealthy for %s", name, time.Since(since).Round(time.Second))
			}
		}
		if len(pending) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not healthy after %s: %s", timeout, strings.Join(pending, ", "))
		}
		fmt.Fprintf(os.Stderr, "  waiting: %s\n", strings.Join(pending, ", "))
		time.Sleep(pollEvery)
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

// oldImagesToDelete is the images a deploy replaced and can delete: each old id once, and
// only if no tag points at it any more. Deleting by id removes every tag on the image, so
// an old image that still carries another tag (app:1.4 beside the app:latest that moved)
// is kept, with the tags that hold it.
func oldImagesToDelete(replaced, now map[string]string) (del map[string][]string, kept map[string][]string) {
	del, kept = map[string][]string{}, map[string][]string{}
	for tag, id := range replaced {
		if now[tag] == id { // the tag didn't move; nothing replaced it
			continue
		}
		del[id] = append(del[id], tag)
	}
	for id := range del {
		for tag, other := range now {
			if other == id {
				kept[id] = append(kept[id], tag)
			}
		}
		if len(kept[id]) > 0 {
			delete(del, id)
		}
	}
	return del, kept
}

func deleteReplaced(client *dsm.Client, replaced, now map[string]string) {
	del, kept := oldImagesToDelete(replaced, now)
	for id, tags := range kept {
		sort.Strings(tags)
		fmt.Printf("kept old image %s: still tagged %s\n", dsm.ShortID(id), strings.Join(tags, ", "))
	}
	for id, tags := range del {
		sort.Strings(tags)
		if err := client.ImageDeleteID(id); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not delete old image %s: %v\n", dsm.ShortID(id), err)
			continue
		}
		fmt.Printf("deleted old image %s (was %s)\n", dsm.ShortID(id), strings.Join(tags, ", "))
	}
}
