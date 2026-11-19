// Command strata-agent is the Strata node agent.
//
// It discovers local hardware and publishes a report in its state
// directory. See internal/agent.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/letthemplay/Strata/internal/agent"
	"github.com/letthemplay/Strata/internal/cli"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)

	cmd := cli.Command{
		Name:        "strata-agent",
		Summary:     "Strata node agent",
		Subcommands: agent.Subcommands(os.DirFS("/"), reload),
	}
	return cmd.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
