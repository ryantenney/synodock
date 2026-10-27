//go:build !darwin && !linux

package activation

import "net"

func Listeners(name string) ([]net.Listener, error) { return nil, nil }
