package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/letthemplay/Strata/internal/version"
)

var testCmd = Command{
	Name:    "strata-test",
	Summary: "test binary",
	Subcommands: []Subcommand{
		{
			Name:    "echo",
			Usage:   "[words...]",
			Summary: "Echo arguments",
			Run: func(_ context.Context, args []string, stdout, _ io.Writer) int {
				_, _ = fmt.Fprintln(stdout, strings.Join(args, " "))
				return ExitOK
			},
		},
		{
			Name:    "fail",
			Summary: "Always fail",
			Run: func(_ context.Context, _ []string, _, stderr io.Writer) int {
				_, _ = fmt.Fprintln(stderr, "failed")
				return ExitError
			},
		},
		{
			Name:    "ctx",
			Summary: "Report context value",
			Run: func(ctx context.Context, _ []string, stdout, _ io.Writer) int {
				_, _ = fmt.Fprintln(stdout, ctx.Value(ctxKey{}))
				return ExitOK
			},
		},
	},
}

type ctxKey struct{}

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = testCmd.Run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestSubcommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"dispatch with args", []string{"echo", "a", "b"}, ExitOK, "a b\n", ""},
		{"dispatch without args", []string{"echo"}, ExitOK, "\n", ""},
		{"subcommand exit code propagates", []string{"fail"}, ExitError, "", "failed\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := run(t, tc.args...)
			if code != tc.wantCode || stdout != tc.wantStdout || stderr != tc.wantStderr {
				t.Errorf("got (%d, %q, %q), want (%d, %q, %q)",
					code, stdout, stderr, tc.wantCode, tc.wantStdout, tc.wantStderr)
			}
		})
	}
}

func TestSubcommandReceivesContext(t *testing.T) {
	t.Parallel()

	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	var out bytes.Buffer
	if code := testCmd.Run(ctx, []string{"ctx"}, &out, io.Discard); code != ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	if out.String() != "marker\n" {
		t.Errorf("stdout = %q, want %q", out.String(), "marker\n")
	}
}

func TestUsageListsSubcommands(t *testing.T) {
	t.Parallel()

	_, stdout, _ := run(t, "help")
	for _, want := range []string{"echo [words...]", "Echo arguments", "fail", "version [--json]", "help"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("usage missing %q:\n%s", want, stdout)
		}
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	// Version values may be injected via -ldflags (e.g. by the Nix build),
	// so compare against the runtime value rather than the defaults.
	wantVersionLine := "strata-test " + version.Get().String()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string // substring; empty means stdout must be empty
		wantStderr string // substring; empty means stderr must be empty
	}{
		{"no args prints usage", nil, ExitUsage, "", "Usage:"},
		{"help", []string{"help"}, ExitOK, "Usage:", ""},
		{"-h", []string{"-h"}, ExitOK, "Usage:", ""},
		{"--help", []string{"--help"}, ExitOK, "Usage:", ""},
		{"help with extra args", []string{"help", "x"}, ExitUsage, "", "help takes no arguments"},
		{"version", []string{"version"}, ExitOK, wantVersionLine, ""},
		{"--version", []string{"--version"}, ExitOK, wantVersionLine, ""},
		{"version bad flag", []string{"version", "--yaml"}, ExitUsage, "", "usage: version [--json]"},
		{"version too many args", []string{"version", "--json", "x"}, ExitUsage, "", "usage: version [--json]"},
		{"unknown command", []string{"apply"}, ExitUsage, "", `unknown command "apply"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := run(t, tc.args...)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			checkStream(t, "stdout", stdout, tc.wantStdout)
			checkStream(t, "stderr", stderr, tc.wantStderr)
		})
	}
}

func checkStream(t *testing.T, name, got, want string) {
	t.Helper()
	if want == "" {
		if got != "" {
			t.Errorf("%s = %q, want empty", name, got)
		}
		return
	}
	if !strings.Contains(got, want) {
		t.Errorf("%s = %q, want substring %q", name, got, want)
	}
}

func TestVersionJSON(t *testing.T) {
	t.Parallel()

	code, stdout, stderr := run(t, "version", "--json")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, ExitOK, stderr)
	}

	var got struct {
		Name      string `json:"name"`
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		GoVersion string `json:"goVersion"`
		Platform  string `json:"platform"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", stdout, err)
	}
	if got.Name != "strata-test" {
		t.Errorf("name = %q, want %q", got.Name, "strata-test")
	}
	want := version.Get()
	if got.Version != want.Version || got.Commit != want.Commit ||
		got.GoVersion != want.GoVersion || got.Platform != want.Platform {
		t.Errorf("decoded = %+v, want %+v", got, want)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestVersionWriteFailure(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"version"}, {"version", "--json"}} {
		var errOut bytes.Buffer
		if code := testCmd.Run(context.Background(), args, failingWriter{}, &errOut); code != ExitError {
			t.Errorf("Run(%v) with failing stdout = %d, want %d", args, code, ExitError)
		}
	}
}
