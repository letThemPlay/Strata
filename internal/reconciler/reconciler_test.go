package reconciler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/letthemplay/Strata/internal/ir"
	"github.com/letthemplay/Strata/internal/workload"
)

// node is a stateful mock provider: an in-memory node that behaves like
// the Incus provider (verified changes, drift refused by updates).
type node struct {
	vms   map[string]workload.VMStatus
	host  workload.Host
	calls []string
	// fail makes the call whose log entry starts with this prefix fail.
	fail string
	// tamper runs after each successful change (to simulate a change
	// outside Strata).
	tamper func(*node)
}

func newNode() *node {
	return &node{vms: map[string]workload.VMStatus{}, host: workload.Host{Pools: []string{"bulk", "fast"}, Networks: []string{"lan"}, Volumes: []string{}}}
}

func (n *node) call(format string, a ...any) error {
	c := fmt.Sprintf(format, a...)
	n.calls = append(n.calls, c)
	if n.fail != "" && strings.HasPrefix(c, n.fail) {
		return errors.New("simulated failure")
	}
	return nil
}

func (n *node) changed() {
	if n.tamper != nil {
		n.tamper(n)
	}
}

func (n *node) ListVMs(context.Context) ([]workload.VMStatus, error) {
	out := []workload.VMStatus{}
	for _, v := range n.vms {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b workload.VMStatus) int { return strings.Compare(a.Spec.Name, b.Spec.Name) })
	return out, nil
}

func (n *node) Host(context.Context) (workload.Host, error) {
	h := n.host
	h.Volumes = slices.Clone(h.Volumes)
	return h, nil
}

func (n *node) CreateVM(_ context.Context, vm workload.VM, meta workload.Meta) error {
	if err := n.call("create %s %s protected=%v", vm.Name, meta.Owner, meta.Protected); err != nil {
		return err
	}
	if _, ok := n.vms[vm.Name]; ok {
		return workload.ErrExists
	}
	for _, d := range vm.Disks {
		if d.Name != workload.RootDisk {
			n.host.Volumes = append(n.host.Volumes, d.Pool+"/"+vm.VolumeName(d))
		}
	}
	n.vms[vm.Name] = workload.VMStatus{Spec: vm, Meta: meta, Status: "Stopped"}
	n.changed()
	return nil
}

func (n *node) VMAction(_ context.Context, name, action string, _ bool, _ int) error {
	if err := n.call("%s %s", action, name); err != nil {
		return err
	}
	v := n.vms[name]
	v.Status = map[string]string{"start": "Running", "stop": "Stopped", "restart": "Running"}[action]
	n.vms[name] = v
	return nil
}

func (n *node) UpdateVM(_ context.Context, vm workload.VM, planID string, _ int) (workload.Plan, error) {
	if err := n.call("update %s", vm.Name); err != nil {
		return workload.Plan{}, err
	}
	cur := n.vms[vm.Name]
	if len(cur.Drift) > 0 {
		return workload.Plan{}, workload.ErrConflict
	}
	plan, err := workload.PlanVM(cur.Spec, vm, cur.Status == "Running")
	if err != nil || plan.ID != planID {
		return workload.Plan{}, fmt.Errorf("stale or refused: %w", err)
	}
	for _, c := range plan.Changes {
		if c.CreatesVolume {
			for _, d := range vm.Disks {
				if "disks."+d.Name == c.Field {
					n.host.Volumes = append(n.host.Volumes, d.Pool+"/"+vm.VolumeName(d))
				}
			}
		}
	}
	cur.Spec = vm
	n.vms[vm.Name] = cur
	n.changed()
	return plan, nil
}

func (n *node) RestoreVM(_ context.Context, name string, _ int) error {
	if err := n.call("restore %s", name); err != nil {
		return err
	}
	v := n.vms[name]
	v.Drift = nil
	n.vms[name] = v
	n.changed()
	return nil
}

func (n *node) SetMeta(_ context.Context, name string, meta workload.Meta) error {
	if err := n.call("meta %s %s protected=%v", name, meta.Owner, meta.Protected); err != nil {
		return err
	}
	v, ok := n.vms[name]
	if !ok {
		return workload.ErrNotFound
	}
	v.Meta = meta
	n.vms[name] = v
	n.changed()
	return nil
}

