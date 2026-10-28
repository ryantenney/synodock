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
		return dsm.Errorf("not a terminal; pass -f to delete %s", name)
	}
	fmt.Fprintf(os.Stderr, "%s Type %s to confirm: ", warning, name)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(answer) != name {
		return dsm.Errorf("not deleted")
	}
	return nil
}
