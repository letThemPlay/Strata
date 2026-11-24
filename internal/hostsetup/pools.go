package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/letthemplay/Strata/internal/blockdev"
)

// storagePool is the part of an Incus storage pool Setup compares.
type storagePool struct {
	Name   string            `json:"name"`
	Driver string            `json:"driver"`
	Config map[string]string `json:"config"`
	// UsedBy lists API URLs of what uses the pool, e.g.
	// "/1.0/instances/c1?project=strata".
	UsedBy []string `json:"used_by"`
}

// usedByProject reports whether instances or profiles of project use the
// pool. Those are what Incus checks before it lets a project lose access
// to a pool; cached images and volumes do not block it.
func (p storagePool) usedByProject(project string) bool {
	for _, u := range p.UsedBy {
		if !strings.HasPrefix(u, "/1.0/instances/") && !strings.HasPrefix(u, "/1.0/profiles/") {
			continue
		}
		if _, query, ok := strings.Cut(u, "?"); ok {
			if q, err := url.ParseQuery(query); err == nil && q.Get("project") == project {
				return true
			}
		}
	}
	return false
}

// source returns the source the pool was created with. Incus rewrites
// "source" after creating a ZFS or LVM pool and keeps the original in
// volatile.initial_source.
func (p storagePool) source() string {
	if s := p.Config["volatile.initial_source"]; s != "" {
		return s
	}
	return p.Config["source"]
}

// ensurePools creates missing declared pools and checks existing ones. It
// returns the pools that exist afterwards and the refusals.
func (s *Setup) ensurePools(ctx context.Context) (map[string]storagePool, error) {
	existing, err := s.listPools(ctx)
	if err != nil {
		return nil, err
	}

	var errs []error
	declared := map[string]bool{}
	for _, want := range s.Config.Pools {
		declared[want.Name] = true
		if have, ok := existing[want.Name]; ok {
			if err := checkExisting(want, have); err != nil {
				errs = append(errs, err)
				s.Log.ErrorContext(ctx, "storage pool differs from its declaration; not modified", "pool", want.Name, "error", err)
			}
			continue
		}
		created, err := s.createPool(ctx, want)
		if err != nil {
			errs = append(errs, err)
			s.Log.ErrorContext(ctx, "storage pool refused", "pool", want.Name, "error", err)
			continue
		}
		existing[want.Name] = created
		s.Log.InfoContext(ctx, "storage pool created", "pool", want.Name, "driver", want.Driver,
			"source", created.Config["source"], "adopted", want.Adopt)
	}
	for name, p := range existing {
		switch {
		case declared[name]:
		case p.usedByProject(s.Config.Project):
			s.Log.WarnContext(ctx, "storage pool is not declared but still used by the project; it stays open "+
				"until its instances are moved or deleted", "pool", name)
		default:
			s.Log.WarnContext(ctx, "storage pool is not declared; left untouched and closed to the project", "pool", name)
		}
	}
	return existing, errors.Join(errs...)
}

// listPools returns the pools that exist in Incus.
func (s *Setup) listPools(ctx context.Context) (map[string]storagePool, error) {
	var list []storagePool
	if err := s.Incus.Do(ctx, http.MethodGet, "/1.0/storage-pools?recursion=1", nil, &list); err != nil {
		return nil, fmt.Errorf("list storage pools: %w", err)
	}
	existing := map[string]storagePool{}
	for _, p := range list {
		existing[p.Name] = p
	}
	return existing, nil
}

// expectedSource is the Incus source of a declared pool.
func expectedSource(p Pool) string {
	switch {
	case p.Driver == "dir":
		return filepath.Join(p.Path, p.Name)
	case p.Adopt:
		return p.Name // the existing zpool or volume group
	case p.Driver == "zfs" && p.Layout == "mirror":
		return "mirror=" + strings.Join(p.Devices, ",")
	default:
		return strings.Join(p.Devices, ",")
	}
}

// checkExisting compares an existing pool with its declaration.
func checkExisting(want Pool, have storagePool) error {
	if have.Driver != want.Driver {
		return fmt.Errorf("pool %s: exists with driver %s, declared %s", want.Name, have.Driver, want.Driver)
	}
	if src := have.source(); src != expectedSource(want) {
		return fmt.Errorf("pool %s: exists with source %q, declared %q", want.Name, src, expectedSource(want))
	}
	return nil
}

// createPool checks the declared devices or path and creates the pool.
func (s *Setup) createPool(ctx context.Context, p Pool) (storagePool, error) {
	cfg := map[string]string{"source": expectedSource(p)}
	switch p.Driver {
	case "dir":
		if err := s.prepareDir(p); err != nil {
			return storagePool{}, err
		}
	case "zfs", "lvm":
		if len(p.Devices) == 0 {
			return storagePool{}, fmt.Errorf("pool %s: no devices", p.Name)
		}
		system, err := blockdev.SystemDisks(s.Root)
		if err != nil {
			return storagePool{}, fmt.Errorf("pool %s: %w", p.Name, err)
		}
		var errs []error
		for _, dev := range p.Devices {
			errs = append(errs, s.checkDevice(ctx, p, dev, system))
		}
		if err := errors.Join(errs...); err != nil {
			return storagePool{}, err
		}
		if p.Driver == "lvm" {
			cfg["lvm.use_thinpool"] = "true"
			if !p.Adopt {
				cfg["lvm.vg_name"] = p.Name
			}
		}
	default:
		return storagePool{}, fmt.Errorf("pool %s: unsupported driver %q", p.Name, p.Driver)
	}

	body := map[string]any{"name": p.Name, "driver": p.Driver, "config": cfg}
	if err := s.Incus.Do(ctx, http.MethodPost, "/1.0/storage-pools", body, nil); err != nil {
		return storagePool{}, fmt.Errorf("pool %s: create: %w", p.Name, err)
	}
	return storagePool{Name: p.Name, Driver: p.Driver, Config: cfg}, nil
}

