// synodock: docker exec and a Docker API for Synology NAS containers, without SSH.
//
// Author: Ryan Tenney
//
//	synodock container ls|logs|top|exec  containers (ps, logs, top, exec: shortcuts, as docker's)
//	synodock service install|uninstall|status
//	                                     serve started on demand by launchd or systemd
//	synodock login [--otp CODE]          check the login; with --otp, save a 2FA device token
//	synodock serve [--socket PATH]       Docker Engine API on a Unix socket, for the docker CLI
//	synodock use [NAME]                  list the NAS in config.toml, or pick the default
//
// Global flags pick the NAS, as docker's do the engine: -H/--host URL, --nas NAME.
//
// Settings (SYNODOCK_URL, SYNODOCK_USER, ...) are described in internal/dsm and
// the README.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/ryantenney/synodock/internal/activation"
	"github.com/ryantenney/synodock/internal/dockerapi"
	"github.com/ryantenney/synodock/internal/dsm"
	"github.com/ryantenney/synodock/internal/service"
	"github.com/ryantenney/synodock/internal/terminal"
)

const version = "0.1.0"

type options struct {
	sel       dsm.Selection
	cmd       string
	use       string
	unnamed   bool
	context   string
	idle      time.Duration
	idleSet   bool // --idle-exit given
	noCtx     bool
	otp       string
	all       bool
	name      string
	tail      int
	keyword   string
	argv      []string
	exec      terminal.RunOptions
	socket    string
	verbose   bool
	noGzip    bool
	file      string // project create/update: the compose file
	sharePath string // project create: the folder, default /docker/NAME
	outFile   string // project config: write to this file, not stdout
	force     bool   // project rm: no confirmation
	folder    string // image load: the NAS folder for the tar
	registry  string // image pull: the registry URL
	user      string // image pull: the registry account
}

func main() {
	// A closed stdout (| head) is an error from Write, not a signal that ends the
	// process before the exec is deleted and the session logged out.
	signal.Ignore(syscall.SIGPIPE)
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	o, code, ok := parse(args)
	if !ok {
		return code
	}
	if o.cmd == "service status" || o.cmd == "service uninstall" {
		if err := serviceCommand(o); err != nil {
			fmt.Fprintf(os.Stderr, "synodock: %v\n", err)
			return 1
		}
		return 0
	}
	if o.cmd == "use" {
		if err := use(o); err != nil {
			fmt.Fprintf(os.Stderr, "synodock: %v\n", err)
			return 1
		}
		return 0
	}
	config, err := dsm.LoadConfig(o.sel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "synodock: %v\n", err)
		return 2
	}
	sigs := []os.Signal{os.Interrupt}
	if o.cmd == "serve" { // kill, launchd: clean up the socket as ^C does
		sigs = append(sigs, syscall.SIGTERM)
	}
	ctx, stop := signal.NotifyContext(context.Background(), sigs...)
	defer stop()
	client := dsm.NewClient(ctx, config)
	defer client.Logout()
	code, err = dispatch(ctx, client, o)
	if err == nil {
		return code
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return 130
	}
	if errors.Is(err, syscall.EPIPE) {
		return 128 + int(syscall.SIGPIPE)
	}
	fmt.Fprintf(os.Stderr, "synodock: %v\n", err)
	return 1
}

