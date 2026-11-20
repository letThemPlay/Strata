package agent

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/hardware"
)

// Defaults for the "run" subcommand.
const (
	// DefaultStateDir is the agent's state directory on a Strata host.
	DefaultStateDir = "/var/lib/strata"
	// DefaultInterval is the period between automatic discovery cycles.
	DefaultInterval = time.Minute
)

// Subcommands returns the strata-agent subcommands. root is the host
// filesystem used for discovery and reload delivers reload requests to
// the "run" subcommand.
func Subcommands(root fs.FS, reload <-chan os.Signal) []cli.Subcommand {
	return []cli.Subcommand{
		{
			Name:    "run",
			Usage:   "[--state-dir DIR] [--interval DURATION]",
			Summary: "Run the agent until terminated (discovery repeats every interval and on SIGHUP)",
			Run: func(ctx context.Context, args []string, _, stderr io.Writer) int {
				fset := flag.NewFlagSet("run", flag.ContinueOnError)
				fset.SetOutput(stderr)
				stateDir := fset.String("state-dir", DefaultStateDir, "directory for agent reports")
				interval := fset.Duration("interval", DefaultInterval, "period between discovery cycles (0 disables)")
				if err := fset.Parse(args); err != nil {
					if errors.Is(err, flag.ErrHelp) {
						return cli.ExitOK
					}
					return cli.ExitUsage
				}
				if fset.NArg() != 0 {
					_, _ = fmt.Fprintf(stderr, "run: unexpected arguments %q\n", fset.Args())
					return cli.ExitUsage
				}
				logger := slog.New(slog.NewJSONHandler(stderr, nil))
				err := Run(ctx, Options{
					StateDir: *stateDir,
					Root:     root,
					Reload:   reload,
					Interval: *interval,
					Logger:   logger,
				})
				if err != nil {
					logger.ErrorContext(ctx, "agent failed", "error", err)
					return cli.ExitError
				}
				return cli.ExitOK
			},
		},
		{
			Name:    "discover",
			Summary: "Print the hardware report as JSON and exit",
			Run: func(_ context.Context, args []string, stdout, stderr io.Writer) int {
				if len(args) != 0 {
					_, _ = fmt.Fprintln(stderr, "discover: takes no arguments")
					return cli.ExitUsage
				}
				report, err := hardware.Discover(root)
				if err != nil {
					_, _ = fmt.Fprintf(stderr, "discover: %v\n", err)
					return cli.ExitError
				}
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					_, _ = fmt.Fprintf(stderr, "discover: %v\n", err)
					return cli.ExitError
				}
				return cli.ExitOK
			},
		},
	}
}
