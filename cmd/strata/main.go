// Command strata is the Strata command-line client.
//
// In milestone M0 it only reports build information.
package main

import (
	"os"

	"github.com/letthemplay/Strata/internal/cli"
)

func main() {
	cmd := cli.Command{Name: "strata", Summary: "Strata command-line client"}
	os.Exit(cmd.Run(os.Args[1:], os.Stdout, os.Stderr))
}