func dispatch(ctx context.Context, client *dsm.Client, o options) (int, error) {
	if o.cmd == "container exec" && len(o.argv) == 0 {
		return 0, dsm.Errorf("exec: no command given")
	}
	// Started on demand by launchd or systemd, serve logs in at the first request,
	// so a failure reaches the client as a message instead of a dropped connection.
	listeners, err := activation.Listeners("Listeners")
	if err != nil {
		return 0, err
	}
	if o.cmd != "serve" || len(listeners) == 0 {
		if err := client.Login(o.otp); err != nil {
			return 0, err
		}
	}
	switch o.cmd {
	case "login":
		fmt.Printf("logged in to %s as %s\n", client.URL, client.Config.User)

	case "container ls":
		rows, err := client.Containers()
		if err != nil {
			return 0, err
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
		for _, c := range rows {
			if o.all || c.Running() {
				fmt.Printf("%-40s %-10s %s\n", c.Name, c.Status, c.Image)
			}
		}

	case "container logs":
		page, err := client.ContainerLogs(o.name, o.tail, 0, "DESC", o.keyword)
		if err != nil {
			return 0, err
		}
		for i := len(page.Logs) - 1; i >= 0; i-- {
			e := page.Logs[i]
			fmt.Printf("%s %-6s %s\n", e.Created, e.Stream, strings.TrimRightFunc(e.Text, unicode.IsSpace))
		}

	case "container top":
		procs, err := client.ContainerProcesses(o.name)
		if err != nil {
			return 0, err
		}
		if len(procs) == 0 { // what DSM says for a stopped container
			return 0, dsm.Errorf("%s is not running", o.name)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, strings.Join(dsm.ProcessTitles, "\t"))
		for i := range procs {
			fmt.Fprintln(tw, strings.Join(procs[i].Row(), "\t"))
		}
		tw.Flush()

	case "project ls", "project config", "project ps", "project create", "project update",
		"project up", "project down", "project start", "project stop", "project restart", "project logs", "project rm":
		return projectCommand(client, o)
	case "image ls", "image rm", "image import", "image load", "image pull":
		return imageCommand(client, o)
	case "container exec":
		return terminal.Run(ctx, client, o.name, o.argv, o.exec)

	case "serve":
		return 0, serve(ctx, client, o, listeners)

	case "service install":
		return 0, serviceInstall(client, o)
	}
	return 0, nil
}

// socketPath is where serve listens for a NAS: docker.sock, or NAME.sock.
func socketPath(name string) string {
	if name == "" {
		return filepath.Join(dsm.CacheDir, "docker.sock")
	}
	return filepath.Join(dsm.CacheDir, name+".sock")
}

// contextName is the docker context for a NAS: synology, or synology-NAME.
func contextName(name string) string {
	if name == "" {
		return "synology"
	}
	return "synology-" + name
}

func contextDescription(c *dsm.Config) string {
	description := "synodock: " + c.User + "@" + c.Host()
	if c.Name != "" {
		description += " (" + c.Name + ")"
	}
	return description
}

func serve(ctx context.Context, client *dsm.Client, o options, listeners []net.Listener) error {
	name := client.Config.Name
	socket := o.socket
	if socket == "" {
		socket = socketPath(name)
	}
	contextName := contextName(name)
	idle := idleExit(o, client.Config, len(listeners) > 0)
	if len(listeners) > 0 { // started by launchd or systemd: they hold the socket
		return dockerapi.Serve(ctx, client, dockerapi.ServeOptions{Socket: socket, Listener: listeners[0],
			IdleExit: idle, Verbose: o.verbose, Version: version, Context: contextName, Created: true})
	}
	created := false
	if o.context != "" {
		if o.context != "auto" {
			contextName = o.context
		}
		if err := dockerapi.DockerContext(contextName, socket, contextDescription(client.Config)); err != nil {
			return err
		}
		created = true
		fmt.Fprintf(os.Stderr, "docker context %s -> unix://%s\n", contextName, socket)
	}
	return dockerapi.Serve(ctx, client, dockerapi.ServeOptions{Socket: socket, IdleExit: idle, Verbose: o.verbose,
		Version: version, Context: contextName, Created: created})
}

// defaultIdleExit is how long serve started on demand waits idle, unless told.
const defaultIdleExit = 15 * time.Minute

// idleExit is serve's idle timeout: --idle-exit, else idle_exit in config.toml,
// else 15m when started on demand; 0 never exits.
func idleExit(o options, c *dsm.Config, onDemand bool) time.Duration {
	switch {
	case o.idleSet:
		return o.idle
	case c.IdleExit != nil:
		return *c.IdleExit
	case onDemand:
		return defaultIdleExit
	}
	return 0
}

// serviceConfig is the NAS a service is for. It has to come from config.toml:
// a service doesn't see this shell's environment or command line.
func serviceConfig(sel dsm.Selection) (*dsm.Config, error) {
	c, chosenBy, err := dsm.Resolve(sel)
	if err != nil {
		return nil, err
	}
	if chosenBy == "--host" || chosenBy == "SYNODOCK_URL" {
		return nil, dsm.Errorf("a service can't use a NAS given by %s: put it in %s (as [nas.NAME], or url and user "+
			"at the top) and use --nas", chosenBy, dsm.ConfigPath())
	}
	return c, nil
}

// serviceInstall runs after a login here, where a 2FA code can still be typed:
// the device token it saves is what lets the service log in unattended.
func serviceInstall(client *dsm.Client, o options) error {
	c, err := serviceConfig(o.sel)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return err
	}
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return dsm.Errorf("this synodock is a temporary go run build; install one first (go install github.com/ryantenney/synodock@latest)")
	}
	args := []string{"serve"}
	if o.idleSet { // else serve reads idle_exit from config.toml at each start
		args = append(args, "--idle-exit", o.idle.String())
	}
	env := map[string]string{"PATH": os.Getenv("PATH")}
	if c.Name != "" {
		args = append([]string{"--nas", c.Name}, args...)
	} else {
		// The unnamed NAS by address, so a later synodock use doesn't move the service.
		env["SYNODOCK_URL"], env["SYNODOCK_USER"] = c.URL, c.User
	}
	if o.verbose {
		args = append(args, "-v")
	}
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	socket := socketPath(c.Name)
	files, err := service.Install(service.Spec{Name: c.Name, Exe: exe, Args: args, Socket: socket, Env: env})
	if err != nil {
		return err
	}
	for _, f := range files {
		fmt.Println("wrote " + f)
	}
	idle, from := idleExit(o, c, true), "idle_exit in "+dsm.ConfigPath()
	switch {
	case o.idleSet:
		from = "--idle-exit, fixed in the service"
	case c.IdleExit == nil:
		from = "the default; set idle_exit in " + dsm.ConfigPath()
	}
	when := "exits after " + idle.String() + " idle"
	if idle == 0 {
		when = "doesn't exit when idle"
	}
	fmt.Printf("%s listens on unix://%s and starts synodock serve on the first connection; it %s (%s)\n",
		service.Manager, socket, when, from)
	if !o.noCtx {
		if err := dockerapi.DockerContext(contextName(c.Name), socket, contextDescription(c)); err != nil {
			return err
		}
		fmt.Printf("docker context %s -> unix://%s\n", contextName(c.Name), socket)
	}
	// The first connection starts it: check it answers, and reaches DSM from there.
	httpc := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}}
	// macOS asks once whether the service may use the local network, and only
	// processes started after the answer get it: serve exits after a refusal, so
	// keep asking while you answer.
	asked := false
	for deadline := time.Now().Add(2 * time.Minute); ; {
		message, err := checkInfo(httpc)
		if err != nil {
			return dsm.Errorf("installed, but serve didn't answer: %v (see %s)", err, service.LogPath(c.Name))
		}
		if message == "" {
			break
		}
		if !strings.Contains(message, dockerapi.LocalNetworkDenied) || time.Now().After(deadline) {
			return dsm.Errorf("installed and started, but it can't reach the NAS: %s (see %s)", message, service.LogPath(c.Name))
		}
		if !asked {
			fmt.Println("macOS is asking whether synodock may use the local network: click Allow " +
				"(or turn it on in System Settings > Privacy & Security > Local Network); waiting...")
			asked = true
		}
		time.Sleep(3 * time.Second)
	}
	fmt.Printf("serve answered from the NAS; try: docker --context %s ps\n", contextName(c.Name))
	return nil
}

