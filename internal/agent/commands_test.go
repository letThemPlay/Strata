package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/hardware"
)

func agentCmd(root fstest.MapFS) cli.Command {
	return cli.Command{Name: "strata-agent", Summary: "test", Subcommands: Subcommands(root, nil)}
}

func TestDiscoverCommand(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	code := agentCmd(hostFS()).Run(context.Background(), []string{"discover"}, &out, &errOut)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	var r hardware.Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	if r.APIVersion != hardware.APIVersion || r.CPU.LogicalCount != 1 {
		t.Errorf("unexpected report %+v", r)
	}
}

func TestDiscoverCommandFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		root     fstest.MapFS
		args     []string
		wantCode int
		wantErr  string
	}{
		{"extra args", hostFS(), []string{"discover", "x"}, cli.ExitUsage, "takes no arguments"},
		{"broken host", fstest.MapFS{}, []string{"discover"}, cli.ExitError, "/proc/cpuinfo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errOut bytes.Buffer
			code := agentCmd(tc.root).Run(context.Background(), tc.args, io.Discard, &errOut)
			if code != tc.wantCode || !strings.Contains(errOut.String(), tc.wantErr) {
				t.Errorf("got (%d, %q), want (%d, containing %q)", code, errOut.String(), tc.wantCode, tc.wantErr)
			}
		})
	}
}

func TestRunCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var errOut bytes.Buffer
	go func() {
		done <- agentCmd(hostFS()).Run(ctx, []string{"run", "--state-dir", dir}, io.Discard, &errOut)
	}()

	path := filepath.Join(dir, HardwareReportFile)
	waitFor(t, func() bool { _, err := os.Stat(path); return err == nil })
	cancel()
	if code := <-done; code != cli.ExitOK {
		t.Errorf("exit = %d, want %d", code, cli.ExitOK)
	}
	if !strings.Contains(errOut.String(), `"msg":"agent started"`) {
		t.Errorf("expected structured JSON log, got %q", errOut.String())
	}
}

func TestRunCommandFailures(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing")
	tests := []struct {
		name     string
		args     []string
		wantCode int
	}{
		{"unknown flag", []string{"run", "--bogus"}, cli.ExitUsage},
		{"positional args", []string{"run", "extra"}, cli.ExitUsage},
		{"help flag", []string{"run", "-h"}, cli.ExitOK},
		{"unwritable state dir", []string{"run", "--state-dir", missing}, cli.ExitError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code := agentCmd(hostFS()).Run(context.Background(), tc.args, io.Discard, io.Discard)
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d", code, tc.wantCode)
			}
		})
	}
}
