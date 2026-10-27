package activation

import "net"

// Listeners are the sockets systemd passed, or nil when it didn't start us.
// (name is launchd's; systemd's socket units pass theirs in order.)
func Listeners(name string) ([]net.Listener, error) { return systemdListeners() }
