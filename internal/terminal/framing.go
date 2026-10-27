package terminal

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ryantenney/synodock/internal/dsm"
)

const (
	recordBytes  = 1024             // 2 KiB of hex: one pty read in practice, so rarely split
	stallTimeout = 10 * time.Second // half a record and nothing more for this long: give up
)

// Records: \x1e<kind> <seq> <n> <hex>\n. D = n bytes of output, E = end of output
// (seq = number of D records), S = exit status n, Z = each chunk that follows is
// its own gzip member (sent first, when the image has gzip and it wasn't turned off).
var record = regexp.MustCompile(`^\x1e([DESZ]) (\d+) (\d+) ([0-9a-f]*)\r?\n$`)

// A "." line from the chunk loop ends a chunk: send what's held, however little.
const awkProgram = `function ack() { fflush(); if ((getline line < "/dev/tty") <= 0) exit 1 }
function emit(n) { printf "\036D %d %d %s\n", seq++, n, substr(buf, 1, 2 * n); buf = substr(buf, 2 * n + 1); ack() }
$0 == "." { if (buf != "") emit(length(buf) / 2); next }
{ gsub(/[^0-9a-f]/, ""); buf = buf $0; while (length(buf) >= 2 * B) emit(B) }
END { if (buf != "") emit(length(buf) / 2); printf "\036E %d 0 \n", seq; ack() }`

// The output is cut into chunks by dd, which takes what the pipe holds right now
// (one read, up to 64 KiB), and each chunk goes through its own gzip and od, so
// nothing waits on a buffer: a line a second arrives a second apart, and bulk output
// still compresses in 64 KiB pieces. mawk reads pipes in blocks unless -W interactive;
// busybox awk ignores that with a warning, so it's only passed when awk is silent.
// The tools' stderr stays off the tty: one stray line there jams the framing.
var framed = `stty -echo -opost 2>/dev/null
if ! command -v od >/dev/null || ! command -v awk >/dev/null || ! command -v dd >/dev/null; then
  echo "synodock exec: the container has no od, awk or dd (use --raw)"; exit 127; fi
case $(printf x | LC_ALL=C dd bs=1 count=1 2>&1 >/dev/null) in *"records in"*) ;; *)
  echo "synodock exec: this dd doesn't report what it read, so the end of output can't be seen (use --raw)"; exit 127;; esac
Z=cat
if [ "$SD_GZIP" = 1 ] && command -v gzip >/dev/null; then Z='gzip -1'; printf '\036Z 0 1 \n'; read -r line </dev/tty; fi
AI=
if [ -z "$(awk -W interactive 'BEGIN {}' </dev/null 2>&1)" ]; then AI='-W interactive'; fi
exec 4>&1
s=$( { { "$@" </dev/null 2>&1 3>&- 4>&-; echo $? >&3; } | {
       while :; do
         st=$( { LC_ALL=C dd bs=65536 count=1 2>&3 | $Z 2>/dev/null | od -An -v -tx1 2>/dev/null >&5; } 3>&1 )
         echo .
         case $st in "0+0 records in"*) break;; esac
       done 5>&1
     } | awk $AI -v B=` + strconv.Itoa(recordBytes) + ` '` + awkProgram + `' 2>/dev/null >&4; } 3>&1 )
printf '\036S 0 %d \n' "$s"
read -r line </dev/tty`

// -d: start the command in the background, out of the exec's session (setsid where
// the image has it, and SIGHUP ignored either way, so the exec ending doesn't take
// it down), output discarded, as docker exec -d does.
const detach = `command -v "$1" >/dev/null 2>&1 || { echo "synodock exec: $1: not found"; exit 127; }
trap '' HUP
if command -v setsid >/dev/null 2>&1; then setsid "$@" </dev/null >/dev/null 2>&1 &
else "$@" </dev/null >/dev/null 2>&1 &
fi`

// Unframed: the exit status is a marker after the output (it can be overtaken too).
const (
	marker = "\x1e__dsm_exit="
	ready  = "\x1e__dsm_ready\n"
)

var markerRE = regexp.MustCompile(regexp.QuoteMeta(marker) + `(\d+)\r?\n?`)

// Join is shell quoting, but of every argument: DSM drops everything from a bare
// "=" on.
func Join(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
	}
	return strings.Join(quoted, " ")
}

