// Package dsm is a client for the DSM Web API calls Container Manager's UI makes.
//
// Author: Ryan Tenney
//
// The SYNO.Docker.* endpoints are undocumented: this speaks to them the way the
// Container Manager UI does, so everything stays in step with the UI.
//
// Settings, each from the environment or ~/.config/synodock/config.toml:
//
//	SYNODOCK_URL       url = "https://nas.example.com:5001"   (required)
//	SYNODOCK_USER      user = "docker"                         (required)
//	SYNODOCK_KEYCHAIN  keychain = "synodock"   macOS keychain service for the password
//	                                           and the 2FA device token (account = user)
//	SYNODOCK_PASSWORD_COMMAND  password_command = "..."   prints the password
//	SYNODOCK_OTP_COMMAND       otp_command = "..."        prints a current 2FA code
//
// The password comes from SYNODOCK_PASSWORD, then password_command, then the
// macOS keychain, then a prompt. The commands run under sh, so any secret store
// works (secret-tool, pass, op, security); their first line of output is used.
//
// With 2FA, the first login with a code (`synodock login --otp <code>`, or one
// from otp_command or a prompt when DSM asks for it) mints a device token (kept
// in the keychain on macOS, else in ~/.config/synodock/, mode 0600), and later
// logins need no code.
package dsm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

var (
	ConfigDir = xdgDir("XDG_CONFIG_HOME", ".config")
	CacheDir  = xdgDir("XDG_CACHE_HOME", ".cache")
	trace     = os.Getenv("SYNODOCK_TRACE") == "1" // log each DSM call with its queue wait and duration
)

func xdgDir(env, fallback string) string {
	base := os.Getenv(env)
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, fallback)
	}
	return filepath.Join(base, "synodock")
}

// Error is a failure reported by DSM, or a problem with the settings. Code is
// DSM's error code, 0 when there is none.
type Error struct {
	Msg  string
	Code int
}

func (e *Error) Error() string { return e.Msg }

func Errorf(format string, args ...any) *Error { return &Error{Msg: fmt.Sprintf(format, args...)} }

// DSM's generic "network unstable or system busy" codes: the backend gave up on the
// request, which for a long operation doesn't mean the operation itself failed.
var busyCodes = map[int]bool{109: true, 110: true, 111: true, 117: true, 118: true}

// JS JSON-encodes a param: SYNO.Docker.* APIs take string params quoted, as the UI sends them.
func JS(value any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	return strings.TrimSuffix(b.String(), "\n")
}

// Params are a call's form fields.
type Params map[string]string

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// secrets are the password and the 2FA device token: macOS keychain where there is one.
type secrets struct {
	config      *Config
	hasKeychain bool
}

func newSecrets(config *Config) *secrets {
	_, err := exec.LookPath("security")
	return &secrets{config: config, hasKeychain: runtime.GOOS == "darwin" && err == nil}
}

// keychainAccount is user@host, so one keychain service can hold several NAS.
func (s *secrets) keychainAccount() string { return s.config.User + "@" + s.config.Host() }

// keychainGet looks under user@host, then under the bare user (entries made
// before there were several NAS, and the simple case of one).
func (s *secrets) keychainGet(service string) (string, bool) {
	for _, account := range []string{s.keychainAccount(), s.config.User} {
		out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
		if err == nil {
			return strings.TrimRight(string(out), "\n"), true
		}
	}
	return "", false
}

// run is a password_command or otp_command: its first line of output. It shares
// our terminal, so a store can ask to be unlocked.
func (s *secrets) run(ctx context.Context, setting, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", Errorf("%s failed: %v", setting, err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	if line = strings.TrimRight(line, "\r"); line == "" {
		return "", Errorf("%s printed nothing", setting)
	}
	return line, nil
}

