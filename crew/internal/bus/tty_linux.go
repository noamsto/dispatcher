package bus

import (
	"syscall"
	"unsafe"
)

// IsTerminal reports whether fd is a terminal, by the TCGETS ioctl rather than
// a char-device check (/dev/null is a char device).
func IsTerminal(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
