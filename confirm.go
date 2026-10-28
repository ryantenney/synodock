package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/ryantenney/synodock/internal/dsm"
)

// confirmTyped asks for name typed back before a delete nothing brings back. Without a
// terminal to ask on, it refuses: the command's -f is how a script says yes.
func confirmTyped(warning, name string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return dsm.Errorf("not a terminal; pass -f to go ahead with %s", name)
	}
	fmt.Fprintf(os.Stderr, "%s Type %s to confirm: ", warning, name)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(answer) != name {
		return dsm.Errorf("not deleted")
	}
	return nil
}

// writePrivate writes data to path readable only by this user, an existing file included:
// os.WriteFile would keep an older file's mode, and exports can hold secrets.
func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
