package dsm

import "strings"

// CPUArch is the NAS's CPU architecture as Go names it (amd64 or arm64), from DSM's
// system info. That names the CPU's vendor rather than its architecture: Synology's x86
// models are AMD or Intel, the rest ARM (Realtek, Annapurna Labs, ...). Asked once.
func (c *Client) CPUArch() (string, error) {
	c.mu.Lock()
	arch := c.arch
	c.mu.Unlock()
	if arch != "" {
		return arch, nil
	}
	var info struct {
		Vendor string `json:"cpu_vendor"`
	}
	if err := c.CallInto(&info, "SYNO.Core.System", "info", 1, nil); err != nil {
		return "", err
	}
	arch = "arm64"
	switch strings.ToLower(info.Vendor) {
	case "amd", "intel":
		arch = "amd64"
	}
	c.mu.Lock()
	c.arch = arch
	c.mu.Unlock()
	return arch, nil
}
