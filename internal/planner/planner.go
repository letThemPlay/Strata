// Package planner compares a node's desired workloads (an IR document)
// with what the node has and produces one plan for the node: which VMs to
// create, update, restore after drift, change the lifecycle of, or
// delete, and which cannot be reconciled and why (M3 slice 2).
//
// Planning is pure: it reads an IR document and an observation and has no
// side effects. internal/reconciler applies plans.
package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/letthemplay/Strata/internal/ir"
	"github.com/letthemplay/Strata/internal/workload"
)

// Item actions, by what an item mainly does. An item may also carry a
// lifecycle change (Protect) and, for a restore, the update that follows.
const (
	ActionCreate         = "create"
	ActionUpdate         = "update"
	ActionRestore        = "restore"
	ActionLifecycle      = "lifecycle"
	ActionDelete         = "delete"
	ActionUnreconcilable = "unreconcilable"
)

// Observed is what the node has: every Strata VM (owned by Git or the
// CLI) and the host objects the workload project can use.
type Observed struct {
	VMs  []workload.VMStatus `json:"vms"`
	Host workload.Host       `json:"host"`
}

// Item is what the plan does to one resource.
type Item struct {
	// Resource is the resource ID, e.g. "vm/web-01".
	Resource string `json:"resource"`
	Action   string `json:"action"`
	// Changes are the spec changes of an update (also after a restore).
	Changes []workload.Change `json:"changes,omitempty"`
	// VMPlanID identifies the VM's update plan (workload.PlanVM); the
	// provider applies the update only if it still has this ID.
	VMPlanID string `json:"vmPlanId,omitempty"`
	// Drift lists what a restore puts back.
	Drift []string `json:"drift,omitempty"`
	// Protect is the new lifecycle.protected value, when it changes.
	Protect *bool `json:"protect,omitempty"`
	// Restarts: a running VM is stopped and started again.
	Restarts bool `json:"restarts,omitempty"`
	// Deletes: the VM and its root disk are deleted (data volumes are
	// kept: KeptVolumes).
	Deletes     bool     `json:"deletes,omitempty"`
	KeptVolumes []string `json:"keptVolumes,omitempty"`
	// Reasons say why the item cannot be reconciled. Its lifecycle change
	// (Protect), if any, is still applied.
	Reasons []string `json:"reasons,omitempty"`
}

// Plan is the plan for one node. It is applied only when approved by its
// ID, which identifies the desired state and the observation it was made
// from.
type Plan struct {
	ID   string `json:"id"`
	Node string `json:"node"`
	// Items are the resources with something to do or that cannot be
	// reconciled, sorted by resource ID.
	Items []Item `json:"items"`
	// InSync are declared resources with nothing to do.
	InSync []string `json:"inSync"`
	// Unmanaged are VMs created with `strata vm` that are not declared;
	// the plan never touches them.
	Unmanaged []string `json:"unmanaged"`
}

// Restarts reports whether applying the plan restarts a running VM.
func (p Plan) Restarts() bool {
	return slices.ContainsFunc(p.Items, func(i Item) bool { return i.Restarts })
}

// Deletes reports whether applying the plan deletes a VM.
func (p Plan) Deletes() bool {
	return slices.ContainsFunc(p.Items, func(i Item) bool { return i.Deletes })
}

// Unreconcilable returns the items that cannot be reconciled.
func (p Plan) Unreconcilable() []Item {
	var out []Item
	for _, i := range p.Items {
		if i.Action == ActionUnreconcilable {
			out = append(out, i)
		}
	}
	return out
}

// Converged reports whether nothing is left to apply: every remaining
// item is unreconcilable and has no lifecycle change.
func (p Plan) Converged() bool {
	return !slices.ContainsFunc(p.Items, func(i Item) bool { return i.Action != ActionUnreconcilable || i.Protect != nil })
}

// Safe reports whether the plan may be applied without approval when
// automatic application of safe plans is enabled (M3 decision 4): no
// restarts, no deletions, no restores and nothing unreconcilable.
func (p Plan) Safe() bool {
	return !slices.ContainsFunc(p.Items, func(i Item) bool {
		return i.Restarts || i.Deletes || i.Action == ActionRestore || i.Action == ActionUnreconcilable
	})
}

// ErrInvalid reports an observation the planner cannot use.
var ErrInvalid = errors.New("invalid observation")

