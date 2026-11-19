// Command strata-controller is the Strata control-plane controller.
//
// It currently only reports build information.
package main

import (
	"context"
	"os"

	"github.com/letthemplay/Strata/internal/cli"
)

func main() {
	cmd := cli.Command{Name: "strata-controller", Summary: "Strata control-plane controller"}
	os.Exit(cmd.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