func (s *secrets) password(ctx context.Context) (string, error) {
	if s.config.Password != "" {
		return s.config.Password, nil
	}
	if s.config.PasswordCommand != "" {
		return s.run(ctx, "password_command", s.config.PasswordCommand)
	}
	if s.hasKeychain {
		if p, ok := s.keychainGet(s.config.Keychain); ok {
			return p, nil
		}
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "DSM password for %s: ", s.config.User)
		p, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(p), err
	}
	where := ""
	if s.hasKeychain {
		where = fmt.Sprintf(" or keychain item service=%s account=%s", s.config.Keychain, s.keychainAccount())
	}
	if s.config.Name != "" {
		return "", Errorf("no password for %s: set password_command in [nas.%s]%s", s.config.Name, s.config.Name, where)
	}
	return "", Errorf("no password: set SYNODOCK_PASSWORD, password_command%s", where)
}

// otp is a 2FA code for when DSM asks for one: otp_command's, else typed in.
func (s *secrets) otp(ctx context.Context) (string, error) {
	if s.config.OTPCommand != "" {
		return s.run(ctx, "otp_command", s.config.OTPCommand)
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "2FA code for %s: ", s.config.User)
		code, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return strings.TrimSpace(string(code)), err
	}
	return "", Errorf("login failed: code 403 2FA code required (run: synodock login --otp <code>, or set otp_command)")
}

func (s *secrets) deviceFile() string {
	return filepath.Join(ConfigDir, fmt.Sprintf("device-%s@%s", s.config.User, s.config.Host()))
}

func (s *secrets) deviceToken() string {
	if s.hasKeychain {
		token, _ := s.keychainGet(s.config.Keychain + "-device")
		return token
	}
	data, err := os.ReadFile(s.deviceFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (s *secrets) saveDeviceToken(token string) (string, error) {
	if s.hasKeychain {
		service := s.config.Keychain + "-device"
		if err := exec.Command("security", "add-generic-password", "-U", "-s", service,
			"-a", s.keychainAccount(), "-w", token).Run(); err != nil {
			return "", fmt.Errorf("saving the device token to the keychain: %w", err)
		}
		return fmt.Sprintf("keychain (%s)", service), nil
	}
	if err := os.MkdirAll(ConfigDir, 0o755); err != nil {
		return "", err
	}
	path := s.deviceFile()
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// DSM makes overlapping SYNO.Docker calls wait in ~5s steps: two concurrent
// Container get take 5.1s each instead of 0.1s, and under load list, stats and
// log calls slow the same way. `synodock serve` handles many requests at once,
// so calls queue here, one at a time, where waiting costs nothing. Slow calls
// (an image import runs for minutes) don't take the lock.
var callLock sync.Mutex

type Client struct {
	Config  *Config
	URL     string
	secrets *secrets
	ctx     context.Context // cancelled (^C) ends whatever request is in flight
	http    *http.Client
	slow    *http.Client

	mu    sync.Mutex
	sid   string
	token string

	loginMu sync.Mutex // one login at a time, so expired sessions log in again once
}

func NewClient(ctx context.Context, config *Config) *Client {
	return &Client{
		Config:  config,
		URL:     config.URL,
		secrets: newSecrets(config),
		ctx:     ctx,
		http:    &http.Client{Timeout: 60 * time.Second},
		slow:    &http.Client{},
	}
}

// SID is the session id, which the Terminal websocket takes as a cookie.
func (c *Client) SID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sid
}

func (c *Client) request(ctx context.Context, params url.Values, body io.Reader, contentType string, length int64, slow bool) (*http.Response, error) {
	c.mu.Lock()
	sid, token := c.sid, c.token
	c.mu.Unlock()
	if sid != "" {
		params.Set("_sid", sid)
	}
	endpoint := c.URL + "/webapi/entry.cgi"
	if body == nil {
		encoded := params.Encode()
		body, length = strings.NewReader(encoded), int64(len(encoded))
		contentType = "application/x-www-form-urlencoded"
	} else {
		endpoint += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = length // else an upload goes out chunked
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("X-SYNO-TOKEN", token)
	}
	if slow {
		return c.slow.Do(req)
	}
	return c.http.Do(req)
}

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   json.RawMessage `json:"error"`
}

func (e *envelope) code() int {
	var body struct {
		Code int `json:"code"`
	}
	_ = json.Unmarshal(e.Error, &body)
	return body.Code
}

func (c *Client) post(params url.Values, slow bool) (*envelope, error) {
	queued := time.Now()
	if !slow {
		callLock.Lock()
		defer callLock.Unlock()
	}
	started := time.Now()
	if trace {
		defer func() {
			fmt.Fprintf(os.Stderr, "synodock: %s.%s waited %.2fs, took %.2fs\n", params.Get("api"), params.Get("method"),
				started.Sub(queued).Seconds(), time.Since(started).Seconds())
		}()
	}
	resp, err := c.request(c.ctx, params, nil, "", 0, slow)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body envelope
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s.%s: %w", params.Get("api"), params.Get("method"), err)
	}
	return &body, nil
}

