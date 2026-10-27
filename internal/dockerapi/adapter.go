// Package dockerapi is a Docker Engine API endpoint backed by Synology Container
// Manager's DSM Web API.
//
// Author: Ryan Tenney
//
// `synodock serve` serves this on a Unix socket only your user can open, so the
// stock `docker` CLI works against the NAS with nothing listening on the network
// and no SSH:
//
//	docker context create synology --docker host=unix://$HOME/.cache/synodock/docker.sock
//	docker --context synology ps -a
//
// Every call is translated to the SYNO.Docker.* / FileStation calls Container
// Manager's UI makes, so it covers what Container Manager's API can do:
//
//	ps, inspect, start, stop, restart, kill, rm, logs (incl. -f, by polling),
//	stats, images, image inspect, rmi, load, pull, network ls, exec,
//	events (polled every 2s), volume ls (only volumes some container mounts)
//
// exec goes through Container Manager's Terminal websocket (package terminal):
// always a TTY inside, so stderr comes back on stdout; no -u/--user. Plain `docker
// exec` (no -i/-t) is framed, so output is in order and binary-safe, gzipped in
// the container unless the server runs with SYNODOCK_GZIP=0; with -i or -t it's
// passed through as it comes and can be out of order (see package terminal). -d
// starts the command in the background; inspect can't follow it after that.
//
// Anything else (run/create, attach, cp, build, volume create/rm, compose) gets a
// 501 with a message instead of a guess.
package dockerapi

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ryantenney/synodock/internal/dsm"
)

const (
	apiVersion    = "1.47"
	minAPIVersion = "1.24"
	loadFolder    = "/docker/.synodock" // where `docker load` tars are staged on the NAS
	statsEvery    = 4 * time.Second     // between DSM stats calls (each ~1.3s, all containers at once)
)

var signals = map[string]int{"SIGHUP": 1, "SIGINT": 2, "SIGQUIT": 3, "SIGKILL": 9, "SIGUSR1": 10, "SIGUSR2": 12, "SIGTERM": 15}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func apiErrorf(status int, format string, args ...any) *apiError {
	return &apiError{status, fmt.Sprintf(format, args...)}
}

// tarManifest is (RepoTags, short image id) from a `docker save` tar; the id is
// the config blob's digest.
func tarManifest(path string) ([]string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, "", apiErrorf(400, "no manifest.json in the image tar")
		} else if err != nil {
			return nil, "", apiErrorf(400, "reading the image tar: %v", err)
		}
		if h.Name != "manifest.json" {
			continue
		}
		var entries []struct {
			Config   string
			RepoTags []string
		}
		if err := json.NewDecoder(tr).Decode(&entries); err != nil {
			return nil, "", apiErrorf(400, "manifest.json: %v", err)
		}
		var tags []string
		for _, e := range entries {
			tags = append(tags, e.RepoTags...)
		}
		id := ""
		if len(entries) > 0 {
			id = strings.TrimSuffix(entries[0].Config[strings.LastIndex(entries[0].Config, "/")+1:], ".json")
			id = id[:min(12, len(id))]
		}
		return tags, id, nil
	}
}

var tsRE = regexp.MustCompile(`^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(\.\d+)?(Z|[+-]\d\d:\d\d)?`)

// parseTS reads DSM/Docker ISO timestamps (nanosecond precision); ok is false for
// none.
func parseTS(value string) (time.Time, bool) {
	if value == "" || strings.HasPrefix(value, "0001-") {
		return time.Time{}, false
	}
	m := tsRE.FindStringSubmatch(value)
	if m == nil {
		return time.Time{}, false
	}
	frac := m[2]
	if len(frac) > 10 {
		frac = frac[:10]
	}
	tz := m[3]
	if tz == "" {
		tz = "Z"
	}
	t, err := time.Parse(time.RFC3339Nano, m[1]+frac+tz)
	return t, err == nil
}