func printfCommand(text string) string {
	return "printf '" + strings.NewReplacer("\x1e", `\036`, "\n", `\n`).Replace(text) + "'"
}

// wrap is unframed: argv under sh, ending with the exit-code marker. Without a TTY
// on our side, turn off echo (piped stdin would come back in the output) and output
// post-processing (\n -> \r\n); with stdin, say when that's done, so input isn't
// sent (and echoed) before.
func wrap(argv []string, interactive, stdin bool) string {
	setup := ""
	if !interactive {
		setup = "stty -echo -opost 2>/dev/null; "
		if stdin {
			setup += printfCommand(ready) + "; "
		}
	}
	script := setup + `"$@"; ` + printfCommand(marker+"%d\n") + " $?"
	return Join(append([]string{"sh", "-c", script, "sh"}, argv...))
}

// Options pick how a command runs; the zero value is framed.
type Options struct {
	Unframed    bool // pass output through as it comes (needed for stdin)
	Interactive bool // unframed, with a terminal on our side
	Stdin       bool // unframed, input to come
	Raw         bool // no sh around it, no exit status
	Detach      bool
	Gzip        bool // framed: compress each chunk in the container
}

// CommandFor is the command line for Terminal's "create".
func CommandFor(argv []string, o Options) string {
	if o.Detach {
		argv = append([]string{"sh", "-c", detach, "sh"}, argv...)
	}
	if o.Raw {
		return Join(argv)
	}
	if !o.Unframed {
		// env, not an sh variable: the framing script reads SD_GZIP; "$@" is argv.
		gz := "SD_GZIP=0"
		if o.Gzip {
			gz = "SD_GZIP=1"
		}
		return Join(append([]string{"env", gz, "sh", "-c", framed, "sh"}, argv...))
	}
	return wrap(argv, o.Interactive, o.Stdin)
}

// GzipDefault says whether to compress framed output: on unless SYNODOCK_GZIP=0.
// Each chunk is its own gzip member, so it costs no latency; it costs ~0.2s on
// 256 KB that won't compress.
func GzipDefault() bool { return os.Getenv("SYNODOCK_GZIP") != "0" }

// exitFilter is unframed: pass output through, holding back anything that could
// be the start of the marker, and pull the exit code out once it arrives.
type exitFilter struct {
	pending string
	code    int
	known   bool
}

func (f *exitFilter) feed(text string) string {
	if f.known {
		return text // out of order behind the marker; still output
	}
	buf := f.pending + text
	if m := markerRE.FindStringSubmatchIndex(buf); m != nil && (m[1] < len(buf) || strings.HasSuffix(buf, "\n")) {
		f.code, _ = strconv.Atoi(buf[m[2]:m[3]])
		f.known = true
		f.pending = ""
		return buf[:m[0]] + buf[m[1]:]
	}
	i := strings.Index(buf, marker)
	if i < 0 {
		i = len(buf)
		for k := min(len(marker)-1, len(buf)); k > 0; k-- {
			if strings.HasPrefix(marker, buf[len(buf)-k:]) {
				i = len(buf) - k
				break
			}
		}
	}
	f.pending = buf[i:]
	return buf[:i]
}

func (f *exitFilter) flush() string {
	out := f.pending
	f.pending = ""
	if m := markerRE.FindStringSubmatchIndex(out); m != nil && !f.known {
		f.code, _ = strconv.Atoi(out[m[2]:m[3]])
		f.known = true
		return out[:m[0]] + out[m[1]:]
	}
	return out
}

type frame struct {
	kind byte
	n    int
	data []byte
}

// deframer is framed: it turns pty reads back into records. Only one record is
// ever in flight, so the pieces on hand all belong to it, in some order.
type deframer struct {
	pieces    []string
	seq       int
	status    int
	hasStatus bool
}

const maxPieces = 4

