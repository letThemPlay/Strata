// Command strata-agent is the Strata privileged node agent.
//
// In milestone M0 it only reports build information.
package main

import (
	"os"

	"github.com/letthemplay/Strata/internal/cli"
)

func main() {
	cmd := cli.Command{Name: "strata-agent", Summary: "Strata privileged node agent"}
	os.Exit(cmd.Run(os.Args[1:], os.Stdout, os.Stderr))
}
