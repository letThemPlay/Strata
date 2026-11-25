package ir

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/letthemplay/Strata/internal/workload"
)

var (
	nodePattern    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	projectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	limitPattern   = regexp.MustCompile(`^([0-9]{1,15})PiB$`)
)

// Validate checks the document on its own: structure, the resources'
// specs, that each resource's dependencies are exactly the host objects
// its spec references and that they are declared, and the project limits.
// Lists must be sorted, as Nix produces them, so that the same desired
// state always has the same representation. It reports every problem
// found. Parse calls it; call it again after changing a document.
func (d *Document) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if d.APIVersion != APIVersion {
		add("apiVersion: must be %q, got %q", APIVersion, d.APIVersion)
	}
	if !nodePattern.MatchString(d.Node) {
		add("node: %q is not a valid node ID", d.Node)
	}
	objects := d.validateHost(add)

	ids := make([]string, len(d.Resources))
	var cpus int
	var memory uint64
	for i := range d.Resources {
		r := &d.Resources[i]
		ids[i] = r.ID
		at := fmt.Sprintf("resources[%d]", i)
		if r.ID != "" {
			at = fmt.Sprintf("resources[%s]", r.ID)
		}
		if r.Type != TypeVM {
			add("%s.type: %q is not a supported resource type (supported: %s)", at, r.Type, TypeVM)
			continue
		}
		vm, err := workload.ParseVM(bytes.NewReader(r.Spec))
		if err != nil {
			add("%s.spec: %w", at, err)
			continue
		}
		r.VM = &vm
		if want := TypeVM + "/" + vm.Name; r.ID != want {
			add("%s.id: must be %q (type/name)", at, want)
		}
		refs := vmRefs(vm)
		if !slices.Equal(r.DependsOn, refs) {
			add("%s.dependsOn: must be %q, the host objects the spec uses, got %q", at, refs, r.DependsOn)
		}
		for _, ref := range refs {
			if !objects[ref] {
				add("%s: %s is not declared on the host", at, ref)
			}
		}
		if vm.ImageServer != "" && !slices.Contains(d.Host.ImageServers, vm.ImageServer) {
			add("%s.spec.imageServer: %q is not an allowed image server on the host", at, vm.ImageServer)
		}
		cpus += vm.Resources.CPU
		if n, err := workload.ParseSize(vm.Resources.Memory); err == nil {
			memory = saturatingAdd(memory, n)
		}
	}
	if !isSortedUnique(ids) {
		add("resources: IDs must be unique and sorted, got %q", ids)
	}

	l := d.Host.Limits
	if l.Instances > 0 && len(d.Resources) > l.Instances {
		add("resources: %d exceed the project's instance limit (%d)", len(d.Resources), l.Instances)
	}
	if l.CPU > 0 && cpus > l.CPU {
		add("resources: %d CPUs in total exceed the project's CPU limit (%d)", cpus, l.CPU)
	}
	if limit, err := parseLimit(l.Memory); err == nil && l.Memory != "" && memory > limit {
		add("resources: %d bytes of memory in total exceed the project's memory limit (%s)", memory, l.Memory)
	}
	return errors.Join(errs...)
}

// validateHost checks the host section and returns the IDs of its
// objects (pool/NAME, network/NAME).
func (d *Document) validateHost(add func(string, ...any)) map[string]bool {
	h := d.Host
	objects := map[string]bool{}
	if !projectPattern.MatchString(h.Project) || h.Project == "default" {
		add("host.project: %q is not a valid Strata project name", h.Project)
	}
	pools := make([]string, len(h.Pools))
	for i, p := range h.Pools {
		pools[i] = p.Name
		if drivers := []string{"dir", "lvm", "zfs"}; !slices.Contains(drivers, p.Driver) {
			add("host.pools[%s].driver: %q is not one of %s", p.Name, p.Driver, strings.Join(drivers, ", "))
		}
		objects["pool/"+p.Name] = true
	}
	if !isSortedUnique(pools) {
		add("host.pools: names must be unique and sorted, got %q", pools)
	}
	if h.DefaultPool != "" && !slices.Contains(pools, h.DefaultPool) {
		add("host.defaultPool: %q is not a declared pool", h.DefaultPool)
	}
	nets := make([]string, len(h.Networks))
	for i, n := range h.Networks {
		nets[i] = n.Name
		objects["network/"+n.Name] = true
	}
	if !isSortedUnique(nets) {
		add("host.networks: names must be unique and sorted, got %q", nets)
	}
	if !isSortedUnique(h.ImageServers) {
		add("host.imageServers: must be unique and sorted, got %q", h.ImageServers)
	}
	if h.Pools == nil || h.Networks == nil || h.ImageServers == nil {
		add("host: pools, networks and imageServers must be present (empty lists when none)")
	}
	l := h.Limits
	if l.CPU < 0 || l.Instances < 0 {
		add("host.limits: cpu and instances must not be negative")
	}
	if _, err := parseLimit(l.Memory); l.Memory != "" && err != nil {
		add("host.limits.memory: %v", err)
	}
	return objects
}

// vmRefs returns the IDs of the host objects a VM spec uses, sorted.
func vmRefs(vm workload.VM) []string {
	var refs []string
	for _, disk := range vm.Disks {
		refs = append(refs, "pool/"+disk.Pool)
	}
	for _, nic := range vm.Networks {
		refs = append(refs, "network/"+nic.Network)
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

func isSortedUnique(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] >= s[i] {
			return false
		}
	}
	return true
}

// parseLimit parses a project limit size: a VM size, or PiB, which
// project limits also accept.
func parseLimit(s string) (uint64, error) {
	m := limitPattern.FindStringSubmatch(s)
	if m == nil {
		n, err := workload.ParseSize(s)
		if err != nil {
			return 0, fmt.Errorf("limit: %w", err)
		}
		return n, nil
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil || n > (1<<64-1)>>50 {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return n << 50, nil
}

func saturatingAdd(a, b uint64) uint64 {
	if a+b < a {
		return 1<<64 - 1
	}
	return a + b
}
