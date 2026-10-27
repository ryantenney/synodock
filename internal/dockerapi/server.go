package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ryantenney/synodock/internal/dsm"
	"github.com/ryantenney/synodock/internal/terminal"
)

// Exec is an exec between create and inspect: what the CLI asked for, then its result.
type Exec struct {
	ID        string
	container *dsm.Container
	config    execConfig

	mu       sync.Mutex
	running  bool
	exitCode *int
	finished time.Time
	session  *terminal.Session
	size     [2]int // rows, cols; zero until known
}

type execConfig struct {
	Cmd         []string
	Env         []string
	WorkingDir  string
	User        string
	Privileged  bool
	AttachStdin bool
	Tty         bool
}

func (ex *Exec) argv() []string {
	argv := append([]string(nil), ex.config.Cmd...)
	if len(ex.config.Env) > 0 {
		argv = append(append([]string{"env"}, ex.config.Env...), argv...)
	}
	if ex.config.WorkingDir != "" {
		argv = append([]string{"sh", "-c", `cd "$1" || exit 126; shift; exec "$@"`, "sh", ex.config.WorkingDir}, argv...)
	}
	return argv
}

func (ex *Exec) resize(rows, cols int) {
	ex.mu.Lock()
	ex.size = [2]int{rows, cols}
	s := ex.session
	ex.mu.Unlock()
	if s != nil {
		_ = s.Resize(rows, cols)
	}
}

func (ex *Exec) inspect() map[string]any {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	cmd := ex.config.Cmd
	var code any
	if ex.exitCode != nil {
		code = *ex.exitCode
	}
	return map[string]any{"ID": ex.ID, "Running": ex.running, "ExitCode": code, "Pid": 0,
		"ContainerID": ex.container.ID, "CanRemove": false, "DetachKeys": "",
		"OpenStdin": ex.config.AttachStdin, "OpenStdout": true, "OpenStderr": true,
		"ProcessConfig": map[string]any{"entrypoint": cmd[0], "arguments": cmd[1:], "tty": ex.config.Tty,
			"privileged": false, "user": ""}}
}

func (ex *Exec) finish(code *int) {
	ex.mu.Lock()
	ex.session = nil
	if code != nil {
		ex.exitCode = code
	}
	ex.running = false
	ex.finished = time.Now()
	ex.mu.Unlock()
}

// query is the request's query string, where a blank value counts as missing
// (as the docker CLI sends filters= and tail=).
type query url.Values

func (q query) get(key, fallback string) string {
	if v := url.Values(q).Get(key); v != "" {
		return v
	}
	return fallback
}

func (q query) flag(key string) bool {
	switch q.get(key, "0") {
	case "1", "true", "True":
		return true
	}
	return false
}

// filters decodes a filters param: {"key": ["value", ...]} or {"key": {"value": true}}.
func (q query) filters() (map[string][]string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(q.get("filters", "{}")), &raw); err != nil {
		return nil, apiErrorf(400, "invalid filters: %v", err)
	}
	out := map[string][]string{}
	for key, value := range raw {
		var list []string
		if json.Unmarshal(value, &list) != nil {
			var set map[string]bool
			if json.Unmarshal(value, &set) != nil {
				var one string
				_ = json.Unmarshal(value, &one)
				list = []string{one}
			}
			for k := range set {
				list = append(list, k)
			}
		}
		out[key] = list
	}
	return out, nil
}

type route struct {
	method  string
	pattern *regexp.Regexp
	handle  func(s *server, w *response, r *http.Request, q query, p []string) error
}

func at(method, pattern string, handle func(s *server, w *response, r *http.Request, q query, p []string) error) route {
	return route{method, regexp.MustCompile("^" + pattern + "$"), handle}
}

