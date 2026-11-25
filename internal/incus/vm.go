package incus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/letthemplay/Strata/internal/workload"
)

// Instance keys Strata sets. Instances without keyManaged are not Strata's
// and are invisible to the provider.
const (
	keyManaged   = "user.strata.managed"
	keySpec      = "user.strata.spec"
	keyOwner     = "user.strata.owner"     // set only for gitops; absent: cli
	keyProtected = "user.strata.protected" // set only when true
)

// instance is the part of an Incus instance the provider reads.
type instance struct {
	Name    string                       `json:"name"`
	Type    string                       `json:"type"`
	Status  string                       `json:"status"`
	Config  map[string]string            `json:"config"`
	Devices map[string]map[string]string `json:"devices"`
	State   *struct {
		Network map[string]struct {
			Addresses []struct {
				Address string `json:"address"`
				Scope   string `json:"scope"`
			} `json:"addresses"`
		} `json:"network"`
	} `json:"state"`
}

// instanceFor returns the Incus configuration and devices for vm and meta.
// The owner and protected keys are set only when they differ from the
// defaults, so VMs created before M3 are unchanged.
func instanceFor(vm workload.VM, meta workload.Meta) (map[string]string, map[string]map[string]string, error) {
	spec, err := json.Marshal(vm)
	if err != nil {
		return nil, nil, fmt.Errorf("encode spec: %w", err)
	}
	cfg := map[string]string{
		"limits.cpu":     strconv.Itoa(vm.Resources.CPU),
		"limits.memory":  vm.Resources.Memory,
		"boot.autostart": strconv.FormatBool(vm.Autostart),
		// The project blocks VM nesting and requires VMs to say so.
		"security.nesting": "false",
		keyManaged:         "true",
		keySpec:            string(spec),
	}
	if meta.Owner == workload.OwnerGitOps {
		cfg[keyOwner] = workload.OwnerGitOps
	}
	if meta.Protected {
		cfg[keyProtected] = "true"
	}
	if vm.CloudInit != nil && len(vm.CloudInit.SSHAuthorizedKeys) > 0 {
		var b strings.Builder
		b.WriteString("#cloud-config\nssh_authorized_keys:\n")
		for _, k := range vm.CloudInit.SSHAuthorizedKeys {
			k, err := json.Marshal(k) // a YAML-compatible quoted string
			if err != nil {
				return nil, nil, fmt.Errorf("encode cloud-init: %w", err)
			}
			fmt.Fprintf(&b, "  - %s\n", k)
		}
		cfg["cloud-init.user-data"] = b.String()
	}
	devices := map[string]map[string]string{}
	for _, d := range vm.Disks {
		if d.Name == workload.RootDisk {
			devices[d.Name] = map[string]string{"type": "disk", "path": "/", "pool": d.Pool, "size": d.Size}
		} else {
			devices[d.Name] = map[string]string{"type": "disk", "pool": d.Pool, "source": vm.VolumeName(d)}
		}
	}
	for _, n := range vm.Networks {
		devices[n.Name] = map[string]string{"type": "nic", "network": n.Network, "name": n.Name}
	}
	return cfg, devices, nil
}

func instancePath(name string) string { return "/1.0/instances/" + url.PathEscape(name) }

func volumePath(pool, name string) string {
	return "/1.0/storage-pools/" + url.PathEscape(pool) + "/volumes/custom/" + url.PathEscape(name)
}

// getInstance returns a Strata-managed instance, or workload.ErrNotFound.
func (c *Client) getInstance(ctx context.Context, name string) (instance, error) {
	var inst instance
	err := c.Do(ctx, http.MethodGet, instancePath(name)+"?recursion=1", nil, &inst)
	if IsNotFound(err) {
		return instance{}, fmt.Errorf("vm %s: %w", name, workload.ErrNotFound)
	}
	if err != nil {
		return instance{}, err
	}
	if inst.Config[keyManaged] != "true" || inst.Type != "virtual-machine" {
		return instance{}, fmt.Errorf("vm %s: %w (an instance of this name exists but is not managed by Strata)", name, workload.ErrNotFound)
	}
	return inst, nil
}

// metaOf returns the meta recorded on an instance.
func metaOf(inst instance) workload.Meta {
	owner := workload.OwnerCLI
	if inst.Config[keyOwner] == workload.OwnerGitOps {
		owner = workload.OwnerGitOps
	}
	return workload.Meta{Owner: owner, Protected: inst.Config[keyProtected] == "true"}
}

