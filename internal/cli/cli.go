// Package cli provides the minimal command-line entrypoint shared by all
// Strata binaries.
//
// Every binary supports the built-in "version" and "help" commands. A
// binary may register additional subcommands; any other invocation is a
// usage error.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/letthemplay/Strata/internal/version"
)

// Exit codes returned by Run.
const (
	ExitOK    = 0 // Command completed successfully.
	ExitError = 1 // Command failed at runtime.
	ExitUsage = 2 // Invalid or unsupported invocation.
)

// Subcommand is a binary-specific command.
type Subcommand struct {
	// Name is the word used to invoke the subcommand. It must not collide
	// with the built-in "version" or "help" commands.
	Name string
	// Usage is the argument synopsis shown after the name, e.g. "[--json]".
	Usage string
	// Summary is a one-line description shown in usage output.
	Summary string
	// Run executes the subcommand with the arguments following its name
	// and returns a process exit code.
	Run func(ctx context.Context, args []string, stdout, stderr io.Writer) int
}

// Command describes a Strata binary.
type Command struct {
	// Name is the executable name, e.g. "strata-agent".
	Name string
	// Summary is a one-line description shown in usage output.
	Summary string
	// Subcommands are the binary-specific commands.
	Subcommands []Subcommand
}

// Run executes the command with the given arguments (excluding the program
// name) and returns a process exit code. It performs no I/O other than
// writing to stdout and stderr and whatever the selected subcommand does.
func (c Command) Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
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
	}

	for _, sc := range c.Subcommands {
		if sc.Name == args[0] {
			return sc.Run(ctx, args[1:], stdout, stderr)
		}
	}
	return c.fail(stderr, fmt.Sprintf("unknown command %q", args[0]))
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
	type row struct{ synopsis, summary string }
	rows := make([]row, 0, len(c.Subcommands)+2)
	for _, sc := range c.Subcommands {
		rows = append(rows, row{strings.TrimSpace(sc.Name + " " + sc.Usage), sc.Summary})
	}
	rows = append(rows,
		row{"version [--json]", "Print build information"},
		row{"help", "Show this help"},
	)
	width := 0
	for _, r := range rows {
		width = max(width, len(r.synopsis))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s - %s\n\nUsage:\n  %s <command> [arguments]\n\nCommands:\n", c.Name, c.Summary, c.Name)
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, r.synopsis, r.summary)
	}
	_, _ = io.WriteString(w, b.String())
}
