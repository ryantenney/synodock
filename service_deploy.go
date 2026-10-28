package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ryantenney/synodock/internal/dsm"
)

// streamContainer is a compose progress line: Container NAME STATE.
var streamContainer = regexp.MustCompile(`Container\s+(\S+)\s+(\w+)`)

// untouched are the progress states compose prints for a container it left alone.
var untouched = map[string]bool{"Running": true, "Waiting": true, "Healthy": true}

// serviceDeploy replaces one service's container from a new image while the rest of the
// project keeps running. Container Manager has no per-service build, but its build is
// compose up -d, which creates a missing container and leaves current ones alone; so the
// image is fetched while everything runs, then just this container is stopped and deleted,
// and the build recreates only it. Deleting first matters: compose never does its
// recreate-in-place rename, which is what leaves Container Manager listing <12hex>_name.
// Compose would also recreate or start any other service that has drifted from the
// compose file, .env or its image tag, so drift blocks the deploy.
func serviceDeploy(client *dsm.Client, o options) (int, error) {
	p, err := client.ProjectByName(o.name)
	if err != nil {
		return 0, err
	}
	var early []string
	if o.file != "" {
		early = append(early, "--compose needs a full deploy: Container Manager only takes compose edits while a project is stopped")
	}
	if len(o.pulls)+len(o.images) != 1 {
		early = append(early, "--service takes exactly one --pull or --image")
	}
	if p.Status != "RUNNING" {
		early = append(early, fmt.Sprintf("%s is %s; --service only redeploys into a running project", p.Name, p.Status))
	}
	if blocked(early) {
		return 1, nil
	}
	services, err := parseServices(p.Content)
	if err != nil {
		return 0, err
	}
	svc, ok := services[o.service]
	if !ok {
		var names []string
		for n := range services {
			names = append(names, n)
		}
		sort.Strings(names)
		blocked([]string{fmt.Sprintf("%s has no service %q (has: %s)", p.Name, o.service, strings.Join(names, ", "))})
		return 1, nil
	}
	if svc.Image == "" {
		blocked([]string{o.service + " has no image: (a build: service can't be redeployed from an image)"})
		return 1, nil
	}
	// Kept in memory only: each container's environment holds secrets.
	running, err := client.ProjectServices(p.ID)
	if err != nil {
		return 0, err
	}
	targetRef := imageRef(svc.Image)
	current := running[o.service]
	name := svc.ContainerName
	if current != nil {
		name = current.BareName()
	} else if name == "" {
		name = composeProjectName(p.Name) + "-" + o.service + "-1"
	}
	oldIDs, err := client.ImageIDsByTag()
	if err != nil {
		return 0, err
	}

	// --- the new image ---
	var problems []string
	var newID, source, folder string
	var info dsm.ImageTar
	var reg *dsm.RegistryAuth
	var repo, tag, target string
	if len(o.pulls) == 1 {
		if reg, err = client.Registry(o.registry, o.user, o.registryPull); err != nil {
			return 0, err
		}
		repo, tag = splitRef(o.pulls[0])
		target = reg.PullHost + "/" + repo + ":" + tag
		if newID, err = dsm.RegistryImageID(reg.URL, repo, tag, reg.User, reg.Password); err != nil {
			return 0, err
		}
		if newID == "" {
			problems = append(problems, fmt.Sprintf("%s:%s is not in %s (push it first)", repo, tag, reg.URL))
		}
		if target != targetRef {
			problems = append(problems, fmt.Sprintf("%s runs %s, not %s", o.service, targetRef, target))
		}
		source = fmt.Sprintf("pull   %s (%s) from %s", target, newID, reg.PullURL)
	} else {
		tarPath := o.images[0]
		folder = strings.TrimSuffix(o.folder, "/")
		if folder == "" {
			folder = p.SharePath + "/images"
		}
		if info, err = dsm.ReadImageTar(tarPath); err != nil {
			return 0, err
		}
		st, err := os.Stat(tarPath)
		if err != nil {
			return 0, err
		}
		newID = info.IDs[targetRef]
		if newID == "" {
			problems = append(problems, fmt.Sprintf("%s is tagged %s; %s runs %s", filepath.Base(tarPath),
				strings.Join(info.Tags, ", "), o.service, targetRef))
		}
		free, err := client.ShareFreeBytes(folder)
		if err != nil {
			return 0, err
		}
		if free < st.Size()*3 {
			problems = append(problems, "not enough free space")
		}
		source = fmt.Sprintf("image  %s (%.0f MB) -> %s  tags=%s", filepath.Base(tarPath), float64(st.Size())/1e6,
			folder, strings.Join(info.Tags, ", "))
	}
	oldImage := ""
	if current != nil {
		oldImage = current.Image
		if newID != "" && dsm.ShortID(oldImage) == newID && current.State.Status == "running" {
			problems = append(problems, fmt.Sprintf("nothing to deploy: %s already runs %s", name, newID))
		}
	}

	// --- nothing else may be touched by the build ---
	for other, s := range services {
		if other == o.service {
			continue
		}
		dependent := s.NetworkMode == "service:"+o.service
		for _, d := range s.dependsOn() {
			dependent = dependent || d == o.service
		}
		for _, v := range s.VolumesFrom {
			dependent = dependent || strings.Split(v, ":")[0] == o.service
		}
		if dependent {
			problems = append(problems, fmt.Sprintf("%s depends on %s; compose would restart or wait on it (use a full deploy)", other, o.service))
		}
		if s.Image != "" && imageRef(s.Image) == targetRef {
			problems = append(problems, fmt.Sprintf("%s runs the same image %s; a full deploy updates both", other, targetRef))
		}
	}
	envFiles := map[string]map[string]*string{}
	readFailed := false
	for _, s := range services {
		for _, path := range s.envFiles() {
			if _, done := envFiles[path]; done {
				continue
			}
			if strings.Contains(path, "/") {
				problems = append(problems, "env_file "+path+" outside the project folder isn't supported")
				readFailed = true
				continue
			}
			data, err := client.FSGet(p.SharePath + "/" + path)
			if err != nil {
				problems = append(problems, "could not read env_file "+path)
				readFailed = true
				continue
			}
			envFiles[path] = dotenv(string(data))
		}
	}
	projectEnv, ok := envFiles[".env"]
	if !ok {
		projectEnv = map[string]*string{}
		if data, err := client.FSGet(p.SharePath + "/.env"); err == nil {
			projectEnv = dotenv(string(data))
		}
	}
	checked, unchecked := 0, 0
	if !readFailed {
		others := make([]string, 0, len(services))
		for other := range services {
			others = append(others, other)
		}
		sort.Strings(others)
		for _, other := range others {
			s := services[other]
			if other == o.service || len(s.Profiles) > 0 {
				continue
			}
			c := running[other]
			if c == nil {
				problems = append(problems, other+" has no container; the build would create it")
				continue
			}
			cname := c.BareName()
			if c.State.Status != "running" {
				problems = append(problems, fmt.Sprintf("%s is %s; the build would start it", cname, c.State.Status))
			}
			if s.Image == "" {
				problems = append(problems, other+" has a build: section; the build might rebuild it")
			} else if !strings.Contains(s.Image, "@") {
				ref := imageRef(s.Image)
				if id, ok := oldIDs[ref]; !ok {
					problems = append(problems, fmt.Sprintf("%s (%s) isn't on the NAS under that tag; the build would pull it", ref, other))
				} else if dsm.ShortID(id) != dsm.ShortID(c.Image) {
					problems = append(problems, fmt.Sprintf("%s runs an older image than %s now names; the build would recreate it", cname, ref))
				}
			}
			have := map[string]string{}
			for _, e := range c.Config.Env {
				if k, v, ok := strings.Cut(e, "="); ok {
					have[k] = v
				}
			}
			var drift []string
			for k, v := range s.expectedEnv(projectEnv, envFiles) {
				if v == nil {
					unchecked++
					continue
				}
				checked++
				if got, ok := have[k]; !ok || got != *v {
					drift = append(drift, k)
				}
			}
			if len(drift) > 0 { // key names only: the values are secrets
				sort.Strings(drift)
				problems = append(problems, fmt.Sprintf("%s's environment no longer matches compose/.env for %s; "+
					"the build would recreate it (a full deploy picks that up)", cname, strings.Join(drift, ", ")))
			}
		}
	}
	stale, err := client.StaleNames(p)
	if err != nil {
		return 0, err
	}
	if len(stale) > 0 {
		problems = append(problems, fmt.Sprintf("Container Manager lists %s under temporary names; run synodock project up %s "+
			"once to resync first", strings.Join(stale, ", "), p.Name))
	}

	fmt.Printf("deploy %s --service %s (%s)\n", p.Name, o.service, p.Status)
	fmt.Printf("  %s\n", source)
	if oldImage != "" {
		action := "deleted after healthy, if nothing else holds it"
		if o.keepOld {
			action = "kept"
		}
		fmt.Printf("  old    %s runs %s (%s)\n", name, dsm.ShortID(oldImage), action)
	}
	note := ""
	if unchecked > 0 {
		note = fmt.Sprintf(" (%d interpolated values not compared)", unchecked)
	}
	fmt.Printf("  others %d services left running: images current, %d environment values match%s\n",
		len(services)-1, checked, note)
	fetch := "upload + load"
	if reg != nil {
		fetch = "pull"
	}
	fmt.Printf("  steps  %s (still up) -> stop + delete %s (DOWN: %s only) -> build -> wait healthy\n", fetch, name, o.service)
	if blocked(problems) {
		return 1, nil
	}
	if o.dryRun {
		return 0, nil
	}
	if !o.yes {
		if err := confirmYes(fmt.Sprintf("take %s down and redeploy it?", name)); err != nil {
			return 1, err
		}
	}

	// --- while still up ---
	if reg != nil {
		fmt.Fprintf(os.Stderr, "pulling %s\n", target)
		if err := client.ImagePull(reg.PullURL, repo, tag, reg.User, reg.Password, time.Hour); err != nil {
			return 1, err
		}
		if err := client.CheckTags(map[string]string{target: newID}); err != nil {
			return 1, dsm.Errorf("after pulling: %v; nothing was taken down", err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "uploading %s\n", filepath.Base(o.images[0]))
		if _, err := client.ImageUpload(o.images[0], folder); err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stderr, "loading %s\n", filepath.Base(o.images[0]))
		if err := client.ImageImport(folder+"/"+filepath.Base(o.images[0]), info.IDs, 30*time.Minute); err != nil {
			return 1, err
		}
		if err := client.CheckTags(info.IDs); err != nil {
			return 1, dsm.Errorf("after loading: %v; nothing was taken down", err)
		}
	}

	// --- downtime, this service only ---
	if err := replaceContainer(client, p, current, name); err != nil {
		goBack := "push it to the registry"
		if reg == nil {
			goBack = "synodock image import its .prev.tar"
		}
		fmt.Fprintf(os.Stderr, "synodock: deploy stopped: %v\n  only %s is affected; the old image %s was left in place. "+
			"To go back, put it under %s again (%s) and run this again\n", err, o.service, orNone(dsm.ShortID(oldImage)), targetRef, goBack)
		return 1, nil
	}
	if err := waitHealthy(client, p.ID, o.timeout, []string{name}); err != nil {
		fmt.Fprintf(os.Stderr, "synodock: deploy stopped: %v\n  only %s is affected; the old image %s was left in place\n",
			err, o.service, orNone(dsm.ShortID(oldImage)))
		return 1, nil
	}

	// Container Manager's own list trails Docker by ~20s after a create.
	for deadline := time.Now().Add(90 * time.Second); ; {
		listed, err := client.Containers()
		if err != nil {
			return 1, err
		}
		found := false
		for _, c := range listed {
			found = found || c.Name == name
		}
		if stale, err := client.StaleNames(p); err == nil && found && len(stale) == 0 {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "warning: Container Manager doesn't list %s under its own name yet; old image kept. "+
				"Check synodock project ps %s before stopping or cleaning the project\n", name, p.Name)
			return 1, nil
		}
		time.Sleep(5 * time.Second)
	}
	fmt.Printf("%s is up and healthy; the rest of %s kept running\n", name, p.Name)

	if oldImage != "" && !o.keepOld && dsm.ShortID(oldImage) != newID {
		deleteOldServiceImage(client, p.ID, oldImage)
	}
	return 0, nil
}

