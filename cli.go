package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

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
}

// shortcuts are top-level names for commands in a group, as docker's ps and exec.
var shortcuts = map[string]string{
	"ps":   "container ls",
	"logs": "container logs",
	"top":  "container top",
	"exec": "container exec",
}

var shortcutOrder = []string{"ps", "logs", "top", "exec"}

// two takes NAME and FILE.
func two(o *options, rest []string) bool {
	if len(rest) != 2 {
		return false
	}
	o.name, o.file = rest[0], rest[1]
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
	{group: "project", name: "start", args: "NAME", short: "compose start", positional: one},
	{group: "project", name: "stop", args: "NAME", short: "compose stop", positional: one},
	{group: "project", name: "restart", args: "NAME", short: "compose restart", positional: one},

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