// Make plans doc against obs. doc must have been parsed and validated
// (ir.Parse).
func Make(doc *ir.Document, obs Observed) (Plan, error) {
	observed := map[string]workload.VMStatus{}
	for _, v := range obs.VMs {
		if _, dup := observed[v.Spec.Name]; dup {
			return Plan{}, fmt.Errorf("%w: VM %s is listed twice", ErrInvalid, v.Spec.Name)
		}
		observed[v.Spec.Name] = v
	}
	hostObjects := map[string]bool{}
	for _, p := range obs.Host.Pools {
		hostObjects["pool/"+p] = true
	}
	for _, n := range obs.Host.Networks {
		hostObjects["network/"+n] = true
	}

	plan := Plan{Node: doc.Node, Items: []Item{}, InSync: []string{}, Unmanaged: []string{}}
	declared := map[string]bool{}
	for _, r := range doc.Resources {
		if r.VM == nil {
			return Plan{}, fmt.Errorf("%w: resource %s has no decoded spec; parse the IR with ir.Parse", ErrInvalid, r.ID)
		}
		declared[r.VM.Name] = true
		item, ok := planVM(r, observed, hostObjects, obs.Host.Volumes)
		if ok {
			plan.Items = append(plan.Items, item)
		} else {
			plan.InSync = append(plan.InSync, r.ID)
		}
	}
	for _, v := range obs.VMs {
		if declared[v.Spec.Name] {
			continue
		}
		if v.Owner != workload.OwnerGitOps {
			plan.Unmanaged = append(plan.Unmanaged, v.Spec.Name)
			continue
		}
		plan.Items = append(plan.Items, planDelete(v))
	}
	checkLimits(&plan, doc, observed)

	slices.SortFunc(plan.Items, func(a, b Item) int { return strings.Compare(a.Resource, b.Resource) })
	slices.Sort(plan.InSync)
	slices.Sort(plan.Unmanaged)
	id, err := planID(doc, obs)
	if err != nil {
		return Plan{}, err
	}
	plan.ID = id
	return plan, nil
}

// planVM plans one declared VM; ok is false when nothing is to be done.
func planVM(r ir.Resource, observed map[string]workload.VMStatus, hostObjects map[string]bool, volumes []string) (Item, bool) {
	desired := *r.VM
	item := Item{Resource: r.ID}
	var reasons []string
	for _, dep := range r.DependsOn {
		if !hostObjects[dep] {
			reasons = append(reasons, fmt.Sprintf("%s does not exist on the host yet: apply the host configuration (nixos-rebuild) first", dep))
		}
	}

	cur, exists := observed[desired.Name]
	if !exists {
		item.Action = ActionCreate
		if r.Lifecycle.Protected {
			p := true
			item.Protect = &p
		}
		for _, d := range desired.Disks {
			if v := d.Pool + "/" + desired.VolumeName(d); d.Name != workload.RootDisk && slices.Contains(volumes, v) {
				reasons = append(reasons, fmt.Sprintf("data volume %s already exists (kept from an earlier VM); it is not reused automatically until volume management (M4)", v))
			}
		}
		return withReasons(item, reasons), true
	}
	if cur.Owner != workload.OwnerGitOps {
		item.Reasons = []string{"a VM of this name was created with `strata vm` and is not managed through Git: adopt it (`strata config adopt`) or rename the declaration"}
		item.Action = ActionUnreconcilable
		return item, true
	}

	running := cur.Status == "Running"
	if r.Lifecycle.Protected != cur.Protected {
		p := r.Lifecycle.Protected
		item.Protect = &p
		item.Action = ActionLifecycle
	}
	if len(cur.Drift) > 0 {
		item.Action = ActionRestore
		item.Drift = cur.Drift
		item.Restarts = running
	}
	vmPlan, err := workload.PlanVM(cur.Spec, desired, running)
	if err != nil {
		reasons = append(reasons, err.Error())
	} else if !vmPlan.Empty() {
		if item.Action != ActionRestore {
			item.Action = ActionUpdate
		}
		item.Changes = vmPlan.Changes
		item.VMPlanID = vmPlan.ID
		item.Restarts = item.Restarts || vmPlan.Restart
		for _, c := range vmPlan.Changes {
			if c.CreatesVolume {
				d := strings.TrimPrefix(c.Field, "disks.")
				for _, disk := range desired.Disks {
					if v := disk.Pool + "/" + desired.VolumeName(disk); disk.Name == d && slices.Contains(volumes, v) {
						reasons = append(reasons, fmt.Sprintf("data volume %s already exists; it is not reused automatically until volume management (M4)", v))
					}
				}
			}
		}
	}
	if cur.Protected && item.Restarts {
		reasons = append(reasons, "the VM is protected and running, and this change restarts it: set lifecycle.protected = false and apply that first")
	}
	if item.Action == "" && len(reasons) == 0 {
		return Item{}, false
	}
	return withReasons(item, reasons), true
}

// withReasons marks an item unreconcilable if there are reasons. Only the
// lifecycle change of an existing VM is kept: everything else is not
// applied.
func withReasons(item Item, reasons []string) Item {
	if len(reasons) == 0 {
		return item
	}
	protect := item.Protect
	if item.Action == ActionCreate {
		protect = nil
	}
	return Item{Resource: item.Resource, Action: ActionUnreconcilable, Protect: protect, Reasons: reasons}
}