// replaceContainer stops and deletes the service's container, then builds. If the delete
// fails, a container that was running is started again.
func replaceContainer(client *dsm.Client, p *dsm.Project, current *dsm.ServiceContainer, name string) error {
	if current != nil {
		wasRunning := current.State.Status == "running"
		if wasRunning {
			if err := client.ContainerAction(name, "stop"); err != nil {
				return err
			}
		}
		if err := client.ContainerDelete(name); err != nil {
			if wasRunning {
				if serr := client.ContainerAction(name, "start"); serr == nil {
					fmt.Fprintf(os.Stderr, "restarted the old %s\n", name)
				}
			}
			return err
		}
	}
	var touched []string
	code, err := client.ProjectStream(p.ID, "build", func(line string) {
		fmt.Println(line)
		for _, m := range streamContainer.FindAllStringSubmatch(line, -1) {
			if m[1] != name && !untouched[m[2]] {
				touched = append(touched, m[1]+" "+m[2])
			}
		}
	})
	if err != nil || code != 0 {
		return fmt.Errorf("build failed (exit %d)%s", code, errSuffix(err))
	}
	if len(touched) > 0 {
		sort.Strings(touched)
		fmt.Fprintf(os.Stderr, "warning: the build also touched %s\n", strings.Join(touched, ", "))
	}
	return nil
}

