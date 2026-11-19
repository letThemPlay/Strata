package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/letthemplay/Strata/internal/version"
)

var testCmd = Command{Name: "strata-test", Summary: "test binary"}

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = testCmd.Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
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
		if code := testCmd.Run(args, failingWriter{}, &errOut); code != ExitError {
			t.Errorf("Run(%v) with failing stdout = %d, want %d", args, code, ExitError)
		}
	}
}
