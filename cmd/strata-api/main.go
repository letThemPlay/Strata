// Command strata-api is the Strata management API server.
//
// In milestone M0 it only reports build information.
package main

import (
	"os"

	"github.com/letthemplay/Strata/internal/cli"
)

func main() {
	cmd := cli.Command{Name: "strata-api", Summary: "Strata management API server"}
	os.Exit(cmd.Run(os.Args[1:], os.Stdout, os.Stderr))
}
