package install

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Default locations on the installer ISO.
const (
	// DefaultSourceDir holds the host template (template/) and the
	// flake.lock pinning the ISO's Strata and nixpkgs (flake.lock).
	DefaultSourceDir = "/etc/strata-install"
	// DefaultTarget is where the new system is mounted during installation.
	DefaultTarget = "/mnt"
)

// Files copied from the template unchanged. host.nix and
// hardware-configuration.nix are generated.
func templateFiles() []string { return []string{"flake.nix", "configuration.nix"} }

// Command timeouts. Builds run offline from the ISO's store, so they
// are bounded by local CPU and disk speed only.
const (
	timeoutQuick   = 2 * time.Minute
	timeoutBuild   = 60 * time.Minute
	timeoutFormat  = 15 * time.Minute
	timeoutInstall = 60 * time.Minute
)

// Command is an external program invocation. Arguments are passed to the
// program directly; no shell is involved.
type Command struct {
	Name    string
	Args    []string
	Dir     string
	Timeout time.Duration
}

func (c Command) String() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

// Runner executes commands. ExecRunner is the real implementation; tests
// substitute a fake.
type Runner interface {
	// Run executes cmd and returns its standard output. Standard error is
	// passed through for the operator to see.
	Run(ctx context.Context, cmd Command) ([]byte, error)
}

// ErrNotConfirmed is returned when the operator does not confirm the
// destruction of the target disk.
var ErrNotConfirmed = errors.New("installation aborted: disk erase not confirmed; nothing was changed")

// Installer installs a Strata host according to an answer file.
type Installer struct {
	Answers Answers
	// Root is the running system's "/" for probing (os.DirFS("/")).
	Root fs.FS
	// SourceDir holds template/ and flake.lock (DefaultSourceDir).
	SourceDir string
	// WorkDir receives the generated host flake. It must be empty or
	// absent.
	WorkDir string
	// Target is the mount point of the new system (DefaultTarget).
	Target string
	// ExtraModule, if set, is copied into the host flake as
	// extra-module.nix, which the template's flake.nix imports.
	ExtraModule string
	// ConfirmWipe, if non-empty, confirms erasing the disk without a
	// prompt. It must equal Answers.Disk.Device exactly.
	ConfirmWipe string
	// Stdin is read for the interactive confirmation.
	Stdin io.Reader
	// Out receives progress messages for the operator.
	Out    io.Writer
	Runner Runner
	Logger *slog.Logger
}

// Steps describes what Apply does, in order. Plan prints it.
func Steps() []string {
	return []string{
		"Check the answer file against this machine (disk, network interface)",
		"Generate the host configuration flake (template, host.nix, hardware-configuration.nix, flake.lock)",
		"Build the system and the disk formatting script, offline (nothing is written to the disk yet)",
		"Ask for confirmation, then erase and partition the disk and mount it",
		"Install the system and its boot loader",
		"Store the configuration in /etc/nixos as a Git repository, with its sources",
		"Unmount the new system",
	}
}

// Plan validates the answers against the machine and describes the
// installation without changing anything.
func (in *Installer) Plan(w io.Writer) error {
	disk, err := Probe(in.Root, in.Answers)
	in.describe(w, disk)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("\nSteps:\n")
	for i, s := range Steps() {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, s)
	}
	_, err = io.WriteString(w, b.String())
	return err //nolint:wrapcheck // Output error, nothing to add.
}

func (in *Installer) describe(w io.Writer, d DiskInfo) {
	a := in.Answers
	var b strings.Builder
	fmt.Fprintf(&b, "Node:       %s\n", a.Node.ID)
	fmt.Fprintf(&b, "Admin:      %s (%d key(s))\n", a.Admin.Name, len(a.Admin.AuthorizedKeys))
	fmt.Fprintf(&b, "Disk:       %s", a.Disk.Device)
	if d.Name != "" {
		fmt.Fprintf(&b, " -> /dev/%s, %s", d.Name, FormatBytes(d.SizeBytes))
		if d.Model != "" {
			fmt.Fprintf(&b, ", %s", d.Model)
		}
		if d.Removable {
			b.WriteString(", removable")
		}
	}
	fmt.Fprintf(&b, "\nLayout:     %s\n", a.Disk.Layout)
	if len(d.Partitions) == 0 {
		b.WriteString("Partitions: none\n")
	} else {
		b.WriteString("Partitions: (all will be erased)\n")
		for _, p := range d.Partitions {
			fmt.Fprintf(&b, "  /dev/%s  %s\n", p.Name, FormatBytes(p.SizeBytes))
		}
	}
	n := a.Network
	fmt.Fprintf(&b, "Management: %s, %s", n.Interface, n.Addressing)
	if n.Addressing == "static" {
		fmt.Fprintf(&b, " %s", strings.Join(n.Addresses, " "))
	}
	b.WriteByte('\n')
	_, _ = io.WriteString(w, b.String())
}

