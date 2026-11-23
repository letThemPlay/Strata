package install

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"

	"github.com/letthemplay/Strata/internal/cli"
)

// Env is what the strata-install subcommands use from the running system.
type Env struct {
	// Root is the system's "/" for probing (os.DirFS("/")).
	Root fs.FS
	// Stdin is read for answers and the erase confirmation.
	Stdin io.Reader
	// Runner executes external programs.
	Runner Runner
	// HTTP downloads SSH keys in the init wizard (NewKeyClient).
	HTTP *http.Client
	// Geteuid returns the effective user ID (os.Geteuid).
	Geteuid func() int
	// HashPassword hashes the console password in the init wizard
	// (HashPassword).
	HashPassword func(ctx context.Context, password string) (string, error)
}

// Subcommands returns the strata-install subcommands.
func Subcommands(env Env) []cli.Subcommand {
	return []cli.Subcommand{
		{
			Name:    "init",
			Usage:   "[--output FILE] [--force] [--extra-module FILE] [--work-dir DIR]",
			Summary: "Ask questions and write an answer file, then check it and offer to install",
			Run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
				return runInit(ctx, env, args, stdout, stderr)
			},
		},
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
				in := &Installer{Answers: answers, Root: env.Root}
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
				in := installFlags(fset)
				answers, code, ok := parseArgs(fset, args, stderr)
				if !ok {
					return code
				}
				in.Answers, in.Root, in.ConfirmWipe, in.Stdin = answers, env.Root, *confirm, env.Stdin
				return apply(ctx, env, in, stdout, stderr)
			},
		},
	}
}

// installFlags registers the flags that configure an installation.
func installFlags(fset *flag.FlagSet) *Installer {
	in := &Installer{}
	fset.StringVar(&in.ExtraModule, "extra-module", "", "NixOS module `FILE` to add to the host configuration as extra-module.nix")
	fset.StringVar(&in.WorkDir, "work-dir", "", "empty `DIR` for the generated configuration (default: a new temporary directory)")
	fset.StringVar(&in.SourceDir, "source-dir", DefaultSourceDir, "`DIR` with the host template and flake.lock")
	fset.StringVar(&in.Target, "target", DefaultTarget, "mount point for the new system")
	return in
}

// apply creates a work directory unless one was given and runs in.
func apply(ctx context.Context, env Env, in *Installer, stdout, stderr io.Writer) int {
	if in.WorkDir == "" {
		dir, err := os.MkdirTemp("", "strata-host-")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "apply: %v\n", err)
			return cli.ExitError
		}
		in.WorkDir = dir
	}
	in.Out = stdout
	in.Runner = env.Runner
	in.Logger = slog.New(slog.NewTextHandler(stderr, nil))
	if err := in.Apply(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "\napply: %v\n", err)
		return cli.ExitError
	}
	return cli.ExitOK
}

func runInit(ctx context.Context, env Env, args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("init", flag.ContinueOnError)
	fset.SetOutput(stderr)
	output := fset.String("output", "answer.json", "answer `FILE` to write")
	force := fset.Bool("force", false, "overwrite an existing answer file")
	in := installFlags(fset)
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return cli.ExitOK
		}
		return cli.ExitUsage
	}
	if fset.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "init: no arguments expected")
		return cli.ExitUsage
	}
	if _, err := os.Lstat(*output); err == nil && !*force {
		_, _ = fmt.Fprintf(stderr, "init: %s already exists; use --force to overwrite it or --output to choose another file\n", *output)
		return cli.ExitError
	}

	p := NewPrompter(ctx, env.Stdin, stdout)
	if f, ok := env.Stdin.(*os.File); ok {
		p.SetEcho = TerminalEcho(f)
	}
	w := &Wizard{Root: env.Root, P: p, HTTP: env.HTTP, ReadFile: os.ReadFile, HashPassword: env.HashPassword}
	answers, err := w.Run(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "\ninit: %v\n", err)
		return cli.ExitError
	}
	if err := WriteAnswers(*output, answers); err != nil {
		_, _ = fmt.Fprintf(stderr, "\ninit: %v\n", err)
		return cli.ExitError
	}
	p.Say("\nWrote %s.\n", *output)

	in.Answers, in.Root = answers, env.Root
	if err := in.Plan(stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "\ninit: %v\nEdit %s and check it with: strata-install plan %s\n", err, *output, *output)
		return cli.ExitError
	}
	later := fmt.Sprintf("To install, run: sudo strata-install apply %s", *output)
	if env.Geteuid() != 0 {
		p.Say("\n%s", later)
		return cli.ExitOK
	}
	p.Say("")
	now, err := p.Confirm("Install now? The system is built first; erasing the disk needs another confirmation", false)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "\ninit: %v\n", err)
		return cli.ExitError
	}
	if !now {
		p.Say("%s", later)
		return cli.ExitOK
	}
	in.Stdin = p
	return apply(ctx, env, in, stdout, stderr)
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
