package install

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"

	"github.com/letthemplay/Strata/internal/blockdev"
	"github.com/letthemplay/Strata/internal/hardware"
)

var macPattern = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// MinDiskBytes is the smallest supported system disk.
const MinDiskBytes = 16 << 30

// DiskInfo describes the target disk as found on the running system.
type DiskInfo struct {
	// Device is the path from the answer file.
	Device string
	// Name is the kernel name the device resolves to, e.g. "nvme0n1".
	Name       string
	SizeBytes  uint64
	Model      string
	Removable  bool
	Partitions []Partition
	// ExistingData describes filesystems and volume or pool memberships
	// found on the disk or its partitions, e.g. `/dev/sdd1: zfs_member
	// "tank"`. They are not in use (Probe rejects those), but erasing them
	// destroys their data.
	ExistingData []string
	// DataUnknown is set when existing data could not be checked (no udev
	// database).
	DataUnknown bool
}

// Partition is an existing partition on the target disk.
type Partition = blockdev.Partition

// Probe checks the answer file against the running system: the target
// disk must be an existing, writable whole disk of at least MinDiskBytes
// with nothing on it mounted or used as swap, and the management
// interface must exist. root is the system's "/" (os.DirFS("/") outside
// tests). It only reads /dev, /sys and /proc.
func Probe(root fs.FS, a Answers) (DiskInfo, error) {
	report, err := hardware.Discover(root)
	if err != nil {
		return DiskInfo{}, fmt.Errorf("probe: %w", err)
	}

	disk, diskErr := probeDisk(root, report, a.Disk.Device)

	var nicErr error
	if !slices.ContainsFunc(report.NetworkInterfaces, func(n hardware.NetworkInterface) bool {
		return n.Name == a.Network.Interface
	}) {
		nicErr = fmt.Errorf("network.interface: no interface named %q (found: %s)",
			a.Network.Interface, interfaceNames(report))
	} else if _, err := InterfaceMAC(root, a.Network.Interface); err != nil {
		nicErr = err
	}
	return disk, errors.Join(diskErr, nicErr)
}

func probeDisk(root fs.FS, report hardware.Report, device string) (DiskInfo, error) {
	info := DiskInfo{Device: device}

	name, err := blockdev.Resolve(root, device)
	if err != nil {
		return info, fmt.Errorf("disk.device: %w", err)
	}
	info.Name = name

	idx := slices.IndexFunc(report.BlockDevices, func(d hardware.BlockDevice) bool { return d.Name == name })
	if idx < 0 {
		return info, fmt.Errorf("disk.device: %s (%s) is not a whole disk", device, name)
	}
	d := report.BlockDevices[idx]
	info.SizeBytes, info.Model, info.Removable = d.SizeBytes, d.Model, d.Removable

	if info.Partitions, err = blockdev.Partitions(root, name); err != nil {
		return info, fmt.Errorf("disk.device: %w", err)
	}
	info.ExistingData, info.DataUnknown = existingData(root, name, info.Partitions)

	var errs []error
	if d.SizeBytes < MinDiskBytes {
		errs = append(errs, fmt.Errorf("disk.device: %s is %s; at least %s is required",
			device, FormatBytes(d.SizeBytes), FormatBytes(MinDiskBytes)))
	}
	if ro, _ := fs.ReadFile(root, "sys/block/"+name+"/ro"); strings.TrimSpace(string(ro)) == "1" {
		errs = append(errs, fmt.Errorf("disk.device: %s is read-only", device))
	}
	inUse, err := blockdev.MountsAndSwap(root)
	if err != nil {
		return info, fmt.Errorf("disk.device: %w", err)
	}
	for _, dev := range append([]string{name}, blockdev.PartitionNames(info.Partitions)...) {
		if use, ok := inUse[dev]; ok {
			errs = append(errs, fmt.Errorf("disk.device: %s is in use (/dev/%s %s); this may be the installation medium", device, dev, use))
		}
		// Holders are device-mapper (LVM, LUKS) or md RAID devices built on
		// this disk or partition.
		if holders := blockdev.Holders(root, name, dev); len(holders) > 0 {
			errs = append(errs, fmt.Errorf("disk.device: %s is in use (/dev/%s is part of /dev/%s); deactivate it first", device, dev, holders[0]))
		}
	}
	return info, errors.Join(errs...)
}

// existingData lists the filesystem and membership signatures that udev
// found (with blkid) on disk name and its partitions. It reports unknown
// if there is no udev database.
func existingData(root fs.FS, name string, parts []Partition) ([]string, bool) {
	sigs, err := blockdev.Signatures(root, name, parts)
	if err != nil {
		return nil, true
	}
	found := make([]string, 0, len(sigs))
	for _, sig := range sigs {
		found = append(found, sig.String())
	}
	return found, false
}

func interfaceNames(r hardware.Report) string {
	names := make([]string, 0, len(r.NetworkInterfaces))
	for _, n := range r.NetworkInterfaces {
		if n.Name != "lo" {
			names = append(names, n.Name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// FormatBytes formats n with a binary unit, e.g. "16.0 GiB".
func FormatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// InterfaceMAC returns the MAC address of interface name, for pinning its
// name (strata.networking.interfaces). It refuses an address the kernel
// did not get from the hardware (addr_assign_type other than 0, e.g. a
// random or set MAC): a pin must match the NIC's permanent address.
func InterfaceMAC(root fs.FS, name string) (string, error) {
	dir := "sys/class/net/" + name
	mac, err := fs.ReadFile(root, dir+"/address")
	if err != nil {
		return "", fmt.Errorf("network.interface: %s: no MAC address: %w", name, err)
	}
	m := strings.ToLower(strings.TrimSpace(string(mac)))
	if !macPattern.MatchString(m) || m == "00:00:00:00:00:00" {
		return "", fmt.Errorf("network.interface: %s: unusable MAC address %q", name, m)
	}
	// Absent on very old kernels: treat as permanent.
	if t, err := fs.ReadFile(root, dir+"/addr_assign_type"); err == nil && strings.TrimSpace(string(t)) != "0" {
		return "", fmt.Errorf("network.interface: %s: %s is not the NIC's permanent MAC address (addr_assign_type %s); "+
			"its name cannot be pinned", name, m, strings.TrimSpace(string(t)))
	}
	return m, nil
}
