// Package install implements strata-install, the Strata host installer.
//
// The installer reads a versioned answer file, generates a host
// configuration flake from the template shipped on the installer ISO,
// builds the system offline, and only then, after explicit confirmation,
// formats the target disk and installs. See ADR-0014.
package install

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// APIVersion identifies the answer file schema.
const APIVersion = "install.strata.dev/v1alpha1"

// Defaults applied by Parse for optional fields.
const (
	DefaultAdminName = "strata-admin"
	DefaultLayout    = "ext4"
)

// Answers is the content of an answer file.
type Answers struct {
	APIVersion string  `json:"apiVersion"`
	Node       Node    `json:"node"`
	Admin      Admin   `json:"admin"`
	Disk       Disk    `json:"disk"`
	Network    Network `json:"network"`
}

// Node identifies the host (strata.node).
type Node struct {
	// ID becomes strata.node.id and the hostname.
	ID string `json:"id"`
}

// Admin configures the administrator account (strata.access.admin).
type Admin struct {
	// Name of the admin account. Defaults to DefaultAdminName.
	Name string `json:"name,omitempty"`
	// AuthorizedKeys are the OpenSSH public keys allowed to log in.
	AuthorizedKeys []string `json:"authorizedKeys"`
	// ConsolePasswordHash, if set, is the crypt(3) hash of a password for
	// local console login (recovery). SSH never accepts passwords. The
	// installer stores it in ConsolePasswordPath on the installed system,
	// outside the Nix store and the host's Git repository.
	ConsolePasswordHash string `json:"consolePasswordHash,omitempty"`
}

// ConsolePasswordPath is where the installed system keeps the console
// password hash (strata.access.admin.consolePasswordFile).
const ConsolePasswordPath = "/etc/strata/admin-console-password" //nolint:gosec // A path, not a credential.

// Disk selects the system disk (strata.disk).
type Disk struct {
	// Device is the disk to format, preferably under /dev/disk/by-id/.
	Device string `json:"device"`
	// Layout is the partition layout. Defaults to DefaultLayout.
	Layout string `json:"layout,omitempty"`
}

// Network configures the management interface
// (strata.networking.management).
type Network struct {
	Interface  string   `json:"interface"`
	Addressing string   `json:"addressing"`
	Addresses  []string `json:"addresses,omitempty"`
	Gateway4   string   `json:"gateway4,omitempty"`
	Gateway6   string   `json:"gateway6,omitempty"`
	DNS        []string `json:"dns,omitempty"`
}

