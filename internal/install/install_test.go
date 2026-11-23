package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeRunner records commands and returns canned output. fail makes the
// command whose name has the given suffix fail.
type fakeRunner struct {
	cmds []Command
	fail string
	// onRun, if set, is called with each command before it "runs".
	onRun func(Command)
}

func (f *fakeRunner) Run(_ context.Context, c Command) ([]byte, error) {
	f.cmds = append(f.cmds, c)
	if f.onRun != nil {
		f.onRun(c)
	}
	if f.fail != "" && strings.HasSuffix(c.Name, f.fail) {
		return nil, errors.New("simulated failure")
	}
	switch c.Name {
	case "nixos-generate-config":
		return []byte("{ }\n"), nil
	case "nix":
		if c.Args[0] == "build" {
			return []byte("/nix/store/aaa-nixos-system\n/nix/store/bbb-disko\n"), nil
		}
	}
	return nil, nil
}

func (f *fakeRunner) names() []string {
	names := make([]string, len(f.cmds))
	for i, c := range f.cmds {
		names[i] = c.Name
		if c.Name == "nix" || c.Name == "git" {
			// Distinguish subcommands.
			for _, a := range c.Args {
				if !strings.HasPrefix(a, "-") && !strings.Contains(a, "/") && !strings.Contains(a, "=") {
					names[i] += " " + a
					break
				}
			}
		}
	}
	return names
}

// destructive reports whether any command that writes to the target
// disk ran.
func (f *fakeRunner) destructive() bool {
	for _, c := range f.cmds {
		if strings.HasSuffix(c.Name, "disko-destroy-format-mount") || c.Name == "nixos-install" {
			return true
		}
	}
	return false
}

