// Command strata-install installs a Strata host from the installer ISO.
//
// See internal/install and docs/installation.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/install"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd := cli.Command{
		Name:        "strata-install",
		Summary:     "Strata host installer",
		Subcommands: install.Subcommands(os.DirFS("/"), os.Stdin, install.ExecRunner{Stderr: os.Stderr}),
	}
	return cmd.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
