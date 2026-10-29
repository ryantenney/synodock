package main

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ryantenney/synodock/internal/dsm"
	"github.com/ryantenney/synodock/internal/terminal"
)

// cpTarget splits CONTAINER:PATH; ok is false for a local path (one with a / before
// any colon, or none, as docker reads it).
func cpTarget(arg string) (container, p string, ok bool) {
	i := strings.Index(arg, ":")
	if i <= 0 || strings.Contains(arg[:i], "/") {
		return "", arg, false
	}
	return arg[:i], arg[i+1:], true
}

// containerCopy is `synodock cp`: a file or directory between here and a container, as
// docker cp copies: into an existing directory under its own name, else to that path.
// Into a container it goes as stdin through synodock-agent (a directory as a tar, which
// needs tar in the image); out of one, a tar where the image has tar, else the file.
func containerCopy(ctx context.Context, client *dsm.Client, o *options) error {
	srcCtr, src, fromCtr := cpTarget(o.cpSrc)
	dstCtr, dst, toCtr := cpTarget(o.cpDst)
	switch {
	case fromCtr == toCtr:
		return dsm.Errorf("cp copies between here and a container: one of SRC and DEST is CONTAINER:PATH, not both")
	case toCtr:
		return copyIn(ctx, client, src, dstCtr, dst)
	default:
		return copyOut(ctx, client, srcCtr, src, dst)
	}
}

// exec runs a script in the container; its stderr goes to ours, and a non-zero exit is
// an error.
func cpExec(ctx context.Context, client *dsm.Client, container string, argv []string, stdin io.Reader, stdout io.Writer) error {
	var errOut strings.Builder
	code, err := terminal.Exec(ctx, client, container, argv, "", stdin, stdout, &errOut)
	if errors.Is(err, terminal.ErrNoAgent) {
		return dsm.Errorf("%s: copying into a container needs synodock-agent, and it couldn't run there "+
			"(no base64, or no /tmp it can run from; or SYNODOCK_AGENT=0 or mount)", container)
	}
	if err != nil {
		return err
	}
	if code != 0 {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", code)
		}
		return dsm.Errorf("%s: %s", container, msg)
	}
	return nil
}

// Into the container: $1 the destination, $2 the source's name; a file is cat's stdin
// (with $3 its mode), a directory tar's.
const (
	copyInFile = `if [ -d "$1" ]; then f="$1/$2"; else f="$1"; fi
cat >"$f" && chmod "$3" "$f"`
	copyInDir = `if [ -d "$1" ]; then d="$1/$2"; else d="$1"; fi
command -v tar >/dev/null 2>&1 || { echo "copying a directory needs tar in the image" >&2; exit 127; }
mkdir -p "$d" && cd "$d" && exec tar -xf -`
)

func copyIn(ctx context.Context, client *dsm.Client, src, container, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	name := filepath.Base(src)
	if !fi.IsDir() {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		return cpExec(ctx, client, container,
			[]string{"sh", "-c", copyInFile, "sh", dst, name, fmt.Sprintf("%o", fi.Mode().Perm())}, f, io.Discard)
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTar(pw, src)) }()
	defer pr.Close()
	return cpExec(ctx, client, container, []string{"sh", "-c", copyInDir, "sh", dst, name}, pr, io.Discard)
}

// writeTar writes dir's contents, as ./ paths: directories, files and symlinks.
func writeTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !fi.Mode().IsRegular() && !fi.IsDir() {
			return nil // sockets, devices and the like: not copied
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		h.Name = "./" + filepath.ToSlash(rel)
		if fi.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", "" // the container's user owns what it writes
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Out of the container: from $1's directory, a tar of $2 after a T, or with no tar, a
// file after an F.
const copyOutScript = `cd "$1" || exit 1
if command -v tar >/dev/null 2>&1; then printf T; exec tar -cf - "$2"; fi
if [ -f "$2" ]; then printf F; exec cat "$2"; fi
echo "copying a directory needs tar in the image" >&2; exit 127`

func copyOut(ctx context.Context, client *dsm.Client, container, src, dst string) error {
	src = path.Clean(src)
	if src == "/" || src == "." {
		return dsm.Errorf("cp copies one file or directory, by name: not %s", src)
	}
	name := path.Base(src)
	// Where it lands: in dst under its own name if dst is a directory, else at dst.
	parent, as := filepath.Dir(dst), filepath.Base(dst)
	if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		parent, as = dst, name
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer root.Close()

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := readCopy(bufio.NewReader(pr), root, name, as)
		_, _ = io.Copy(io.Discard, pr) // whatever's left, so the exec isn't held up
		done <- err
	}()
	err = cpExec(ctx, client, container, []string{"sh", "-c", copyOutScript, "sh", path.Dir(src), name}, nil, pw)
	pw.CloseWithError(err)
	if rerr := <-done; err == nil {
		err = rerr
	}
	return err
}

// readCopy writes what copyOutScript sends into root: name (the tar's top entry) as as.
func readCopy(r *bufio.Reader, root *os.Root, name, as string) error {
	kind, err := r.ReadByte()
	if err != nil {
		return dsm.Errorf("cp: nothing came back")
	}
	if kind == 'F' {
		f, err := root.OpenFile(as, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		return errors.Join(err, f.Close())
	}
	if kind != 'T' {
		return dsm.Errorf("cp: unexpected output from the container")
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel := path.Clean(h.Name)
		if rel != name && !strings.HasPrefix(rel, name+"/") {
			return dsm.Errorf("cp: unexpected entry %q in the container's tar", h.Name)
		}
		p := filepath.FromSlash(as + strings.TrimPrefix(rel, name)) // os.Root keeps it inside
		mode := fs.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(p, 0o755); err != nil {
				return err
			}
			_ = root.Chmod(p, mode)
		case tar.TypeReg:
			f, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if err = errors.Join(err, f.Close()); err != nil {
				return err
			}
			_ = root.Chmod(p, mode)
		case tar.TypeSymlink:
			_ = root.Remove(p)
			if err := root.Symlink(h.Linkname, p); err != nil {
				return err
			}
		}
	}
}