// deleteOldServiceImage deletes the image the service ran before, unless a container of
// the project or a tag still holds it.
func deleteOldServiceImage(client *dsm.Client, projectID, oldImage string) {
	running, err := client.ProjectServices(projectID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: old image %s kept: %v\n", dsm.ShortID(oldImage), err)
		return
	}
	for _, c := range running {
		if c.Image == oldImage {
			fmt.Printf("kept old image %s: %s still runs it\n", dsm.ShortID(oldImage), c.BareName())
			return
		}
	}
	tags, err := client.ImageIDsByTag()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: old image %s kept: %v\n", dsm.ShortID(oldImage), err)
		return
	}
	for tag, id := range tags {
		if id == oldImage {
			fmt.Printf("kept old image %s: still tagged %s\n", dsm.ShortID(oldImage), tag)
			return
		}
	}
	if err := client.ImageDeleteID(oldImage); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete old image %s: %v\n", dsm.ShortID(oldImage), err)
		return
	}
	fmt.Printf("deleted old image %s\n", dsm.ShortID(oldImage))
}

// blocked prints each problem as a BLOCKED line and says whether there were any.
func blocked(problems []string) bool {
	for _, p := range problems {
		fmt.Printf("  BLOCKED: %s\n", p)
	}
	return len(problems) > 0
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