// feed returns the record once it's complete, else nil.
func (d *deframer) feed(text string) (*frame, error) {
	d.pieces = append(d.pieces, text)
	if len(d.pieces) > maxPieces {
		return nil, dsm.Errorf("exec output didn't decode (not from the framing script?): %q", truncate(d.leftover(), 300))
	}
	var m []string
	permute(d.pieces, func(order []string) bool {
		m = record.FindStringSubmatch(strings.Join(order, ""))
		return m != nil
	})
	if m == nil {
		return nil, nil
	}
	d.pieces = nil
	kind := m[1][0]
	seq, _ := strconv.Atoi(m[2])
	n, _ := strconv.Atoi(m[3])
	switch kind {
	case 'S':
		d.status, d.hasStatus = n, true
		return &frame{kind: kind, n: n}, nil
	case 'Z':
		return &frame{kind: kind}, nil
	}
	if seq != d.seq {
		return nil, dsm.Errorf("exec output record %d arrived, expected %d", seq, d.seq)
	}
	if kind == 'E' {
		return &frame{kind: kind}, nil
	}
	if len(m[4]) != 2*n {
		return nil, dsm.Errorf("exec output record %d has %d bytes, said %d", seq, len(m[4])/2, n)
	}
	data, err := hex.DecodeString(m[4])
	if err != nil {
		return nil, err
	}
	d.seq++
	return &frame{kind: kind, n: n, data: data}, nil
}

func (d *deframer) leftover() string { return strings.Join(d.pieces, "") }

