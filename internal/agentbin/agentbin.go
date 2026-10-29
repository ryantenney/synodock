// Package agentbin holds synodock-agent (agent-rs), built static for each arch, to send
// with an exec into a container that doesn't have it mounted. agent-rs/embed.sh
// rebuilds it.
package agentbin

import (
	_ "embed"
	"encoding/base64"
	"sync"
)

var (
	//go:embed synodock-agent-amd64
	amd64 []byte
	//go:embed synodock-agent-arm64
	arm64 []byte
)

var (
	mu      sync.Mutex
	encoded = map[string]string{}
)

// Base64 is the agent for arch (amd64 or arm64), base64-encoded; "" for another arch.
func Base64(arch string) string {
	var bin []byte
	switch arch {
	case "amd64":
		bin = amd64
	case "arm64":
		bin = arm64
	default:
		return ""
	}
	mu.Lock()
	defer mu.Unlock()
	if encoded[arch] == "" {
		encoded[arch] = base64.StdEncoding.EncodeToString(bin)
	}
	return encoded[arch]
}

// Uname is what uname -m says on arch.
func Uname(arch string) string {
	switch arch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return ""
}
