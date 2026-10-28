// synodock-agent runs a command inside a container for synodock exec, in place of the
// shell framing script, when it's in the container at /opt/synodock/agent.
//
// Author: Ryan Tenney
//
// The script hands over to it before doing anything else, so a container without it
// keeps working as before. It talks to synodock over its tty, through the NAS relay,
// in the records of package wire: several records in flight instead of one, base64
// instead of hex, and a CRC on each, since the relay cuts and reorders what it reads.
// With SD_GZIP=1 (synodock's default) and a go-ahead that says synodock can take it,
// each stream is one gzip stream, flushed after every read so slow output stays live.
//
//	synodock-agent run COMMAND [ARG]...
//
// Build it static for the NAS:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o synodock-agent ./cmd/synodock-agent
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/ryantenney/synodock/internal/wire"
)

// window is how many records may be on their way at once: about 32 KiB.
const window = 32

func main() {
	if len(os.Args) < 3 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: synodock-agent run COMMAND [ARG]...")
		os.Exit(2)
	}
	argv := os.Args[2:]
	if argv[0] == "--" {
		argv = argv[1:]
	}
	os.Exit(run(argv))
}

type chunk struct {
	stream int
	data   []byte
}

func run(argv []string) int {
	// Raw: no echo, no output processing, ^C a byte rather than a signal to us.
	if saved, err := term.MakeRaw(0); err == nil {
		defer term.Restore(0, saved)
	}
	tty := bufio.NewReader(os.Stdin)
	if _, err := io.WriteString(os.Stdout, wire.Hello); err != nil {
		return 1
	}
	goAhead, err := readLine(tty)
	if err != nil {
		return 1
	}
	compress := os.Getenv("SD_GZIP") == "1" && strings.Contains(goAhead, "z")

	s := &sender{}
	s.cond = sync.NewCond(&s.mu)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = commandEnv()
	stdout, err1 := cmd.StdoutPipe()
	stderr, err2 := cmd.StderrPipe()
	if err := errors.Join(err1, err2); err != nil {
		return s.fail(tty, err.Error(), 126)
	}
	if err := cmd.Start(); err != nil {
		code := 126
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			code = 127
		}
		return s.fail(tty, fmt.Sprintf("synodock exec: %s: %v\n", argv[0], err), code)
	}
	go s.readAcks(tty, cmd)

	readSize := wire.MaxPayload
	if compress {
		readSize = 16 << 10 // compresses to a few records
	}
	chunks := make(chan chunk, window)
	var readers sync.WaitGroup
	for stream, r := range map[int]io.Reader{1: stdout, 2: stderr} {
		readers.Add(1)
		go func() {
			defer readers.Done()
			buf := make([]byte, readSize)
			for {
				n, err := r.Read(buf) // what the pipe holds now: slow output stays live
				if n > 0 {
					chunks <- chunk{stream, append([]byte(nil), buf[:n]...)}
				}
				if err != nil {
					return
				}
			}
		}()
	}
	go func() { readers.Wait(); close(chunks) }()
	if !compress {
		for c := range chunks {
			s.send(wire.Record{Kind: 'D', Stream: c.stream, Payload: c.data})
		}
		return s.exit(status(cmd.Wait()))
	}
	z := map[int]*zstream{}
	for c := range chunks {
		if z[c.stream] == nil {
			z[c.stream] = newZstream()
		}
		s.sendZ(c.stream, z[c.stream].flush(c.data))
	}
	for stream, zs := range z {
		s.sendZ(stream, zs.close()) // the trailer: its CRC checks the whole stream
	}
	return s.exit(status(cmd.Wait()))
}

// zstream is one output stream's gzip stream.
type zstream struct {
	buf bytes.Buffer
	w   *gzip.Writer
}

func newZstream() *zstream {
	z := &zstream{}
	z.w, _ = gzip.NewWriterLevel(&z.buf, gzip.BestSpeed)
	return z
}

// flush compresses data and returns everything needed to decompress it.
func (z *zstream) flush(data []byte) []byte {
	_, _ = z.w.Write(data)
	_ = z.w.Flush()
	return z.take()
}

func (z *zstream) close() []byte {
	_ = z.w.Close()
	return z.take()
}

func (z *zstream) take() []byte {
	out := bytes.Clone(z.buf.Bytes())
	z.buf.Reset()
	return out
}

// sendZ sends compressed bytes as C records.
func (s *sender) sendZ(stream int, data []byte) {
	for len(data) > 0 {
		n := min(len(data), wire.MaxPayload)
		s.send(wire.Record{Kind: 'C', Stream: stream, Payload: data[:n]})
		data = data[n:]
	}
}

// commandEnv is ours without the framing script's switches.
func commandEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SD_GZIP=") && !strings.HasPrefix(kv, "SD_AGENT=") {
			env = append(env, kv)
		}
	}
	return env
}

// sender numbers records and keeps no more than window of them unacknowledged.
type sender struct {
	mu    sync.Mutex
	cond  *sync.Cond
	seq   int // the next record's
	acked int // every record below this has arrived
}

func (s *sender) send(r wire.Record) {
	s.mu.Lock()
	for s.seq-s.acked >= window {
		s.cond.Wait()
	}
	r.Seq = s.seq
	s.seq++
	s.mu.Unlock()
	_, _ = io.WriteString(os.Stdout, wire.Encode(r)) // one write per record
}

// exit sends the exit status and waits (a while) for it to arrive, so the tty isn't
// closed under it.
func (s *sender) exit(code int) int {
	s.send(wire.Record{Kind: 'X', Payload: []byte(strconv.Itoa(code))})
	done := make(chan struct{})
	go func() {
		s.mu.Lock()
		for s.acked < s.seq {
			s.cond.Wait()
		}
		s.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
	return 0
}

// fail reports a command that didn't start, as its stderr and exit status.
func (s *sender) fail(tty *bufio.Reader, msg string, code int) int {
	go s.readAcks(tty, nil)
	s.send(wire.Record{Kind: 'D', Stream: 2, Payload: []byte(msg)})
	return s.exit(code)
}

// readAcks takes synodock's "A<n>" lines; a ^C byte interrupts the command.
func (s *sender) readAcks(tty *bufio.Reader, cmd *exec.Cmd) {
	for {
		line, err := readLine(tty)
		for _, b := range []byte(line) {
			if b == 3 && cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Signal(os.Interrupt)
			}
		}
		if len(line) > 1 && line[0] == 'A' {
			if n, err := strconv.Atoi(line[1:]); err == nil {
				s.mu.Lock()
				if n > s.acked {
					s.acked = n
				}
				s.cond.Broadcast()
				s.mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

// readLine reads to a newline or carriage return, without it.
func readLine(r *bufio.Reader) (string, error) {
	var b []byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			return string(b), err
		}
		if c == '\n' || c == '\r' {
			return string(b), nil
		}
		if c == 3 { // ^C: hand it on now, without waiting for a newline
			return string(append(b, c)), nil
		}
		b = append(b, c)
	}
}

// status is the command's exit status as a shell reports it.
func status(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exitErr.ExitCode()
	}
	if err != nil {
		return 126
	}
	return 0
}
