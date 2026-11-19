// Command strata-controller is the Strata control-plane controller.
//
// In milestone M0 it only reports build information.
package main

import (
	"os"

	"github.com/letthemplay/Strata/internal/cli"
)

func main() {
	cmd := cli.Command{Name: "strata-controller", Summary: "Strata control-plane controller"}
	os.Exit(cmd.Run(os.Args[1:], os.Stdout, os.Stderr))
}