// checkDevice checks that dev may be used for pool p: a whole disk, not a
// system disk, unused, and empty (or, when adopting, holding exactly the
// zpool or volume group named like the pool).
func (s *Setup) checkDevice(ctx context.Context, p Pool, dev string, system map[string]bool) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("pool %s: %s: %s", p.Name, dev, fmt.Sprintf(format, a...))
	}
	name, err := blockdev.Resolve(s.Root, dev)
	if err != nil {
		return fail("%v", err)
	}
	if !blockdev.IsWholeDisk(s.Root, name) {
		return fail("/dev/%s is not a whole disk", name)
	}
	if system[name] {
		return fail("/dev/%s holds the running system", name)
	}
	parts, err := blockdev.Partitions(s.Root, name)
	if err != nil {
		return fail("%v", err)
	}
	uses, err := blockdev.Uses(s.Root, name, parts)
	if err != nil {
		return fail("%v", err)
	}
	if len(uses) > 0 {
		return fail("in use: %s", strings.Join(uses, "; "))
	}
	sigs, err := blockdev.Signatures(s.Root, name, parts)
	if err != nil {
		return fail("%v", err)
	}
	table, err := blockdev.PartitionTable(s.Root, name)
	if err != nil {
		return fail("%v", err)
	}

	if !p.Adopt {
		var found []string
		if table != "" {
			found = append(found, "/dev/"+name+": "+table+" partition table")
		}
		for _, sig := range sigs {
			found = append(found, sig.String())
		}
		if len(found) > 0 {
			return fail("not empty (%s); Strata does not erase disks: wipe it deliberately (wipefs -a) or adopt the existing pool",
				strings.Join(found, ", "))
		}
		if len(parts) > 0 {
			return fail("has partitions; Strata does not erase disks")
		}
		return nil
	}

	// Adoption: every signature on the disk (whole disk or partitions;
	// zpool create partitions whole disks) must belong to the pool's own
	// zpool or volume group, and there must be at least one.
	want := map[string]string{"zfs": "zfs_member", "lvm": "LVM2_member"}[p.Driver]
	if len(sigs) == 0 {
		return fail("cannot adopt: no %s signature found", want)
	}
	for _, sig := range sigs {
		if sig.Type != want {
			return fail("cannot adopt: found %s, expected only %s", sig, want)
		}
		switch p.Driver {
		case "zfs":
			if sig.Label != p.Name {
				return fail("cannot adopt: %s belongs to zpool %q, not %q", sig, sig.Label, p.Name)
			}
		case "lvm":
			out, err := s.Exec(ctx, "pvs", "--noheadings", "-o", "vg_name", "/dev/"+sig.Device)
			if err != nil {
				return fail("cannot adopt: pvs /dev/%s: %v", sig.Device, err)
			}
			if vg := strings.TrimSpace(string(out)); vg != p.Name {
				return fail("cannot adopt: /dev/%s belongs to volume group %q, not %q", sig.Device, vg, p.Name)
			}
		}
	}
	return nil
}

// prepareDir checks that a dir pool's path is a mount point outside the
// system disks and creates the pool directory in it (Incus requires an
// existing, empty directory).
func (s *Setup) prepareDir(p Pool) error {
	dev, err := blockdev.MountedDevice(s.Root, p.Path)
	if err != nil {
		return fmt.Errorf("pool %s: %w", p.Name, err)
	}
	if dev == "" {
		return fmt.Errorf("pool %s: nothing is mounted on %s", p.Name, p.Path)
	}
	system, err := blockdev.SystemDisks(s.Root)
	if err != nil {
		return fmt.Errorf("pool %s: %w", p.Name, err)
	}
	for _, d := range blockdev.DisksBehind(s.Root, dev) {
		if system[d] {
			return fmt.Errorf("pool %s: %s is on /dev/%s, which holds the running system", p.Name, p.Path, d)
		}
	}
	dir := filepath.Join(p.Path, p.Name)
	if err := os.Mkdir(dir, 0o711); err != nil && !errors.Is(err, os.ErrExist) { //nolint:gosec // Traversal only, as Incus does.
		return fmt.Errorf("pool %s: %w", p.Name, err)
	}
	// 0711 like Incus's own pool directories, whatever the umask:
	// unprivileged containers must be able to traverse it.
	if err := os.Chmod(dir, 0o711); err != nil { //nolint:gosec // Traversal only, as Incus does.
		return fmt.Errorf("pool %s: %w", p.Name, err)
	}
	return nil
}