var routes = []route{
	at("GET", `/_ping`, (*server).ping), at("HEAD", `/_ping`, (*server).ping),
	at("GET", `/version`, (*server).versionJSON), at("GET", `/info`, (*server).infoJSON),
	at("GET", `/containers/json`, (*server).containersJSON),
	at("GET", `/containers/([^/]+)/json`, (*server).containerJSON),
	at("GET", `/containers/([^/]+)/logs`, (*server).containerLogs),
	at("GET", `/containers/([^/]+)/stats`, (*server).containerStats),
	at("GET", `/containers/([^/]+)/top`, (*server).containerTop),
	at("POST", `/containers/([^/]+)/(start|stop|restart|kill)`, (*server).containerAction),
	at("DELETE", `/containers/([^/]+)`, (*server).containerDelete),
	at("GET", `/images/json`, (*server).imagesJSON),
	at("POST", `/images/load`, (*server).imagesLoad),
	at("POST", `/images/create`, (*server).imagesCreate),
	at("GET", `/images/(.+)/json`, (*server).imageJSON),
	at("DELETE", `/images/(.+)`, (*server).imageDelete),
	at("GET", `/networks`, (*server).networksJSON),
	at("GET", `/volumes`, (*server).volumesJSON),
	at("GET", `/events`, (*server).events),
	at("POST", `/containers/([^/]+)/exec`, (*server).execCreate),
	at("POST", `/exec/([0-9a-f]{64})/start`, (*server).execStart),
	at("POST", `/exec/([0-9a-f]{64})/resize`, (*server).execResize),
	at("GET", `/exec/([0-9a-f]{64})/json`, (*server).execJSON),
}

var versionPrefix = regexp.MustCompile(`^/v1\.\d+`)

type server struct {
	adapter *Adapter
	verbose bool
	version string

	onDemand bool        // started by launchd or systemd
	denied   atomic.Bool // macOS refused it the local network

	idleMu   sync.Mutex
	open     int       // requests in progress, hijacked execs included
	lastDone time.Time // when the last one ended (or we started)
}

// busy counts a request as open until the returned func is called.
func (s *server) busy() func() {
	s.idleMu.Lock()
	s.open++
	s.idleMu.Unlock()
	return func() {
		s.idleMu.Lock()
		s.open--
		s.lastDone = time.Now()
		s.idleMu.Unlock()
	}
}

// response notes whether anything has gone out, so an error after a stream has
// begun isn't written as a second response.
type response struct {
	http.ResponseWriter
	status int
}

func (w *response) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *response) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(b)
}

func (s *server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	defer s.busy()()
	w := &response{ResponseWriter: rw}
	h := w.Header()
	h.Set("Api-Version", apiVersion)
	h.Set("Docker-Experimental", "false")
	h.Set("Ostype", "linux")
	h.Set("Server", "synodock")
	path := versionPrefix.ReplaceAllString(r.URL.EscapedPath(), "")
	q := query(r.URL.Query())
	err := error(nil)
	matched := false
	for _, rt := range routes {
		m := rt.pattern.FindStringSubmatch(path)
		if m != nil && rt.method == r.Method {
			matched = true
			err = rt.handle(s, w, r, q, m[1:])
			break
		}
	}
	if !matched {
		_, _ = io.Copy(io.Discard, r.Body)
		err = apiErrorf(501, "%s %s is not supported by the DSM adapter (see the synodock README for what is)", r.Method, path)
	}
	var apiErr *apiError
	var dsmErr *dsm.Error
	switch {
	case err == nil, errors.Is(err, syscall.EPIPE), errors.Is(err, syscall.ECONNRESET):
	case w.status != 0: // mid-stream: nothing more to say
	case errors.As(err, &apiErr):
		writeJSON(w, apiErr.status, map[string]string{"message": apiErr.msg})
	case errors.As(err, &dsmErr):
		writeJSON(w, 502, map[string]string{"message": "DSM: " + dsmErr.Error()})
	case errors.Is(err, syscall.EHOSTUNREACH) && s.onDemand && runtime.GOOS == "darwin":
		// The permission only reaches processes started after it's given: exit once
		// this request is done, so the next connection starts one that has it.
		s.denied.Store(true)
		writeJSON(w, 502, map[string]string{"message": "DSM: " + err.Error() + " (" + LocalNetworkDenied + ")"})
	default:
		writeJSON(w, 500, map[string]string{"message": err.Error()})
	}
	if s.verbose {
		fmt.Fprintf(os.Stderr, "%s %s -> %d\n", r.Method, r.URL.RequestURI(), max(w.status, 101))
	}
}