// status returns a VM's status: its recorded spec and meta, and how the
// instance differs from them (drift).
func status(inst instance) (workload.VMStatus, error) {
	var vm workload.VM
	if err := json.Unmarshal([]byte(inst.Config[keySpec]), &vm); err != nil {
		return workload.VMStatus{}, fmt.Errorf("vm %s: stored spec: %w", inst.Name, err)
	}
	meta := metaOf(inst)
	drift, err := differences(vm, meta, inst)
	if err != nil {
		return workload.VMStatus{}, err
	}
	st := workload.VMStatus{Spec: vm, Meta: meta, Status: inst.Status, Drift: drift}
	if inst.State != nil {
		for name, n := range inst.State.Network {
			if name == "lo" {
				continue
			}
			for _, a := range n.Addresses {
				if a.Scope == "global" {
					st.Addresses = append(st.Addresses, a.Address)
				}
			}
		}
		slices.Sort(st.Addresses)
	}
	return st, nil
}

// GetVM returns a Strata-managed VM.
func (c *Client) GetVM(ctx context.Context, name string) (workload.VMStatus, error) {
	inst, err := c.getInstance(ctx, name)
	if err != nil {
		return workload.VMStatus{}, err
	}
	return status(inst)
}

// ListVMs returns the Strata-managed VMs.
func (c *Client) ListVMs(ctx context.Context) ([]workload.VMStatus, error) {
	var list []instance
	if err := c.Do(ctx, http.MethodGet, "/1.0/instances?recursion=2", nil, &list); err != nil {
		return nil, err
	}
	out := []workload.VMStatus{}
	for _, inst := range list {
		if inst.Config[keyManaged] != "true" || inst.Type != "virtual-machine" {
			continue
		}
		st, err := status(inst)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b workload.VMStatus) int { return strings.Compare(a.Spec.Name, b.Spec.Name) })
	return out, nil
}

// CreateVM creates a stopped VM from vm, recording meta, and verifies the
// result. Data
// volumes are created first; if anything fails, the volumes created by this
// call (new and empty) are removed again. Existing volumes are never
// reused or removed.
func (c *Client) CreateVM(ctx context.Context, vm workload.VM, meta workload.Meta) (err error) {
	if err := vm.Validate(); err != nil {
		return fmt.Errorf("vm %s: %w", vm.Name, err)
	}
	cfg, devices, err := instanceFor(vm, meta)
	if err != nil {
		return err
	}
	if err := c.Do(ctx, http.MethodGet, instancePath(vm.Name), nil, nil); err == nil {
		return fmt.Errorf("vm %s: an instance of this name %w", vm.Name, workload.ErrExists)
	} else if !IsNotFound(err) {
		return err
	}

	var data []workload.Disk
	for _, d := range vm.Disks {
		if d.Name != workload.RootDisk {
			data = append(data, d)
		}
	}
	created, err := c.createVolumes(ctx, vm, data)
	defer func() {
		if err != nil {
			err = errors.Join(err, c.removeNewVolumes(context.WithoutCancel(ctx), vm, created))
		}
	}()
	if err != nil {
		return err
	}

	source := map[string]string{"type": "none"}
	switch {
	case vm.ImageServer != "":
		// Incus enforces the project's allowed servers.
		source = map[string]string{"type": "image", "alias": vm.Image, "server": "https://" + vm.ImageServer, "protocol": "simplestreams"}
	case vm.Image != "":
		source = map[string]string{"type": "image", "alias": vm.Image}
	}
	body := map[string]any{
		"name":     vm.Name,
		"type":     "virtual-machine",
		"source":   source,
		"config":   cfg,
		"devices":  devices,
		"profiles": []string{}, // the spec alone defines the VM
	}
	if err := c.Do(ctx, http.MethodPost, "/1.0/instances", body, nil); err != nil {
		return fmt.Errorf("vm %s: create: %w", vm.Name, err)
	}
	created = nil // the VM now owns its volumes; never remove them here
	inst, err := c.getInstance(ctx, vm.Name)
	if err != nil {
		return err
	}
	return verify(vm, meta, inst)
}

// verify compares an instance with the spec and meta it was created from.
func verify(vm workload.VM, meta workload.Meta, inst instance) error {
	diffs, err := differences(vm, meta, inst)
	if err != nil {
		return err
	}
	if len(diffs) > 0 {
		return fmt.Errorf("vm %s: %w: %s", vm.Name, workload.ErrVerify, strings.Join(diffs, "; "))
	}
	return nil
}

