package activation

import (
	"net"
	"os"
	"syscall"
	"unsafe"
)

// launch_activate_socket and free from libSystem, called the way
// golang.org/x/sys/unix calls libc on darwin: no cgo, so synodock stays one
// static binary.

//go:cgo_import_dynamic libc_launch_activate_socket launch_activate_socket "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_free free "/usr/lib/libSystem.B.dylib"

var (
	libc_launch_activate_socket_trampoline_addr uintptr
	libc_free_trampoline_addr                   uintptr
)

//go:linkname syscall_syscall syscall.syscall
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

// Listeners are the sockets launchd opened for the job's Sockets entry name, or
// nil when launchd didn't start us.
func Listeners(name string) ([]net.Listener, error) {
	cname, err := syscall.BytePtrFromString(name)
	if err != nil {
		return nil, err
	}
	var fds *int32
	var count uintptr
	r, _, _ := syscall_syscall(libc_launch_activate_socket_trampoline_addr, uintptr(unsafe.Pointer(cname)),
		uintptr(unsafe.Pointer(&fds)), uintptr(unsafe.Pointer(&count)))
	switch syscall.Errno(r) {
	case 0:
	case syscall.ESRCH: // not a launchd job
		return nil, nil
	default:
		return nil, os.NewSyscallError("launch_activate_socket", syscall.Errno(r))
	}
	defer syscall_syscall(libc_free_trampoline_addr, uintptr(unsafe.Pointer(fds)), 0, 0)
	var listeners []net.Listener
	for _, fd := range unsafe.Slice(fds, count) {
		f := os.NewFile(uintptr(fd), name)
		l, err := net.FileListener(f)
		f.Close() // FileListener has its own copy
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, l)
	}
	return listeners, nil
}
