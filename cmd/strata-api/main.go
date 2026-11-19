// Command strata-api is the Strata management API server.
//
// It currently only reports build information.
package main

import (
	"context"
	"os"

	"github.com/letthemplay/Strata/internal/cli"
)

func main() {
	cmd := cli.Command{Name: "strata-api", Summary: "Strata management API server"}
	os.Exit(cmd.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