func encodeJSON(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func writeJSON(w http.ResponseWriter, status int, body any) error {
	data := encodeJSON(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, err := w.Write(data)
	return err
}

func empty(w http.ResponseWriter, status int) error {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
	return nil
}

// stream starts a chunked response; send writes one piece of it.
func stream(w http.ResponseWriter, contentType string) func([]byte) error {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	if flusher == nil {
		flusher, _ = w.(*response).ResponseWriter.(http.Flusher)
	}
	flusher.Flush()
	return func(data []byte) error {
		if len(data) == 0 {
			return nil
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
}

func jsonLines(send func([]byte) error) func(any) error {
	return func(msg any) error { return send(append(encodeJSON(msg), '\n')) }
}

// sleep waits d, or returns false once the client has gone.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// multiplexed frames one stream's bytes as dockerd does without a TTY.
func multiplexed(stream byte, data []byte) []byte {
	header := make([]byte, 8, 8+len(data))
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(data)))
	return append(header, data...)
}

// --- endpoints --------------------------------------------------------------

func (s *server) ping(w *response, r *http.Request, q query, p []string) error {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", "2")
	w.WriteHeader(200)
	_, err := w.Write([]byte("OK"))
	return err
}

func (s *server) versionJSON(w *response, r *http.Request, q query, p []string) error {
	return writeJSON(w, 200, map[string]any{"Version": s.version, "ApiVersion": apiVersion, "MinAPIVersion": minAPIVersion,
		"Os": "linux", "Arch": "amd64", "KernelVersion": "", "GitCommit": "", "GoVersion": "", "BuildTime": "",
		"Components": []map[string]any{{"Name": "Engine", "Version": s.version, "Details": map[string]string{"ApiVersion": apiVersion}}}})
}

func (s *server) infoJSON(w *response, r *http.Request, q query, p []string) error {
	info, err := s.adapter.info()
	if err != nil {
		return err
	}
	return writeJSON(w, 200, info)
}

func (s *server) containersJSON(w *response, r *http.Request, q query, p []string) error {
	filters, err := q.filters()
	if err != nil {
		return err
	}
	list, err := s.adapter.listContainers(q.flag("all"), filters)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, list)
}

func (s *server) containerJSON(w *response, r *http.Request, q query, p []string) error {
	details, err := s.adapter.inspectContainer(p[0])
	if err != nil {
		return err
	}
	return writeJSON(w, 200, details)
}

func (s *server) containerAction(w *response, r *http.Request, q query, p []string) error {
	_, _ = io.Copy(io.Discard, r.Body)
	status, err := s.adapter.containerAction(p[0], p[1], q.get("signal", "SIGKILL"))
	if err != nil {
		return err
	}
	return empty(w, status)
}

func (s *server) containerDelete(w *response, r *http.Request, q query, p []string) error {
	if err := s.adapter.removeContainer(p[0], q.flag("force")); err != nil {
		return err
	}
	return empty(w, 204)
}

func (s *server) containerStats(w *response, r *http.Request, q query, p []string) error {
	if v := q.get("stream", "1"); v == "0" || v == "false" || v == "False" {
		stats, err := s.adapter.stats(p[0])
		if err != nil {
			return err
		}
		return writeJSON(w, 200, stats)
	}
	stats, err := s.adapter.stats(p[0]) // a bad id is a 404, not an empty stream
	if err != nil {
		return err
	}
	send := jsonLines(stream(w, "application/json"))
	for {
		if err := send(stats); err != nil {
			return err
		}
		if !sleep(r.Context(), statsEvery) {
			return nil
		}
		if stats, err = s.adapter.stats(p[0]); err != nil {
			return err
		}
	}
}

func (s *server) containerTop(w *response, r *http.Request, q query, p []string) error {
	if q.get("ps_args", "") != "" {
		return apiErrorf(501, "docker top with ps options is not supported by the DSM adapter "+
			"(Container Manager reports PID, %%CPU, %%MEM, RSS, STIME and CMD)")
	}
	top, err := s.adapter.top(p[0])
	if err != nil {
		return err
	}
	return writeJSON(w, 200, top)
}

func (s *server) containerLogs(w *response, r *http.Request, q query, p []string) error {
	want := map[string]bool{"stdout": q.flag("stdout"), "stderr": q.flag("stderr")}
	tail := -1
	if t := q.get("tail", "all"); t != "all" {
		n, err := strconv.Atoi(t)
		if err != nil {
			return apiErrorf(400, "invalid tail %q", t)
		}
		tail = n
	}
	var since time.Time
	if v, err := strconv.ParseFloat(q.get("since", "0"), 64); err == nil && v != 0 {
		since = time.Unix(0, int64(v*1e9)).UTC()
	}
	timestamps, follow := q.flag("timestamps"), q.flag("follow")

	c, err := s.adapter.container(p[0])
	if err != nil {
		return err
	}
	raw, err := s.adapter.details(c.Name)
	if err != nil {
		return err
	}
	var details struct{ Config struct{ Tty bool } }
	_ = json.Unmarshal(raw, &details)
	tty := details.Config.Tty
	entries, err := s.adapter.logEntries(c.Name, tail, since)
	if err != nil {
		return err
	}
	contentType := "application/vnd.docker.multiplexed-stream"
	if tty {
		contentType = "application/vnd.docker.raw-stream"
	}
	send := stream(w, contentType)

	emit := func(entries []dsm.LogEntry) error {
		var out []byte
		for _, e := range entries {
			name := e.Stream
			if name == "" {
				name = "stdout"
			}
			if !want[name] {
				continue
			}
			text := e.Text
			if !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			if timestamps {
				text = e.Created + " " + text
			}
			if tty {
				out = append(out, text...)
			} else if name == "stderr" {
				out = append(out, multiplexed(2, []byte(text))...)
			} else {
				out = append(out, multiplexed(1, []byte(text))...)
			}
		}
		return send(out)
	}
	if err := emit(entries); err != nil {
		return err
	}
	last := ""
	for _, e := range entries {
		last = max(last, e.Created)
	}
	// Container Manager's log API is a paged store, not a stream: poll it.
	for follow && sleep(r.Context(), time.Second) {
		page, err := s.adapter.dsm.ContainerLogs(c.Name, 500, 0, "DESC", "")
		if err != nil {
			return err
		}
		var fresh []dsm.LogEntry
		for i := len(page.Logs) - 1; i >= 0; i-- {
			if page.Logs[i].Created > last {
				fresh = append(fresh, page.Logs[i])
			}
		}
		if len(fresh) > 0 {
			if err := emit(fresh); err != nil {
				return err
			}
			last = fresh[len(fresh)-1].Created
		}
	}
	return nil
}

func (s *server) imagesJSON(w *response, r *http.Request, q query, p []string) error {
	list, err := s.adapter.listImages()
	if err != nil {
		return err
	}
	return writeJSON(w, 200, list)
}

func (s *server) imageJSON(w *response, r *http.Request, q query, p []string) error {
	name, _ := url.PathUnescape(p[0])
	img, err := s.adapter.inspectImage(name)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, img)
}

