package incus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/letthemplay/Strata/internal/workload"
)

// isManagedKey reports whether a spec determines an instance
// configuration key; an update replaces exactly these and keeps the rest
// (Incus's volatile.* and image.* keys).
func isManagedKey(k string) bool {
	switch k {
	case "limits.cpu", "limits.memory", "boot.autostart", "security.nesting",
		"cloud-init.user-data", keyManaged, keySpec, keyOwner, keyProtected:
		return true
	}
	return false
}

// managed returns a VM's instance and its recorded status, refusing a VM
// that was changed outside Strata (drift): a plan must compare against
// what Incus actually has. RestoreVM repairs drift.
func (c *Client) managed(ctx context.Context, name string) (instance, workload.VMStatus, error) {
	inst, err := c.getInstance(ctx, name)
	if err != nil {
		return instance{}, workload.VMStatus{}, err
	}
	st, err := status(inst)
	if err != nil {
		return instance{}, workload.VMStatus{}, err
	}
	if len(st.Drift) > 0 {
		return instance{}, workload.VMStatus{}, fmt.Errorf("vm %s: %w: it was changed outside Strata (%s)", name, workload.ErrConflict, strings.Join(st.Drift, "; "))
	}
	return inst, st, nil
}

// PlanVM returns the plan that turns the VM into desired.
func (c *Client) PlanVM(ctx context.Context, desired workload.VM) (workload.Plan, error) {
	inst, st, err := c.managed(ctx, desired.Name)
	if err != nil {
		return workload.Plan{}, err
	}
	current := st.Spec
	plan, err := workload.PlanVM(current, desired, inst.Status == "Running")
	if err != nil {
		return workload.Plan{}, fmt.Errorf("vm %s: %w", desired.Name, err)
	}
	return plan, nil
}

// UpdateVM applies the plan with ID planID. The plan is computed again
// and must still have that ID, so what is applied is what was confirmed.
// A running VM is stopped gracefully (stopTimeout seconds), changed and
// started again when the plan needs it (ADR-0021). On failure after the
// stop, new volumes are removed and the VM is started again unchanged.
func (c *Client) UpdateVM(ctx context.Context, desired workload.VM, planID string, stopTimeout int) (workload.Plan, error) {
	inst, st, err := c.managed(ctx, desired.Name)
	if err != nil {
		return workload.Plan{}, err
	}
	current, meta := st.Spec, st.Meta
	plan, err := workload.PlanVM(current, desired, inst.Status == "Running")
	if err != nil {
		return workload.Plan{}, fmt.Errorf("vm %s: %w", desired.Name, err)
	}
	if plan.ID != planID {
		return workload.Plan{}, fmt.Errorf("vm %s: %w: the VM changed since plan %s was made; plan again", desired.Name, workload.ErrConflict, planID)
	}
	if plan.Empty() {
		return plan, nil
	}
	name := desired.Name

	if plan.Restart {
		if err := c.VMAction(ctx, name, workload.ActionStop, false, stopTimeout); err != nil {
			return workload.Plan{}, fmt.Errorf("vm %s: stop before the change: %w", name, err)
		}
	}
	restore := func(cause error) error {
		if plan.Restart {
			if err := c.VMAction(context.WithoutCancel(ctx), name, workload.ActionStart, false, 0); err != nil {
				return errors.Join(cause, fmt.Errorf("vm %s: start again after the failed change: %w", name, err))
			}
		}
		return cause
	}

	var newDisks []workload.Disk
	have := map[string]bool{}
	for _, d := range current.Disks {
		have[d.Name] = true
	}
	for _, d := range desired.Disks {
		if !have[d.Name] {
			newDisks = append(newDisks, d)
		}
	}
	created, err := c.createVolumes(ctx, desired, newDisks)
	if err != nil {
		return workload.Plan{}, restore(errors.Join(err, c.removeNewVolumes(context.WithoutCancel(ctx), desired, created)))
	}

	if err := c.putInstance(ctx, desired, meta); err != nil {
		cleanup := c.removeNewVolumes(context.WithoutCancel(ctx), desired, created)
		return workload.Plan{}, restore(errors.Join(fmt.Errorf("vm %s: apply: %w", name, err), cleanup))
	}
	if plan.Restart {
		if err := c.VMAction(ctx, name, workload.ActionStart, false, 0); err != nil {
			return workload.Plan{}, fmt.Errorf("vm %s: start after the change: %w", name, err)
		}
	}
	inst, err = c.getInstance(ctx, name)
	if err != nil {
		return workload.Plan{}, err
	}
	return plan, verify(desired, meta, inst)
}

// putInstance replaces the spec- and meta-determined configuration and
// all devices.
func (c *Client) putInstance(ctx context.Context, vm workload.VM, meta workload.Meta) error {
	var raw map[string]any
	if err := c.Do(ctx, http.MethodGet, instancePath(vm.Name), nil, &raw); err != nil {
		return err
	}
	cfg, devices, err := instanceFor(vm, meta)
	if err != nil {
		return err
	}
	have, _ := raw["config"].(map[string]any)
	config := map[string]any{}
	for k, v := range have {
		if !isManagedKey(k) {
			config[k] = v
		}
	}
	for k, v := range cfg {
		config[k] = v
	}
	body := map[string]any{"config": config, "devices": devices, "profiles": []string{}}
	for _, k := range []string{"architecture", "ephemeral", "stateful", "description"} {
		if v, ok := raw[k]; ok {
			body[k] = v
		}
	}
	return c.Do(ctx, http.MethodPut, instancePath(vm.Name), body, nil)
}

// DeleteVM deletes a VM and its root disk. A running VM is refused unless
// stop is set; then it is stopped gracefully first. Data volumes are never
// deleted: they are checked to still exist and returned as "pool/volume".
func (c *Client) DeleteVM(ctx context.Context, name string, stop bool, stopTimeout int) ([]string, error) {
	inst, err := c.getInstance(ctx, name)
	if err != nil {
		return nil, err
	}
	var kept []string
	for _, dev := range inst.Devices {
		if dev["type"] == "disk" && dev["source"] != "" && dev["pool"] != "" {
			kept = append(kept, dev["pool"]+"/"+dev["source"])
		}
	}
	slices.Sort(kept)

	if inst.Status != "Stopped" {
		if !stop {
			return nil, fmt.Errorf("vm %s: %w: it is %s; stop it first (or delete with --stop)", name, workload.ErrConflict, inst.Status)
		}
		if err := c.VMAction(ctx, name, workload.ActionStop, false, stopTimeout); err != nil {
			return nil, fmt.Errorf("vm %s: stop before delete: %w", name, err)
		}
	}
	if err := c.Do(ctx, http.MethodDelete, instancePath(name), nil, nil); err != nil {
		return nil, fmt.Errorf("vm %s: delete: %w", name, err)
	}
	if err := c.Do(ctx, http.MethodGet, instancePath(name), nil, nil); !IsNotFound(err) {
		return nil, fmt.Errorf("vm %s: %w: the instance still exists after delete", name, workload.ErrVerify)
	}
	for _, v := range kept {
		pool, vol, _ := strings.Cut(v, "/")
		if err := c.Do(ctx, http.MethodGet, volumePath(pool, vol), nil, nil); err != nil {
			return kept, fmt.Errorf("vm %s: %w: data volume %s is missing after delete: %w", name, workload.ErrVerify, v, err)
		}
	}
	return kept, nil
}
