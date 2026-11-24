package agent

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

// listenFDsStart is the first file descriptor systemd passes (SD_LISTEN_FDS_START).
const listenFDsStart = 3

// SystemdListener returns the socket systemd passed to this process
// (socket activation), or nil if there is none. The variables are unset
// so that child processes do not inherit them.
func SystemdListener() (net.Listener, error) {
	defer func() {
		_ = os.Unsetenv("LISTEN_PID")
		_ = os.Unsetenv("LISTEN_FDS")
		_ = os.Unsetenv("LISTEN_FDNAMES")
	}()
	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return nil, nil //nolint:nilnil // Not socket-activated.
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n < 1 {
		return nil, nil //nolint:nilnil // Not socket-activated.
	}
	if n != 1 {
		return nil, fmt.Errorf("agent: expected one socket from systemd, got %d", n)
	}
	f := os.NewFile(uintptr(listenFDsStart), "strata-agent.socket")
	defer func() { _ = f.Close() }()
	l, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("agent: socket from systemd: %w", err)
	}
	return l, nil
}