func (s *server) imageDelete(w *response, r *http.Request, q query, p []string) error {
	name, _ := url.PathUnescape(p[0])
	out, err := s.adapter.removeImage(name)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, out)
}

// streamError reports a failure inside a progress stream, as dockerd does.
func streamError(emit func(any) error, err error) error {
	var apiErr *apiError
	var dsmErr *dsm.Error
	if errors.As(err, &apiErr) || errors.As(err, &dsmErr) {
		return emit(map[string]any{"errorDetail": map[string]string{"message": err.Error()}, "error": err.Error()})
	}
	return err
}

func (s *server) imagesLoad(w *response, r *http.Request, q query, p []string) error {
	dir := filepath.Join(dsm.CacheDir, "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "*.tar")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = io.Copy(f, r.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	emit := jsonLines(stream(w, "application/json"))
	if err := s.adapter.load(f.Name(), emit); err != nil {
		return streamError(emit, err)
	}
	return nil
}

func (s *server) imagesCreate(w *response, r *http.Request, q query, p []string) error {
	_, _ = io.Copy(io.Discard, r.Body)
	if _, ok := q["fromSrc"]; ok {
		return apiErrorf(501, "docker import is not supported by the DSM adapter")
	}
	emit := jsonLines(stream(w, "application/json"))
	if err := s.adapter.pull(q.get("fromImage", ""), q.get("tag", ""), emit); err != nil {
		return streamError(emit, err)
	}
	return nil
}

func (s *server) networksJSON(w *response, r *http.Request, q query, p []string) error {
	list, err := s.adapter.networks()
	if err != nil {
		return err
	}
	return writeJSON(w, 200, list)
}

func (s *server) volumesJSON(w *response, r *http.Request, q query, p []string) error {
	list, err := s.adapter.volumes()
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"Volumes": list,
		"Warnings": []string{"only volumes some container mounts are listed (DSM has no volume list)"}})
}