// RandomHex is n random bytes in hex.
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Call makes one DSM API call and returns its data.
func (c *Client) Call(api, method string, version int, params Params) (json.RawMessage, error) {
	return c.call(api, method, version, false, params)
}

func (c *Client) call(api, method string, version int, slow bool, params Params) (json.RawMessage, error) {
	all := url.Values{"api": {api}, "method": {method}, "version": {strconv.Itoa(version)}}
	for k, v := range params {
		all.Set(k, v)
	}
	sid := c.SID()
	if sid == "" && api != "SYNO.API.Auth" { // not logged in yet: serve started on demand logs in at first use
		if err := c.relogin(""); err != nil {
			return nil, err
		}
		sid = c.SID()
	}
	body, err := c.post(all, slow)
	if err != nil {
		return nil, err
	}
	// 119 = session not found (seen transiently right after login), 106 = session
	// timed out (a long-lived server idles past it): log in again, once.
	if code := body.code(); !body.Success && (code == 106 || code == 119) && api != "SYNO.API.Auth" {
		if err := c.relogin(sid); err != nil {
			fmt.Fprintf(os.Stderr, "synodock: the DSM session ended and logging in again failed: %v\n", err)
			return nil, &Error{Msg: "the DSM session ended and logging in again failed: " + err.Error(), Code: code}
		}
		if body, err = c.post(all, slow); err != nil {
			return nil, err
		}
	}
	if !body.Success {
		return nil, &Error{Msg: fmt.Sprintf("%s.%s failed: %s", api, method, body.Error), Code: body.code()}
	}
	return body.Data, nil
}

// CallInto makes a call and decodes its data into v.
func (c *Client) CallInto(v any, api, method string, version int, params Params) error {
	data, err := c.Call(api, method, version, params)
	if err != nil {
		return err
	}
	return Decode(data, v)
}

// Decode is json.Unmarshal that keeps numbers as json.Number, so ids and byte
// counts passed through don't round through float64. null decodes to nothing.
func Decode(data json.RawMessage, v any) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// relogin logs in again after a call found the session sid gone, unless another
// call has done so already.
func (c *Client) relogin(sid string) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.SID() != sid {
		return nil
	}
	return c.login("")
}

// Login starts a session. With 2FA and no device token (or one DSM no longer
// takes), the code comes from otp (synodock login --otp), otp_command or a
// prompt, and the device token it mints is saved.
func (c *Client) Login(otp string) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.login(otp)
}

func (c *Client) login(otp string) error {
	password, err := c.secrets.password(c.ctx)
	if err != nil {
		return err
	}
	err = c.authenticate(password, otp)
	var dsmErr *Error
	if otp == "" && errors.As(err, &dsmErr) && dsmErr.Code == 403 {
		if otp, err = c.secrets.otp(c.ctx); err != nil {
			return err
		}
		err = c.authenticate(password, otp)
	}
	return err
}