// permute calls try with each ordering of items, in itertools.permutations order,
// until it returns true.
func permute(items []string, try func([]string) bool) bool {
	used := make([]bool, len(items))
	order := make([]string, 0, len(items))
	var walk func() bool
	walk = func() bool {
		if len(order) == len(items) {
			return try(order)
		}
		for i, item := range items {
			if used[i] {
				continue
			}
			used[i] = true
			order = append(order, item)
			if walk() {
				return true
			}
			order = order[:len(order)-1]
			used[i] = false
		}
		return false
	}
	return walk()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

var errAborted = errors.New("aborted")

// gunzip turns gzip members back to back (one per chunk) into the bytes they hold.
// gzip.Reader in multistream mode reads the next member's header before handing
// over the end of the last one, which on a pipe would hold each chunk's tail back
// until the next chunk: so one member at a time, in a goroutine fed through a pipe.
type gunzip struct {
	pw   *io.PipeWriter
	done chan error
}

func newGunzip(write func([]byte) error) *gunzip {
	pr, pw := io.Pipe()
	g := &gunzip{pw: pw, done: make(chan error, 1)}
	go func() {
		err := inflate(pr, write)
		if err != nil {
			pr.CloseWithError(err)
		} else {
			pr.CloseWithError(errAborted)
		}
		g.done <- err
	}()
	return g
}

func inflate(r io.Reader, write func([]byte) error) error {
	br := bufio.NewReader(r)
	zr, err := gzip.NewReader(br)
	if err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	buf := make([]byte, 64<<10)
	for {
		zr.Multistream(false)
		for {
			n, err := zr.Read(buf)
			if n > 0 {
				if werr := write(buf[:n]); werr != nil {
					return werr
				}
			}
			if err == io.EOF {
				break
			} else if err != nil {
				return err
			}
		}
		if err := zr.Reset(br); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
	}
}

func (g *gunzip) feed(data []byte) error {
	_, err := g.pw.Write(data)
	return err
}

// finish waits for the output to be written; io.ErrUnexpectedEOF means it ended
// partway through a member.
func (g *gunzip) finish() error {
	g.pw.Close()
	return <-g.done
}

func (g *gunzip) abort() {
	g.pw.CloseWithError(errAborted)
	<-g.done
}

// PumpFramed writes the output in order and returns the exit status. Cancelling
// ctx sends ^C and returns ctx's error.
func PumpFramed(ctx context.Context, s *Session, write func([]byte) error) (int, error) {
	d := &deframer{}
	var gz *gunzip
	defer func() {
		if gz != nil {
			gz.abort()
		}
	}()
	ended := false
	last := time.Now()
	for {
		text, err := s.Recv(ctx, stallTimeout)
		if errors.Is(err, errQuiet) {
			// Quiet is fine (the command may sleep), but not halfway through a
			// record: something that isn't a record got in, and nothing more is
			// coming until we ack, so both ends would wait forever.
			if len(d.pieces) > 0 && time.Since(last) >= stallTimeout {
				return 0, dsm.Errorf("%s: exec output stopped mid-record: %q", s.Container, truncate(d.leftover(), 300))
			}
			continue
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = s.Send("\x03") // stop the remote command too; Close deletes the exec
			return 0, err
		}
		last = time.Now()
		rec, err := d.feed(text)
		if err != nil {
			return 0, err
		}
		if rec == nil {
			continue
		}
		switch rec.kind {
		case 'Z':
			gz = newGunzip(write)
		case 'D':
			if gz != nil {
				err = gz.feed(rec.data)
			} else {
				err = write(rec.data)
			}
		case 'E':
			ended = true
			if gz != nil {
				err = gz.finish()
				gz = nil
				if err == io.ErrUnexpectedEOF {
					err = dsm.Errorf("%s: exec output ended mid-gzip stream", s.Container)
				}
			}
		case 'S':
			if !ended {
				err = dsm.Errorf("%s: the framing in the container failed (output incomplete); exit status was %d",
					s.Container, rec.n)
			}
		}
		if err != nil {
			return 0, err
		}
		if err := s.Send("\n"); err != nil {
			return 0, err
		}
	}
	if err := s.checkClose(); err != nil {
		return 0, err
	}
	if !d.hasStatus {
		if left := strings.TrimSpace(d.leftover()); left != "" {
			return 0, dsm.Errorf("%s: exec ended without an exit status: %s", s.Container, left)
		}
		return 0, dsm.Errorf("%s: exec ended without an exit status", s.Container)
	}
	return d.status, nil
}

// PumpUnframed writes output as it comes. stdin (if not nil) is sent from a
// goroutine, after wrap's ready line if waitReady, then ^D if sendEOF. Returns the
// exit code, with known false if it can't be known.
func PumpUnframed(ctx context.Context, s *Session, write func(string) error, stdin io.Reader,
	waitReady, sendEOF, raw bool) (code int, known bool, err error) {
	var filt *exitFilter
	if !raw {
		filt = &exitFilter{}
	}
	filter := func(text string) string {
		if filt == nil {
			return text
		}
		return filt.feed(text)
	}
	readyCh := make(chan struct{})
	isReady := !waitReady
	if isReady {
		close(readyCh)
	}
	stop := make(chan struct{})
	defer close(stop)
	if stdin != nil {
		go func() {
			select {
			case <-readyCh:
			case <-stop:
				return
			}
			last := byte('\n')
			err := textChunks(stdin, func(text string) error {
				last = text[len(text)-1]
				return s.Send(text)
			})
			if err == nil && sendEOF {
				// Canonical mode: ^D is EOF at the start of a line; mid-line it only
				// hands over the partial line, so that needs a second one.
				if last == '\n' {
					_ = s.Send("\x04")
				} else {
					_ = s.Send("\x04\x04")
				}
			}
		}()
	}
	head := ""
	for {
		text, err := s.Recv(ctx, 0)
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = s.Send("\x03")
			return 0, false, err
		}
		if !isReady {
			head += text
			if !strings.Contains(head, ready) {
				continue
			}
			text = strings.Replace(head, ready, "", 1)
			isReady = true
			close(readyCh)
		}
		if err := write(filter(text)); err != nil {
			return 0, false, err
		}
	}
	if !isReady { // ended before saying ready (no sh or no stty?)
		if err := write(filter(head)); err != nil {
			return 0, false, err
		}
	}
	if filt != nil {
		if err := write(filt.flush()); err != nil {
			return 0, false, err
		}
	}
	if err := s.checkClose(); err != nil {
		return 0, false, err
	}
	if filt == nil {
		return 0, false, nil
	}
	return filt.code, filt.known, nil
}

// textChunks reads r into UTF-8 text, holding back a sequence cut off at the end
// of a read until the next. nil at EOF.
func textChunks(r io.Reader, send func(string) error) error {
	buf := make([]byte, 4096)
	var pending []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := append(pending, buf[:n]...)
			cut := completeUpTo(data)
			pending = append([]byte(nil), data[cut:]...)
			if text := strings.ToValidUTF8(string(data[:cut]), "�"); text != "" {
				if err := send(text); err != nil {
					return err
				}
			}
		}
		if err == io.EOF {
			if len(pending) > 0 {
				return send(strings.ToValidUTF8(string(pending), "�"))
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// completeUpTo is where an incomplete UTF-8 sequence at the end of data begins
// (len(data) if there is none).
func completeUpTo(data []byte) int {
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				return i
			}
			break
		}
	}
	return len(data)
}
