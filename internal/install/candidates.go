package install

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/letthemplay/Strata/internal/blockdev"
	"github.com/letthemplay/Strata/internal/hardware"
)

// DiskCandidate is a whole disk offered by the init wizard.
type DiskCandidate struct {
	// Device is the stable /dev/disk/by-id/ path, or /dev/NAME when the
	// disk has none.
	Device string
	Info   DiskInfo
	// Problem, if set, is why the disk cannot be used.
	Problem error
}

// Stable reports whether Device is a /dev/disk/by-id/ path.
func (c DiskCandidate) Stable() bool { return strings.HasPrefix(c.Device, byIDDir) }

const byIDDir = "/dev/disk/by-id/"

// DiskCandidates lists the machine's whole disks with a stable device
// path each, checked as Probe checks the answer file's disk.
func DiskCandidates(root fs.FS) ([]DiskCandidate, error) {
	report, err := hardware.Discover(root)
	if err != nil {
		return nil, fmt.Errorf("discover hardware: %w", err)
	}
	links, err := byIDLinks(root)
	if err != nil {
		return nil, err
	}
	out := make([]DiskCandidate, 0, len(report.BlockDevices))
	for _, d := range report.BlockDevices {
		device := "/dev/" + d.Name
		if ids := links[d.Name]; len(ids) > 0 {
			device = byIDDir + ids[0]
		}
		info, problem := probeDisk(root, report, device)
		out = append(out, DiskCandidate{Device: device, Info: info, Problem: firstError(problem)})
	}
	return out, nil
}

// byIDLinks maps kernel device names to their /dev/disk/by-id/ names,
// preferred first: names derived from model and serial number before
// WWN/EUI-based ones, which are stable but not recognizable.
func byIDLinks(root fs.FS) (map[string][]string, error) {
	entries, err := fs.ReadDir(root, strings.Trim(byIDDir, "/"))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string][]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", byIDDir, err)
	}
	links := map[string][]string{}
	for _, e := range entries {
		name, err := blockdev.Resolve(root, byIDDir+e.Name())
		if err != nil {
			continue
		}
		links[name] = append(links[name], e.Name())
	}
	opaque := func(id string) bool {
		return strings.HasPrefix(id, "wwn-") || strings.HasPrefix(id, "nvme-eui.") || strings.HasPrefix(id, "nvme-nvme.")
	}
	for _, ids := range links {
		slices.SortFunc(ids, func(a, b string) int {
			if oa, ob := opaque(a), opaque(b); oa != ob {
				if oa {
					return 1
				}
				return -1
			}
			return strings.Compare(a, b)
		})
	}
	return links, nil
}

// firstError returns the first error joined into err (by errors.Join).
func firstError(err error) error {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		if errs := joined.Unwrap(); len(errs) > 0 {
			return errs[0]
		}
	}
	return err
}

// InterfaceCandidates lists network interfaces for the management
// network: physical ones, or all if there are none (e.g. in some VMs).
func InterfaceCandidates(root fs.FS) ([]hardware.NetworkInterface, error) {
	report, err := hardware.Discover(root)
	if err != nil {
		return nil, fmt.Errorf("discover hardware: %w", err)
	}
	phys := slices.DeleteFunc(slices.Clone(report.NetworkInterfaces), func(n hardware.NetworkInterface) bool { return !n.Physical })
	if len(phys) > 0 {
		return phys, nil
	}
	return report.NetworkInterfaces, nil
}
