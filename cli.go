package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/ryantenney/synodock/internal/terminal"
)

// The command line is docker's shape: noun groups (synodock container ls), a few
// commands of their own (login, serve, use), and top-level shortcuts for the
// common ones (synodock ps = synodock container ls).

// A command is one verb: options.cmd holds its full name, "container ls" or "login".
type command struct {
	group, name string
	aliases     []string // other names in the group, as docker's container ps for ls
	opts        string   // the flags, for the list: "[-a]"
	args        string   // the positional arguments: "NAME"
	short       string
	note        string                              // after the flags in -h
	flags       func(fs *pflag.FlagSet, o *options) // nil: none
	commandArgs bool                                // exec: flags after the first argument are the command's
	positional  func(o *options, rest []string) bool
}

func (c *command) full() string {
	if c.group == "" {
		return c.name
	}
	return c.group + " " + c.name
}

// synopsis is what the lists show: "ls [-a]".
func (c *command) synopsis() string {
	return words(c.name, c.opts, c.args)
}

// words joins the non-empty parts with single spaces.
func words(parts ...string) string {
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

type group struct{ name, short string }

var groups = []group{
	{"container", "containers: list, logs, processes, exec"},
	{"service", "run serve as a service launchd/systemd starts on demand"},
	{"project", "Container Manager projects: list, config, ps, create, update, up, down, start, stop, restart"},
	{"image", "images on the NAS: list, rm, import, load, pull"},
	{"network", "Docker networks: list, inspect, create, rm"},
	{"registry", "registries Container Manager pulls from: list, add, rm, use, search, tags"},
	{"events", "Container Manager's own log (its Log tab): list, clear"},
}

// shortcuts are top-level names for commands in a group, as docker's ps and exec.
var shortcuts = map[string]string{
	"ps":   "container ls",
	"logs": "container logs",
	"top":  "container top",
	"exec": "container exec",
}

var shortcutOrder = []string{"ps", "logs", "top", "exec"}

// deployFlags are project deploy's flags; plan has the same ones without -y and --dry-run,
// since it only ever plans.
func deployFlags(deploy bool) func(fs *pflag.FlagSet, o *options) {
	return func(fs *pflag.FlagSet, o *options) {
		fs.StringArrayVar(&o.images, "image", nil, "a docker save tar to load (repeatable)")
		fs.StringArrayVar(&o.pulls, "pull", nil, "a registry image, REPO[:TAG], to pull (repeatable)")
		fs.StringVar(&o.registry, "registry", "", "the registry's URL, for --pull (default: registry in the profile)")
		fs.StringVar(&o.user, "user", "", "the registry account, for --pull (default: registry_user in the profile)")
		fs.StringVar(&o.file, "compose", "", "a new compose file for the project")
		fs.StringVar(&o.folder, "images-folder", "", "the NAS folder for tars (default: <project folder>/images)")
		fs.BoolVar(&o.keepOld, "keep-old-images", false, "don't delete the images the deploy replaces")
		fs.DurationVar(&o.timeout, "timeout", 10*time.Minute, "how long to wait for the containers to be healthy")
		if deploy {
			fs.BoolVar(&o.dryRun, "dry-run", false, "print the plan and stop (the same as project plan)")
			fs.BoolVarP(&o.yes, "yes", "y", false, "don't ask before taking the project down")
		} else {
			o.dryRun = true
		}
	}
}

// two takes NAME and FILE.
func two(o *options, rest []string) bool {
	if len(rest) != 2 {
		return false
	}
	o.name, o.file = rest[0], rest[1]
	return true
}

// onePath takes one positional argument, a file path on the NAS or here.
func onePath(o *options, rest []string) bool {
	if len(rest) != 1 {
		return false
	}
	o.file = rest[0]
	return true
}

// tarAndFolder takes TAR and FOLDER.
func tarAndFolder(o *options, rest []string) bool {
	if len(rest) != 2 {
		return false
	}
	o.file, o.folder = rest[0], rest[1]
	return true
}

// twoRegistry takes NAME and URL.
func twoRegistry(o *options, rest []string) bool {
	if len(rest) != 2 {
		return false
	}
	o.name, o.registry = rest[0], rest[1]
	return true
}

// one takes exactly one positional argument, a container's name.
func one(o *options, rest []string) bool {
	if len(rest) != 1 {
		return false
	}
	o.name = rest[0]
	return true
}

var commands = []*command{
	{group: "container", name: "ls", aliases: []string{"ps", "list"}, opts: "[-a]", short: "list containers",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.BoolVarP(&o.all, "all", "a", false, "include stopped containers")
		}},
	{group: "container", name: "logs", opts: "[-n N] [-k WORD]", args: "NAME", short: "a container's log, oldest first",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.IntVarP(&o.tail, "tail", "n", 100, "last N lines")
			fs.StringVarP(&o.keyword, "keyword", "k", "", "only lines containing this")
		}, positional: one},
	{group: "container", name: "top", args: "NAME", short: "a container's processes", positional: one},
	{group: "container", name: "export", opts: "[-o FILE]", args: "NAME",
		short: "the container's settings as Container Manager exports them (JSON; includes environment variables)",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.StringVarP(&o.output, "output", "o", "", "write to this file (mode 0600), not stdout")
		}, positional: one},
	{group: "container", name: "create", opts: "--profile FILE [--start]", short: "create a standalone container from a profile (the JSON container export writes)",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.StringVar(&o.file, "profile", "", "the profile file")
			fs.BoolVar(&o.start, "start", false, "start it at once")
		}, positional: func(o *options, rest []string) bool { return len(rest) == 0 && o.file != "" }},
	{group: "container", name: "update", opts: "--profile FILE", args: "NAME",
		short: "apply a profile (the JSON container export writes) to a container; its name in the profile can rename it",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.StringVar(&o.file, "profile", "", "the profile file")
		}, positional: func(o *options, rest []string) bool {
			if len(rest) != 1 || o.file == "" {
				return false
			}
			o.name = rest[0]
			return true
		}},
	{group: "container", name: "exec", opts: "[-i] [-t] [-d]", args: "NAME [--] COMMAND...",
		short: "run a command in a running container", commandArgs: true,
		note: "Runs through Container Manager's Terminal: always on a TTY, as the container's default user, so stderr comes back with stdout.",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.BoolVarP(&o.exec.Stdin, "interactive", "i", false, "send stdin to the command")
			fs.BoolVarP(&o.exec.TTY, "tty", "t", false, "with -i on a terminal: raw mode, resize, ^C goes through")
			fs.BoolVarP(&o.exec.Detach, "detach", "d", false, "start it in the background and return; output discarded")
			fs.BoolVar(&o.exec.Raw, "raw", false, "don't wrap in sh (images without one); no exit status")
			fs.BoolVar(&o.noGzip, "no-gzip", false, "don't compress output in the container (SYNODOCK_GZIP=0 does the same)")
		},
		positional: func(o *options, rest []string) bool {
			if len(rest) == 0 {
				return false
			}
			o.name, o.argv = rest[0], rest[1:]
			if len(o.argv) > 0 && o.argv[0] == "--" {
				o.argv = o.argv[1:]
			}
			o.exec.TTY = o.exec.TTY && o.exec.Stdin
			o.exec.Gzip = terminal.GzipDefault() && !o.noGzip
			return true
		}},

	{group: "service", name: "install", opts: "[--idle-exit D] [--no-docker-context] [-v]",
		short: "install it, log in, create a docker context and check serve answers",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.DurationVar(&o.idle, "idle-exit", 0, "fix serve's idle timeout in the service, over idle_exit in config.toml")
			fs.BoolVar(&o.noCtx, "no-docker-context", false, "don't create a docker context for the socket")
			fs.BoolVarP(&o.verbose, "verbose", "v", false, "serve logs each request")
		}},
	{group: "service", name: "uninstall", short: "remove it (the docker context is left)"},
	{group: "service", name: "status", short: "whether it's loaded and running"},

	{group: "project", name: "ls", aliases: []string{"list"}, short: "projects, with their status and folder"},
	{group: "project", name: "config", opts: "[-o FILE]", args: "NAME", short: "a project's compose file",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.StringVarP(&o.outFile, "output", "o", "", "write it to this file")
		}, positional: one},
	{group: "project", name: "ps", args: "NAME", short: "a project's containers, with health", positional: one},
	{group: "project", name: "create", opts: "[--path DIR]", args: "NAME FILE",
		short: "create a project from a compose file; makes its folder and the bind-mount folders in it",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.StringVar(&o.sharePath, "path", "", "the project's folder on the NAS (default /docker/NAME)")
		}, positional: two},
	{group: "project", name: "update", args: "NAME FILE",
		short: "replace a stopped project's compose file; the old one is backed up first", positional: two},
	{group: "project", name: "up", aliases: []string{"build"}, args: "NAME",
		short: "bring a stopped project up: compose build, streamed", positional: one},
	{group: "project", name: "down", aliases: []string{"clean"}, args: "NAME",
		short: "take a project down: compose clean (containers and network; named volumes stay)", positional: one},
	{group: "project", name: "deploy", opts: "[--image TAR]... [--pull REPO[:TAG]]... [--compose FILE] [--dry-run] [-y]", args: "NAME",
		short: "take it down, load or pull new images, update the compose file, build, wait healthy, then clean up old images",
		flags: deployFlags(true), positional: one},
	{group: "project", name: "plan", opts: "[--image TAR]... [--pull REPO[:TAG]]... [--compose FILE]", args: "NAME",
		short: "deploy's checks and plan; changes nothing (deploy --dry-run, under a name an allowlist can match)",
		flags: deployFlags(false), positional: one},
	{group: "project", name: "logs", args: "NAME", short: "the compose output from its last action", positional: one},
	{group: "project", name: "rm", opts: "[-f]", args: "NAME",
		short: "delete a stopped project with its containers and data; asks for the name unless -f",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.BoolVarP(&o.force, "force", "f", false, "don't ask")
		}, positional: one},
	{group: "project", name: "start", args: "NAME", short: "compose start", positional: one},
	{group: "project", name: "stop", args: "NAME", short: "compose stop", positional: one},
	{group: "project", name: "restart", args: "NAME", short: "compose restart", positional: one},

	{group: "events", name: "ls", aliases: []string{"list"}, opts: "[-n N] [--level L] [-k WORD]",
		short: "the newest events; --level is info, warn or err",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.IntVarP(&o.tail, "tail", "n", 100, "the newest N events")
			fs.StringVar(&o.level, "level", "", "only this level: info, warn or err")
			fs.StringVarP(&o.keyword, "keyword", "k", "", "only events containing this")
		}},
	{group: "events", name: "export", opts: "[--format csv|html] [-o FILE] [--level L] [-k WORD]",
		short: "the log as Container Manager's own CSV or HTML export; to stdout unless -o",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.StringVar(&o.format, "format", "csv", "csv or html")
			fs.StringVarP(&o.output, "output", "o", "", "write to this file")
			fs.StringVar(&o.level, "level", "", "only this level: info, warn or err")
			fs.StringVarP(&o.keyword, "keyword", "k", "", "only events containing this")
		}},
	{group: "events", name: "clear", opts: "[-f]", short: "empty the log; asks for the word clear unless -f",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.BoolVarP(&o.force, "force", "f", false, "don't ask")
		}},
	{group: "registry", name: "ls", aliases: []string{"list"}, short: "registries; * marks the one in use"},
	{group: "registry", name: "add", opts: "[--user NAME]", args: "NAME URL",
		short: "add a registry; a password comes from SYNODOCK_REGISTRY_PASSWORD",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.StringVar(&o.user, "user", "", "the account, for a registry that needs one")
		}, positional: twoRegistry},
	{group: "registry", name: "rm", opts: "[-f]", args: "NAME",
		short: "remove a registry (not the one in use); asks for the name unless -f",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.BoolVarP(&o.force, "force", "f", false, "don't ask")
		}, positional: one},
	{group: "registry", name: "use", args: "NAME", short: "make a registry the one in use", positional: one},
	{group: "registry", name: "search", opts: "[-n N]", args: "QUERY",
		short: "search the registry in use (Docker Hub, by default)",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.IntVarP(&o.tail, "limit", "n", 10, "at most this many results")
		}, positional: one},
	{group: "registry", name: "tags", args: "REPO", short: "a repository's tags, in the registry in use", positional: one},
	{group: "network", name: "ls", aliases: []string{"list"}, short: "networks, with their subnets and containers"},
	{group: "network", name: "inspect", args: "NAME", short: "one network, and the containers on it", positional: one},
	{group: "network", name: "create", opts: "--subnet CIDR --gateway IP [--iprange CIDR] [--disable-masquerade]", args: "NAME",
		short: "create a network with a fixed subnet",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.StringVar(&o.subnet, "subnet", "", "the network's address range, e.g. 172.31.0.0/16")
			fs.StringVar(&o.gateway, "gateway", "", "the gateway address inside the subnet")
			fs.StringVar(&o.iprange, "iprange", "", "addresses containers get, when narrower than the subnet")
			fs.BoolVar(&o.noMasquerade, "disable-masquerade", false, "don't NAT outbound traffic")
		}, positional: one},
	{group: "network", name: "rm", opts: "[-f]", args: "NAME",
		short: "delete a network with no containers on it; asks for the name unless -f",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.BoolVarP(&o.force, "force", "f", false, "don't ask")
		}, positional: one},
	{group: "image", name: "ls", aliases: []string{"list"}, short: "images, one row per tag"},
	{group: "image", name: "rm", opts: "[-f]", args: "ID", short: "delete an image by id; asks for the id unless -f",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.BoolVarP(&o.force, "force", "f", false, "don't ask")
		}, positional: one},
	{group: "image", name: "save", opts: "--to FOLDER", args: "REPO[:TAG]",
		short: "save an image as a docker save tar in a NAS folder, named by Container Manager",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.StringVar(&o.folder, "to", "", "the NAS folder; it must exist")
		}, positional: one},
	{group: "image", name: "upgrade", opts: "[-f]", args: "REPO",
		short: "pull a newer REPO:latest and recreate its containers; lists them and asks first unless -f",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.BoolVarP(&o.force, "force", "f", false, "don't ask")
		}, positional: one},
	{group: "image", name: "prune", opts: "[-f]",
		short: "delete the images no container uses; lists them, then asks for the word prune unless -f",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.BoolVarP(&o.force, "force", "f", false, "don't ask")
		}},
	{group: "image", name: "import", args: "PATH", short: "load a docker save tar already on the NAS", positional: onePath},
	{group: "image", name: "load", args: "TAR FOLDER",
		short:      "upload a docker save tar to a NAS folder and load it; the old tar there is kept as .prev.tar",
		positional: tarAndFolder},
	{group: "image", name: "pull", opts: "--registry URL --user NAME", args: "REPO[:TAG]",
		short: "pull from a registry onto the NAS; the password comes from SYNODOCK_REGISTRY_PASSWORD",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.StringVar(&o.registry, "registry", "", "the registry's URL, e.g. https://registry.example.com")
			fs.StringVar(&o.user, "user", "", "the registry account")
		}, positional: one},
	{name: "login", opts: "[--otp CODE]", short: "check that the login works; with --otp, save a 2FA device token",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.StringVar(&o.otp, "otp", "", "a current code from the authenticator app")
		}},
	{name: "serve", opts: "[--socket PATH] [--docker-context[=NAME]] [-v]",
		short: "serve a Docker Engine API (subset) on a Unix socket, for the docker CLI",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.StringVar(&o.socket, "socket", "", "the socket to serve on (default ~/.cache/synodock/docker.sock, or NAME.sock for --nas NAME)")
			fs.StringVar(&o.context, "docker-context", "", "create or update a docker context for this socket (default name synology, or synology-NAME)")
			fs.Lookup("docker-context").NoOptDefVal = "auto"
			fs.DurationVar(&o.idle, "idle-exit", 0, "exit after no requests for this long (default: idle_exit in "+
				"config.toml, else 15m when started on demand, else never)")
			fs.BoolVarP(&o.verbose, "verbose", "v", false, "log each request")
		}},
	{name: "use", opts: "[NAME | --unnamed]", short: "list the NAS in config.toml, or pick the one commands use",
		flags: func(fs *pflag.FlagSet, o *options) {
			fs.BoolVar(&o.unnamed, "unnamed", false, "go back to the NAS at the top of config.toml")
		},
		positional: func(o *options, rest []string) bool {
			if len(rest) > 1 || len(rest) == 1 && o.unnamed {
				return false
			}
			if len(rest) == 1 {
				o.use = rest[0]
			}
			return true
		}},
}