// events: Container Manager has no event stream, so poll it every 2s and report
// what changed, from now on (no replay of --since).
func (s *server) events(w *response, r *http.Request, q query, p []string) error {
	filters, err := q.filters()
	if err != nil {
		return err
	}
	var deadline time.Time
	if until := q.get("until", ""); until != "" {
		if v, err := strconv.ParseFloat(until, 64); err == nil {
			deadline = time.Unix(0, int64(v*1e9))
		}
	}
	wanted := func(e event) bool {
		attrs := e.Actor.Attributes
		for key, values := range filters {
			switch key {
			case "type":
				if !contains(values, e.Type) {
					return false
				}
			case "event":
				if !contains(values, e.Action) {
					return false
				}
			case "container":
				if e.Type != "container" || !anyOf(values, func(v string) bool {
					return v == attrs["name"] || strings.HasPrefix(e.Actor.ID, v)
				}) {
					return false
				}
			case "image":
				if !anyOf(values, func(v string) bool { return v == attrs["image"] || v == attrs["name"] }) {
					return false
				}
			}
		}
		return true
	}
	last, err := s.adapter.snapshot()
	if err != nil {
		return err
	}
	send := jsonLines(stream(w, "application/json"))
	for (deadline.IsZero() || time.Now().Before(deadline)) && sleep(r.Context(), 2*time.Second) {
		current, err := s.adapter.snapshot()
		if err != nil {
			return err
		}
		for _, e := range eventsBetween(last, current) {
			if wanted(e) {
				if err := send(e); err != nil {
					return err
				}
			}
		}
		last = current
	}
	return nil
}

// --- exec -------------------------------------------------------------------

func (s *server) exec(id string) (*Exec, error) {
	s.adapter.execMu.Lock()
	ex := s.adapter.execs[id]
	s.adapter.execMu.Unlock()
	if ex == nil {
		return nil, apiErrorf(404, "No such exec instance: %s", id)
	}
	return ex, nil
}

func readJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return apiErrorf(400, "invalid JSON: %v", err)
	}
	return nil
}

func (s *server) execCreate(w *response, r *http.Request, q query, p []string) error {
	var config execConfig
	if err := readJSON(r, &config); err != nil {
		return err
	}
	if len(config.Cmd) == 0 {
		return apiErrorf(400, "No exec command specified")
	}
	if config.User != "" {
		return apiErrorf(501, "exec --user is not supported by the DSM adapter (Container Manager's "+
			"terminal runs as the container's user)")
	}
	if config.Privileged {
		return apiErrorf(501, "exec --privileged is not supported by the DSM adapter")
	}
	c, err := s.adapter.container(p[0])
	if err != nil {
		return err
	}
	if c.Status != "running" {
		return apiErrorf(409, "Container %s is not running", c.Name)
	}
	ex := &Exec{ID: dsm.RandomHex(32), container: c, config: config}
	a := s.adapter
	a.execMu.Lock()
	cutoff := time.Now().Add(-10 * time.Minute)
	for id, old := range a.execs {
		old.mu.Lock()
		stale := !old.finished.IsZero() && old.finished.Before(cutoff)
		old.mu.Unlock()
		if stale {
			delete(a.execs, id)
		}
	}
	a.execs[ex.ID] = ex
	a.execMu.Unlock()
	return writeJSON(w, 201, map[string]string{"Id": ex.ID})
}

func (s *server) execResize(w *response, r *http.Request, q query, p []string) error {
	_, _ = io.Copy(io.Discard, r.Body)
	ex, err := s.exec(p[0])
	if err != nil {
		return err
	}
	rows, _ := strconv.Atoi(q.get("h", "0"))
	cols, _ := strconv.Atoi(q.get("w", "0"))
	if rows > 0 && cols > 0 {
		ex.resize(rows, cols)
	}
	return empty(w, 200)
}

func (s *server) execJSON(w *response, r *http.Request, q query, p []string) error {
	ex, err := s.exec(p[0])
	if err != nil {
		return err
	}
	return writeJSON(w, 200, ex.inspect())
}

