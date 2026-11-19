// Package cli provides the minimal command-line entrypoint shared by all
// Strata binaries during milestone M0.
//
// Each binary only supports reporting its version and usage. Functional
// commands are introduced in later milestones; until then any other
// invocation is reported as unsupported with a usage error.
package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/letthemplay/Strata/internal/version"
)

// Exit codes returned by Run.
const (
	ExitOK    = 0 // Command completed successfully.
	ExitError = 1 // Command failed at runtime (e.g. output could not be written).
	ExitUsage = 2 // Invalid or unsupported invocation.
)

// Command describes a Strata binary.
type Command struct {
	// Name is the executable name, e.g. "strata-agent".
	Name string
	// Summary is a one-line description shown in usage output.
	Summary string
}

// Run executes the command with the given arguments (excluding the program
// name) and returns a process exit code. It performs no I/O other than
// writing to stdout and stderr, so it can be exercised directly in tests.
func (c Command) Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		c.usage(stderr)
		return ExitUsage
	}

	switch args[0] {
	case "help", "-h", "--help":
		if len(args) > 1 {
			return c.fail(stderr, "help takes no arguments")
		}
		c.usage(stdout)
		return ExitOK
	case "version", "--version":
		return c.version(args[1:], stdout, stderr)
	default:
		return c.fail(stderr, fmt.Sprintf("unknown command %q", args[0]))
	}
}

func (c Command) version(args []string, stdout, stderr io.Writer) int {
	info := version.Get()

	switch {
	case len(args) == 0:
		if _, err := fmt.Fprintf(stdout, "%s %s\n", c.Name, info); err != nil {
			return ExitError
		}
		return ExitOK
	case len(args) == 1 && args[0] == "--json":
		enc := json.NewEncoder(stdout)
		if err := enc.Encode(struct {
			Name string `json:"name"`
			version.Info
		}{c.Name, info}); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: encode version: %v\n", c.Name, err)
			return ExitError
		}
		return ExitOK
	default:
		return c.fail(stderr, "usage: version [--json]")
	}
}

func (c Command) fail(stderr io.Writer, msg string) int {
	_, _ = fmt.Fprintf(stderr, "%s: %s\n\n", c.Name, msg)
	c.usage(stderr)
	return ExitUsage
}

func (c Command) usage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `%s - %s

Usage:
  %s <command> [arguments]

Commands:
  version [--json]  Print build information
  help              Show this help

Status: milestone M0 (foundation). No infrastructure functionality is
implemented yet.
`, c.Name, c.Summary, c.Name)
}