func (c *Client) authenticate(password, otp string) error {
	params := url.Values{"api": {"SYNO.API.Auth"}, "version": {"6"}, "method": {"login"},
		"account": {c.Config.User}, "passwd": {password}, "session": {"ContainerManager"}, "format": {"sid"},
		"enable_syno_token": {"yes"}, "device_name": {"synodock"}}
	if otp != "" {
		params.Set("otp_code", otp)
		params.Set("enable_device_token", "yes")
	} else if device := c.secrets.deviceToken(); device != "" {
		params.Set("device_id", device)
	}
	c.mu.Lock()
	c.sid, c.token = "", ""
	c.mu.Unlock()
	resp, err := c.request(c.ctx, params, nil, "", 0, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var body envelope
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if !body.Success {
		code := body.code()
		hint := map[int]string{400: "wrong account or password", 403: "2FA code required",
			404: "2FA code rejected", 406: "the account must enroll in 2FA first"}[code]
		return &Error{Msg: strings.TrimRight(fmt.Sprintf("login failed: code %d %s", code, hint), " "), Code: code}
	}
	var data struct {
		SID       string `json:"sid"`
		SynoToken string `json:"synotoken"`
		DID       string `json:"did"`
	}
	if err := json.Unmarshal(body.Data, &data); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	c.mu.Lock()
	c.sid, c.token = data.SID, data.SynoToken
	c.mu.Unlock()
	if otp != "" && data.DID != "" {
		where, err := c.secrets.saveDeviceToken(data.DID)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "device token saved to %s; later logins need no code\n", where)
	}
	return nil
}

// Logout ends the session, even after ^C, and ignores any failure.
func (c *Client) Logout() {
	if c.SID() == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	params := url.Values{"api": {"SYNO.API.Auth"}, "version": {"6"}, "method": {"logout"}, "session": {"ContainerManager"}}
	if resp, err := c.request(ctx, params, nil, "", 0, false); err == nil {
		resp.Body.Close()
	}
}

// --- containers -------------------------------------------------------------

type State struct {
	Status     string          `json:"Status"`
	Running    bool            `json:"Running"`
	StartedAt  string          `json:"StartedAt"`
	FinishedAt string          `json:"FinishedAt"`
	ExitCode   json.RawMessage `json:"ExitCode"`
	Health     *struct {
		Status string `json:"Status"`
	} `json:"Health"`
}

// Container is an entry of SYNO.Docker.Container list.
type Container struct {
	Name            string          `json:"name"`
	ID              string          `json:"id"`
	Status          string          `json:"status"`
	Image           string          `json:"image"`
	ImageUpper      string          `json:"Image"`
	ImageID         string          `json:"ImageID"`
	Cmd             json.RawMessage `json:"cmd"`
	Created         json.RawMessage `json:"created"`
	Labels          json.RawMessage `json:"Labels"`
	State           *State          `json:"State"`
	NetworkSettings *struct {
		Networks json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
}

// UnmarshalJSON reads each field from its key exactly: encoding/json would also
// take a key differing only in case, and DSM's entries mix its own keys (name, id,
// status: "running") with Docker's (Name: "/web", Id, Status: "Up 3 hours").
func (c *Container) UnmarshalJSON(data []byte) error {
	var networks struct {
		Networks json.RawMessage `json:"Networks"`
	}
	err := decodeExact(data, map[string]any{"name": &c.Name, "id": &c.ID, "status": &c.Status, "image": &c.Image,
		"Image": &c.ImageUpper, "ImageID": &c.ImageID, "cmd": &c.Cmd, "created": &c.Created, "Labels": &c.Labels,
		"State": &c.State, "NetworkSettings": &networks})
	if networks.Networks != nil {
		c.NetworkSettings = &struct {
			Networks json.RawMessage `json:"Networks"`
		}{networks.Networks}
	}
	return err
}

// decodeExact sets each target from the key of exactly that name. A value of the
// wrong type leaves its target as it was, as a missing key does.
func decodeExact(data []byte, targets map[string]any) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	for key, target := range targets {
		if raw, ok := m[key]; ok {
			_ = Decode(raw, target)
		}
	}
	return nil
}

func (c *Container) Running() bool { return c.State != nil && c.State.Running }

// ImageName is the image as the list names it, under either key.
func (c *Container) ImageName() string { return firstOf(c.Image, c.ImageUpper) }

func (c *Client) Containers() ([]Container, error) {
	var data struct {
		Containers []Container `json:"containers"`
	}
	err := c.CallInto(&data, "SYNO.Docker.Container", "list", 1, Params{"limit": "-1", "offset": "0", "type": JS("all")})
	return data.Containers, err
}

