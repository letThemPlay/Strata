package install

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Wizard asks the operator for the answers interactively
// (strata-install init).
type Wizard struct {
	// Root is the running system's "/" for listing disks and interfaces.
	Root fs.FS
	P    *Prompter
	// HTTP downloads keys (NewKeyClient).
	HTTP *http.Client
	// ReadFile reads key files (os.ReadFile).
	ReadFile func(name string) ([]byte, error)
	// HashPassword returns the crypt(3) hash of a password (HashPassword
	// in this package, which runs mkpasswd).
	HashPassword func(ctx context.Context, password string) (string, error)
}

// minConsolePassword is the shortest accepted console password.
const minConsolePassword = 8

// Run asks all questions and returns validated answers.
func (w *Wizard) Run(ctx context.Context) (Answers, error) {
	a := Answers{APIVersion: APIVersion, Disk: Disk{Layout: DefaultLayout}}
	p := w.P
	var err error

	p.Say("This wizard writes an answer file for strata-install. Press Ctrl-C to abort.\n")

	if a.Node.ID, err = p.Ask("Node ID (also the hostname)", "", checkNodeID); err != nil {
		return Answers{}, err
	}
	if a.Admin.Name, err = p.Ask("Administrator account", DefaultAdminName, checkAdminName); err != nil {
		return Answers{}, err
	}
	if a.Admin.AuthorizedKeys, err = w.keys(ctx, a.Admin.Name); err != nil {
		return Answers{}, err
	}
	if a.Admin.ConsolePasswordHash, err = w.consolePassword(ctx, a.Admin.Name); err != nil {
		return Answers{}, err
	}
	if a.Disk.Device, err = w.disk(); err != nil {
		return Answers{}, err
	}
	if a.Network, err = w.network(); err != nil {
		return Answers{}, err
	}
	if err := a.Validate(); err != nil {
		// Every answer was checked when given; this is a bug.
		return Answers{}, fmt.Errorf("wizard produced invalid answers: %w", err)
	}
	return a, nil
}