// lookup finds a command by its full name or a group's alias for it.
func lookup(groupName, name string) *command {
	for _, c := range commands {
		if c.group != groupName {
			continue
		}
		if c.name == name {
			return c
		}
		for _, a := range c.aliases {
			if a == name {
				return c
			}
		}
	}
	return nil
}

// shortcut is the command a top-level shortcut stands for.
func shortcut(name string) *command {
	groupName, verb, _ := strings.Cut(shortcuts[name], " ")
	return lookup(groupName, verb)
}

func isGroup(name string) bool {
	for _, g := range groups {
		if g.name == name {
			return true
		}
	}
	return false
}

// listLine is one row of a command list, the description on the next line when
// the left side doesn't fit, as pflag does.
func listLine(w io.Writer, left, short string) {
	const width = 34
	if len(left) <= width {
		fmt.Fprintf(w, "  %-*s %s\n", width, left, short)
		return
	}
	fmt.Fprintf(w, "  %s\n  %-*s %s\n", left, width, "", short)
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `usage: synodock [-H URL | --nas NAME] <command> [flags]

docker exec and a Docker API for Synology Container Manager, over DSM's web API (no SSH)

  -H, --host URL                     a NAS by address: https://user@nas.example.com:5001
      --nas NAME                     a [nas.NAME] in config.toml (else SYNODOCK_URL, SYNODOCK_NAS,
                                     default = in config.toml, then its top-level url)

commands:
`)
	for _, g := range groups {
		listLine(w, g.name, g.short)
	}
	for _, c := range commands {
		if c.group == "" {
			listLine(w, c.synopsis(), c.short)
		}
	}
	fmt.Fprint(w, "\nshortcuts:\n")
	for _, s := range shortcutOrder {
		c := shortcut(s)
		listLine(w, words(s, c.opts, c.args), "synodock "+c.full())
	}
	fmt.Fprint(w, "\nRun synodock <command> -h for a command's flags, synodock <group> for its commands.\n")
}

