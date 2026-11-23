package install

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// TerminalEcho returns a function that turns echo on f on or off, or nil
// if f is not a terminal.
func TerminalEcho(f *os.File) func(on bool) error {
	fd := f.Fd()
	if _, err := getTermios(fd); err != nil {
		return nil
	}
	return func(on bool) error {
		t, err := getTermios(fd)
		if err != nil {
			return err
		}
		if on {
			t.Lflag |= syscall.ECHO
		} else {
			t.Lflag &^= syscall.ECHO
		}
		return setTermios(fd, t)
	}
}

func getTermios(fd uintptr) (*syscall.Termios, error) {
	var t syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t))); errno != 0 { //nolint:gosec // Required by the ioctl ABI.
		return nil, fmt.Errorf("get terminal attributes: %w", errno)
	}
	return &t, nil
}

func setTermios(fd uintptr, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(t))); errno != 0 { //nolint:gosec // Required by the ioctl ABI.
		return fmt.Errorf("set terminal attributes: %w", errno)
	}
	return nil
}