func (c *Client) ContainerAction(name, action string) error {
	_, err := c.Call("SYNO.Docker.Container", action, 1, Params{"name": JS(name)})
	return err
}

type LogEntry struct {
	Created string `json:"created"`
	Stream  string `json:"stream"`
	Text    string `json:"text"`
}

type LogPage struct {
	Logs  []LogEntry `json:"logs"`
	Total int        `json:"total"`
}

// ContainerLogs is one page of a container's log, sortDir "ASC" or "DESC".
func (c *Client) ContainerLogs(name string, limit, offset int, sortDir, keyword string) (*LogPage, error) {
	var page LogPage
	err := c.CallInto(&page, "SYNO.Docker.Container.Log", "get", 1, Params{"name": JS(name),
		"limit": strconv.Itoa(limit), "offset": strconv.Itoa(offset), "sort_dir": JS(sortDir), "keyword": JS(keyword)})
	return &page, err
}

// Process is an entry of SYNO.Docker.Container get_process (the UI's Processes
// view): cpu and memoryPercent are percentages, memory is bytes, start is HH:MM.
type Process struct {
	Command       string          `json:"command"`
	CPU           json.Number     `json:"cpu"`
	Memory        json.Number     `json:"memory"`
	MemoryPercent json.Number     `json:"memoryPercent"`
	PID           json.RawMessage `json:"pid"` // a string, so far
	Start         string          `json:"start"`
}

// PIDText is the pid, whether DSM sends it as a string or a number.
func (p *Process) PIDText() string {
	var s string
	if json.Unmarshal(p.PID, &s) == nil {
		return s
	}
	return string(p.PID)
}

// ProcessTitles head ProcessRow's columns: what docker top shows, less what DSM
// doesn't report (UID, PPID, TTY, TIME). RSS is in KiB, as ps gives it.
var ProcessTitles = []string{"PID", "%CPU", "%MEM", "RSS", "STIME", "CMD"}

func (p *Process) Row() []string {
	cpu, _ := p.CPU.Float64()
	mem, _ := p.MemoryPercent.Float64()
	rss, _ := p.Memory.Float64()
	return []string{p.PIDText(), strconv.FormatFloat(cpu, 'f', 1, 64), strconv.FormatFloat(mem, 'f', 1, 64),
		strconv.FormatInt(int64(rss)/1024, 10), p.Start, p.Command}
}

// ContainerProcesses lists a running container's processes; no exec needed.
func (c *Client) ContainerProcesses(name string) ([]Process, error) {
	var data struct {
		Processes []Process `json:"processes"`
	}
	err := c.CallInto(&data, "SYNO.Docker.Container", "get_process", 1, Params{"name": JS(name)})
	return data.Processes, err
}

// --- file station -----------------------------------------------------------

func (c *Client) FSDelete(path string) error {
	_, err := c.Call("SYNO.FileStation.Delete", "delete", 2, Params{"path": JS([]string{path}), "recursive": "false"})
	return err
}

// FSPut uploads a local file into a NAS folder, streamed from disk: image tars
// run to hundreds of MB.
func (c *Client) FSPut(localPath, folder string, createParents, overwrite bool) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	filename := filepath.Base(localPath)
	boundary := RandomHex(16)
	var head bytes.Buffer
	for _, field := range [][2]string{{"path", folder}, {"create_parents", strconv.FormatBool(createParents)},
		{"overwrite", strconv.FormatBool(overwrite)}} {
		fmt.Fprintf(&head, "--%s\r\nContent-Disposition: form-data; name=\"%s\"\r\n\r\n%s\r\n", boundary, field[0], field[1])
	}
	fmt.Fprintf(&head, "--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n"+
		"Content-Type: application/octet-stream\r\n\r\n", boundary, filename)
	tail := fmt.Sprintf("\r\n--%s--\r\n", boundary)
	total := int64(head.Len()) + info.Size() + int64(len(tail))
	body := &progressReader{r: io.MultiReader(&head, f, strings.NewReader(tail)), total: total, label: filename}
	params := url.Values{"api": {"SYNO.FileStation.Upload"}, "version": {"2"}, "method": {"upload"}}
	resp, err := c.request(c.ctx, params, body, "multipart/form-data; boundary="+boundary, total, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result envelope
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	if !result.Success {
		return Errorf("upload failed: %s", result.Error)
	}
	return nil
}

