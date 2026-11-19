// Package hardware discovers the hardware of the local node.
//
// Discovery only reads kernel-provided files (procfs and sysfs) through an
// fs.FS rooted at the host's "/" directory. It never executes external
// commands, which keeps it safe to run unprivileged and makes it testable
// with in-memory fixtures (see testing/fstest).
package hardware

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// APIVersion identifies the schema of Report. It must change whenever the
// serialized form changes incompatibly.
const APIVersion = "hardware.strata.dev/v1alpha1"

// Virtualization extension identifiers reported in Virtualization.Extension.
const (
	ExtensionIntelVTx = "vmx"
	ExtensionAMDV     = "svm"
)

// sectorSize is the unit of /sys/block/<dev>/size, independent of the
// device's logical block size.
const sectorSize = 512

// Report is the hardware inventory of a single node.
type Report struct {
	APIVersion        string             `json:"apiVersion"`
	CPU               CPU                `json:"cpu"`
	Memory            Memory             `json:"memory"`
	Firmware          Firmware           `json:"firmware"`
	System            System             `json:"system"`
	Virtualization    Virtualization     `json:"virtualization"`
	NetworkInterfaces []NetworkInterface `json:"networkInterfaces"`
	BlockDevices      []BlockDevice      `json:"blockDevices"`
}

// CPU describes the host processors.
type CPU struct {
	Vendor       string `json:"vendor,omitempty"`
	Model        string `json:"model,omitempty"`
	LogicalCount int    `json:"logicalCount"`
}

// Memory describes installed memory.
type Memory struct {
	TotalBytes uint64 `json:"totalBytes"`
}

// Firmware describes the boot firmware.
type Firmware struct {
	UEFI bool `json:"uefi"`
}

// System holds DMI identification readable without privileges.
type System struct {
	Vendor  string `json:"vendor,omitempty"`
	Product string `json:"product,omitempty"`
}

// Virtualization describes hardware virtualization support.
type Virtualization struct {
	// Extension is "vmx" (Intel VT-x), "svm" (AMD-V) or empty if the CPU
	// does not advertise hardware virtualization.
	Extension string `json:"extension"`
	// KVMDevice reports whether /dev/kvm exists, i.e. a KVM module is loaded.
	KVMDevice bool `json:"kvmDevice"`
	// IOMMUGroups is the number of IOMMU groups; zero means the IOMMU is
	// disabled or unsupported.
	IOMMUGroups int `json:"iommuGroups"`
}

// NetworkInterface describes a network interface.
type NetworkInterface struct {
	Name       string `json:"name"`
	MACAddress string `json:"macAddress,omitempty"`
	OperState  string `json:"operState,omitempty"`
	// Physical is true when the interface is backed by a device rather than
	// being virtual (bridge, bond, veth, tap, ...).
	Physical bool `json:"physical"`
}

// BlockDevice describes a whole-disk block device.
type BlockDevice struct {
	Name       string `json:"name"`
	SizeBytes  uint64 `json:"sizeBytes"`
	Model      string `json:"model,omitempty"`
	Rotational bool   `json:"rotational"`
	Removable  bool   `json:"removable"`
}

// Discover builds a Report from the filesystem rooted at fsys, which must
// correspond to the host's "/" (for example os.DirFS("/")).
//
// /proc/cpuinfo and /proc/meminfo are required; all other sources are
// optional and yield zero values when absent.
func Discover(fsys fs.FS) (Report, error) {
	cpu, flags, err := discoverCPU(fsys)
	if err != nil {
		return Report{}, err
	}
	mem, err := discoverMemory(fsys)
	if err != nil {
		return Report{}, err
	}
	nics, err := discoverNetworkInterfaces(fsys)
	if err != nil {
		return Report{}, err
	}
	disks, err := discoverBlockDevices(fsys)
	if err != nil {
		return Report{}, err
	}

	return Report{
		APIVersion: APIVersion,
		CPU:        cpu,
		Memory:     mem,
		Firmware:   Firmware{UEFI: exists(fsys, "sys/firmware/efi")},
		System: System{
			Vendor:  readTrimmed(fsys, "sys/class/dmi/id/sys_vendor"),
			Product: readTrimmed(fsys, "sys/class/dmi/id/product_name"),
		},
		Virtualization: Virtualization{
			Extension:   virtualizationExtension(flags),
			KVMDevice:   exists(fsys, "dev/kvm"),
			IOMMUGroups: countEntries(fsys, "sys/kernel/iommu_groups"),
		},
		NetworkInterfaces: nics,
		BlockDevices:      disks,
	}, nil
}