func ago(when time.Time) string {
	secs := int(time.Since(when).Seconds())
	for _, u := range []struct {
		name string
		size int
	}{{"days", 86400}, {"hours", 3600}, {"minutes", 60}} {
		if secs >= 2*u.size {
			return fmt.Sprintf("%d %s", secs/u.size, u.name)
		}
	}
	switch {
	case secs >= 3600:
		return "About an hour"
	case secs >= 60:
		return "About a minute"
	}
	return fmt.Sprintf("%d seconds", max(secs, 1))
}

func stateStatus(c *dsm.Container) string {
	if c.State != nil && c.State.Status != "" {
		return c.State.Status
	}
	return c.Status
}

func statusText(s *dsm.State) string {
	if s == nil {
		return ""
	}
	switch s.Status {
	case "running":
		text := "Up"
		if started, ok := parseTS(s.StartedAt); ok {
			text = "Up " + ago(started)
		}
		if s.Health != nil && s.Health.Status != "" {
			text += " (" + s.Health.Status + ")"
		}
		return text
	case "exited":
		if finished, ok := parseTS(s.FinishedAt); ok {
			code := strings.TrimSpace(string(s.ExitCode))
			if code == "" || code == "null" {
				code = "0"
			}
			return fmt.Sprintf("Exited (%s) %s ago", code, ago(finished))
		}
		return "Exited"
	}
	if s.Status == "" {
		return ""
	}
	return strings.ToUpper(s.Status[:1]) + strings.ToLower(s.Status[1:])
}

// orRaw is raw, or fallback when DSM left it out.
func orRaw(raw json.RawMessage, fallback string) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(fallback)
	}
	return raw
}

// firstKey is the first key of a JSON object, in DSM's order.
func firstKey(raw json.RawMessage) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", false
	}
	tok, err := dec.Token()
	key, ok := tok.(string)
	return key, err == nil && ok
}

// cache shares one DSM answer among concurrent requests for a while: a dashboard
// like lazydocker streams stats for every container at once, and each sample
// would otherwise be two DSM calls. One fetch per key at a time; the others wait
// for it (a stats call takes ~1.3s, and 20 at once slowed every DSM call to ~5s).
type cache struct {
	mu         sync.Mutex
	entries    map[string]cacheEntry
	fetchLocks map[string]*sync.Mutex
}

type cacheEntry struct {
	at    time.Time
	value any
}

func cached[T any](c *cache, key string, ttl time.Duration, fetch func() (T, error)) (T, error) {
	c.mu.Lock()
	if c.fetchLocks == nil {
		c.fetchLocks, c.entries = map[string]*sync.Mutex{}, map[string]cacheEntry{}
	}
	lock := c.fetchLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		c.fetchLocks[key] = lock
	}
	c.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	c.mu.Lock()
	hit, ok := c.entries[key]
	c.mu.Unlock()
	if ok && time.Since(hit.at) < ttl {
		return hit.value.(T), nil
	}
	value, err := fetch()
	if err != nil {
		return value, err
	}
	c.mu.Lock()
	c.entries[key] = cacheEntry{time.Now(), value}
	c.mu.Unlock()
	return value, nil
}