// checkInfo asks serve for /info, which needs the NAS: "" if that worked, else
// serve's message.
func checkInfo(httpc *http.Client) (string, error) {
	resp, err := httpc.Get("http://synodock/info")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return "", nil
	}
	var body struct{ Message string }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Message == "" {
		body.Message = resp.Status
	}
	return body.Message, nil
}

// serviceCommand is service uninstall and status, which need no login.
func serviceCommand(o options) error {
	c, err := serviceConfig(o.sel)
	if err != nil {
		return err
	}
	switch o.cmd {
	case "service status":
		state, err := service.Status(c.Name)
		if err != nil {
			return err
		}
		fmt.Printf("%s (%s): %s\n", service.Label(c.Name), service.Manager, state)
	case "service uninstall":
		files, err := service.Uninstall(c.Name, socketPath(c.Name))
		if err != nil {
			return err
		}
		for _, f := range files {
			fmt.Println("removed " + f)
		}
		fmt.Printf("(docker context %s is left; docker context rm %s removes it)\n", contextName(c.Name), contextName(c.Name))
	}
	return nil
}

// use lists the NAS in config.toml, marking the one commands use, or changes
// the default.
func use(o options) error {
	if o.sel != (dsm.Selection{}) {
		return dsm.Errorf("use doesn't take -H or --nas: it changes the default they override")
	}
	if o.use != "" || o.unnamed {
		if err := dsm.SetDefault(o.use); err != nil {
			return err
		}
		if o.unnamed {
			fmt.Println("default NAS: the unnamed one at the top of " + dsm.ConfigPath())
		} else {
			fmt.Println("default NAS: " + o.use)
		}
		if os.Getenv("SYNODOCK_URL") != "" || os.Getenv("SYNODOCK_NAS") != "" {
			fmt.Fprintln(os.Stderr, "note: SYNODOCK_URL or SYNODOCK_NAS is set, and overrides it")
		}
		return nil
	}
	profiles, chosenBy, err := dsm.Profiles()
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		fmt.Printf("no NAS set up: add url and user, or [nas.NAME] sections, to %s\n", dsm.ConfigPath())
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tURL\tUSER")
	for _, p := range profiles {
		mark, name := "  ", p.Name
		if p.Active {
			mark = "* "
		}
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%s\n", mark, name, p.URL, p.User)
	}
	tw.Flush()
	if chosenBy == "SYNODOCK_URL" || chosenBy == "SYNODOCK_NAS" {
		fmt.Printf("(%s is set and picks the NAS marked *)\n", chosenBy)
	}
	return nil
}
