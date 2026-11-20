package install

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/letthemplay/Strata/internal/hardware"
)

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
}

// Partition is an existing partition on the target disk.
type Partition struct {
	Name      string
	SizeBytes uint64
}

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
	}
	return disk, errors.Join(diskErr, nicErr)
}

func probeDisk(root fs.FS, report hardware.Report, device string) (DiskInfo, error) {
	info := DiskInfo{Device: device}

	name, err := resolveDevice(root, device)
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

	if info.Partitions, err = partitions(root, name); err != nil {
		return info, fmt.Errorf("disk.device: %w", err)
	}

	var errs []error
	if d.SizeBytes < MinDiskBytes {
		errs = append(errs, fmt.Errorf("disk.device: %s is %s; at least %s is required",
			device, FormatBytes(d.SizeBytes), FormatBytes(MinDiskBytes)))
	}
	if ro, _ := fs.ReadFile(root, "sys/block/"+name+"/ro"); strings.TrimSpace(string(ro)) == "1" {
		errs = append(errs, fmt.Errorf("disk.device: %s is read-only", device))
	}
	inUse, err := devicesInUse(root)
	if err != nil {
		return info, fmt.Errorf("disk.device: %w", err)
	}
	for _, dev := range append([]string{name}, partitionNames(info.Partitions)...) {
		if use, ok := inUse[dev]; ok {
			errs = append(errs, fmt.Errorf("disk.device: %s is in use (/dev/%s %s); this may be the installation medium", device, dev, use))
		}
		// Holders are device-mapper (LVM, LUKS) or md RAID devices built on
		// this disk or partition.
		holdersDir := "sys/block/" + name + "/holders"
		if dev != name {
			holdersDir = "sys/block/" + name + "/" + dev + "/holders"
		}
		if holders, err := fs.ReadDir(root, holdersDir); err == nil && len(holders) > 0 {
			errs = append(errs, fmt.Errorf("disk.device: %s is in use (/dev/%s is part of /dev/%s); deactivate it first", device, dev, holders[0].Name()))
		}
	}
	return info, errors.Join(errs...)
}

// resolveDevice follows symlinks (as under /dev/disk/by-id/) from device
// to a kernel device name under /dev.
func resolveDevice(root fs.FS, device string) (string, error) {
	p := strings.TrimPrefix(path.Clean(device), "/")
	for range 16 {
		fi, err := fs.Lstat(root, p)
		if err != nil {
			return "", fmt.Errorf("%s does not exist", device)
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			if path.Dir(p) != "dev" {
				return "", fmt.Errorf("%s does not resolve to a device node directly under /dev", device)
			}
			return path.Base(p), nil
		}
		target, err := fs.ReadLink(root, p)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", device, err)
		}
		if !path.IsAbs(target) {
			target = path.Join("/", path.Dir(p), target)
		}
		p = strings.TrimPrefix(path.Clean(target), "/")
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", device)
}

// partitions lists the partitions of disk name from /sys/block/<name>/.
func partitions(root fs.FS, name string) ([]Partition, error) {
	dir := "sys/block/" + name
	entries, err := fs.ReadDir(root, dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var parts []Partition
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), name) {
			continue
		}
		if _, err := fs.Stat(root, dir+"/"+e.Name()+"/partition"); err != nil {
			continue
		}
		p := Partition{Name: e.Name()}
		if s, err := fs.ReadFile(root, dir+"/"+e.Name()+"/size"); err == nil {
			if sectors, err := strconv.ParseUint(strings.TrimSpace(string(s)), 10, 64); err == nil {
				p.SizeBytes = sectors * 512
			}
		}
		parts = append(parts, p)
	}
	return parts, nil
}

func partitionNames(parts []Partition) []string {
	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = p.Name
	}
	return names
}

// devicesInUse maps kernel device names that are mounted or used as swap
// to a description of the use.
func devicesInUse(root fs.FS) (map[string]string, error) {
	inUse := map[string]string{}
	scan := func(file string, use func(fields []string) (string, string, bool)) error {
		f, err := root.Open(file)
		if err != nil {
			return fmt.Errorf("read /%s: %w", file, err)
		}
		defer func() { _ = f.Close() }()
		s := bufio.NewScanner(f)
		for s.Scan() {
			if dev, desc, ok := use(strings.Fields(s.Text())); ok {
				inUse[dev] = desc
			}
		}
		if err := s.Err(); err != nil {
			return fmt.Errorf("read /%s: %w", file, err)
		}
		return nil
	}

	err := scan("proc/mounts", func(f []string) (string, string, bool) {
		if len(f) < 2 || !strings.HasPrefix(f[0], "/dev/") {
			return "", "", false
		}
		return strings.TrimPrefix(f[0], "/dev/"), "is mounted on " + f[1], true
	})
	if err != nil {
		return nil, err
	}
	// /proc/swaps may be absent (no swap support); that is not an error.
	if _, statErr := fs.Stat(root, "proc/swaps"); statErr == nil {
		err = scan("proc/swaps", func(f []string) (string, string, bool) {
			if len(f) < 1 || !strings.HasPrefix(f[0], "/dev/") {
				return "", "", false
			}
			return strings.TrimPrefix(f[0], "/dev/"), "is used as swap", true
		})
		if err != nil {
			return nil, err
		}
	}
	return inUse, nil
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
