package install

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"

	"github.com/letthemplay/Strata/internal/cli"
)

// Subcommands returns the strata-install subcommands. root is the running
// system's "/" used for probing, stdin is read for the erase confirmation
// and runner executes external programs.
func Subcommands(root fs.FS, stdin io.Reader, runner Runner) []cli.Subcommand {
	return []cli.Subcommand{
		{
			Name:    "plan",
			Usage:   "ANSWER-FILE",
			Summary: "Check an answer file against this machine and show what apply would do (changes nothing)",
			Run: func(_ context.Context, args []string, stdout, stderr io.Writer) int {
				fset := flag.NewFlagSet("plan", flag.ContinueOnError)
				fset.SetOutput(stderr)
				answers, code, ok := parseArgs(fset, args, stderr)
				if !ok {
					return code
				}
				in := &Installer{Answers: answers, Root: root}
				if err := in.Plan(stdout); err != nil {
					_, _ = fmt.Fprintf(stderr, "\nplan: %v\n", err)
					return cli.ExitError
				}
				return cli.ExitOK
			},
		},
		{
			Name:    "apply",
			Usage:   "[--confirm-wipe DEVICE] [--extra-module FILE] [--work-dir DIR] ANSWER-FILE",
			Summary: "Install Strata as described by the answer file (erases the disk after confirmation)",
			Run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
				fset := flag.NewFlagSet("apply", flag.ContinueOnError)
				fset.SetOutput(stderr)
				confirm := fset.String("confirm-wipe", "", "confirm erasing `DEVICE` without a prompt; must equal disk.device")
				extra := fset.String("extra-module", "", "NixOS module `FILE` to add to the host configuration as extra-module.nix")
				workDir := fset.String("work-dir", "", "empty `DIR` for the generated configuration (default: a new temporary directory)")
				sourceDir := fset.String("source-dir", DefaultSourceDir, "`DIR` with the host template and flake.lock")
				target := fset.String("target", DefaultTarget, "mount point for the new system")
				answers, code, ok := parseArgs(fset, args, stderr)
				if !ok {
					return code
				}

				if *workDir == "" {
					dir, err := os.MkdirTemp("", "strata-host-")
					if err != nil {
						_, _ = fmt.Fprintf(stderr, "apply: %v\n", err)
						return cli.ExitError
					}
					*workDir = dir
				}
				in := &Installer{
					Answers:     answers,
					Root:        root,
					SourceDir:   *sourceDir,
					WorkDir:     *workDir,
					Target:      *target,
					ExtraModule: *extra,
					ConfirmWipe: *confirm,
					Stdin:       stdin,
					Out:         stdout,
					Runner:      runner,
					Logger:      slog.New(slog.NewTextHandler(stderr, nil)),
				}
				if err := in.Apply(ctx); err != nil {
					_, _ = fmt.Fprintf(stderr, "\napply: %v\n", err)
					return cli.ExitError
				}
				return cli.ExitOK
			},
		},
	}
}

// parseArgs parses flags and reads the answer file named by the single
// positional argument. On failure it returns the exit code and false.
func parseArgs(fset *flag.FlagSet, args []string, stderr io.Writer) (Answers, int, bool) {
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Answers{}, cli.ExitOK, false
		}
		return Answers{}, cli.ExitUsage, false
	}
	if fset.NArg() != 1 {
		_, _ = fmt.Fprintf(stderr, "%s: exactly one answer file is required\n", fset.Name())
		return Answers{}, cli.ExitUsage, false
	}
	f, err := os.Open(fset.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", fset.Name(), err)
		return Answers{}, cli.ExitError, false
	}
	defer func() { _ = f.Close() }()
	answers, err := Parse(f)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: invalid answer file %s:\n%v\n", fset.Name(), fset.Arg(0), err)
		return Answers{}, cli.ExitError, false
	}
	return answers, cli.ExitOK, true
}
