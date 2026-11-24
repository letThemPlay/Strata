// Package workload defines Strata's workload schemas, independent of the
// backend that runs them (ADR-0015, ADR-0021).
//
// A VM spec (vm.strata.dev/v1alpha1) describes a virtual machine: its
// resources, disks, network interfaces and boot image. In M2 the CLI
// applies specs imperatively through the agent; in M3 the same schema is
// read from Git and reconciled.
package workload

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// VMAPIVersion identifies the VM schema.
const VMAPIVersion = "vm.strata.dev/v1alpha1"

// RootDisk is the name of a VM's boot disk.
const RootDisk = "root"

// Limits of the schema.
const (
	MaxCPU           = 256
	MinMemoryBytes   = 128 << 20
	MinRootDiskBytes = 1 << 30
	maxDisks         = 16
	maxNICs          = 8
	maxSpecBytes     = 64 << 10
)

// VM is a virtual machine spec.
type VM struct {
	APIVersion string    `json:"apiVersion"`
	Name       string    `json:"name"`
	Resources  Resources `json:"resources"`
	// Image is a local image alias in the Strata project. Empty: an empty
	// VM (to be installed later).
	Image     string     `json:"image,omitempty"`
	Disks     []Disk     `json:"disks"`
	Networks  []NIC      `json:"networks,omitempty"`
	Autostart bool       `json:"autostart,omitempty"`
	CloudInit *CloudInit `json:"cloudInit,omitempty"`
}

// Resources are a VM's CPU and memory.
type Resources struct {
	CPU    int    `json:"cpu"`
	Memory string `json:"memory"`
}

// Disk is a VM disk. The disk named "root" is the boot disk and belongs
// to the VM; other disks are data volumes named "<vm>-<disk>".
type Disk struct {
	Name string `json:"name"`
	Size string `json:"size"`
	Pool string `json:"pool"`
}

// NIC is a VM network interface on a guest network.
type NIC struct {
	Name    string `json:"name"`
	Network string `json:"network"`
}

// CloudInit configures the guest on first boot (images with cloud-init).
type CloudInit struct {
	SSHAuthorizedKeys []string `json:"sshAuthorizedKeys,omitempty"`
}

var (
	vmNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	diskNamePattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	nicNamePattern   = regexp.MustCompile(`^[a-z][a-z0-9]{0,14}$`)
	objectPattern    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)
	imagePattern     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	sizePattern      = regexp.MustCompile(`^([0-9]{1,15})(B|KiB|MiB|GiB|TiB)$`)
	sshKeyPattern    = regexp.MustCompile(`^(ssh-(ed25519|rsa|dss)|ecdsa-sha2-nistp(256|384|521)|sk-(ssh-ed25519|ecdsa-sha2-nistp256)@openssh\.com) [A-Za-z0-9+/]+={0,3}( [^\n]*)?$`)
	errUnknownFields = errors.New("unknown field")
)

// ParseVM decodes and validates a VM spec. Unknown fields are rejected.
func ParseVM(r io.Reader) (VM, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSpecBytes+1))
	if err != nil {
		return VM{}, fmt.Errorf("read VM spec: %w", err)
	}
	if len(data) > maxSpecBytes {
		return VM{}, fmt.Errorf("VM spec exceeds %d bytes", maxSpecBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var vm VM
	if err := dec.Decode(&vm); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return VM{}, fmt.Errorf("VM spec: %w: %s", errUnknownFields, strings.TrimPrefix(err.Error(), "json: "))
		}
		return VM{}, fmt.Errorf("VM spec: %w", err)
	}
	if dec.More() {
		return VM{}, errors.New("VM spec: trailing data after the JSON object")
	}
	if err := vm.Validate(); err != nil {
		return VM{}, err
	}
	return vm, nil
}