// progressReader reports big uploads (image tars) every 10%.
type progressReader struct {
	r                     io.Reader
	total, sent, reported int64
	label                 string
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.sent += int64(n)
	if p.total >= 256<<20 {
		if pct := p.sent * 100 / p.total; pct >= p.reported+10 {
			p.reported = pct - pct%10
			fmt.Fprintf(os.Stderr, "  %s: %d%% of %.1f GB\n", p.label, p.reported, float64(p.total)/1e9)
		}
	}
	return n, err
}

// --- images -----------------------------------------------------------------

// Image is an entry of SYNO.Docker.Image list.
type Image struct {
	ID          string      `json:"id"`
	Repository  string      `json:"repository"`
	Tags        []string    `json:"tags"`
	Digest      string      `json:"digest"`
	Created     json.Number `json:"created"`
	Size        json.Number `json:"size"`
	VirtualSize json.Number `json:"virtual_size"`
	Description string      `json:"description"`
}

// UnmarshalJSON reads each field from its key exactly (see Container).
func (img *Image) UnmarshalJSON(data []byte) error {
	return decodeExact(data, map[string]any{"id": &img.ID, "repository": &img.Repository, "tags": &img.Tags,
		"digest": &img.Digest, "created": &img.Created, "size": &img.Size, "virtual_size": &img.VirtualSize,
		"description": &img.Description})
}

func (c *Client) Images() ([]Image, error) {
	var data struct {
		Images []Image `json:"images"`
	}
	err := c.CallInto(&data, "SYNO.Docker.Image", "list", 1, Params{"limit": "-1", "offset": "0", "show_dsm": "false"})
	return data.Images, err
}

// ImageImport loads an image tar already on the NAS (Container Manager > Image >
// Add > Add From File).
//
// expect is {repo:tag: short image id}. A multi-GB load can outlast DSM's proxy
// timeout while carrying on server-side, so if the request itself fails, poll the
// image list until every tag points at its expected id.
func (c *Client) ImageImport(nasPath string, expect map[string]string, wait time.Duration) error {
	_, err := c.call("SYNO.Docker.Image", "import", 1, true, Params{"path": JS(nasPath)})
	if err == nil {
		return nil
	}
	var dsmErr *Error
	if len(expect) == 0 || errors.As(err, &dsmErr) && !busyCodes[dsmErr.Code] || errors.Is(err, context.Canceled) {
		return err
	}
	fmt.Fprintf(os.Stderr, "import request ended early (%v); waiting for the load to finish on the NAS\n", err)
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-time.After(15 * time.Second):
		}
		current, err := c.ImageIDsByTag()
		if err != nil {
			continue
		}
		done := true
		for tag, id := range expect {
			if ShortID(current[tag]) != id {
				done = false
			}
		}
		if done {
			return nil
		}
	}
	return Errorf("%s was not loaded within %s", nasPath, wait)
}

// ImageIDsByTag maps repo:tag to image id. An image replaced by a load or pull
// loses its tag (and drops out of this map) while still holding disk space until
// deleted by id.
func (c *Client) ImageIDsByTag() (map[string]string, error) {
	images, err := c.Images()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, img := range images {
		for _, tag := range img.Tags {
			out[img.Repository+":"+tag] = img.ID
		}
	}
	return out, nil
}

// ShortID is the first 12 hex digits of an image id, "sha256:" or not.
func ShortID(id string) string {
	id = id[strings.LastIndex(id, ":")+1:]
	return id[:min(12, len(id))]
}

func (c *Client) ImageDeleteID(id string) error {
	// Short (12 hex) ids are what's been verified to work; "sha256:..." is not.
	_, err := c.Call("SYNO.Docker.Image", "delete", 1, Params{"images": JS([]map[string]string{{"identity": ShortID(id)}})})
	return err
}