func (s *server) execStart(w *response, r *http.Request, q query, p []string) error {
	var body struct {
		Detach      bool
		ConsoleSize []int
	}
	if err := readJSON(r, &body); err != nil {
		return err
	}
	ex, err := s.exec(p[0])
	if err != nil {
		return err
	}
	ex.mu.Lock()
	if ex.running || !ex.finished.IsZero() {
		ex.mu.Unlock()
		return apiErrorf(409, "exec %s has already been started", ex.ID)
	}
	ex.running = true
	if len(body.ConsoleSize) == 2 && body.ConsoleSize[0] > 0 && body.ConsoleSize[1] > 0 {
		ex.size = [2]int{body.ConsoleSize[0], body.ConsoleSize[1]}
	}
	size := ex.size
	ex.mu.Unlock()
	if body.Detach {
		return s.execDetached(w, r, ex)
	}
	tty, stdin := ex.config.Tty, ex.config.AttachStdin

	// Hijack the connection, as dockerd does: from here on it's a raw stream.
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		ex.finish(nil)
		return apiErrorf(500, "can't hijack the connection")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		ex.finish(nil)
		return err
	}
	w.status = 101
	defer conn.Close()
	contentType := "application/vnd.docker.multiplexed-stream"
	if tty {
		contentType = "application/vnd.docker.raw-stream"
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "tcp") {
		fmt.Fprintf(rw, "HTTP/1.1 101 UPGRADED\r\nContent-Type: %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", contentType)
	} else {
		fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\n\r\n", contentType)
	}
	if err := rw.Flush(); err != nil {
		ex.finish(nil)
		return nil
	}

	var wmu sync.Mutex
	write := func(data []byte, stream byte) error {
		if len(data) == 0 {
			return nil
		}
		wmu.Lock()
		defer wmu.Unlock()
		if !tty {
			data = multiplexed(stream, data)
		}
		if _, err := rw.Write(data); err != nil {
			return err
		}
		return rw.Flush()
	}

	code := 126
	defer func() { ex.finish(&code) }()
	framed := !tty && !stdin
	rows, cols := 50, 200
	if tty {
		rows, cols = 24, 80
	}
	if size[0] > 0 {
		rows, cols = size[0], size[1]
	}
	command := terminal.CommandFor(ex.argv(), terminal.Options{Unframed: !framed, Interactive: tty, Stdin: stdin,
		Gzip: terminal.GzipDefault()})
	if !framed && !tty {
		_ = write([]byte("synodock exec: unframed mode (-i), output may arrive out of order\n"), 2)
	}
	ctx := context.Background() // the hijacked connection is ours to watch now
	session, err := terminal.Open(ctx, s.adapter.dsm, ex.container.Name, command, rows, cols)
	if err != nil {
		_ = write([]byte(fmt.Sprintf("synodock exec: %v\n", err)), 2)
		return nil
	}
	defer session.Close()
	ex.mu.Lock()
	ex.session = session
	if ex.size[0] > 0 && ex.size != [2]int{rows, cols} {
		_ = session.Resize(ex.size[0], ex.size[1])
	}
	ex.mu.Unlock()
	if framed {
		code, err = terminal.PumpFramed(ctx, session, func(b []byte) error { return write(b, 1) })
	} else {
		var input io.Reader
		if stdin {
			input = stdinReader{rw.Reader}
		}
		var known bool
		code, known, err = terminal.PumpUnframed(ctx, session, func(t string) error { return write([]byte(t), 1) },
			input, stdin && !tty, !tty, false)
		if err == nil && !known {
			code = 255
		}
	}
	if err != nil {
		code = 126
		var dsmErr *dsm.Error
		if errors.As(err, &dsmErr) {
			_ = write([]byte(fmt.Sprintf("synodock exec: %v\n", err)), 2)
		}
	}
	return nil
}

// stdinReader is the CLI's stdin over the hijacked connection; any failure reads
// as the end of it.
type stdinReader struct{ r *bufio.Reader }

func (s stdinReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		err = io.EOF
	}
	return n, err
}

