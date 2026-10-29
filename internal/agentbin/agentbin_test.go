package agentbin

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The embedded binaries were built from the agent's current sources (agent-rs/embed.sh
// rebuilds them and records the hash), so a change to agent-rs can't ship stale.
func TestBuiltFromCurrentSources(t *testing.T) {
	root := filepath.Join("..", "..")
	files := []string{"agent-rs/Cargo.toml", "agent-rs/Cargo.lock", "agent-rs/.cargo/config.toml"}
	src, err := filepath.Glob(filepath.Join(root, "agent-rs", "src", "*.rs"))
	if err != nil {
		t.Fatal(err)
	}
	var rs []string
	for _, f := range src {
		rel, _ := filepath.Rel(root, f)
		rs = append(rs, filepath.ToSlash(rel))
	}
	sort.Strings(rs)
	h := sha256.New()
	for _, f := range append(files, rs...) {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		h.Write([]byte(f + "\n"))
		h.Write(b)
	}
	want, err := os.ReadFile("sources.sha256")
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != strings.TrimSpace(string(want)) {
		t.Errorf("agent-rs changed since the embedded agent was built (%s, recorded %s): run agent-rs/embed.sh",
			got, strings.TrimSpace(string(want)))
	}
}

// Each is a static ELF for its arch, small enough to send in an exec's command (Linux
// takes up to 128 KiB in one argument, and the framing script is one).
func TestBinaries(t *testing.T) {
	for arch, machine := range map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64} {
		bin := map[string][]byte{"amd64": amd64, "arm64": arm64}[arch]
		f, err := elf.NewFile(bytes.NewReader(bin))
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if f.Machine != machine {
			t.Errorf("%s: machine %v", arch, f.Machine)
		}
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				t.Errorf("%s: dynamically linked", arch)
			}
		}
		if n := len(Base64(arch)); n > 100<<10 {
			t.Errorf("%s: %d bytes as base64, too close to the 128 KiB argument limit", arch, n)
		}
	}
	if Base64("riscv64") != "" {
		t.Error("an arch without a build")
	}
}