func (c *cache) drop(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

func (c *cache) clear() {
	c.mu.Lock()
	clear(c.entries)
	c.mu.Unlock()
}

// Adapter translates Engine API calls into DSM calls. One DSM session, shared.
type Adapter struct {
	dsm   *dsm.Client
	cache cache

	execMu sync.Mutex
	execs  map[string]*Exec

	statsMu   sync.Mutex
	statsPrev map[string]json.RawMessage
	statsLast map[string]json.RawMessage

	volMu      sync.Mutex
	volAt      time.Time
	volList    []map[string]any
	volRefresh sync.Mutex
}

func NewAdapter(client *dsm.Client) *Adapter {
	return &Adapter{dsm: client, execs: map[string]*Exec{}}
}

func (a *Adapter) invalidate() { a.cache.clear() }

// --- lookups ----------------------------------------------------------------

func (a *Adapter) containers() ([]dsm.Container, error) {
	return cached(&a.cache, "containers", time.Second, a.dsm.Containers)
}

func (a *Adapter) container(ref string) (*dsm.Container, error) {
	ref = strings.TrimLeft(ref, "/")
	all, err := a.containers()
	if err != nil {
		return nil, err
	}
	var found []*dsm.Container
	for i := range all {
		c := &all[i]
		if ref == c.Name || ref == c.ID || len(ref) >= 3 && strings.HasPrefix(c.ID, ref) {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return nil, apiErrorf(404, "No such container: %s", ref)
	case 1:
		return found[0], nil
	}
	return nil, apiErrorf(400, "Multiple containers match %s", ref)
}

func (a *Adapter) images() ([]dsm.Image, error) {
	return cached(&a.cache, "images", time.Second, a.dsm.Images)
}

func (a *Adapter) image(ref string) (*dsm.Image, error) {
	bare, tag := ref, "latest"
	if i := strings.Index(ref, ":"); i >= 0 && !strings.HasPrefix(ref, "sha256:") {
		bare, tag = ref[:i], ref[i+1:]
	}
	short := strings.TrimPrefix(ref, "sha256:")
	images, err := a.images()
	if err != nil {
		return nil, err
	}
	for i := range images {
		img := &images[i]
		if img.Repository == bare && contains(img.Tags, tag) ||
			len(short) >= 3 && strings.HasPrefix(strings.TrimPrefix(img.ID, "sha256:"), short) {
			return img, nil
		}
	}
	return nil, apiErrorf(404, "No such image: %s", ref)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- containers -------------------------------------------------------------

func (a *Adapter) listContainers(showAll bool, filters map[string][]string) ([]map[string]any, error) {
	all, err := a.containers()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for i := range all {
		c := &all[i]
		if !showAll && !c.Running() || !matches(c, filters) {
			continue
		}
		networks := json.RawMessage("{}")
		if c.NetworkSettings != nil {
			networks = orRaw(c.NetworkSettings.Networks, "{}")
		}
		mode, ok := firstKey(networks)
		if !ok {
			mode = "default"
		}
		out = append(out, map[string]any{
			"Id":              c.ID,
			"Names":           []string{"/" + c.Name},
			"Image":           c.ImageName(),
			"ImageID":         c.ImageID,
			"Command":         orRaw(c.Cmd, `""`),
			"Created":         orRaw(c.Created, "0"),
			"Ports":           []any{},
			"Labels":          orRaw(c.Labels, "{}"),
			"State":           stateStatus(c),
			"Status":          statusText(c.State),
			"HostConfig":      map[string]string{"NetworkMode": mode},
			"NetworkSettings": map[string]any{"Networks": networks},
			"Mounts":          []any{},
		})
	}
	return out, nil
}

func matches(c *dsm.Container, filters map[string][]string) bool {
	var labels map[string]string
	_ = json.Unmarshal(orRaw(c.Labels, "{}"), &labels)
	for key, values := range filters {
		switch key {
		case "name":
			if !anyOf(values, func(v string) bool { return strings.Contains(c.Name, strings.TrimLeft(v, "/")) }) {
				return false
			}
		case "id":
			if !anyOf(values, func(v string) bool { return strings.HasPrefix(c.ID, v) }) {
				return false
			}
		case "status":
			if c.State == nil || !contains(values, c.State.Status) {
				return false
			}
		case "label":
			for _, v := range values {
				k, val, _ := strings.Cut(v, "=")
				have, ok := labels[k]
				if !ok || val != "" && have != val {
					return false
				}
			}
		}
	}
	return true
}

func anyOf(values []string, pred func(string) bool) bool {
	for _, v := range values {
		if pred(v) {
			return true
		}
	}
	return false
}

func (a *Adapter) inspectContainer(ref string) (json.RawMessage, error) {
	c, err := a.container(ref)
	if err != nil {
		return nil, err
	}
	return a.details(c.Name)
}

// details is the container's full docker inspect. lazydocker re-inspects every
// container a few times a second.
func (a *Adapter) details(name string) (json.RawMessage, error) {
	return cached(&a.cache, "details\x00"+name, 2*time.Second, func() (json.RawMessage, error) {
		var data struct {
			Details json.RawMessage `json:"details"`
		}
		err := a.dsm.CallInto(&data, "SYNO.Docker.Container", "get", 1, dsm.Params{"name": dsm.JS(name)})
		return orRaw(data.Details, "{}"), err
	})
}

func (a *Adapter) containerAction(ref, action, signal string) (int, error) {
	c, err := a.container(ref)
	if err != nil {
		return 0, err
	}
	if action == "start" && c.Running() || action == "stop" && !c.Running() {
		return 304, nil
	}
	if action == "kill" {
		sig := strings.ToUpper(signal)
		num, err := strconv.Atoi(sig)
		if err != nil {
			if !strings.HasPrefix(sig, "SIG") {
				sig = "SIG" + sig
			}
			var ok bool
			if num, ok = signals[sig]; !ok {
				return 0, apiErrorf(400, "unsupported signal %s", signal)
			}
		}
		_, err = a.dsm.Call("SYNO.Docker.Container", "signal", 1, dsm.Params{"name": dsm.JS(c.Name), "signal": strconv.Itoa(num)})
	} else {
		err = a.dsm.ContainerAction(c.Name, action)
	}
	a.invalidate()
	if err != nil {
		return 0, err
	}
	return 204, nil
}

func (a *Adapter) removeContainer(ref string, force bool) error {
	c, err := a.container(ref)
	if err != nil {
		return err
	}
	if c.Running() && !force {
		return apiErrorf(409, "You cannot remove a running container %s. Stop it first or use -f", c.ID)
	}
	_, err = a.dsm.Call("SYNO.Docker.Container", "delete", 1, dsm.Params{"name": dsm.JS(c.Name),
		"force": strconv.FormatBool(force), "preserve_profile": "false"})
	a.invalidate()
	return err
}

// stats is one stats sample. CPU % is a delta and DSM's precpu_stats is always
// empty, so the previous all-containers sample stands in for it.
func (a *Adapter) stats(ref string) (map[string]json.RawMessage, error) {
	c, err := a.container(ref)
	if err != nil {
		return nil, err
	}
	current, previous, err := a.allStats()
	if err != nil {
		return nil, err
	}
	s := map[string]json.RawMessage{}
	_ = dsm.Decode(current[c.ID], &s)
	setDefault(s, "name", "/"+c.Name)
	setDefault(s, "id", c.ID)
	s["read"] = mustJSON(time.Now().UTC().Format(time.RFC3339Nano))
	var prev map[string]json.RawMessage
	_ = dsm.Decode(previous[c.ID], &prev)
	s["precpu_stats"] = orRaw(prev["cpu_stats"], "{}")
	if _, ok := s["pids_stats"]; !ok {
		s["pids_stats"] = json.RawMessage(`{"current":` + string(orRaw(s["num_procs"], "0")) + `}`)
	}
	return s, nil
}

func setDefault(m map[string]json.RawMessage, key string, value any) {
	if _, ok := m[key]; !ok {
		m[key] = mustJSON(value)
	}
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

// allStats is (latest, the one before) of DSM's stats, which come for every
// container in one call of ~1.3s. Fetched at most every statsEvery, so a
// dashboard streaming stats for 20 containers costs one call per interval.
func (a *Adapter) allStats() (latest, previous map[string]json.RawMessage, err error) {
	fetch := func() (bool, error) {
		var data map[string]json.RawMessage
		if err := a.dsm.CallInto(&data, "SYNO.Docker.Container", "stats", 1, nil); err != nil {
			return false, err
		}
		if data == nil {
			data = map[string]json.RawMessage{}
		}
		a.statsMu.Lock()
		a.statsPrev, a.statsLast = a.statsLast, data
		a.statsMu.Unlock()
		return true, nil
	}
	if _, err := cached(&a.cache, "stats", statsEvery, fetch); err != nil {
		return nil, nil, err
	}
	a.statsMu.Lock()
	first := len(a.statsPrev) == 0
	a.statsMu.Unlock()
	if first { // first ever: a delta needs a second sample
		time.Sleep(time.Second)
		a.cache.drop("stats")
		if _, err := cached(&a.cache, "stats", statsEvery, fetch); err != nil {
			return nil, nil, err
		}
	}
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	return a.statsLast, a.statsPrev, nil
}

// top is docker top, from Container Manager's Processes view. lazydocker polls
// it for the selected container, so the answer is shared for a second.
func (a *Adapter) top(ref string) (map[string]any, error) {
	c, err := a.container(ref)
	if err != nil {
		return nil, err
	}
	if !c.Running() {
		return nil, apiErrorf(409, "Container %s is not running", c.ID)
	}
	procs, err := cached(&a.cache, "top\x00"+c.Name, time.Second, func() ([]dsm.Process, error) {
		return a.dsm.ContainerProcesses(c.Name)
	})
	if err != nil {
		return nil, err
	}
	rows := [][]string{}
	for i := range procs {
		rows = append(rows, procs[i].Row())
	}
	return map[string]any{"Titles": dsm.ProcessTitles, "Processes": rows}, nil
}

// logEntries are oldest first: the last tail (< 0 = all), at or after since.
func (a *Adapter) logEntries(name string, tail int, since time.Time) ([]dsm.LogEntry, error) {
	var entries []dsm.LogEntry
	if tail >= 0 {
		page, err := a.dsm.ContainerLogs(name, tail, 0, "DESC", "")
		if err != nil {
			return nil, err
		}
		for i := len(page.Logs) - 1; i >= 0; i-- {
			entries = append(entries, page.Logs[i])
		}
	} else {
		for offset := 0; ; {
			page, err := a.dsm.ContainerLogs(name, 1000, offset, "ASC", "")
			if err != nil {
				return nil, err
			}
			entries = append(entries, page.Logs...)
			offset += len(page.Logs)
			if len(page.Logs) == 0 || offset >= page.Total {
				break
			}
		}
	}
	if !since.IsZero() {
		kept := entries[:0]
		for _, e := range entries {
			if t, ok := parseTS(e.Created); !ok || !t.Before(since) {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	return entries, nil
}

// --- images -----------------------------------------------------------------

func repoTags(img *dsm.Image) []string {
	tags := []string{}
	for _, t := range img.Tags {
		tags = append(tags, img.Repository+":"+t)
	}
	return tags
}

func digests(img *dsm.Image) []string {
	if img.Digest == "" {
		return []string{}
	}
	return []string{img.Digest}
}

func number(n json.Number) json.Number {
	if n == "" {
		return "0"
	}
	return n
}

func virtualSize(img *dsm.Image) json.Number {
	if img.VirtualSize != "" {
		return img.VirtualSize
	}
	return number(img.Size)
}

func (a *Adapter) listImages() ([]map[string]any, error) {
	images, err := a.images()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for i := range images {
		img := &images[i]
		tags := repoTags(img)
		if len(tags) == 0 {
			tags = []string{"<none>:<none>"}
		}
		out = append(out, map[string]any{
			"Id": img.ID, "ParentId": "", "RepoTags": tags, "RepoDigests": digests(img),
			"Created": number(img.Created), "Size": number(img.Size), "VirtualSize": virtualSize(img),
			"SharedSize": -1, "Labels": map[string]string{}, "Containers": -1,
		})
	}
	return out, nil
}

func (a *Adapter) inspectImage(ref string) (map[string]any, error) {
	img, err := a.image(ref)
	if err != nil {
		return nil, err
	}
	created, _ := number(img.Created).Float64()
	return map[string]any{
		"Id": img.ID, "RepoTags": repoTags(img), "RepoDigests": digests(img),
		"Created": time.Unix(int64(created), 0).UTC().Format(time.RFC3339),
		"Size":    number(img.Size), "VirtualSize": virtualSize(img),
		"Os": "linux", "Architecture": "amd64", "Config": map[string]any{},
		"RootFS":  map[string]any{"Type": "layers", "Layers": []string{}},
		"Comment": img.Description,
	}, nil
}

func (a *Adapter) removeImage(ref string) ([]map[string]string, error) {
	img, err := a.image(ref)
	if err != nil {
		return nil, err
	}
	defer a.invalidate()
	if short := strings.TrimPrefix(ref, "sha256:"); short != "" && strings.HasPrefix(strings.TrimPrefix(img.ID, "sha256:"), short) {
		if err := a.dsm.ImageDeleteID(img.ID); err != nil {
			return nil, err
		}
		return []map[string]string{{"Deleted": img.ID}}, nil
	}
	repo, tag, ok := strings.Cut(ref, ":")
	if !ok {
		tag = "latest"
	}
	_, err = a.dsm.Call("SYNO.Docker.Image", "delete", 1, dsm.Params{"images": dsm.JS([]map[string]any{{"repository": repo, "tags": []string{tag}}})})
	if err != nil {
		return nil, err
	}
	return []map[string]string{{"Untagged": repo + ":" + tag}}, nil
}

func (a *Adapter) load(tarPath string, emit func(any) error) error {
	tags, imageID, err := tarManifest(tarPath)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("load-%d-%d.tar", os.Getpid(), time.Now().Unix())
	staged := filepath.Join(filepath.Dir(tarPath), name)
	if err := os.Rename(tarPath, staged); err != nil {
		return err
	}
	err = a.dsm.FSPut(staged, loadFolder, true, true)
	os.Remove(staged)
	if err != nil {
		return err
	}
	var expect map[string]string
	if len(tags) > 0 {
		expect = map[string]string{}
		for _, t := range tags {
			expect[t] = imageID
		}
	}
	err = a.dsm.ImageImport(loadFolder+"/"+name, expect, 30*time.Minute)
	_ = a.dsm.FSDelete(loadFolder + "/" + name)
	if err != nil {
		return err
	}
	a.invalidate()
	if len(tags) == 0 {
		tags = []string{"sha256:" + imageID}
	}
	for _, tag := range tags {
		if err := emit(map[string]string{"stream": "Loaded image: " + tag + "\n"}); err != nil {
			return err
		}
	}
	return nil
}

var (
	hubPrefix     = regexp.MustCompile(`^(docker\.io|index\.docker\.io|registry-1\.docker\.io)/`)
	otherRegistry = regexp.MustCompile(`^[^/]+[.:][^/]*/`)
)

func (a *Adapter) pull(fromImage, tag string, emit func(any) error) error {
	// The CLI sends Docker Hub images fully qualified (docker.io/library/redis).
	fromImage = hubPrefix.ReplaceAllString(fromImage, "")
	if strings.Contains(fromImage, "@") || otherRegistry.MatchString(fromImage) {
		return apiErrorf(501, "the DSM adapter only pulls from Docker Hub by repository:tag")
	}
	if i := strings.LastIndex(fromImage, ":"); i >= 0 && tag == "" {
		fromImage, tag = fromImage[:i], fromImage[i+1:]
	}
	if tag == "" {
		tag = "latest"
	}
	repo := strings.TrimPrefix(fromImage, "library/")
	var task struct {
		TaskID string `json:"task_id"`
	}
	if err := a.dsm.CallInto(&task, "SYNO.Docker.Image", "pull_start", 1, dsm.Params{"repository": dsm.JS(repo),
		"tag": dsm.JS(tag), "registry": dsm.JS(""), "username": dsm.JS(""), "password": dsm.JS("")}); err != nil {
		return err
	}
	if err := emit(map[string]string{"status": "Pulling from " + repo, "id": tag}); err != nil {
		return err
	}
	for deadline := time.Now().Add(time.Hour); time.Now().Before(deadline); {
		time.Sleep(2 * time.Second)
		var status map[string]any
		if err := a.dsm.CallInto(&status, "SYNO.Docker.Image", "pull_status", 1, dsm.Params{"task_id": dsm.JS(task.TaskID)}); err != nil {
			return err
		}
		if e := firstSet(status["error"], status["errors"]); e != nil {
			return apiErrorf(500, "pull failed: %v", e)
		}
		if truthy(status["finished"]) {
			a.invalidate()
			return emit(map[string]string{"status": fmt.Sprintf("Status: Image is up to date for %s:%s", repo, tag)})
		}
		if truthy(status["downloaded"]) {
			if err := emit(map[string]any{"status": "Downloading", "progressDetail": map[string]any{"current": status["downloaded"]}, "id": tag}); err != nil {
				return err
			}
		}
	}
	return apiErrorf(504, "pull did not finish within an hour")
}

// truthy is Python truthiness, for DSM's loosely typed status fields.
func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case json.Number:
		f, _ := v.Float64()
		return f != 0
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

func firstSet(values ...any) any {
	for _, v := range values {
		if truthy(v) {
			return v
		}
	}
	return nil
}

// --- networks / system ------------------------------------------------------

func (a *Adapter) networks() ([]map[string]any, error) {
	var data struct {
		Network []map[string]any `json:"network"`
	}
	if err := a.dsm.CallInto(&data, "SYNO.Docker.Network", "list", 1, nil); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, n := range data.Network {
		str := func(key string) any {
			if v, ok := n[key]; ok && v != nil {
				return v
			}
			return ""
		}
		ipv6 := n["enable_ipv6"]
		if ipv6 == nil {
			ipv6 = false
		}
		out = append(out, map[string]any{
			"Name": str("name"), "Id": str("id"), "Driver": str("driver"), "Scope": "local",
			"EnableIPv6": ipv6, "Internal": false, "Attachable": false,
			"IPAM":       map[string]any{"Driver": "default", "Config": []map[string]any{{"Subnet": str("subnet"), "Gateway": str("gateway")}}},
			"Containers": map[string]any{}, "Options": map[string]any{}, "Labels": map[string]any{},
		})
	}
	return out, nil
}

// volumes are the named volumes, gathered from the containers that mount them:
// DSM has no volume list, so a volume no container uses doesn't show. That's one
// DSM call per container, ~50 queued behind everything else, so Serve gathers them
// at startup and a request gets the last list at once, refreshed in the background
// when it's over 30s old.
func (a *Adapter) volumes() ([]map[string]any, error) {
	a.volMu.Lock()
	at, list := a.volAt, a.volList
	a.volMu.Unlock()
	if list == nil {
		return a.gatherVolumes()
	}
	if time.Since(at) > 30*time.Second && a.volRefresh.TryLock() {
		go func() {
			defer a.volRefresh.Unlock()
			_, _ = a.gatherVolumes()
		}()
	}
	return list, nil
}

func (a *Adapter) gatherVolumes() ([]map[string]any, error) {
	all, err := a.containers()
	if err != nil {
		return nil, err
	}
	found := map[string]map[string]any{}
	for _, c := range all {
		raw, err := a.details(c.Name)
		if err != nil {
			return nil, err
		}
		var details struct {
			Mounts []struct{ Type, Name, Driver, Source string }
		}
		_ = json.Unmarshal(raw, &details)
		for _, m := range details.Mounts {
			if m.Type != "volume" || m.Name == "" || found[m.Name] != nil {
				continue
			}
			driver := m.Driver
			if driver == "" {
				driver = "local"
			}
			found[m.Name] = map[string]any{"Name": m.Name, "Driver": driver, "Mountpoint": m.Source,
				"Scope": "local", "Labels": map[string]any{}, "Options": map[string]any{}, "CreatedAt": ""}
		}
	}
	list := make([]map[string]any, 0, len(found))
	for _, v := range found {
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool { return list[i]["Name"].(string) < list[j]["Name"].(string) })
	a.volMu.Lock()
	a.volAt, a.volList = time.Now(), list // kept apart from the cache invalidate() clears
	a.volMu.Unlock()
	return list, nil
}

type snapContainer struct{ id, name, image, status, started string }

type snapImage struct {
	id   string
	tags []string
}

// snapshot is what /events diffs: containers (name, image, status, started at)
// and image tags, in DSM's order. StartedAt catches a restart quicker than the poll.
type snapshot struct {
	containers []snapContainer
	images     []snapImage
}

func (a *Adapter) snapshot() (*snapshot, error) {
	cs, err := a.containers()
	if err != nil {
		return nil, err
	}
	imgs, err := a.images()
	if err != nil {
		return nil, err
	}
	s := &snapshot{}
	for i := range cs {
		c := &cs[i]
		started := ""
		if c.State != nil {
			started = c.State.StartedAt
		}
		s.containers = append(s.containers, snapContainer{c.ID, c.Name, c.ImageName(), stateStatus(c), started})
	}
	for _, img := range imgs {
		var tags []string
		for _, t := range img.Tags {
			tags = append(tags, img.Repository+":"+t)
		}
		s.images = append(s.images, snapImage{img.ID, tags})
	}
	return s, nil
}

type event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Scope    string `json:"scope"`
	Time     int64  `json:"time"`
	TimeNano int64  `json:"timeNano"`
	Status   string `json:"status,omitempty"`
	ID       string `json:"id,omitempty"`
	From     string `json:"from,omitempty"`
}

// eventsBetween are Docker-style events for what changed between two snapshots.
func eventsBetween(old, cur *snapshot) []event {
	now := time.Now().UnixNano()
	var out []event
	add := func(kind, action, actor string, attrs map[string]string, from string) {
		e := event{Type: kind, Action: action, Scope: "local", Time: now / 1e9, TimeNano: now}
		e.Actor.ID, e.Actor.Attributes = actor, attrs
		if kind == "container" {
			e.Status, e.ID, e.From = action, actor, from
		}
		out = append(out, e)
	}
	container := func(action string, c snapContainer) {
		add("container", action, c.id, map[string]string{"name": c.name, "image": c.image}, c.image)
	}
	oldC := map[string]snapContainer{}
	for _, c := range old.containers {
		oldC[c.id] = c
	}
	newC := map[string]bool{}
	for _, c := range cur.containers {
		newC[c.id] = true
		before, ok := oldC[c.id]
		switch {
		case !ok:
			container("create", c)
			if c.status == "running" {
				container("start", c)
			}
		case before.status == "running" && c.status == "running" && before.started != c.started:
			for _, action := range []string{"die", "start", "restart"} {
				container(action, c)
			}
		case before.status != c.status:
			switch {
			case c.status == "running":
				if before.status == "paused" {
					container("unpause", c)
				} else {
					container("start", c)
				}
			case c.status == "paused":
				container("pause", c)
			case before.status == "running" || before.status == "paused":
				container("die", c)
				container("stop", c)
			}
		}
	}
	for _, c := range old.containers {
		if !newC[c.id] {
			container("destroy", c)
		}
	}
	oldI := map[string][]string{}
	for _, img := range old.images {
		oldI[img.id] = img.tags
	}
	newI := map[string][]string{}
	for _, img := range cur.images {
		newI[img.id] = img.tags
		before, existed := oldI[img.id]
		for _, tag := range img.tags {
			if !contains(before, tag) {
				action := "pull"
				if existed {
					action = "tag"
				}
				add("image", action, img.id, map[string]string{"name": tag}, tag)
			}
		}
	}
	for _, img := range old.images {
		after, ok := newI[img.id]
		if !ok {
			name := img.id
			if len(img.tags) > 0 {
				name = img.tags[0]
			}
			add("image", "delete", img.id, map[string]string{"name": name}, img.id)
			continue
		}
		for _, tag := range img.tags {
			if !contains(after, tag) {
				add("image", "untag", img.id, map[string]string{"name": tag}, tag)
			}
		}
	}
	return out
}

func (a *Adapter) info() (map[string]any, error) {
	cs, err := a.containers()
	if err != nil {
		return nil, err
	}
	imgs, err := a.images()
	if err != nil {
		return nil, err
	}
	running := 0
	for i := range cs {
		if cs[i].Running() {
			running++
		}
	}
	host := ""
	if u, err := url.Parse(a.dsm.URL); err == nil {
		host = u.Hostname()
	}
	return map[string]any{
		"ID": "synodock", "Name": host,
		"Containers": len(cs), "ContainersRunning": running, "ContainersPaused": 0,
		"ContainersStopped": len(cs) - running, "Images": len(imgs),
		"ServerVersion": "Container Manager (via DSM Web API)", "OperatingSystem": "Synology DSM",
		"OSType": "linux", "Architecture": "x86_64", "Driver": "", "Plugins": map[string]any{}, "Labels": []string{},
		"Warnings": []string{"Served by synodock: only a subset of the Engine API is supported"},
	}, nil
}