// execDetached is docker exec -d: start it in the background and answer at once,
// no stream. Nothing can see it after that, so inspect reports it as not running
// with no exit code; a command that doesn't exist fails here, as with dockerd.
func (s *server) execDetached(w *response, r *http.Request, ex *Exec) error {
	var out bytes.Buffer
	command := terminal.CommandFor(ex.argv(), terminal.Options{Detach: true})
	session, err := terminal.Open(r.Context(), s.adapter.dsm, ex.container.Name, command, 24, 200)
	if err != nil {
		ex.finish(nil)
		return err
	}
	code, err := terminal.PumpFramed(r.Context(), session, func(b []byte) error { out.Write(b); return nil })
	session.Close()
	if err != nil {
		ex.finish(nil)
		return err
	}
	if code != 0 {
		ex.finish(&code)
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			msg = fmt.Sprintf("exec failed with status %d", code)
		}
		return apiErrorf(500, "%s", msg)
	}
	ex.finish(nil)
	return empty(w, 200)
}

// LocalNetworkDenied ends the message for a serve macOS keeps off the LAN.
const LocalNetworkDenied = "macOS keeps services launchd starts off the local network until allowed: " +
	"System Settings > Privacy & Security > Local Network; allow synodock, then try again"

// ServeOptions say where and how to serve.
type ServeOptions struct {
	Socket   string
	Listener net.Listener // a socket launchd or systemd opened; Socket is then only for messages
	IdleExit time.Duration
	Verbose  bool
	Version  string
	Context  string // the docker context to suggest, or that was pointed here
	Created  bool   // whether Context was created or updated already
}

// Serve serves the Docker Engine API on a Unix socket until ctx is done, or
// with IdleExit, until no request has been open for that long (a service
// manager holding the socket starts it again on the next connection).
func Serve(ctx context.Context, client *dsm.Client, o ServeOptions) error {
	socketPath, listener := o.Socket, o.Listener
	if strings.HasPrefix(socketPath, "~/") {
		home, _ := os.UserHomeDir()
		socketPath = filepath.Join(home, socketPath[2:])
	}
	adapter := NewAdapter(client)
	if listener == nil {
		if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
			return err
		}
		_ = os.Remove(socketPath)
		old := syscall.Umask(0o177) // socket is 0600: only this user can talk to it
		var err error
		listener, err = net.Listen("unix", socketPath)
		syscall.Umask(old)
		if err != nil {
			return err
		}
		defer os.Remove(socketPath)
		// Gather volumes before any client asks: it's ~50 DSM calls (see
		// Adapter.volumes). Not when started on demand: they'd queue ahead of the
		// request that started us.
		go func() { _, _ = adapter.volumes() }()
	}
	handler := &server{adapter: adapter, verbose: o.Verbose, version: o.Version, lastDone: time.Now(),
		onDemand: o.Listener != nil}
	srv := &http.Server{Handler: handler}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if o.IdleExit > 0 || handler.onDemand {
		go handler.exitWhenIdle(srv, o.IdleExit)
	}
	if o.Listener != nil {
		fmt.Fprintf(os.Stderr, "Docker API for %s on unix://%s (started on demand)\n", client.URL, socketPath)
	} else {
		fmt.Fprintf(os.Stderr, "Docker API for %s on unix://%s\n", client.URL, socketPath)
		if !o.Created {
			fmt.Fprintf(os.Stderr, "  docker context create %s --docker host=unix://%s\n", o.Context, socketPath)
		}
		fmt.Fprintf(os.Stderr, "  docker --context %s ps -a\n", o.Context)
	}
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// exitWhenIdle closes the server once no request (a log stream, an exec, events
// for lazydocker) has been open for idle.
//
// Started on demand, it also exits once no request is open after macOS refused
// it the local network, so the next connection starts a process with the
// permission, if it has been given since.
func (s *server) exitWhenIdle(srv *http.Server, idle time.Duration) {
	check := time.Second
	if idle > 0 {
		check = min(idle/4, check)
	}
	for range time.Tick(check) {
		s.idleMu.Lock()
		open, since := s.open, time.Since(s.lastDone)
		s.idleMu.Unlock()
		switch {
		case open == 0 && s.denied.Load():
			fmt.Fprintln(os.Stderr, "no local network access; exiting so the next connection starts afresh")
		case open == 0 && idle > 0 && since >= idle:
			fmt.Fprintf(os.Stderr, "no requests for %s; exiting until the next connection\n", idle)
		default:
			continue
		}
		_ = srv.Close()
		return
	}
}
