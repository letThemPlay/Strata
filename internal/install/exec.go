package install

import (
	"context"
	"os/exec"
)

// execCommand builds the process for c. The program and its arguments are
// fixed by the installer or derived from validated answers and store
// paths it built itself; they are never passed through a shell.
func execCommand(ctx context.Context, c Command) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...) //nolint:gosec // See function comment.
	cmd.Dir = c.Dir
	return cmd
}
