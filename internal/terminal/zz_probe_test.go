package terminal

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ryantenney/synodock/internal/dsm"
)

// A throwaway probe of the NAS relay: how a burst of numbered lines comes back.
func TestProbeRelay(t *testing.T) {
	if os.Getenv("SYNODOCK_PROBE") == "" {
		t.Skip("probe only")
	}
	cfg, err := dsm.LoadConfig(dsm.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	client := dsm.NewClient(context.Background(), cfg)
	if err := client.Login(""); err != nil {
		t.Fatal(err)
	}
	defer client.Logout()
	for _, size := range []int{100, 400, 1000, 3000} {
		// N lines of `size` bytes each, numbered, written in one burst.
		script := fmt.Sprintf(`stty -echo -opost raw 2>/dev/null; awk 'BEGIN { x = ""; for (k = 0; k < %d; k++) x = x "x"; for (i = 0; i < 60; i++) { printf "%%05d%%s\n", i, x; fflush() } }'; sleep 1`, size-6)
		s, err := Open(context.Background(), client, "iperf3", Join([]string{"sh", "-c", script}), 50, 200)
		if err != nil {
			t.Fatal(err)
		}
		var pieces []string
		for {
			text, err := s.Recv(context.Background(), 3*time.Second)
			if err != nil {
				break
			}
			pieces = append(pieces, text)
		}
		s.Close()
		all := strings.Join(pieces, "")
		lines := strings.Split(strings.TrimRight(all, "\n"), "\n")
		inOrder := sort.SliceIsSorted(lines, func(i, j int) bool { return lines[i] < lines[j] })
		var sizes []int
		whole := 0
		for _, p := range pieces {
			sizes = append(sizes, len(p))
			if strings.HasSuffix(p, "\n") {
				whole++
			}
		}
		sort.Ints(sizes)
		med := sizes[len(sizes)/2]
		// How often does a piece start at a line's start, and how many pieces hold exactly whole lines?
		starts, exact := 0, 0
		for _, p := range pieces {
			if len(p) >= 5 && strings.Trim(p[:5], "0123456789") == "" {
				starts++
				if strings.HasSuffix(p, "\n") {
					exact++
				}
			}
		}
		fmt.Printf("lines of %4d B: %3d pieces, size min %d / median %d / max %d; start on a line: %d, whole lines only: %d; in order: %v (%d lines)\n",
			size, len(pieces), sizes[0], med, sizes[len(sizes)-1], starts, exact, inOrder, len(lines))
	}
}