func (w *Wizard) keys(ctx context.Context, admin string) ([]string, error) {
	p := w.P
	var keys []string
	p.Say("\nSSH public keys for %s. Password login is disabled, so these keys are the only way in.", admin)
	for {
		choice, err := p.Choose("Add keys:", []string{
			"paste a public key",
			"read public keys from a file (e.g. on a USB stick you mounted)",
			"download public keys over HTTPS (e.g. https://github.com/USER.keys; needs network)",
		}, nil, 0)
		if err != nil {
			return nil, err
		}
		var kl KeyList
		switch choice {
		case 0:
			k, err := p.Ask("Public key", "", func(s string) error {
				return checkSSHKey(strings.Join(strings.Fields(s), " "))
			})
			if err != nil {
				return nil, err
			}
			kl.Keys = []string{strings.Join(strings.Fields(k), " ")}
		case 1:
			name, err := p.Ask("File", "", func(s string) error {
				if s == "" {
					return errors.New("enter a path")
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			data, err := w.ReadFile(name)
			if err != nil {
				p.Say("  %v", err)
				continue
			}
			kl = ParseKeyList(string(data))
		case 2:
			u, err := p.Ask("URL", "", func(s string) error {
				if !strings.HasPrefix(s, "https://") {
					return errors.New("enter an https:// URL")
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			p.Say("  downloading...")
			if kl, err = FetchKeys(ctx, w.HTTP, u); err != nil {
				if ctx.Err() != nil {
					return nil, fmt.Errorf("wizard: %w", ctx.Err())
				}
				p.Say("  %v", err)
				continue
			}
		}

		for i, r := range kl.Rejected {
			if i == keyListMaxReport {
				p.Say("  ... and %d more unusable line(s)", len(kl.Rejected)-i)
				break
			}
			p.Say("  skipped %s", r)
		}
		var added []string
		for _, k := range kl.Keys {
			if !slices.Contains(keys, k) {
				added = append(added, k)
			}
		}
		if len(added) == 0 {
			p.Say("  no new keys found")
		} else {
			p.Say("  found %d key(s):", len(added))
			for _, k := range added {
				p.Say("    %s", DescribeKey(k))
			}
			p.Say("  Compare the fingerprints with ssh-keygen -lf on your workstation.")
			ok, err := p.Confirm("Use these keys?", true)
			if err != nil {
				return nil, err
			}
			if ok {
				keys = append(keys, added...)
			}
		}

		if len(keys) == 0 {
			p.Say("At least one key is required.")
			continue
		}
		more, err := p.Confirm(fmt.Sprintf("%d key(s) added. Add more?", len(keys)), false)
		if err != nil {
			return nil, err
		}
		if !more {
			return keys, nil
		}
	}
}

// consolePassword asks for an optional console password and returns its
// hash, or "" for none. The password itself is never stored.
func (w *Wizard) consolePassword(ctx context.Context, admin string) (string, error) {
	p := w.P
	p.Say("\nOptional console password for %s. It allows logging in on the local console", admin)
	p.Say("(or through a remote KVM) to recover when the network is unreachable. SSH never")
	p.Say("accepts passwords. Leave it empty for none; recovery then requires the installer.")
	for {
		pw, err := p.Secret("Console password")
		if err != nil {
			return "", err
		}
		if pw == "" {
			p.Say("  no console password")
			return "", nil
		}
		if len([]rune(pw)) < minConsolePassword {
			p.Say("  use at least %d characters", minConsolePassword)
			continue
		}
		again, err := p.Secret("Repeat it")
		if err != nil {
			return "", err
		}
		if again != pw {
			p.Say("  the passwords differ")
			continue
		}
		h, err := w.HashPassword(ctx, pw)
		if err != nil {
			return "", fmt.Errorf("hash the console password: %w", err)
		}
		if !passwordHashPattern.MatchString(h) {
			return "", errors.New("hash the console password: unexpected output from the hashing tool")
		}
		p.Say("  console password set (only its hash is kept)")
		return h, nil
	}
}

func (w *Wizard) disk() (string, error) {
	p := w.P
	disks, err := DiskCandidates(w.Root)
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(disks, func(d DiskCandidate) bool { return d.Problem == nil }) {
		for _, d := range disks {
			p.Say("  %s: %v", d.Device, d.Problem)
		}
		return "", errors.New("no usable disk found (whole disk, at least " + FormatBytes(MinDiskBytes) + ", not in use)")
	}
	items := make([]string, len(disks))
	disabled := make([]string, len(disks))
	def, usable := -1, 0
	for i, d := range disks {
		desc := fmt.Sprintf("%s  %s", d.Device, FormatBytes(d.Info.SizeBytes))
		if d.Info.Model != "" {
			desc += ", " + d.Info.Model
		}
		if d.Info.Removable {
			desc += ", removable"
		}
		if n := len(d.Info.Partitions); n > 0 {
			desc += fmt.Sprintf(", %d partition(s)", n)
		}
		if len(d.Info.ExistingData) > 0 {
			desc += ", HAS EXISTING DATA"
		}
		items[i] = desc
		if d.Problem != nil {
			disabled[i] = d.Problem.Error()
		} else {
			usable++
			def = i
		}
	}
	if usable > 1 {
		def = -1 // Make the operator choose.
	}
	var i int
	for {
		p.Say("")
		if i, err = p.Choose("System disk (it will be erased completely):", items, disabled, def); err != nil {
			return "", err
		}
		data := disks[i].Info.ExistingData
		if len(data) == 0 {
			break
		}
		p.Say("  %s contains existing data that will be destroyed:", disks[i].Device)
		for _, e := range data {
			p.Say("    %s", e)
		}
		erase, err := p.Confirm("Erase this data?", false)
		if err != nil {
			return "", err
		}
		if erase {
			break
		}
	}
	if !disks[i].Stable() {
		p.Say("  note: %s has no /dev/disk/by-id/ name; its name may change between boots.", disks[i].Device)
	}
	return disks[i].Device, nil
}

func (w *Wizard) network() (Network, error) {
	p := w.P
	name, err := w.managementInterface()
	if err != nil {
		return Network{}, err
	}
	n := Network{Interface: name}

	mode, err := p.Choose("Addressing:", []string{"DHCP", "static"}, nil, 0)
	if err != nil {
		return Network{}, err
	}
	if mode == 0 {
		n.Addressing = "dhcp"
		return n, nil
	}
	n.Addressing = "static"
	if n.Addresses, err = w.list("Addresses with prefix length (e.g. 192.0.2.10/24 2001:db8::10/64)", true, checkAddressPrefix); err != nil {
		return Network{}, err
	}
	optional := func(check func(string) error) func(string) error {
		return func(s string) error {
			if s == "" {
				return nil
			}
			return check(s)
		}
	}
	if n.Gateway4, err = p.Ask("IPv4 gateway (empty for none)", "", optional(checkIPv4)); err != nil {
		return Network{}, err
	}
	if n.Gateway6, err = p.Ask("IPv6 gateway (empty for none)", "", optional(checkIPv6)); err != nil {
		return Network{}, err
	}
	if n.DNS, err = w.list("DNS servers (empty for none)", false, checkIP); err != nil {
		return Network{}, err
	}
	return n, nil
}

// managementInterface asks for the management interface. An interface
// with a link is the default; without any link there is no default, and
// an empty answer re-scans, so the operator can connect the cable to
// identify the port. Only this interface is configured on the installed
// host: the others stay down.
func (w *Wizard) managementInterface() (string, error) {
	p := w.P
	for {
		nics, err := InterfaceCandidates(w.Root)
		if err != nil {
			return "", err
		}
		if len(nics) == 0 {
			return "", errors.New("no network interface found")
		}
		items := make([]string, len(nics))
		def := -1
		for i, n := range nics {
			state := "no link"
			if n.OperState == "up" {
				state = "link up"
				if def < 0 {
					def = i
				}
			}
			items[i] = fmt.Sprintf("%-15s %s  %s", n.Name, n.MACAddress, state)
		}
		p.Say("")
		if def >= 0 {
			i, err := p.Choose("Management network interface (the only one configured on the host):", items, nil, def)
			if err != nil {
				return "", err
			}
			return nics[i].Name, nil
		}

		p.Say("Management network interface (the only one configured on the host):")
		for i, item := range items {
			p.Say("  %d) %s", i+1, item)
		}
		p.Say("No interface has a link. Connect the management cable and press Enter to")
		p.Say("re-scan, so that the right port shows \"link up\"; or choose a number.")
		answer, err := p.Ask("Choice (Enter to re-scan)", "", func(s string) error {
			if s == "" {
				return nil
			}
			if n, err := strconv.Atoi(s); err != nil || n < 1 || n > len(nics) {
				return errors.New("enter a number from the list, or nothing to re-scan")
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		if answer == "" {
			continue
		}
		n, _ := strconv.Atoi(answer)
		return nics[n-1].Name, nil
	}
}

// list asks for space- or comma-separated values.
func (w *Wizard) list(question string, required bool, check func(string) error) ([]string, error) {
	split := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	}
	answer, err := w.P.Ask(question, "", func(s string) error {
		items := split(s)
		if required && len(items) == 0 {
			return errors.New("at least one is required")
		}
		var errs []error
		for _, item := range items {
			if err := check(item); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
	if err != nil {
		return nil, err
	}
	return split(answer), nil
}

// WriteAnswers writes a as an indented JSON answer file, replacing name
// atomically.
func WriteAnswers(name string, a Answers) error {
	data, err := MarshalAnswers(a)
	if err != nil {
		return err
	}
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // Public keys and host settings only.
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, name); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}
