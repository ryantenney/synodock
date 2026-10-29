package dockerapi

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/ryantenney/synodock/internal/terminal"
)

// The archive endpoints, which docker cp uses, run through framed exec: HEAD stats a
// path, GET sends it as a tar (both with the stat in X-Docker-Container-Path-Stat), PUT
// extracts a tar into a directory, as stdin through synodock-agent.

// statScript prints $1's stat as one line, "<size> <raw mode, hex> <mtime> <link
// target>", or exits 44 when it isn't there. With $2 = tar, a tar of it follows, from
// its directory.
const statScript = `p=$1
if [ ! -e "$p" ] && [ ! -L "$p" ]; then echo "Could not find the file $p in container" >&2; exit 44; fi
command -v stat >/dev/null 2>&1 || { echo "the image has no stat" >&2; exit 127; }
s=$(stat -c '%s %f %Y' "$p") || exit 1
l=; if [ -L "$p" ]; then l=$(readlink "$p"); fi
printf '%s %s\n' "$s" "$l"
[ "$2" = tar ] || exit 0
command -v tar >/dev/null 2>&1 || { echo "copying needs tar in the image" >&2; exit 127; }
cd "$(dirname "$p")" && exec tar -cf - "$(basename "$p")"`

// putScript extracts stdin, a tar, into $1, which must be a directory: as the exec's
// user (root) owns it, as docker cp does, unless $2 = keep (docker cp -a). -o is "don't
// keep owners" to both GNU and busybox tar.
const putScript = `[ -d "$1" ] || { echo "Could not find the file $1 in container" >&2; exit 44; }
command -v tar >/dev/null 2>&1 || { echo "copying needs tar in the image" >&2; exit 127; }
o=-o; [ "$2" = keep ] && o=
cd "$1" && exec tar -x $o -f -`

type pathStat struct {
	Name       string      `json:"name"`
	Size       int64       `json:"size"`
	Mode       fs.FileMode `json:"mode"`
	Mtime      time.Time   `json:"mtime"`
	LinkTarget string      `json:"linkTarget"`
}

// parseStat reads statScript's line for path p.
func parseStat(p, line string) (pathStat, error) {
	f := strings.SplitN(strings.TrimRight(line, "\n"), " ", 4)
	if len(f) < 3 {
		return pathStat{}, errors.New("stat: unexpected output " + strconv.Quote(line))
	}
	size, err1 := strconv.ParseInt(f[0], 10, 64)
	raw, err2 := strconv.ParseUint(f[1], 16, 32)
	mtime, err3 := strconv.ParseInt(f[2], 10, 64)
	if err := errors.Join(err1, err2, err3); err != nil {
		return pathStat{}, errors.New("stat: unexpected output " + strconv.Quote(line))
	}
	st := pathStat{Name: path.Base(p), Size: size, Mode: unixMode(uint32(raw)), Mtime: time.Unix(mtime, 0).UTC()}
	if len(f) == 4 {
		st.LinkTarget = f[3]
	}
	return st, nil
}

// unixMode is st_mode as Go's FileMode, which docker's stat header carries.
func unixMode(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	switch m & 0o170000 {
	case 0o040000:
		mode |= fs.ModeDir
	case 0o120000:
		mode |= fs.ModeSymlink
	case 0o010000:
		mode |= fs.ModeNamedPipe
	case 0o140000:
		mode |= fs.ModeSocket
	case 0o020000:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case 0o060000:
		mode |= fs.ModeDevice
	}
	if m&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if m&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if m&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

func setStatHeader(w http.ResponseWriter, st pathStat) {
	b, _ := json.Marshal(st)
	w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(b))
}

// archiveExec runs script in the container behind ref, mapping its errors: exit 44 is
// 404, no agent (for stdin) 501.
func (s *server) archiveExec(r *http.Request, ref string, argv []string, stdin io.Reader, stdout io.Writer) error {
	c, err := s.adapter.container(ref)
	if err != nil {
		return err
	}
	if c.Status != "running" {
		return apiErrorf(409, "Container %s is not running (copying goes through exec)", c.Name)
	}
	var errOut strings.Builder
	code, err := terminal.Exec(r.Context(), s.adapter.dsm, c.Name, argv, "", stdin, stdout, &errOut)
	msg := strings.TrimSpace(errOut.String())
	switch {
	case errors.Is(err, terminal.ErrNoAgent):
		return apiErrorf(501, "copying into %s needs synodock-agent, which couldn't run there "+
			"(no base64, or no /tmp it can run from)", c.Name)
	case err != nil:
		return err
	case code == 44:
		return apiErrorf(404, "%s", strings.Replace(msg, "in container", "in container "+c.Name, 1))
	case code != 0:
		if msg == "" {
			msg = "exit status " + strconv.Itoa(code)
		}
		return apiErrorf(500, "%s: %s", c.Name, msg)
	}
	return nil
}

func (s *server) archiveHead(w *response, r *http.Request, q query, p []string) error {
	pth := q.get("path", "")
	if pth == "" {
		return apiErrorf(400, "path is required")
	}
	var out strings.Builder
	if err := s.archiveExec(r, p[0], []string{"sh", "-c", statScript, "sh", pth}, nil, &out); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && w.status == 0 {
			w.WriteHeader(apiErr.status) // HEAD: no body
			return nil
		}
		return err
	}
	st, err := parseStat(pth, out.String())
	if err != nil {
		return err
	}
	setStatHeader(w, st)
	w.WriteHeader(200)
	return nil
}

func (s *server) archiveGet(w *response, r *http.Request, q query, p []string) error {
	pth := q.get("path", "")
	if pth == "" {
		return apiErrorf(400, "path is required")
	}
	// The stat line comes first, then the tar, in the one stream: headers, then body.
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		br := bufio.NewReader(pr)
		line, err := br.ReadString('\n')
		if err != nil {
			_, _ = io.Copy(io.Discard, br)
			done <- nil // the exec's error says why
			return
		}
		st, err := parseStat(pth, line)
		if err != nil {
			_, _ = io.Copy(io.Discard, br)
			done <- err
			return
		}
		setStatHeader(w, st)
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(200)
		_, err = io.Copy(w, br)
		_, _ = io.Copy(io.Discard, br)
		done <- err
	}()
	err := s.archiveExec(r, p[0], []string{"sh", "-c", statScript, "sh", pth, "tar"}, nil, pw)
	pw.CloseWithError(err)
	if cerr := <-done; err == nil {
		err = cerr
	}
	if err != nil && w.status == 0 {
		return err
	}
	return err
}

func (s *server) archivePut(w *response, r *http.Request, q query, p []string) error {
	pth := q.get("path", "")
	if pth == "" {
		return apiErrorf(400, "path is required")
	}
	defer r.Body.Close()
	owners := "drop"
	if q.flag("copyUIDGID") {
		owners = "keep"
	}
	if err := s.archiveExec(r, p[0], []string{"sh", "-c", putScript, "sh", pth, owners}, r.Body, io.Discard); err != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		return err
	}
	w.WriteHeader(200)
	return nil
}