// differences lists how an instance differs from a spec and meta, sorted.
func differences(vm workload.VM, meta workload.Meta, inst instance) ([]string, error) {
	cfg, devices, err := instanceFor(vm, meta)
	if err != nil {
		return nil, err
	}
	var diffs []string
	for k, v := range cfg {
		if inst.Config[k] != v {
			diffs = append(diffs, fmt.Sprintf("config %s: %q, want %q", k, inst.Config[k], v))
		}
	}
	for name, want := range devices {
		if !maps.Equal(inst.Devices[name], want) {
			diffs = append(diffs, fmt.Sprintf("device %s: %v, want %v", name, inst.Devices[name], want))
		}
	}
	for name := range inst.Devices {
		if _, ok := devices[name]; !ok {
			diffs = append(diffs, fmt.Sprintf("unexpected device %s", name))
		}
	}
	for _, k := range []string{keyOwner, keyProtected, "cloud-init.user-data"} {
		if _, want := cfg[k]; !want && inst.Config[k] != "" {
			diffs = append(diffs, fmt.Sprintf("config %s: %q, want unset", k, inst.Config[k]))
		}
	}
	slices.Sort(diffs)
	return diffs, nil
}

// wantStatus is the status an action must reach.
func wantStatus(action string) (string, bool) {
	switch action {
	case workload.ActionStart, workload.ActionRestart:
		return "Running", true
	case workload.ActionStop:
		return "Stopped", true
	}
	return "", false
}

// VMAction starts, stops or restarts a VM and verifies its status. A
// graceful stop or restart waits up to timeoutSeconds for the guest; force
// stops it immediately.
func (c *Client) VMAction(ctx context.Context, name, action string, force bool, timeoutSeconds int) error {
	want, ok := wantStatus(action)
	if !ok {
		return fmt.Errorf("vm %s: unknown action %q", name, action)
	}
	inst, err := c.getInstance(ctx, name)
	if err != nil {
		return err
	}
	if inst.Status == want && action != workload.ActionRestart {
		return nil // already there
	}
	body := map[string]any{"action": action, "force": force, "timeout": timeoutSeconds}
	if err := c.Do(ctx, http.MethodPut, instancePath(name)+"/state", body, nil); err != nil {
		return fmt.Errorf("vm %s: %s: %w", name, action, err)
	}
	inst, err = c.getInstance(ctx, name)
	if err != nil {
		return err
	}
	if inst.Status != want {
		return fmt.Errorf("vm %s: %w: status %s after %s, want %s", name, workload.ErrVerify, inst.Status, action, want)
	}
	return nil
}

// createVolumes creates the data volumes for disks, refusing any that
// already exist (data volumes are never reused implicitly). It returns the
// disks whose volumes it created, also on error.
func (c *Client) createVolumes(ctx context.Context, vm workload.VM, disks []workload.Disk) ([]workload.Disk, error) {
	var created []workload.Disk
	for _, d := range disks {
		name := vm.VolumeName(d)
		if err := c.Do(ctx, http.MethodGet, volumePath(d.Pool, name), nil, nil); err == nil {
			return created, fmt.Errorf("vm %s: volume %s on pool %s %w; data volumes are never reused implicitly", vm.Name, name, d.Pool, workload.ErrExists)
		} else if !IsNotFound(err) {
			return created, fmt.Errorf("vm %s: volume %s: %w", vm.Name, name, err)
		}
		body := map[string]any{"name": name, "content_type": "block", "config": map[string]string{"size": d.Size}}
		if err := c.Do(ctx, http.MethodPost, "/1.0/storage-pools/"+url.PathEscape(d.Pool)+"/volumes/custom", body, nil); err != nil {
			return created, fmt.Errorf("vm %s: create volume %s: %w", vm.Name, name, err)
		}
		created = append(created, d)
	}
	return created, nil
}

// removeNewVolumes removes volumes this request created (new and empty)
// after a failure.
func (c *Client) removeNewVolumes(ctx context.Context, vm workload.VM, created []workload.Disk) error {
	var errs []error
	for _, d := range created {
		if err := c.Do(ctx, http.MethodDelete, volumePath(d.Pool, vm.VolumeName(d)), nil, nil); err != nil {
			errs = append(errs, fmt.Errorf("clean up new volume %s: %w", vm.VolumeName(d), err))
		}
	}
	return errors.Join(errs...)
}
