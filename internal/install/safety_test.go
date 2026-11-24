package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// testHash is a well-formed yescrypt hash (of "correct horse").
const testHash = "$y$j9T$7Y4oRx3Y0E3D0RkGxQ6bS/$Qn9n1bW3Y5gE0kT1q2aZ8v4C6x7m9L0pR3s5u7w9yA1"

func fakeHasher(got *string) func(context.Context, string) (string, error) {
	return func(_ context.Context, pw string) (string, error) {
		*got = pw
		return testHash, nil
	}
}

// withUdevData marks vda1 as a ZFS pool member in the udev database.
func withUdevData(fsys fstest.MapFS) fstest.MapFS {
	fsys["sys/block/vda/dev"] = file("253:0\n")
	fsys["sys/block/vda/vda1/partition"] = file("1\n")
	fsys["sys/block/vda/vda1/size"] = file("2097152\n")
	fsys["sys/block/vda/vda1/dev"] = file("253:1\n")
	fsys["run/udev/data/b253:0"] = file("E:ID_PART_TABLE_TYPE=gpt\n")
	fsys["run/udev/data/b253:1"] = file("S:disk/by-label/tank\nE:ID_FS_TYPE=zfs_member\nE:ID_FS_LABEL=tank\nE:ID_FS_VERSION=5000\n")
	return fsys
}

func TestProbeReportsExistingData(t *testing.T) {
	d, err := Probe(withUdevData(machineFS()), probeAnswers(t))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if d.DataUnknown || len(d.ExistingData) != 1 || d.ExistingData[0] != `/dev/vda1: zfs_member "tank"` {
		t.Errorf("existing data: %q (unknown %v)", d.ExistingData, d.DataUnknown)
	}

	fsys := machineFS()
	fsys["run/udev/data/b253:7"] = file("E:ID_FS_TYPE=ext4\n") // another device
	if d, _ := Probe(fsys, probeAnswers(t)); d.DataUnknown || len(d.ExistingData) != 0 {
		t.Errorf("empty disk: %q (unknown %v)", d.ExistingData, d.DataUnknown)
	}
	if d, _ := Probe(machineFS(), probeAnswers(t)); !d.DataUnknown {
		t.Error("without a udev database the data must be reported as unknown")
	}
}

func TestPlanAndConfirmationListExistingData(t *testing.T) {
	in := newInstaller(t, &fakeRunner{})
	in.Root = withUdevData(machineFS())
	var out strings.Builder
	if err := in.Plan(&out); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(out.String(), "Existing data: (will be destroyed)\n  /dev/vda1: zfs_member \"tank\"") {
		t.Errorf("plan:\n%s", out.String())
	}

	in.Stdin = strings.NewReader("no\n")
	err := in.Apply(context.Background())
	if !errors.Is(err, ErrNotConfirmed) || !strings.Contains(in.Out.(interface{ String() string }).String(), `existing data: /dev/vda1: zfs_member "tank"`) {
		t.Errorf("Apply: %v\n%s", err, in.Out)
	}
}

func TestWizardConfirmsDiskWithExistingData(t *testing.T) {
	input := lines("hv-03", "", "1", testKey, "", "", "",
		"", "n", // the default disk has data: decline
		"2", "y", // choose it again: erase
		"", "1")
	a, out, err := runWizard(t, input, &Wizard{Root: withUdevData(wizardFS())})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	if a.Disk.Device != "/dev/disk/by-id/virtio-system" {
		t.Errorf("disk: %s", a.Disk.Device)
	}
	if strings.Count(out, `/dev/vda1: zfs_member "tank"`) != 2 || !strings.Contains(out, "HAS EXISTING DATA") {
		t.Errorf("output:\n%s", out)
	}
}

func TestWizardConsolePassword(t *testing.T) {
	var hashed string
	input := lines("hv-03", "", "1", testKey, "", "",
		"short",                          // too short
		"correct horse", "correct horsE", // differ
		"correct horse", "correct horse",
		"", "", "1")
	p, out := newPrompter(input)
	var echo []bool
	p.SetEcho = func(on bool) error { echo = append(echo, on); return nil }
	w := &Wizard{Root: wizardFS(), P: p, ReadFile: os.ReadFile, HashPassword: fakeHasher(&hashed)}
	a, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	if a.Admin.ConsolePasswordHash != testHash || hashed != "correct horse" {
		t.Errorf("hash %q of %q", a.Admin.ConsolePasswordHash, hashed)
	}
	if strings.Contains(out.String(), "correct horse") {
		t.Error("the password must not be printed")
	}
	for _, s := range []string{"at least 8 characters", "the passwords differ", "console password set"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	// Echo is turned off and back on around each of the 5 secret prompts.
	if len(echo) != 10 || echo[0] || !echo[1] {
		t.Errorf("echo toggles: %v", echo)
	}
}

func TestWizardRescansWhenNoInterfaceHasALink(t *testing.T) {
	fsys := wizardFS()
	fsys["sys/class/net/eno1/operstate"] = file("down\n")
	input := lines("hv-03", "", "1", testKey, "", "", "", "",
		"",  // no default interface: Enter re-scans
		"2", // eth0
		"1")
	a, out, err := runWizard(t, input, &Wizard{Root: fsys})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	if a.Network.Interface != "eth0" {
		t.Errorf("interface: %s", a.Network.Interface)
	}
	if strings.Count(out, "No interface has a link") != 2 {
		t.Errorf("expected a re-scan:\n%s", out)
	}
}

func TestConsolePasswordHashValidation(t *testing.T) {
	a := mustParse(t, dhcpAnswers)
	a.Admin.ConsolePasswordHash = testHash
	if err := a.Validate(); err != nil {
		t.Errorf("valid hash rejected: %v", err)
	}
	for _, bad := range []string{"correct horse", "!", "*", "$1$abcdefgh$md5isweak0000000000", testHash + "\nroot::0:0"} {
		a.Admin.ConsolePasswordHash = bad
		if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "consolePasswordHash") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestRenderReferencesConsolePasswordFileNotHash(t *testing.T) {
	a := mustParse(t, dhcpAnswers)
	a.Admin.ConsolePasswordHash = testHash
	got := RenderHostNix(a, "52:54:00:12:34:56")
	if !containsLine(got, `consolePasswordFile = "/etc/strata/admin-console-password";`) || strings.Contains(got, testHash) {
		t.Errorf("host.nix:\n%s", got)
	}
}

func TestApplyStoresConsolePasswordBeforeInstall(t *testing.T) {
	r := &fakeRunner{}
	in := newInstaller(t, r)
	in.ConfirmWipe = in.Answers.Disk.Device
	in.Answers.Admin.ConsolePasswordHash = testHash
	path := filepath.Join(in.Target, ConsolePasswordPath)
	var presentAtInstall bool
	r.onRun = func(c Command) {
		if c.Name == "nixos-install" {
			_, err := os.Stat(path)
			presentAtInstall = err == nil
		}
	}
	if err := in.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !presentAtInstall {
		t.Error("the hash file must exist when nixos-install creates the account")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if data, _ := os.ReadFile(path); string(data) != testHash+"\n" {
		t.Errorf("content %q", data)
	}
	// The hash is not part of the host flake (stored in Git).
	_ = filepath.WalkDir(in.WorkDir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			if data, _ := os.ReadFile(p); strings.Contains(string(data), testHash) { //nolint:gosec // Test temp directory.
				t.Errorf("%s contains the hash", p)
			}
		}
		return nil
	})
}
