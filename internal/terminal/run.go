package terminal

import (
	"context"
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
}

// Run is `synodock exec`: it returns the command's exit code (0 once started,
// with Detach).
func Run(ctx context.Context, client *dsm.Client, container string, argv []string, o RunOptions) (int, error) {
	rows, cols := terminalSize()
	if o.Detach || !(o.Stdin || o.TTY || o.Raw) {
		command := CommandFor(argv, Options{Raw: o.Raw, Detach: o.Detach, Gzip: o.Gzip})
		s, err := Open(ctx, client, container, command, rows, cols)
		if err != nil {
			return 0, err
		}
		defer s.Close()
		if o.Raw { // --raw -d: no framing, so no exit status either
			_, _, err := PumpUnframed(ctx, s, writeString, nil, false, false, true)
			return 0, err
		}
		return PumpFramed(ctx, s, func(b []byte) error {
			_, err := os.Stdout.Write(b)
			return err
		})
	}

	interactive := o.TTY && term.IsTerminal(int(os.Stdin.Fd()))
	stdin := o.Stdin || interactive
	if !interactive {
		fmt.Fprintln(os.Stderr, "synodock exec: unframed mode, output may arrive out of order")
	}
	command := CommandFor(argv, Options{Unframed: true, Interactive: interactive, Stdin: stdin, Raw: o.Raw})
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
