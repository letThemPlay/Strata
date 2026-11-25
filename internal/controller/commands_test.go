package controller

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/letthemplay/Strata/internal/cli"
)

func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := cli.Command{Name: "strata-controller", Subcommands: Subcommands(strings.NewReader(stdin))}
	code := cmd.Run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestValidateIRFile(t *testing.T) {
	code, out, errOut := run(t, "", "validate-ir", "../ir/testdata/standalone.json")
	if code != cli.ExitOK || !strings.Contains(out, "valid ir.strata.dev/v1alpha1 for node hv-01") ||
		!strings.Contains(out, "3 resources: vm/db-01, vm/scratch, vm/web-01") {
		t.Errorf("code %d\nout %s\nerr %s", code, out, errOut)
	}
}

func TestValidateIRStdin(t *testing.T) {
	data, err := os.ReadFile("../ir/testdata/standalone.json")
	if err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, string(data), "validate-ir", "-"); code != cli.ExitOK || !strings.HasPrefix(out, "-: valid") {
		t.Errorf("code %d out %s", code, out)
	}
}

func TestValidateIRInvalid(t *testing.T) {
	bad := `{"apiVersion": "ir.strata.dev/v1alpha1", "node": "Bad", "host": {"project": "strata", "pools": [], "networks": [], "imageServers": []}, "resources": []}`
	code, out, errOut := run(t, bad, "validate-ir", "-")
	if code != cli.ExitError || out != "" || !strings.Contains(errOut, "-: invalid:\n  node:") {
		t.Errorf("code %d\nout %s\nerr %s", code, out, errOut)
	}
}

func TestValidateIRUsage(t *testing.T) {
	if code, _, _ := run(t, "", "validate-ir"); code != cli.ExitUsage {
		t.Errorf("code %d", code)
	}
	if code, _, errOut := run(t, "", "validate-ir", "/nonexistent.json"); code != cli.ExitError || !strings.Contains(errOut, "open IR") {
		t.Errorf("code %d err %s", code, errOut)
	}
}
