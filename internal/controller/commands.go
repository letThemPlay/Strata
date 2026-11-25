// Package controller is the Strata control plane (M3): it synchronizes the
// configuration repository, evaluates the IR of verified commits and plans
// them against the node through the agent (ADR-0025). It also validates IR
// documents (`validate-ir`).
package controller

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/ir"
)

// Subcommands returns the controller's subcommands. stdin is read by
// `validate-ir -`.
func Subcommands(stdin io.Reader) []cli.Subcommand {
	return []cli.Subcommand{runCommand(), evalCommand(stdin, os.Stdout), {
		Name:    "validate-ir",
		Usage:   "FILE|-",
		Summary: "Validate an IR document (nix eval --json .#nixosConfigurations.NODE.config.strata.ir)",
		Run: func(_ context.Context, args []string, stdout, stderr io.Writer) int {
			if len(args) != 1 {
				_, _ = fmt.Fprintln(stderr, "usage: strata-controller validate-ir FILE|-")
				return cli.ExitUsage
			}
			if err := validateIR(args[0], stdin, stdout); err != nil {
				_, _ = fmt.Fprintf(stderr, "strata-controller: %v\n", err)
				return cli.ExitError
			}
			return cli.ExitOK
		},
	}}
}

func validateIR(path string, stdin io.Reader, stdout io.Writer) error {
	r := stdin
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // A file named by the operator.
		if err != nil {
			return fmt.Errorf("open IR: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	doc, err := ir.Parse(r)
	if err != nil {
		return fmt.Errorf("%s: invalid:\n%w", path, indent(err))
	}
	ids := make([]string, len(doc.Resources))
	for i, res := range doc.Resources {
		ids[i] = res.ID
	}
	list := "none"
	if len(ids) > 0 {
		list = strings.Join(ids, ", ")
	}
	if _, err := fmt.Fprintf(stdout, "%s: valid %s for node %s (Strata %s), %d resources: %s\n",
		path, doc.APIVersion, doc.Node, doc.Generator.StrataVersion, len(ids), list); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// indent puts each line of a (joined) error on its own indented line.
type indented struct{ err error }

func (e indented) Error() string {
	return "  " + strings.ReplaceAll(e.err.Error(), "\n", "\n  ")
}

func (e indented) Unwrap() error { return e.err }

func indent(err error) error { return indented{err} }
