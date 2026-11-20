package install

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/letthemplay/Strata/internal/cli"
)

// initInput answers the wizard for wizardFS with DHCP on eno1.
func initInput(extra ...string) string {
	return lines(append([]string{"hv-08", "", "1", testKey, "", "", "", "", "1"}, extra...)...)
}

func runInitCmd(t *testing.T, env Env, args ...string) (int, string, string) {
	t.Helper()
	if env.Root == nil {
		env.Root = wizardFS()
	}
	if env.Geteuid == nil {
		env.Geteuid = func() int { return 1000 }
	}
	var stdout, stderr bytes.Buffer
	code := runInit(context.Background(), env, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestInitWritesAnswerFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "answer.json")
	r := &fakeRunner{}
	code, stdout, stderr := runInitCmd(t, Env{Stdin: strings.NewReader(initInput()), Runner: r}, "--output", out)
	if code != cli.ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	a, err := Parse(f)
	if err != nil {
		t.Fatalf("written file does not parse: %v", err)
	}
	if a.Node.ID != "hv-08" || a.Network.Interface != "eno1" {
		t.Errorf("answers: %+v", a)
	}
	// Not root: the plan is shown, apply is suggested, nothing runs.
	for _, s := range []string{"Steps:", "sudo strata-install apply " + out} {
		if !strings.Contains(stdout, s) {
			t.Errorf("output lacks %q:\n%s", s, stdout)
		}
	}
	if len(r.cmds) != 0 {
		t.Errorf("commands ran: %v", r.names())
	}
}

func TestInitRefusesToOverwrite(t *testing.T) {
	out := filepath.Join(t.TempDir(), "answer.json")
	if err := os.WriteFile(out, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runInitCmd(t, Env{Stdin: strings.NewReader(initInput())}, "--output", out)
	if code == cli.ExitOK || !strings.Contains(stderr, "already exists") {
		t.Errorf("exit %d, %s", code, stderr)
	}
	if data, _ := os.ReadFile(out); string(data) != "keep" {
		t.Errorf("file overwritten: %q", data)
	}

	code, _, stderr = runInitCmd(t, Env{Stdin: strings.NewReader(initInput())}, "--output", out, "--force")
	if code != cli.ExitOK {
		t.Errorf("--force: exit %d, %s", code, stderr)
	}
}

func TestInitWritesNothingOnAbort(t *testing.T) {
	out := filepath.Join(t.TempDir(), "answer.json")
	code, _, stderr := runInitCmd(t, Env{Stdin: strings.NewReader(lines("hv-08", ""))}, "--output", out)
	if code == cli.ExitOK || !strings.Contains(stderr, "nothing was written") {
		t.Errorf("exit %d, %s", code, stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("answer file exists: %v", err)
	}
}

func TestInitAsRootDeclinesInstall(t *testing.T) {
	out := filepath.Join(t.TempDir(), "answer.json")
	r := &fakeRunner{}
	env := Env{Stdin: strings.NewReader(initInput("")), Runner: r, Geteuid: func() int { return 0 }}
	code, stdout, stderr := runInitCmd(t, env, "--output", out)
	if code != cli.ExitOK || len(r.cmds) != 0 || !strings.Contains(stdout, "Install now?") {
		t.Errorf("exit %d, commands %v\n%s\n%s", code, r.names(), stdout, stderr)
	}
}

func TestInitInstallsWithTypedConfirmation(t *testing.T) {
	src := newInstaller(t, nil).SourceDir
	out := filepath.Join(t.TempDir(), "answer.json")
	r := &fakeRunner{}
	env := Env{
		Stdin:   strings.NewReader(initInput("y", "/dev/disk/by-id/virtio-system")),
		Runner:  r,
		Geteuid: func() int { return 0 },
	}
	code, stdout, stderr := runInitCmd(t, env, "--output", out,
		"--source-dir", src, "--work-dir", filepath.Join(t.TempDir(), "host"), "--target", t.TempDir())
	if code != cli.ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if !r.destructive() || !strings.Contains(stdout, "WILL BE DESTROYED") {
		t.Errorf("install did not run after confirmation: %v\n%s", r.names(), stdout)
	}
}

func TestInitInstallAbortsOnWrongConfirmation(t *testing.T) {
	src := newInstaller(t, nil).SourceDir
	r := &fakeRunner{}
	env := Env{
		Stdin:   strings.NewReader(initInput("y", "/dev/vda")),
		Runner:  r,
		Geteuid: func() int { return 0 },
	}
	code, _, stderr := runInitCmd(t, env, "--output", filepath.Join(t.TempDir(), "a.json"),
		"--source-dir", src, "--work-dir", filepath.Join(t.TempDir(), "host"), "--target", t.TempDir())
	if code == cli.ExitOK || r.destructive() || !strings.Contains(stderr, "nothing was changed") {
		t.Errorf("exit %d, commands %v\n%s", code, r.names(), stderr)
	}
}