// Apply performs the installation. Nothing is written to the target disk
// before the system has been built and the erase has been confirmed.
func (in *Installer) Apply(ctx context.Context) error {
	log := in.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	id := in.Answers.Node.ID

	in.step(1, "Checking the answer file against this machine")
	disk, err := Probe(in.Root, in.Answers)
	in.describe(in.Out, disk)
	if err != nil {
		return err
	}

	in.step(2, "Generating the host configuration in "+in.WorkDir)
	if err := in.generate(ctx); err != nil {
		return err
	}

	in.step(3, "Building the system (offline)")
	flake := "path:" + in.WorkDir + "#nixosConfigurations." + id + ".config.system.build."
	out, err := in.Runner.Run(ctx, Command{
		Name:    "nix",
		Args:    []string{"build", "--offline", "--no-link", "--print-out-paths", flake + "toplevel", flake + "destroyFormatMount"},
		Timeout: timeoutBuild,
	})
	if err != nil {
		return fmt.Errorf("build the system (the disk has not been touched): %w", err)
	}
	paths := strings.Fields(string(out))
	if len(paths) != 2 {
		return fmt.Errorf("build the system: expected 2 store paths, got %q", out)
	}
	system, format := paths[0], paths[1]
	log.InfoContext(ctx, "system built", "toplevel", system, "format", format)

	in.step(4, "Confirming")
	if err := in.confirm(disk); err != nil {
		return err
	}
	in.say("Erasing, partitioning and mounting %s", in.Answers.Disk.Device)
	if _, err := in.Runner.Run(ctx, Command{
		Name:    format + "/bin/disko-destroy-format-mount",
		Args:    []string{"--yes-wipe-all-disks"},
		Timeout: timeoutFormat,
	}); err != nil {
		return fmt.Errorf("format the disk: %w", err)
	}

	in.step(5, "Installing the system")
	if _, err := in.Runner.Run(ctx, Command{
		Name:    "nixos-install",
		Args:    []string{"--root", in.Target, "--system", system, "--no-root-passwd", "--no-channel-copy"},
		Timeout: timeoutInstall,
	}); err != nil {
		return fmt.Errorf("install the system: %w", err)
	}

	in.step(6, "Storing the configuration in /etc/nixos")
	if err := in.storeConfiguration(ctx); err != nil {
		return err
	}

	in.step(7, "Unmounting "+in.Target)
	if _, err := in.Runner.Run(ctx, Command{Name: "umount", Args: []string{"--recursive", in.Target}, Timeout: timeoutQuick}); err != nil {
		return fmt.Errorf("unmount %s: %w", in.Target, err)
	}

	in.say("\nInstallation complete. Remove the installation medium and reboot.")
	in.say("Log in as %s with your SSH key on %s.", in.Answers.Admin.Name, in.Answers.Network.Interface)
	in.say("Push /etc/nixos to your own Git repository; it is the host's configuration.")
	return nil
}

// generate writes the host flake into WorkDir.
func (in *Installer) generate(ctx context.Context) error {
	if entries, err := os.ReadDir(in.WorkDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("work directory %s is not empty", in.WorkDir)
	}
	if err := os.MkdirAll(in.WorkDir, 0o700); err != nil {
		return fmt.Errorf("create work directory: %w", err)
	}

	for _, name := range templateFiles() {
		if err := copyFile(filepath.Join(in.SourceDir, "template", name), filepath.Join(in.WorkDir, name)); err != nil {
			return err
		}
	}
	if err := copyFile(filepath.Join(in.SourceDir, "flake.lock"), filepath.Join(in.WorkDir, "flake.lock")); err != nil {
		return err
	}
	if in.ExtraModule != "" {
		if err := copyFile(in.ExtraModule, filepath.Join(in.WorkDir, "extra-module.nix")); err != nil {
			return err
		}
	}
	if err := writeFile(filepath.Join(in.WorkDir, "host.nix"), []byte(RenderHostNix(in.Answers))); err != nil {
		return err
	}

	hw, err := in.Runner.Run(ctx, Command{
		Name:    "nixos-generate-config",
		Args:    []string{"--no-filesystems", "--show-hardware-config"},
		Timeout: timeoutQuick,
	})
	if err != nil {
		return fmt.Errorf("detect hardware: %w", err)
	}
	return writeFile(filepath.Join(in.WorkDir, "hardware-configuration.nix"), hw)
}

