// Command strata is the Strata command-line client.
//
// In M2 it manages VMs on the local host through the agent's local API
// (`strata vm`). Remote management arrives with the management API (M5).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/configcli"
	"github.com/letthemplay/Strata/internal/hostcli"
	"github.com/letthemplay/Strata/internal/vmcli"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cmd := cli.Command{
		Name:        "strata",
		Summary:     "Strata command-line client",
		Subcommands: []cli.Subcommand{vmcli.Subcommand(os.Stdin), configcli.Subcommand(os.Stdin), hostcli.Subcommand()},
	}
	return cmd.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
