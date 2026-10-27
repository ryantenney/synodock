package main

import (
	"os"
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devnull, devnull // usage and errors
	defer func() { os.Stdout, os.Stderr = stdout, stderr }()

	for _, tc := range []struct {
		args  []string
		cmd   string
		code  int
		check func(o options) bool
	}{
		// Shortcuts and the group forms are the same command.
		{[]string{"ps", "-a"}, "container ls", 0, func(o options) bool { return o.all }},
		{[]string{"container", "ls", "-a"}, "container ls", 0, func(o options) bool { return o.all }},
		{[]string{"container", "ps"}, "container ls", 0, nil},
		{[]string{"container", "list"}, "container ls", 0, nil},
		{[]string{"logs", "web", "-n", "5"}, "container logs", 0, func(o options) bool { return o.name == "web" && o.tail == 5 }},
		{[]string{"container", "logs", "-k", "err", "web"}, "container logs", 0, func(o options) bool {
			return o.name == "web" && o.keyword == "err" && o.tail == 100
		}},
		{[]string{"top", "web"}, "container top", 0, func(o options) bool { return o.name == "web" }},
		{[]string{"container", "top", "web"}, "container top", 0, nil},
		// exec: flags after NAME belong to the command; a leading -- is dropped.
		{[]string{"exec", "-i", "web", "ls", "-la"}, "container exec", 0, func(o options) bool {
			return o.name == "web" && o.exec.Stdin && reflect.DeepEqual(o.argv, []string{"ls", "-la"})
		}},
		{[]string{"container", "exec", "web", "--", "echo", "-n"}, "container exec", 0, func(o options) bool {
			return reflect.DeepEqual(o.argv, []string{"echo", "-n"})
		}},
		{[]string{"exec", "-t", "web", "sh"}, "container exec", 0, func(o options) bool { return !o.exec.TTY }}, // -t needs -i
		{[]string{"service", "install", "--idle-exit", "5m", "-v"}, "service install", 0, func(o options) bool {
			return o.idleSet && o.verbose && o.idle.Minutes() == 5
		}},
		{[]string{"service", "status"}, "service status", 0, nil},
		{[]string{"--nas", "home", "service", "uninstall"}, "service uninstall", 0, func(o options) bool { return o.sel.NAS == "home" }},
		{[]string{"login", "--otp", "123456"}, "login", 0, func(o options) bool { return o.otp == "123456" }},
		{[]string{"serve", "--docker-context"}, "serve", 0, func(o options) bool { return o.context == "auto" }},
		{[]string{"use", "home"}, "use", 0, func(o options) bool { return o.use == "home" }},
		{[]string{"-H", "https://u@nas:5001", "ps"}, "container ls", 0, func(o options) bool { return o.sel.Host == "https://u@nas:5001" }},

		// Help: exit 0, nothing to run.
		{[]string{"--help"}, "", 0, nil},
		{[]string{"help"}, "", 0, nil},
		{[]string{"container", "-h"}, "", 0, nil},
		{[]string{"container", "help"}, "", 0, nil},
		{[]string{"container", "ls", "-h"}, "", 0, nil},
		{[]string{"--version"}, "", 0, nil},

		// Mistakes: exit 2.
		{nil, "", 2, nil},
		{[]string{"container"}, "", 2, nil},
		{[]string{"container", "nope"}, "", 2, nil},
		{[]string{"nope"}, "", 2, nil},
		{[]string{"--bogus"}, "", 2, nil},
		{[]string{"ps", "--bogus"}, "", 2, nil},
		{[]string{"ps", "extra"}, "", 2, nil},
		{[]string{"logs"}, "", 2, nil},
		{[]string{"logs", "a", "b"}, "", 2, nil},
		{[]string{"exec"}, "", 2, nil},
		{[]string{"service", "install", "extra"}, "", 2, nil},
		{[]string{"service", "status", "-v"}, "", 2, nil}, // install's flags only
		{[]string{"use", "a", "b"}, "", 2, nil},
		{[]string{"use", "--unnamed", "home"}, "", 2, nil},
		{[]string{"install"}, "", 2, nil}, // a verb outside its group
	} {
		o, code, ok := parse(tc.args)
		if wantOK := tc.cmd != ""; ok != wantOK || code != tc.code {
			t.Errorf("%q: ok %v code %d, want ok %v code %d", tc.args, ok, code, wantOK, tc.code)
			continue
		}
		if !ok {
			continue
		}
		if o.cmd != tc.cmd {
			t.Errorf("%q: cmd %q, want %q", tc.args, o.cmd, tc.cmd)
		}
		if tc.check != nil && !tc.check(o) {
			t.Errorf("%q: options %+v", tc.args, o)
		}
	}
}

// Every shortcut names a real command, and every command's group is listed.
func TestCommandTable(t *testing.T) {
	for _, s := range shortcutOrder {
		if shortcut(s) == nil {
			t.Errorf("shortcut %s -> %q: no such command", s, shortcuts[s])
		}
	}
	if len(shortcutOrder) != len(shortcuts) {
		t.Errorf("shortcutOrder has %d, shortcuts %d", len(shortcutOrder), len(shortcuts))
	}
	seen := map[string]bool{}
	for _, c := range commands {
		if c.group != "" && !isGroup(c.group) {
			t.Errorf("%s: group %q not in groups", c.full(), c.group)
		}
		for _, n := range append([]string{c.name}, c.aliases...) {
			key := c.group + " " + n
			if seen[key] {
				t.Errorf("%s: name %q used twice", c.full(), n)
			}
			seen[key] = true
		}
		if c.group == "" && (isGroup(c.name) || shortcuts[c.name] != "") {
			t.Errorf("%s: shadowed by a group or shortcut", c.name)
		}
	}
}
