// Package blockdev inspects block devices through procfs, sysfs, /dev and
// the udev database, without running commands. It answers the questions
// both the installer and storage pool setup must answer before writing to
// a disk: which device a path resolves to, what is on it, and whether
// anything uses it.
//
// All functions take the system's "/" as an fs.FS (os.DirFS("/") outside
// tests).
package blockdev

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

// ByIDDir is the directory of stable device names.
const ByIDDir = "/dev/disk/by-id/"

// Partition is a partition of a disk.
type Partition struct {
	Name      string
	SizeBytes uint64
}

// Resolve follows symlinks (as under /dev/disk/by-id/) from device to a
// kernel device name under /dev, e.g. "nvme0n1".
func Resolve(root fs.FS, device string) (string, error) {
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

// IsWholeDisk reports whether name is a whole disk (/sys/block/<name>).
func IsWholeDisk(root fs.FS, name string) bool {
	_, err := fs.Stat(root, "sys/block/"+name)
	return err == nil
}

// Partitions lists the partitions of disk name from /sys/block/<name>/.
func Partitions(root fs.FS, name string) ([]Partition, error) {
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

// PartitionNames returns the names of parts.
func PartitionNames(parts []Partition) []string {
	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = p.Name
	}
	return names
}

// Signature is a filesystem or membership signature that udev (blkid)
// found on a disk or partition.
type Signature struct {
	// Device is the kernel name, e.g. "sdb1".
	Device string
	// Type is the blkid type, e.g. "ext4", "zfs_member", "LVM2_member".
	Type string
	// Label is the filesystem label or, for zfs_member, the pool name.
	Label string
}

func (s Signature) String() string {
	desc := "/dev/" + s.Device + ": " + s.Type
	if s.Label != "" {
		desc += fmt.Sprintf(" %q", s.Label)
	}
	return desc
}

// ErrNoUdevDatabase is returned when signatures cannot be checked.
var ErrNoUdevDatabase = errors.New("no udev database (/run/udev/data): existing data cannot be checked")

// Signatures lists the signatures on disk name and the given partitions.
func Signatures(root fs.FS, name string, parts []Partition) ([]Signature, error) {
	if _, err := fs.Stat(root, "run/udev/data"); err != nil {
		return nil, ErrNoUdevDatabase
	}
	var found []Signature
	for _, dev := range append([]string{name}, PartitionNames(parts)...) {
		props := properties(root, name, dev)
		if t := props["ID_FS_TYPE"]; t != "" {
			found = append(found, Signature{Device: dev, Type: t, Label: props["ID_FS_LABEL"]})
		}
	}
	return found, nil
}

// PartitionTable returns the partition table type of disk name ("gpt",
// "dos") or "" if there is none. It requires the udev database.
func PartitionTable(root fs.FS, name string) (string, error) {
	if _, err := fs.Stat(root, "run/udev/data"); err != nil {
		return "", ErrNoUdevDatabase
	}
	return properties(root, name, name)["ID_PART_TABLE_TYPE"], nil
}

// properties returns the udev properties of dev, which is disk itself or
// one of its partitions.
func properties(root fs.FS, disk, dev string) map[string]string {
	sysDir := "sys/block/" + disk
	if dev != disk {
		sysDir += "/" + dev
	}
	num, err := fs.ReadFile(root, sysDir+"/dev")
	if err != nil {
		return map[string]string{}
	}
	return udevProperties(root, "run/udev/data/b"+strings.TrimSpace(string(num)))
}

// udevProperties reads the E: (property) lines of a udev database file.
func udevProperties(root fs.FS, file string) map[string]string {
	props := map[string]string{}
	data, err := fs.ReadFile(root, file)
	if err != nil {
		return props
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, isProperty := strings.CutPrefix(line, "E:")
		if k, v, ok := strings.Cut(rest, "="); isProperty && ok {
			props[k] = v
		}
	}
	return props
}

// Uses describes how disk name and its partitions are used: mounted, used
// as swap, or held by device-mapper (LVM, LUKS) or md RAID devices. Each
// entry reads like "/dev/sdb1 is mounted on /srv".
func Uses(root fs.FS, name string, parts []Partition) ([]string, error) {
	inUse, err := MountsAndSwap(root)
	if err != nil {
		return nil, err
	}
	var uses []string
	for _, dev := range append([]string{name}, PartitionNames(parts)...) {
		if use, ok := inUse[dev]; ok {
			uses = append(uses, "/dev/"+dev+" "+use)
		}
		for _, h := range Holders(root, name, dev) {
			uses = append(uses, "/dev/"+dev+" is part of /dev/"+h)
		}
	}
	return uses, nil
}

// Holders lists the device-mapper or md devices built on dev, which is
// disk itself or one of its partitions.
func Holders(root fs.FS, disk, dev string) []string {
	dir := "sys/block/" + disk + "/holders"
	if dev != disk {
		dir = "sys/block/" + disk + "/" + dev + "/holders"
	}
	entries, err := fs.ReadDir(root, dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// MountsAndSwap maps kernel device names that are mounted or used as swap
// to a description of the use, e.g. "is mounted on /boot".
func MountsAndSwap(root fs.FS) (map[string]string, error) {
	inUse := map[string]string{}
	err := scanMounts(root, func(dev, mountpoint string) {
		inUse[dev] = "is mounted on " + mountpoint
	})
	if err != nil {
		return nil, err
	}
	// /proc/swaps may be absent (no swap support); that is not an error.
	if _, statErr := fs.Stat(root, "proc/swaps"); statErr == nil {
		err = scanLines(root, "proc/swaps", func(f []string) {
			if len(f) >= 1 && strings.HasPrefix(f[0], "/dev/") {
				inUse[strings.TrimPrefix(f[0], "/dev/")] = "is used as swap"
			}
		})
		if err != nil {
			return nil, err
		}
	}
	return inUse, nil
}

// SystemDisks returns the whole disks that hold the running system: those
// behind the filesystems mounted on /, /boot and /nix (and /nix/store),
// following partitions and device-mapper or md devices down to the disks.
func SystemDisks(root fs.FS) (map[string]bool, error) {
	systemMounts := []string{"/", "/boot", "/nix", "/nix/store"}
	var devs []string
	err := scanMounts(root, func(dev, mountpoint string) {
		if slices.Contains(systemMounts, mountpoint) {
			devs = append(devs, dev)
		}
	})
	if err != nil {
		return nil, err
	}
	disks := map[string]bool{}
	for _, dev := range devs {
		// /dev/mapper/<name> and similar are symlinks to dm-N.
		if name, err := Resolve(root, "/dev/"+dev); err == nil {
			dev = name
		}
		for _, d := range disksBehind(root, dev, 0) {
			disks[d] = true
		}
	}
	return disks, nil
}

// disksBehind returns the whole disks that dev (a disk, partition, dm or
// md device) is built on.
func disksBehind(root fs.FS, dev string, depth int) []string {
	if depth > 8 {
		return nil
	}
	// A device-mapper or md device: follow its slaves.
	if slaves, err := fs.ReadDir(root, "sys/block/"+dev+"/slaves"); err == nil && len(slaves) > 0 {
		var out []string
		for _, s := range slaves {
			out = append(out, disksBehind(root, s.Name(), depth+1)...)
		}
		return out
	}
	if IsWholeDisk(root, dev) {
		return []string{dev}
	}
	// A partition: find its disk.
	disks, err := fs.ReadDir(root, "sys/block")
	if err != nil {
		return nil
	}
	for _, d := range disks {
		if _, err := fs.Stat(root, "sys/block/"+d.Name()+"/"+dev+"/partition"); err == nil {
			return []string{d.Name()}
		}
	}
	return nil
}

// scanMounts calls fn for each /proc/mounts entry backed by a /dev device.
func scanMounts(root fs.FS, fn func(dev, mountpoint string)) error {
	return scanLines(root, "proc/mounts", func(f []string) {
		if len(f) >= 2 && strings.HasPrefix(f[0], "/dev/") {
			fn(strings.TrimPrefix(f[0], "/dev/"), f[1])
		}
	})
}

func scanLines(root fs.FS, file string, fn func(fields []string)) error {
	f, err := root.Open(file)
	if err != nil {
		return fmt.Errorf("read /%s: %w", file, err)
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fn(strings.Fields(s.Text()))
	}
	if err := s.Err(); err != nil {
		return fmt.Errorf("read /%s: %w", file, err)
	}
	return nil
}

// MountedDevice returns the kernel name of the device mounted on
// mountpoint (resolving /dev/mapper links), or "" if nothing is.
func MountedDevice(root fs.FS, mountpoint string) (string, error) {
	var dev string
	err := scanMounts(root, func(d, mp string) {
		if mp == mountpoint {
			dev = d // the last mount on a path is the visible one
		}
	})
	if err != nil || dev == "" {
		return "", err
	}
	if name, err := Resolve(root, "/dev/"+dev); err == nil {
		return name, nil
	}
	return dev, nil
}

// DisksBehind returns the whole disks that dev (a disk, partition, dm or
// md device) is built on.
func DisksBehind(root fs.FS, dev string) []string {
	return disksBehind(root, dev, 0)
}