func printGroupUsage(w io.Writer, groupName string) {
	fmt.Fprintf(w, "usage: synodock %s <command> [flags]\n\ncommands:\n", groupName)
	for _, c := range commands {
		if c.group == groupName {
			listLine(w, c.synopsis(), c.short)
		}
	}
	fmt.Fprintf(w, "\nRun synodock %s <command> -h for a command's flags.\n", groupName)
}

// parse reads the command line; ok is false when there's nothing to run, with
// the exit code to leave with.
func parse(args []string) (o options, code int, ok bool) {
	global := pflag.NewFlagSet("synodock", pflag.ContinueOnError)
	global.SetInterspersed(false) // flags after the command are the command's
	global.Usage = func() {}      // printed below, once
	global.StringVarP(&o.sel.Host, "host", "H", "", "")
	global.StringVar(&o.sel.NAS, "nas", "", "")
	showVersion := global.Bool("version", false, "")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			printUsage(os.Stdout)
			return o, 0, false
		}
		fmt.Fprintf(os.Stderr, "synodock: %v\n\n", err)
		printUsage(os.Stderr)
		return o, 2, false
	}
	if *showVersion {
		fmt.Println("synodock " + version)
		return o, 0, false
	}
	args = global.Args()
	if len(args) == 0 {
		printUsage(os.Stderr)
		return o, 2, false
	}
	if args[0] == "help" {
		printUsage(os.Stdout)
		return o, 0, false
	}

	var c *command
	switch {
	case isGroup(args[0]):
		groupName := args[0]
		if len(args) == 1 {
			printGroupUsage(os.Stderr, groupName)
			return o, 2, false
		}
		if args[1] == "-h" || args[1] == "--help" || args[1] == "help" {
			printGroupUsage(os.Stdout, groupName)
			return o, 0, false
		}
		if c = lookup(groupName, args[1]); c == nil {
			fmt.Fprintf(os.Stderr, "synodock %s: unknown command %q\n\n", groupName, args[1])
			printGroupUsage(os.Stderr, groupName)
			return o, 2, false
		}
		args = args[2:]
	case shortcuts[args[0]] != "":
		c = shortcut(args[0])
		args = args[1:]
	default:
		if c = lookup("", args[0]); c == nil {
			fmt.Fprintf(os.Stderr, "synodock: unknown command %q\n\n", args[0])
			printUsage(os.Stderr)
			return o, 2, false
		}
		args = args[1:]
	}
	o.cmd = c.full()

	fs := pflag.NewFlagSet("synodock "+o.cmd, pflag.ContinueOnError)
	fs.SortFlags = false
	fs.SetInterspersed(!c.commandArgs)
	if c.flags != nil {
		c.flags(fs, &o)
	}
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s\n\n%s", words("synodock", o.cmd, "[flags]", c.args), fs.FlagUsages())
		if c.note != "" {
			fmt.Fprintln(os.Stderr, "\n"+c.note)
		}
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return o, 0, false
		}
		fmt.Fprintf(os.Stderr, "synodock %s: %v\n\n", o.cmd, err)
		fs.Usage()
		return o, 2, false
	}
	o.idleSet = fs.Changed("idle-exit")
	rest := fs.Args()
	if c.positional == nil {
		if len(rest) > 0 {
			fmt.Fprintf(os.Stderr, "synodock %s: unexpected %q\n", o.cmd, rest[0])
			return o, 2, false
		}
	} else if !c.positional(&o, rest) {
		fs.Usage()
		return o, 2, false
	}
	return o, 0, true
}