// Validate checks a VM spec and reports every problem found.
func (vm VM) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if vm.APIVersion != VMAPIVersion {
		add("apiVersion: must be %q, got %q", VMAPIVersion, vm.APIVersion)
	}
	if !vmNamePattern.MatchString(vm.Name) {
		add("name: %q must be 1-63 lowercase letters, digits and hyphens, starting with a letter", vm.Name)
	}
	if vm.Resources.CPU < 1 || vm.Resources.CPU > MaxCPU {
		add("resources.cpu: must be 1-%d, got %d", MaxCPU, vm.Resources.CPU)
	}
	if n, err := ParseSize(vm.Resources.Memory); err != nil {
		add("resources.memory: %v", err)
	} else if n < MinMemoryBytes {
		add("resources.memory: at least 128MiB")
	}
	if vm.Image != "" && !imagePattern.MatchString(vm.Image) {
		add("image: %q must be a local image alias (letters, digits, '.', '_', '-'); remote images (server:alias) are not allowed", vm.Image)
	}

	if len(vm.Disks) == 0 || vm.Disks[0].Name != RootDisk {
		add("disks: the first disk must be named %q (the boot disk)", RootDisk)
	}
	if len(vm.Disks) > maxDisks {
		add("disks: at most %d", maxDisks)
	}
	seen := map[string]bool{}
	for i, d := range vm.Disks {
		where := fmt.Sprintf("disks[%d]", i)
		if !diskNamePattern.MatchString(d.Name) {
			add("%s.name: %q must be 1-31 lowercase letters, digits and hyphens, starting with a letter", where, d.Name)
		}
		if seen[d.Name] {
			add("%s.name: %q is used twice", where, d.Name)
		}
		seen[d.Name] = true
		if !objectPattern.MatchString(d.Pool) {
			add("%s.pool: %q is not a valid pool name", where, d.Pool)
		}
		n, err := ParseSize(d.Size)
		switch {
		case err != nil:
			add("%s.size: %v", where, err)
		case d.Name == RootDisk && n < MinRootDiskBytes:
			add("%s.size: the root disk needs at least 1GiB", where)
		case n == 0:
			add("%s.size: must not be zero", where)
		}
		if d.Name != RootDisk && len(vm.Name)+1+len(d.Name) > 63 {
			add("%s.name: volume name %q is longer than 63 characters", where, vm.Name+"-"+d.Name)
		}
	}

	if len(vm.Networks) > maxNICs {
		add("networks: at most %d", maxNICs)
	}
	seenNIC := map[string]bool{}
	for i, n := range vm.Networks {
		where := fmt.Sprintf("networks[%d]", i)
		if !nicNamePattern.MatchString(n.Name) || n.Name == RootDisk || seen[n.Name] {
			add("%s.name: %q must be 1-15 lowercase letters and digits, starting with a letter, and not a disk name", where, n.Name)
		}
		if seenNIC[n.Name] {
			add("%s.name: %q is used twice", where, n.Name)
		}
		seenNIC[n.Name] = true
		if !objectPattern.MatchString(n.Network) {
			add("%s.network: %q is not a valid network name", where, n.Network)
		}
	}

	if vm.CloudInit != nil {
		for i, k := range vm.CloudInit.SSHAuthorizedKeys {
			if !sshKeyPattern.MatchString(k) {
				add("cloudInit.sshAuthorizedKeys[%d]: not an OpenSSH public key", i)
			}
		}
	}
	return errors.Join(errs...)
}

// VolumeName is the backend volume name of data disk d of vm.
func (vm VM) VolumeName(d Disk) string { return vm.Name + "-" + d.Name }

// ParseSize parses a size like "20GiB" into bytes.
func ParseSize(s string) (uint64, error) {
	m := sizePattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q must be a number followed by B, KiB, MiB, GiB or TiB", s)
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}
	mult := uint64(1)
	switch m[2] {
	case "KiB":
		mult = 1 << 10
	case "MiB":
		mult = 1 << 20
	case "GiB":
		mult = 1 << 30
	case "TiB":
		mult = 1 << 40
	}
	if n > (1<<64-1)/mult {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return n * mult, nil
}

// VMStatus is a VM as observed in the backend.
type VMStatus struct {
	Spec VM `json:"spec"`
	// Status is the backend's status, e.g. "Running", "Stopped".
	Status string `json:"status"`
	// Addresses are the guest's global IP addresses, if known.
	Addresses []string `json:"addresses,omitempty"`
}

// Errors returned by workload providers.
var (
	// ErrNotFound: no Strata-managed workload of that name.
	ErrNotFound = errors.New("not found")
	// ErrExists: a workload or volume of that name already exists.
	ErrExists = errors.New("already exists")
	// ErrVerify: the result does not match the spec.
	ErrVerify = errors.New("verification failed")
)

// VM actions.
const (
	ActionStart   = "start"
	ActionStop    = "stop"
	ActionRestart = "restart"
)