func discoverCPU(fsys fs.FS) (CPU, map[string]bool, error) {
	const name = "proc/cpuinfo"
	f, err := fsys.Open(name)
	if err != nil {
		return CPU{}, nil, fmt.Errorf("read /%s: %w", name, err)
	}
	defer func() { _ = f.Close() }()

	var cpu CPU
	flags := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // flags lines are long.
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "processor":
			cpu.LogicalCount++
		case "vendor_id":
			if cpu.Vendor == "" {
				cpu.Vendor = value
			}
		case "model name":
			if cpu.Model == "" {
				cpu.Model = value
			}
		case "flags":
			for _, fl := range strings.Fields(value) {
				flags[fl] = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return CPU{}, nil, fmt.Errorf("parse /%s: %w", name, err)
	}
	if cpu.LogicalCount == 0 {
		return CPU{}, nil, fmt.Errorf("parse /%s: no processor entries found", name)
	}
	return cpu, flags, nil
}

func virtualizationExtension(flags map[string]bool) string {
	switch {
	case flags[ExtensionIntelVTx]:
		return ExtensionIntelVTx
	case flags[ExtensionAMDV]:
		return ExtensionAMDV
	default:
		return ""
	}
}

func discoverMemory(fsys fs.FS) (Memory, error) {
	const name = "proc/meminfo"
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Memory{}, fmt.Errorf("read /%s: %w", name, err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return Memory{}, fmt.Errorf("parse /%s: unexpected MemTotal format %q", name, line)
		}
		kib, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return Memory{}, fmt.Errorf("parse /%s: MemTotal: %w", name, err)
		}
		return Memory{TotalBytes: kib * 1024}, nil
	}
	return Memory{}, fmt.Errorf("parse /%s: MemTotal not found", name)
}

func discoverNetworkInterfaces(fsys fs.FS) ([]NetworkInterface, error) {
	const dir = "sys/class/net"
	names, err := listDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	nics := make([]NetworkInterface, 0, len(names))
	for _, n := range names {
		if n == "lo" {
			continue
		}
		base := path.Join(dir, n)
		nics = append(nics, NetworkInterface{
			Name:       n,
			MACAddress: readTrimmed(fsys, path.Join(base, "address")),
			OperState:  readTrimmed(fsys, path.Join(base, "operstate")),
			Physical:   exists(fsys, path.Join(base, "device")),
		})
	}
	return nics, nil
}

// virtualBlockPrefixes lists block devices that are not physical disks.
var virtualBlockPrefixes = []string{"loop", "ram", "zram", "dm-", "md", "nbd", "sr"} //nolint:gochecknoglobals // Read-only lookup table.

func discoverBlockDevices(fsys fs.FS) ([]BlockDevice, error) {
	const dir = "sys/block"
	names, err := listDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	disks := make([]BlockDevice, 0, len(names))
	for _, n := range names {
		if isVirtualBlock(n) {
			continue
		}
		base := path.Join(dir, n)
		var size uint64
		if s := readTrimmed(fsys, path.Join(base, "size")); s != "" {
			sectors, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse /%s/size: %w", base, err)
			}
			size = sectors * sectorSize
		}
		disks = append(disks, BlockDevice{
			Name:       n,
			SizeBytes:  size,
			Model:      readTrimmed(fsys, path.Join(base, "device/model")),
			Rotational: readTrimmed(fsys, path.Join(base, "queue/rotational")) == "1",
			Removable:  readTrimmed(fsys, path.Join(base, "removable")) == "1",
		})
	}
	return disks, nil
}

func isVirtualBlock(name string) bool {
	for _, p := range virtualBlockPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// listDir returns the sorted entry names of dir. A missing directory yields
// no entries; other errors are returned.
func listDir(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list /%s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

func countEntries(fsys fs.FS, dir string) int {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return 0
	}
	return len(entries)
}

func exists(fsys fs.FS, name string) bool {
	_, err := fs.Stat(fsys, name)
	return err == nil
}

// readTrimmed returns the trimmed content of an optional file, or "" if it
// cannot be read.
func readTrimmed(fsys fs.FS, name string) string {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