// planDelete plans the deletion of a Git-owned VM that is no longer
// declared.
func planDelete(v workload.VMStatus) Item {
	item := Item{Resource: ir.TypeVM + "/" + v.Spec.Name, Action: ActionDelete, Deletes: true}
	for _, d := range v.Spec.Disks {
		if d.Name != workload.RootDisk {
			item.KeptVolumes = append(item.KeptVolumes, d.Pool+"/"+v.Spec.VolumeName(d))
		}
	}
	if v.Protected {
		return Item{Resource: item.Resource, Action: ActionUnreconcilable, Reasons: []string{
			"the VM is protected: it was removed from the configuration but is not deleted; declare it again, or declare it with lifecycle.protected = false, apply, and then remove it",
		}}
	}
	return item
}

// checkLimits marks creates and updates unreconcilable when the VMs the
// node would have after the plan (including VMs created with `strata vm`)
// exceed the project limits the host declares.
func checkLimits(plan *Plan, doc *ir.Document, observed map[string]workload.VMStatus) {
	l := doc.Host.Limits
	if l.CPU == 0 && l.Memory == "" && l.Instances == 0 {
		return
	}
	desired := map[string]workload.VM{}
	for _, r := range doc.Resources {
		desired[r.ID] = *r.VM
	}
	final := map[string]workload.VM{}
	for name, v := range observed {
		final[name] = v.Spec
	}
	for _, it := range plan.Items {
		name := strings.TrimPrefix(it.Resource, ir.TypeVM+"/")
		switch it.Action {
		case ActionCreate, ActionUpdate, ActionRestore:
			final[name] = desired[it.Resource]
		case ActionDelete:
			delete(final, name)
		}
	}
	var cpus int
	var memory uint64
	for _, vm := range final {
		cpus += vm.Resources.CPU
		if n, err := workload.ParseSize(vm.Resources.Memory); err == nil {
			memory += n
		}
	}
	var over []string
	if l.Instances > 0 && len(final) > l.Instances {
		over = append(over, fmt.Sprintf("%d VMs exceed the project's instance limit (%d)", len(final), l.Instances))
	}
	if l.CPU > 0 && cpus > l.CPU {
		over = append(over, fmt.Sprintf("%d CPUs in total exceed the project's CPU limit (%d)", cpus, l.CPU))
	}
	if limit, err := workload.ParseSize(l.Memory); err == nil && memory > limit {
		over = append(over, fmt.Sprintf("the VMs' memory in total exceeds the project's memory limit (%s)", l.Memory))
	}
	if len(over) == 0 {
		return
	}
	note := "with the VMs created with `strata vm`, " + strings.Join(over, "; ")
	for i, it := range plan.Items {
		if it.Action == ActionCreate || grows(it.Changes) {
			plan.Items[i] = withReasons(it, []string{note})
		}
	}
}

// grows reports whether changes add CPU or memory.
func grows(changes []workload.Change) bool {
	for _, c := range changes {
		switch c.Field {
		case "resources.cpu":
			var from, to int
			_, _ = fmt.Sscan(c.From, &from)
			_, _ = fmt.Sscan(c.To, &to)
			if to > from {
				return true
			}
		case "resources.memory":
			from, _ := workload.ParseSize(c.From)
			to, _ := workload.ParseSize(c.To)
			if to > from {
				return true
			}
		}
	}
	return false
}

// planID identifies the desired state and the observation a plan was made
// from: the same inputs always give the same ID.
func planID(doc *ir.Document, obs Observed) (string, error) {
	type observedVM struct {
		Spec    workload.VM   `json:"spec"`
		Meta    workload.Meta `json:"meta"`
		Running bool          `json:"running"`
		Drift   []string      `json:"drift"`
	}
	type desiredResource struct {
		ID        string       `json:"id"`
		DependsOn []string     `json:"dependsOn"`
		Lifecycle ir.Lifecycle `json:"lifecycle"`
		Spec      workload.VM  `json:"spec"`
	}
	resources := make([]desiredResource, len(doc.Resources))
	for i, r := range doc.Resources {
		resources[i] = desiredResource{ID: r.ID, DependsOn: r.DependsOn, Lifecycle: r.Lifecycle, Spec: *r.VM}
	}
	vms := make([]observedVM, len(obs.VMs))
	for i, v := range obs.VMs {
		vms[i] = observedVM{Spec: v.Spec, Meta: v.Meta, Running: v.Status == "Running", Drift: v.Drift}
	}
	slices.SortFunc(vms, func(a, b observedVM) int { return strings.Compare(a.Spec.Name, b.Spec.Name) })
	// Sorted copies: the caller's observation is not modified.
	sorted := func(s []string) []string { c := slices.Clone(s); slices.Sort(c); return c }
	host := workload.Host{Pools: sorted(obs.Host.Pools), Networks: sorted(obs.Host.Networks), Volumes: sorted(obs.Host.Volumes)}
	data, err := json.Marshal(struct {
		Node      string            `json:"node"`
		Host      ir.Host           `json:"host"`
		Resources []desiredResource `json:"resources"`
		VMs       []observedVM      `json:"vms"`
		Observed  workload.Host     `json:"observed"`
	}{doc.Node, doc.Host, resources, vms, host})
	if err != nil {
		return "", fmt.Errorf("plan ID: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8]), nil
}