// Validation patterns. They match the NixOS option types of the Strata
// modules; where those are looser (device paths, key comments), the
// installer is stricter, so that every accepted value is also safe to
// write into a Nix string.
var (
	nodeIDPattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	userNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	sshKeyPattern   = regexp.MustCompile(`^(ssh-(ed25519|rsa)|ecdsa-sha2-nistp(256|384|521)|sk-(ssh-ed25519|ecdsa-sha2-nistp256)@openssh\.com) [A-Za-z0-9+/]+=*( [^\x00-\x1f\x7f]*)?$`)
	// Hashes as written by mkpasswd/crypt(3): yescrypt, gost-yescrypt,
	// scrypt, bcrypt, SHA-512 and SHA-256 crypt. Locked or empty values
	// ("!", "*", "") are not hashes.
	passwordHashPattern = regexp.MustCompile(`^\$(y|gy|7|2[aby]|6|5)\$[A-Za-z0-9./$=,]{8,500}$`)
	devicePattern       = regexp.MustCompile(`^/dev/[A-Za-z0-9._:+@/-]+$`)
	interfacePattern    = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,15}$`)
)

// layouts lists the supported disk layouts (installer/partitioning).
func layouts() []string { return []string{"ext4"} }

// Parse decodes and validates an answer file. Unknown fields, trailing
// data and an unexpected apiVersion are errors. Defaults are filled in.
func Parse(r io.Reader) (Answers, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var a Answers
	if err := dec.Decode(&a); err != nil {
		return Answers{}, fmt.Errorf("answer file: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Answers{}, errors.New("answer file: unexpected data after the JSON object")
	}

	if a.Admin.Name == "" {
		a.Admin.Name = DefaultAdminName
	}
	if a.Disk.Layout == "" {
		a.Disk.Layout = DefaultLayout
	}
	if err := a.Validate(); err != nil {
		return Answers{}, err
	}
	return a, nil
}

// Validate reports every problem in a, not just the first.
func (a Answers) Validate() error {
	var errs []error
	add := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
	}

	if a.APIVersion != APIVersion {
		add("apiVersion", "must be %q, got %q", APIVersion, a.APIVersion)
	}

	if err := checkNodeID(a.Node.ID); err != nil {
		add("node.id", "%v", err)
	}
	if err := checkAdminName(a.Admin.Name); err != nil {
		add("admin.name", "%v", err)
	}
	if len(a.Admin.AuthorizedKeys) == 0 {
		add("admin.authorizedKeys", "at least one key is required, otherwise nobody can log in")
	}
	for i, k := range a.Admin.AuthorizedKeys {
		if err := checkSSHKey(k); err != nil {
			add(fmt.Sprintf("admin.authorizedKeys[%d]", i), "%v", err)
		}
	}
	if h := a.Admin.ConsolePasswordHash; h != "" && !passwordHashPattern.MatchString(h) {
		add("admin.consolePasswordHash", "not a supported password hash (e.g. from `mkpasswd -m yescrypt`); never put a plain password here")
	}

	if !devicePattern.MatchString(a.Disk.Device) || strings.Contains(a.Disk.Device, "/../") {
		add("disk.device", "%q is not a device path under /dev/", a.Disk.Device)
	}
	if !slices.Contains(layouts(), a.Disk.Layout) {
		add("disk.layout", "unknown layout %q (supported: %s)", a.Disk.Layout, strings.Join(layouts(), ", "))
	}

	errs = append(errs, a.Network.validate()...)
	return errors.Join(errs...)
}

func (n Network) validate() []error {
	var errs []error
	add := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("network.%s: %s", field, fmt.Sprintf(format, args...)))
	}

	if !interfacePattern.MatchString(n.Interface) {
		add("interface", "%q is not a valid interface name", n.Interface)
	}

	switch n.Addressing {
	case "static":
		if len(n.Addresses) == 0 {
			add("addresses", "at least one address is required with static addressing")
		}
	case "dhcp":
		if len(n.Addresses) > 0 || n.Gateway4 != "" || n.Gateway6 != "" || len(n.DNS) > 0 {
			add("addressing", "addresses, gateways and dns are only allowed with static addressing")
		}
	default:
		add("addressing", "must be \"static\" or \"dhcp\", got %q", n.Addressing)
	}

	for i, s := range n.Addresses {
		if err := checkAddressPrefix(s); err != nil {
			add(fmt.Sprintf("addresses[%d]", i), "%v", err)
		}
	}
	if n.Gateway4 != "" {
		if err := checkIPv4(n.Gateway4); err != nil {
			add("gateway4", "%v", err)
		}
	}
	if n.Gateway6 != "" {
		if err := checkIPv6(n.Gateway6); err != nil {
			add("gateway6", "%v", err)
		}
	}
	for i, s := range n.DNS {
		if err := checkIP(s); err != nil {
			add(fmt.Sprintf("dns[%d]", i), "%v", err)
		}
	}
	return errs
}

// Field checks shared by Validate and the init wizard.

func checkNodeID(s string) error {
	if !nodeIDPattern.MatchString(s) {
		return fmt.Errorf("%q is not a valid node ID (lowercase letters, digits and '-', 1-63 characters, not starting or ending with '-')", s)
	}
	return nil
}

func checkAdminName(s string) error {
	switch {
	case !userNamePattern.MatchString(s):
		return fmt.Errorf("%q is not a valid user name", s)
	case s == "root":
		return errors.New("must not be root (root login is disabled)")
	}
	return nil
}

// checkSSHKey checks that k is one OpenSSH public key line whose key data
// is valid base64 and starts with the key type the line names.
func checkSSHKey(k string) error {
	const msg = "not a supported OpenSSH public key (one line: type, base64 key, optional comment)"
	if !sshKeyPattern.MatchString(k) {
		return errors.New(msg)
	}
	fields := strings.SplitN(k, " ", 3)
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return errors.New(msg + ": the key data is not valid base64")
	}
	// The blob starts with the key type as an SSH string: a 4-byte
	// big-endian length and the name.
	if len(blob) < 4 {
		return errors.New(msg + ": the key data is truncated")
	}
	n := binary.BigEndian.Uint32(blob)
	if rest := blob[4:]; int64(n) > int64(len(rest)) || string(rest[:n]) != fields[0] {
		return fmt.Errorf("%s: the key data is not a %s key", msg, fields[0])
	}
	return nil
}

func checkAddressPrefix(s string) error {
	if p, err := netip.ParsePrefix(s); err != nil || p.Addr().Zone() != "" {
		return fmt.Errorf("%q is not an address with prefix length (e.g. 192.0.2.10/24)", s)
	}
	return nil
}

func checkIPv4(s string) error {
	if a, err := netip.ParseAddr(s); err != nil || !a.Is4() {
		return fmt.Errorf("%q is not an IPv4 address", s)
	}
	return nil
}

func checkIPv6(s string) error {
	if a, err := netip.ParseAddr(s); err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" {
		return fmt.Errorf("%q is not an IPv6 address", s)
	}
	return nil
}

func checkIP(s string) error {
	if a, err := netip.ParseAddr(s); err != nil || a.Zone() != "" {
		return fmt.Errorf("%q is not an IP address", s)
	}
	return nil
}

// MarshalAnswers encodes a as an indented answer file.
func MarshalAnswers(a Answers) ([]byte, error) {
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode answers: %w", err)
	}
	return append(data, '\n'), nil
}
