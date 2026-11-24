// Command strata-host applies host-level Strata configuration that NixOS
// cannot express directly. It runs as root, started only by NixOS units:
//
//	strata-host incus-setup --config FILE
//
// brings Incus to the declared state (project, agent identity, storage
// pools). See internal/hostsetup.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/hostsetup"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cmd := cli.Command{
		Name:        "strata-host",
		Summary:     "Strata host setup (run by NixOS units, as root)",
		Subcommands: hostsetup.Subcommands(os.DirFS("/")),
	}
	return cmd.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