func (n *node) DeleteVM(_ context.Context, name string, stop bool, _ int) ([]string, error) {
	if err := n.call("delete %s stop=%v", name, stop); err != nil {
		return nil, err
	}
	v := n.vms[name]
	delete(n.vms, name)
	var kept []string
	for _, d := range v.Spec.Disks {
		if d.Name != workload.RootDisk {
			kept = append(kept, d.Pool+"/"+v.Spec.VolumeName(d))
		}
	}
	n.changed()
	return kept, nil
}

func vm(name string, cpu int, disks ...workload.Disk) workload.VM {
	return workload.VM{
		APIVersion: workload.VMAPIVersion, Name: name,
		Resources: workload.Resources{CPU: cpu, Memory: "1GiB"},
		Disks:     append([]workload.Disk{{Name: "root", Size: "10GiB", Pool: "fast"}}, disks...),
		Networks:  []workload.NIC{{Name: "eth0", Network: "lan"}},
	}
}

func res(v workload.VM, protected bool) ir.Resource {
	deps := []string{"network/lan"}
	for _, d := range v.Disks {
		deps = append(deps, "pool/"+d.Pool)
	}
	slices.Sort(deps)
	vv := v
	return ir.Resource{ID: "vm/" + v.Name, Type: ir.TypeVM, DependsOn: slices.Compact(deps), Lifecycle: ir.Lifecycle{Protected: protected}, VM: &vv}
}

func doc(rs ...ir.Resource) *ir.Document {
	return &ir.Document{APIVersion: ir.APIVersion, Node: "hv-01", Host: ir.Host{Project: "strata"}, Resources: rs}
}

func gitops(n *node, v workload.VM, status string, protected bool) {
	n.vms[v.Name] = workload.VMStatus{Spec: v, Meta: workload.Meta{Owner: workload.OwnerGitOps, Protected: protected}, Status: status}
}

func planAndApply(t *testing.T, n *node, d *ir.Document) (Result, error) {
	t.Helper()
	p, err := Plan(context.Background(), n, d)
	if err != nil {
		t.Fatal(err)
	}
	return Apply(context.Background(), n, d, p.ID, Options{StopTimeout: 30})
}

func TestApplyOrderAndConvergence(t *testing.T) {
	n := newNode()
	gitops(n, vm("upd", 1), "Running", false)
	gitops(n, vm("old", 1, workload.Disk{Name: "data", Size: "1GiB", Pool: "bulk"}), "Running", false)
	gitops(n, vm("life", 1), "Running", false)
	gitops(n, vm("drift", 1), "Running", false)
	d := n.vms["drift"]
	d.Drift = []string{"config limits.cpu"}
	n.vms["drift"] = d
	n.vms["manual"] = workload.VMStatus{Spec: vm("manual", 1), Meta: workload.Meta{Owner: workload.OwnerCLI}, Status: "Running"}

	desired := doc(res(vm("drift", 1), false), res(vm("life", 1), true), res(vm("new", 1), true), res(vm("upd", 2), false))
	r, err := planAndApply(t, n, desired)
	if err != nil {
		t.Fatalf("Apply: %v\n%+v", err, r)
	}
	want := []string{
		"meta life gitops protected=true",
		"restore drift",
		"update upd",
		"delete old stop=true",
		"create new gitops protected=true", "start new",
	}
	if !slices.Equal(n.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(n.calls, "\n"), strings.Join(want, "\n"))
	}
	if !r.Converged || r.After == nil || len(r.After.Items) != 0 {
		t.Errorf("result %+v", r)
	}
	for _, it := range r.Items {
		if it.Status != StatusSucceeded {
			t.Errorf("item %+v", it)
		}
		if it.Resource == "vm/old" && !slices.Equal(it.KeptVolumes, []string{"bulk/old-data"}) {
			t.Errorf("kept volumes %+v", it)
		}
	}
	if _, ok := n.vms["manual"]; !ok {
		t.Error("an unmanaged VM was touched")
	}

	// Idempotent: nothing left to do, and applying again changes nothing.
	n.calls = nil
	p, err := Plan(context.Background(), n, desired)
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("second plan %+v %v", p, err)
	}
	if _, err := Apply(context.Background(), n, desired, p.ID, Options{}); err != nil || len(n.calls) != 0 {
		t.Errorf("second apply: %v, calls %v", err, n.calls)
	}
}

