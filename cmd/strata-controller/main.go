// Command strata-controller is the Strata control-plane controller.
//
// It currently validates IR documents (`validate-ir`); Git
// synchronization, planning and reconciliation follow in M3.
package main

import (
	"context"
	"os"

	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/controller"
)

func main() {
	cmd := cli.Command{
		Name:        "strata-controller",
		Summary:     "Strata control-plane controller",
		Subcommands: controller.Subcommands(os.Stdin),
	}
	os.Exit(cmd.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