func newInstaller(t *testing.T, runner *fakeRunner) *Installer {
	t.Helper()
	src := t.TempDir()
	for name, content := range map[string]string{
		"template/flake.nix":                  "{ flake }\n",
		"template/configuration.nix":          "{ configuration }\n",
		"template/host.nix":                   "{ template host }\n",
		"template/hardware-configuration.nix": "{ template hardware }\n",
		"flake.lock":                          "{ \"lock\": true }\n",
	} {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &Installer{
		Answers:   probeAnswers(t),
		Root:      machineFS(),
		SourceDir: src,
		WorkDir:   filepath.Join(t.TempDir(), "host"),
		Target:    t.TempDir(),
		Stdin:     strings.NewReader(""),
		Out:       &bytes.Buffer{},
		Runner:    runner,
	}
}

func TestApplyRunsStepsInOrder(t *testing.T) {
	r := &fakeRunner{}
	in := newInstaller(t, r)
	in.ConfirmWipe = in.Answers.Disk.Device

	if err := in.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []string{
		"nixos-generate-config",
		"nix build",
		"/nix/store/bbb-disko/bin/disko-destroy-format-mount",
		"nixos-install",
		"nix flake",
		"git init",
		"git add",
		"git commit",
		"umount",
	}
	if got := r.names(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	build := r.cmds[1]
	for _, arg := range []string{"--offline", "path:" + in.WorkDir + "#nixosConfigurations.hv-02.config.system.build.toplevel"} {
		if !slices.Contains(build.Args, arg) {
			t.Errorf("nix build args %q lack %q", build.Args, arg)
		}
	}
	install := r.cmds[3]
	if !slices.Contains(install.Args, "/nix/store/aaa-nixos-system") || !slices.Contains(install.Args, in.Target) {
		t.Errorf("nixos-install args %q", install.Args)
	}
}

func TestApplyWritesHostFlake(t *testing.T) {
	r := &fakeRunner{}
	in := newInstaller(t, r)
	in.ConfirmWipe = in.Answers.Disk.Device
	extra := filepath.Join(t.TempDir(), "site.nix")
	if err := os.WriteFile(extra, []byte("{ extra }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in.ExtraModule = extra

	if err := in.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := map[string]string{
		"flake.nix":                  "{ flake }\n",
		"configuration.nix":          "{ configuration }\n",
		"flake.lock":                 "{ \"lock\": true }\n",
		"hardware-configuration.nix": "{ }\n",
		"host.nix":                   RenderHostNix(in.Answers),
		"extra-module.nix":           "{ extra }\n",
	}
	for _, dir := range []string{in.WorkDir, filepath.Join(in.Target, "etc", "nixos")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != len(want) {
			t.Errorf("%s has %d files, want %d", dir, len(entries), len(want))
		}
		for name, content := range want {
			got, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if string(got) != content {
				t.Errorf("%s/%s = %q, want %q", dir, name, got, content)
			}
		}
	}
}

func TestApplyInteractiveConfirmation(t *testing.T) {
	tests := []struct {
		name, input string
		ok          bool
	}{
		{"typed device", "/dev/disk/by-id/virtio-system\n", true},
		{"typed device without newline", "/dev/disk/by-id/virtio-system", true},
		{"yes is not enough", "yes\n", false},
		{"other device", "/dev/vda\n", false},
		{"no input", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{}
			in := newInstaller(t, r)
			in.Stdin = strings.NewReader(tt.input)
			err := in.Apply(context.Background())
			if tt.ok {
				if err != nil {
					t.Fatalf("Apply: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrNotConfirmed) {
				t.Fatalf("Apply error = %v, want ErrNotConfirmed", err)
			}
			if r.destructive() {
				t.Error("disk was written without confirmation")
			}
		})
	}
}

func TestApplyWrongConfirmWipeFlag(t *testing.T) {
	r := &fakeRunner{}
	in := newInstaller(t, r)
	in.ConfirmWipe = "/dev/vda"
	// Interactive input must not be consulted when the flag is given.
	in.Stdin = strings.NewReader(in.Answers.Disk.Device + "\n")

	if err := in.Apply(context.Background()); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("Apply error = %v, want ErrNotConfirmed", err)
	}
	if r.destructive() {
		t.Error("disk was written despite a mismatched --confirm-wipe")
	}
}

func TestApplyStopsBeforeTheDiskOnFailure(t *testing.T) {
	for _, step := range []string{"nixos-generate-config", "nix"} {
		t.Run(step, func(t *testing.T) {
			r := &fakeRunner{fail: step}
			in := newInstaller(t, r)
			in.ConfirmWipe = in.Answers.Disk.Device
			if err := in.Apply(context.Background()); err == nil {
				t.Fatal("Apply succeeded, want error")
			}
			if r.destructive() {
				t.Errorf("disk was written after %s failed", step)
			}
		})
	}
}

func TestApplyStopsAfterFailedFormat(t *testing.T) {
	r := &fakeRunner{fail: "disko-destroy-format-mount"}
	in := newInstaller(t, r)
	in.ConfirmWipe = in.Answers.Disk.Device
	if err := in.Apply(context.Background()); err == nil {
		t.Fatal("Apply succeeded, want error")
	}
	for _, c := range r.cmds {
		if c.Name == "nixos-install" {
			t.Error("nixos-install ran after formatting failed")
		}
	}
}

func TestApplyRunsNothingWhenProbeFails(t *testing.T) {
	r := &fakeRunner{}
	in := newInstaller(t, r)
	in.ConfirmWipe = in.Answers.Disk.Device
	in.Answers.Network.Interface = "eno9"
	if err := in.Apply(context.Background()); err == nil {
		t.Fatal("Apply succeeded, want error")
	}
	if len(r.cmds) != 0 {
		t.Errorf("commands ran: %v", r.names())
	}
}

func TestApplyRefusesNonEmptyWorkDir(t *testing.T) {
	r := &fakeRunner{}
	in := newInstaller(t, r)
	in.ConfirmWipe = in.Answers.Disk.Device
	if err := os.MkdirAll(in.WorkDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in.WorkDir, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := in.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("Apply error = %v, want work directory not empty", err)
	}
	if r.destructive() {
		t.Error("disk was written")
	}
}

func TestPlanChangesNothing(t *testing.T) {
	in := &Installer{Answers: probeAnswers(t), Root: machineFS()}
	var out bytes.Buffer
	if err := in.Plan(&out); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, want := range []string{"hv-02", "/dev/vda, 32.0 GiB, QEMU HARDDISK", "Partitions: none", "eth0, dhcp", "Steps:", "7. Unmount"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestPlanReportsProbeErrors(t *testing.T) {
	a := probeAnswers(t)
	a.Disk.Device = "/dev/nvme9n1"
	in := &Installer{Answers: a, Root: machineFS()}
	var out bytes.Buffer
	err := in.Plan(&out)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Plan error = %v, want missing device", err)
	}
	if strings.Contains(out.String(), "Steps:") {
		t.Error("plan printed steps for an invalid installation")
	}
}