func TestStalePlanChangesNothing(t *testing.T) {
	n := newNode()
	desired := doc(res(vm("web", 1), false))
	p, err := Plan(context.Background(), n, desired)
	if err != nil {
		t.Fatal(err)
	}
	gitops(n, vm("other", 1), "Running", false) // the node changed
	if _, err := Apply(context.Background(), n, desired, p.ID, Options{}); !errors.Is(err, ErrStale) {
		t.Fatalf("err %v", err)
	}
	if len(n.calls) != 0 {
		t.Errorf("calls %v", n.calls)
	}
	if _, err := Apply(context.Background(), n, desired, "not-a-plan", Options{}); !errors.Is(err, ErrStale) {
		t.Errorf("unknown ID: %v", err)
	}
}

func TestStopsAtFirstFailure(t *testing.T) {
	n := newNode()
	gitops(n, vm("a", 1), "Stopped", false)
	n.fail = "update a"
	r, err := planAndApply(t, n, doc(res(vm("a", 2), false), res(vm("b", 1), false)))
	if err == nil || !strings.Contains(err.Error(), "update vm/a") {
		t.Fatalf("err %v", err)
	}
	statuses := map[string]string{}
	for _, it := range r.Items {
		statuses[it.Resource] = it.Status
	}
	if statuses["vm/a"] != StatusFailed || statuses["vm/b"] != StatusNotAttempted || r.Converged {
		t.Errorf("result %+v", r)
	}
	if slices.ContainsFunc(n.calls, func(c string) bool { return strings.HasPrefix(c, "create") }) {
		t.Errorf("continued after the failure: %v", n.calls)
	}
}

func TestUnreconcilableItemsAreSkipped(t *testing.T) {
	n := newNode()
	n.vms["web"] = workload.VMStatus{Spec: vm("web", 1), Meta: workload.Meta{Owner: workload.OwnerCLI}, Status: "Running"}
	prot := vm("db", 1)
	gitops(n, prot, "Running", true)
	// db: unprotect and restart. The flag is applied; the restart is not.
	r, err := planAndApply(t, n, doc(res(vm("db", 2), false), res(vm("web", 1), false), res(vm("new", 1), false)))
	if err == nil || !errors.Is(err, ErrNotConverged) {
		t.Fatalf("want not converged, got %v\n%+v", err, r)
	}
	want := []string{"meta db gitops protected=false", "create new gitops protected=false", "start new"}
	if !slices.Equal(n.calls, want) {
		t.Errorf("calls %v", n.calls)
	}
	byRes := map[string]ItemResult{}
	for _, it := range r.Items {
		byRes[it.Resource] = it
	}
	if byRes["vm/web"].Status != StatusSkipped || !strings.Contains(byRes["vm/web"].Error, "strata vm") || byRes["vm/db"].Status != StatusSucceeded {
		t.Errorf("results %+v", r.Items)
	}
	// The next plan restarts db, which is no longer protected.
	n.calls = nil
	r, err = planAndApply(t, n, doc(res(vm("db", 2), false), res(vm("web", 1), false), res(vm("new", 1), false)))
	if err != nil || !r.Converged || !slices.Equal(n.calls, []string{"update db"}) {
		t.Errorf("second apply: %v %v %+v", err, n.calls, r.After)
	}
}

func TestNotConvergedWhenChangedDuringApply(t *testing.T) {
	n := newNode()
	n.tamper = func(n *node) {
		if v, ok := n.vms["web"]; ok {
			v.Drift = []string{"config limits.cpu changed"}
			n.vms["web"] = v
		}
	}
	r, err := planAndApply(t, n, doc(res(vm("web", 1), false)))
	if !errors.Is(err, ErrNotConverged) || r.Converged || !strings.Contains(err.Error(), "restore vm/web") {
		t.Errorf("err %v result %+v", err, r)
	}
}

func TestApplyEmptyPlan(t *testing.T) {
	n := newNode()
	r, err := planAndApply(t, n, doc())
	if err != nil || !r.Converged || len(r.Items) != 0 || len(n.calls) != 0 {
		t.Errorf("%+v %v %v", r, err, n.calls)
	}
}

var _ Provider = (*node)(nil)

// The planner and reconciler agree on what is left after an apply.
func TestAfterIsAPlan(t *testing.T) {
	n := newNode()
	r, err := planAndApply(t, n, doc(res(vm("web", 1), false)))
	if err != nil {
		t.Fatal(err)
	}
	if r.After.Node != "hv-01" || !slices.Equal(r.After.InSync, []string{"vm/web"}) {
		t.Errorf("after %+v", r.After)
	}
}
