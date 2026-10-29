package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/ryantenney/synodock/internal/dsm"
)

// RunOptions are `synodock exec`'s flags.
type RunOptions struct {
	Stdin, TTY, Raw, Detach bool
	Gzip                    bool
	User                    string
}

// Run is `synodock exec`: it returns the command's exit code (0 once started,
// with Detach).
func Run(ctx context.Context, client *dsm.Client, container string, argv []string, o RunOptions) (int, error) {
	if o.Raw && o.User != "" {
		return 0, dsm.Errorf("--raw runs the command without a shell, so it can't take -u")
	}
	rows, cols := terminalSize()
	if o.Detach || !(o.Stdin || o.TTY || o.Raw) {
		command := CommandFor(argv, Options{Raw: o.Raw, Detach: o.Detach, Gzip: o.Gzip, User: o.User,
			AgentArch: SendAgentFor(client)})
		s, err := Open(ctx, client, container, command, rows, cols)
		if err != nil {
			return 0, err
		}
		defer s.Close()
		if o.Raw { // --raw -d: no framing, so no exit status either
			_, _, err := PumpUnframed(ctx, s, writeString, nil, false, false, true)
			return 0, err
		}
		return PumpFramed(ctx, s, writeStream)
	}

	interactive := o.TTY && term.IsTerminal(int(os.Stdin.Fd()))
	if o.Stdin && !interactive && !o.Raw && AgentDefault() {
		// Piped stdin goes framed, through synodock-agent: in order, whole, and paced to
		// the command. Without an agent the command hasn't run; it goes unframed below.
		code, err := runFramedStdin(ctx, client, container, argv, o, rows, cols)
		if !errors.Is(err, ErrNoAgent) {
			return code, err
		}
	}
	stdin := o.Stdin || interactive
	if !interactive {
		fmt.Fprintln(os.Stderr, "synodock exec: unframed mode, output may arrive out of order")
	}
	command := CommandFor(argv, Options{Unframed: true, Interactive: interactive, Stdin: stdin, Raw: o.Raw, User: o.User})
	var input io.Reader
	if stdin {
		input = os.Stdin
	}
	s, err := Open(ctx, client, container, command, rows, cols)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	if interactive {
		fd := int(os.Stdin.Fd())
		saved, err := term.MakeRaw(fd)
		if err != nil {
			return 0, err
		}
		defer term.Restore(fd, saved)
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				_ = s.Resize(terminalSize())
			}
		}()
	}
	code, known, err := PumpUnframed(ctx, s, writeString, input, stdin && !interactive && !o.Raw, !interactive, o.Raw)
	if err != nil || o.Raw {
		return 0, err
	}
	if !known {
		fmt.Fprintf(os.Stderr, "synodock exec: %s: no exit status (no sh in the image? try --raw)\n", container)
		return 255, nil
	}
	return code, nil
}

// Exec runs argv in container, framed, with output to stdout and stderr, and returns its
// exit status. stdin (if not nil) goes to it through synodock-agent (ErrNoAgent where
// there isn't one; the command hasn't run). user is -u's, or "".
func Exec(ctx context.Context, client *dsm.Client, container string, argv []string, user string,
	stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	o := Options{Gzip: GzipDefault(), User: user, AgentArch: SendAgentFor(client), FramedStdin: stdin != nil}
	s, err := Open(ctx, client, container, CommandFor(argv, o), 50, 200)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	return PumpFramedInput(ctx, s, stdin, func(stream int, b []byte) error {
		w := stdout
		if stream == Stderr {
			w = stderr
		}
		_, err := w.Write(b)
		return err
	})
}

func runFramedStdin(ctx context.Context, client *dsm.Client, container string, argv []string, o RunOptions, rows, cols int) (int, error) {
	command := CommandFor(argv, Options{Gzip: o.Gzip, User: o.User, AgentArch: SendAgentFor(client), FramedStdin: true})
	s, err := Open(ctx, client, container, command, rows, cols)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	return PumpFramedInput(ctx, s, os.Stdin, writeStream)
}

// writeStream writes framed output to our stdout or stderr, as it came.
func writeStream(stream int, b []byte) error {
	out := os.Stdout
	if stream == Stderr {
		out = os.Stderr
	}
	_, err := out.Write(b)
	return err
}

func writeString(text string) error {
	_, err := io.WriteString(os.Stdout, text)
	return err
}

// terminalSize is (rows, cols) of our terminal, else 50x200.
func terminalSize() (int, int) {
	for _, f := range []*os.File{os.Stdout, os.Stdin} {
		if cols, rows, err := term.GetSize(int(f.Fd())); err == nil && cols > 0 && rows > 0 {
			return rows, cols
		}
	}
	return 50, 200
}
