// Command strata is the Strata command-line client.
//
// It currently only reports build information.
package main

import (
	"context"
	"os"

	"github.com/letthemplay/Strata/internal/cli"
)

func main() {
	cmd := cli.Command{Name: "strata", Summary: "Strata command-line client"}
	os.Exit(cmd.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
