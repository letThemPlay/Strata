package install

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// execCommand builds the process for c. The program and its arguments are
// fixed by the installer or derived from validated answers and store
// paths it built itself; they are never passed through a shell.
func execCommand(ctx context.Context, c Command) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...) //nolint:gosec // See function comment.
	cmd.Dir = c.Dir
	return cmd
}

// HashPassword hashes password with mkpasswd (yescrypt), passing it on
// standard input so that it never appears in a process listing.
func HashPassword(ctx context.Context, password string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutQuick)
	defer cancel()
	cmd := exec.CommandContext(ctx, "mkpasswd", "--method=yescrypt", "--stdin")
	cmd.Stdin = strings.NewReader(password + "\n")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("mkpasswd: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
