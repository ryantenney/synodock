package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCpTarget(t *testing.T) {
	for arg, want := range map[string][3]string{
		"web:/etc/nginx": {"web", "/etc/nginx", "true"},
		"./web:/x":       {"", "./web:/x", "false"},
		"/tmp/a:b":       {"", "/tmp/a:b", "false"},
		"file.txt":       {"", "file.txt", "false"},
		":/x":            {"", ":/x", "false"},
		"ctr:relative/p": {"ctr", "relative/p", "true"},
	} {
		c, p, ok := cpTarget(arg)
		if c != want[0] || p != want[1] || (ok != (want[2] == "true")) {
			t.Errorf("%q: %q %q %v", arg, c, p, ok)
		}
	}
}

// A directory written by writeTar and read back as the container's tar would be (tar
// -cf - NAME, after a T) comes out the same, landing under the name given.
func TestCopyRoundTrip(t *testing.T) {
	src := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(src, "sub", "deeper"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o640))
	must(t, os.WriteFile(filepath.Join(src, "sub", "deeper", "b.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(t, os.Symlink("a.txt", filepath.Join(src, "link")))

	// writeTar's ./ paths, extracted by the system tar under NAME, as the container has it
	var buf bytes.Buffer
	must(t, writeTar(&buf, src))
	ctr := filepath.Join(t.TempDir(), "conf")
	must(t, os.Mkdir(ctr, 0o755))
	x := exec.Command("tar", "-xf", "-", "-C", ctr)
	x.Stdin = &buf
	if out, err := x.CombinedOutput(); err != nil {
		t.Fatalf("tar -x: %v %s", err, out)
	}
	// and back: tar -cf - conf, read by readCopy as "copy"
	var back bytes.Buffer
	back.WriteByte('T')
	c := exec.Command("tar", "-cf", "-", "conf")
	c.Dir = filepath.Dir(ctr)
	c.Env = append(os.Environ(), "COPYFILE_DISABLE=1") // macOS tar's ._ files; containers' tars have none
	c.Stdout = &back
	must(t, c.Run())
	dst := t.TempDir()
	root, err := os.OpenRoot(dst)
	must(t, err)
	defer root.Close()
	must(t, readCopy(bufio.NewReader(&back), root, "conf", "copy"))

	for p, want := range map[string]string{"a.txt": "alpha", "sub/deeper/b.sh": "#!/bin/sh\n"} {
		b, err := os.ReadFile(filepath.Join(dst, "copy", p))
		if err != nil || string(b) != want {
			t.Errorf("%s: %q %v", p, b, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dst, "copy", "sub", "deeper", "b.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("b.sh mode: %v %v", fi, err)
	}
	if l, err := os.Readlink(filepath.Join(dst, "copy", "link")); err != nil || l != "a.txt" {
		t.Errorf("link: %q %v", l, err)
	}
}

// A container's tar can't put anything outside the destination: not by .., and not
// through a symlink it made first.
func TestCopyOutStaysInside(t *testing.T) {
	outside := t.TempDir()
	for name, entries := range map[string][]*tar.Header{
		"dotdot": {{Name: "conf/../../escaped", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}},
		"other":  {{Name: "elsewhere/x", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}},
		"symlink": {
			{Name: "conf/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "conf/out", Typeflag: tar.TypeSymlink, Linkname: outside},
			{Name: "conf/out/escaped", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		},
	} {
		var buf bytes.Buffer
		buf.WriteByte('T')
		tw := tar.NewWriter(&buf)
		for _, h := range entries {
			must(t, tw.WriteHeader(h))
			if h.Size > 0 {
				_, _ = tw.Write([]byte("x"))
			}
		}
		must(t, tw.Close())
		dst := t.TempDir()
		root, err := os.OpenRoot(dst)
		must(t, err)
		err = readCopy(bufio.NewReader(&buf), root, "conf", "conf")
		root.Close()
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if _, err := os.Stat(filepath.Join(outside, "escaped")); err == nil {
			t.Fatalf("%s: wrote outside the destination", name)
		}
		if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dst), "escaped")); len(matches) > 0 {
			t.Fatalf("%s: wrote beside the destination", name)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