// storeConfiguration copies the host flake to /etc/nixos on the new
// system as a Git repository, and copies the flake's inputs (Strata,
// nixpkgs) into the new system's store so the configuration can be
// evaluated without network access.
func (in *Installer) storeConfiguration(ctx context.Context) error {
	if _, err := in.Runner.Run(ctx, Command{
		Name: "nix",
		// The sources are local paths without binary cache signatures, like
		// the system closure that nixos-install copies with --no-check-sigs.
		Args:    []string{"flake", "archive", "--offline", "--no-check-sigs", "--to", "local?root=" + in.Target, "path:" + in.WorkDir},
		Timeout: timeoutInstall,
	}); err != nil {
		return fmt.Errorf("copy configuration sources: %w", err)
	}

	dest := filepath.Join(in.Target, "etc", "nixos")
	if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s is not empty", dest)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil { //nolint:gosec // /etc/nixos is world-readable by convention.
		return fmt.Errorf("create %s: %w", dest, err)
	}
	entries, err := os.ReadDir(in.WorkDir)
	if err != nil {
		return fmt.Errorf("read work directory: %w", err)
	}
	for _, e := range entries {
		if err := copyFile(filepath.Join(in.WorkDir, e.Name()), filepath.Join(dest, e.Name())); err != nil {
			return err
		}
	}

	git := func(args ...string) error {
		base := []string{"-C", dest, "-c", "user.name=strata-install", "-c", "user.email=strata-install@localhost"}
		_, err := in.Runner.Run(ctx, Command{Name: "git", Args: append(base, args...), Timeout: timeoutQuick})
		if err != nil {
			return fmt.Errorf("git %s: %w", args[0], err)
		}
		return nil
	}
	if err := git("init", "--quiet", "--initial-branch=main"); err != nil {
		return err
	}
	if err := git("add", "--all"); err != nil {
		return err
	}
	return git("commit", "--quiet", "--message", "Initial configuration of "+in.Answers.Node.ID+" (strata-install)")
}

// confirm asks the operator to type the device path, unless ConfirmWipe
// already matches it.
func (in *Installer) confirm(d DiskInfo) error {
	device := in.Answers.Disk.Device
	if in.ConfirmWipe != "" {
		if in.ConfirmWipe != device {
			return fmt.Errorf("%w (--confirm-wipe %q does not match disk.device %q)", ErrNotConfirmed, in.ConfirmWipe, device)
		}
		in.say("Erase confirmed by --confirm-wipe.")
		return nil
	}
	in.say("\nALL DATA ON %s (/dev/%s, %s) WILL BE DESTROYED.", device, d.Name, FormatBytes(d.SizeBytes))
	in.say("Type the device path to confirm, anything else aborts:")
	line, err := bufio.NewReader(in.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(line) != device {
		return ErrNotConfirmed
	}
	return nil
}

func (in *Installer) step(n int, msg string) {
	in.say("\n==> [%d/%d] %s", n, len(Steps()), msg)
}

func (in *Installer) say(format string, args ...any) {
	_, _ = fmt.Fprintf(in.Out, format+"\n", args...)
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // Paths come from the operator's command line or the ISO.
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	return writeFile(dst, data)
}

func writeFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // Configuration files are world-readable, like /etc/nixos.
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// ExecRunner runs commands with os/exec. Standard output is captured and
// returned; standard error is passed through to Stderr.
type ExecRunner struct {
	Stderr io.Writer
	Logger *slog.Logger
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	if r.Logger != nil {
		r.Logger.DebugContext(ctx, "run", "cmd", c.String())
	}
	cmd := execCommand(ctx, c)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = r.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return stdout.Bytes(), fmt.Errorf("%s: %w", c.Name, ctx.Err())
		}
		return stdout.Bytes(), fmt.Errorf("%s: %w", c.Name, err)
	}
	return stdout.Bytes(), nil
}
