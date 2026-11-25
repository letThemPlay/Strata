package planner

import (
	"slices"
	"strings"
	"testing"

	"github.com/letthemplay/Strata/internal/ir"
	"github.com/letthemplay/Strata/internal/workload"
)

func vm(name string, cpu int, disks ...workload.Disk) workload.VM {
	v := workload.VM{
		APIVersion: workload.VMAPIVersion,
		Name:       name,
		Resources:  workload.Resources{CPU: cpu, Memory: "1GiB"},
		Disks:      append([]workload.Disk{{Name: "root", Size: "10GiB", Pool: "fast"}}, disks...),
		Networks:   []workload.NIC{{Name: "eth0", Network: "lan"}},
	}
	return v
}

func data(size string) workload.Disk { return workload.Disk{Name: "data", Size: size, Pool: "bulk"} }

// resource is a declared VM as ir.Parse returns it.
func resource(v workload.VM, protected bool) ir.Resource {
	var deps []string
	for _, d := range v.Disks {
		deps = append(deps, "pool/"+d.Pool)
	}
	for _, n := range v.Networks {
		deps = append(deps, "network/"+n.Network)
	}
	slices.Sort(deps)
	vv := v
	return ir.Resource{ID: "vm/" + v.Name, Type: ir.TypeVM, DependsOn: slices.Compact(deps), Lifecycle: ir.Lifecycle{Protected: protected}, VM: &vv}
}

func doc(resources ...ir.Resource) *ir.Document {
	return &ir.Document{
		APIVersion: ir.APIVersion, Node: "hv-01",
		Host:      ir.Host{Project: "strata", Pools: []ir.Pool{{Name: "bulk", Driver: "lvm"}, {Name: "fast", Driver: "zfs"}}, Networks: []ir.Network{{Name: "lan"}}, ImageServers: []string{}},
		Resources: resources,
	}
}

func host() workload.Host {
	return workload.Host{Pools: []string{"bulk", "fast"}, Networks: []string{"lan"}, Volumes: []string{}}
}

// observed is an existing VM.
func observed(v workload.VM, owner string, running bool) workload.VMStatus {
	st := "Stopped"
	if running {
		st = "Running"
	}
	return workload.VMStatus{Spec: v, Meta: workload.Meta{Owner: owner}, Status: st}
}

func mustPlan(t *testing.T, d *ir.Document, obs Observed) Plan {
	t.Helper()
	p, err := Make(d, obs)
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	return p
}

func only(t *testing.T, p Plan) Item {
	t.Helper()
	if len(p.Items) != 1 {
		t.Fatalf("want one item, got %+v", p.Items)
	}
	return p.Items[0]
}

func TestCreate(t *testing.T) {
	p := mustPlan(t, doc(resource(vm("web", 2, data("5GiB")), true)), Observed{Host: host()})
	it := only(t, p)
	if it.Action != ActionCreate || it.Resource != "vm/web" || it.Protect == nil || !*it.Protect || it.Restarts || it.Deletes {
		t.Errorf("item %+v", it)
	}
	if !p.Safe() || p.Converged() || len(p.InSync) != 0 {
		t.Errorf("plan %+v", p)
	}
}

func TestInSyncIsEmpty(t *testing.T) {
	v := vm("web", 2)
	p := mustPlan(t, doc(resource(v, false)), Observed{VMs: []workload.VMStatus{observed(v, workload.OwnerGitOps, true)}, Host: host()})
	if len(p.Items) != 0 || !slices.Equal(p.InSync, []string{"vm/web"}) || !p.Converged() || !p.Safe() {
		t.Errorf("plan %+v", p)
	}
}

func TestUpdate(t *testing.T) {
	cur, want := vm("web", 2), vm("web", 4)
	for _, running := range []bool{true, false} {
		p := mustPlan(t, doc(resource(want, false)), Observed{VMs: []workload.VMStatus{observed(cur, workload.OwnerGitOps, running)}, Host: host()})
		it := only(t, p)
		vmPlan, _ := workload.PlanVM(cur, want, running)
		if it.Action != ActionUpdate || it.VMPlanID != vmPlan.ID || len(it.Changes) != 1 || it.Restarts != running {
			t.Errorf("running=%v: %+v", running, it)
		}
		if p.Safe() == running {
			t.Errorf("running=%v: safe=%v", running, p.Safe())
		}
	}
}

func TestRestoreThenUpdate(t *testing.T) {
	cur := observed(vm("web", 2), workload.OwnerGitOps, true)
	cur.Drift = []string{`config limits.cpu: "8", want "2"`}
	p := mustPlan(t, doc(resource(vm("web", 4), false)), Observed{VMs: []workload.VMStatus{cur}, Host: host()})
	it := only(t, p)
	if it.Action != ActionRestore || len(it.Drift) != 1 || len(it.Changes) != 1 || !it.Restarts || p.Safe() {
		t.Errorf("item %+v", it)
	}
	// Drift alone on a stopped VM: a restore without restart, still not safe.
	cur.Status = "Stopped"
	p = mustPlan(t, doc(resource(vm("web", 2), false)), Observed{VMs: []workload.VMStatus{cur}, Host: host()})
	if it := only(t, p); it.Action != ActionRestore || it.Restarts || len(it.Changes) != 0 || p.Safe() {
		t.Errorf("item %+v", it)
	}
}

func TestLifecycle(t *testing.T) {
	v := vm("web", 2)
	p := mustPlan(t, doc(resource(v, true)), Observed{VMs: []workload.VMStatus{observed(v, workload.OwnerGitOps, true)}, Host: host()})
	if it := only(t, p); it.Action != ActionLifecycle || it.Protect == nil || !*it.Protect || it.Restarts || !p.Safe() {
		t.Errorf("item %+v", it)
	}
}

func TestDeleteKeepsVolumes(t *testing.T) {
	v := vm("old", 1, data("5GiB"))
	p := mustPlan(t, doc(), Observed{VMs: []workload.VMStatus{observed(v, workload.OwnerGitOps, true)}, Host: host()})
	it := only(t, p)
	if it.Action != ActionDelete || !it.Deletes || !slices.Equal(it.KeptVolumes, []string{"bulk/old-data"}) || !p.Deletes() || p.Safe() {
		t.Errorf("item %+v", it)
	}
}

func TestUnmanagedVMsAreNotTouched(t *testing.T) {
	p := mustPlan(t, doc(), Observed{VMs: []workload.VMStatus{observed(vm("manual", 1), workload.OwnerCLI, true)}, Host: host()})
	if len(p.Items) != 0 || !slices.Equal(p.Unmanaged, []string{"manual"}) {
		t.Errorf("plan %+v", p)
	}
}

func TestUnreconcilable(t *testing.T) {
	protectedRunning := observed(vm("web", 2), workload.OwnerGitOps, true)
	protectedRunning.Protected = true
	for name, tc := range map[string]struct {
		res  ir.Resource
		obs  Observed
		want string
	}{
		"owned by the CLI": {
			resource(vm("web", 2), false),
			Observed{VMs: []workload.VMStatus{observed(vm("web", 2), workload.OwnerCLI, false)}, Host: host()},
			"created with `strata vm`",
		},
		"pool missing on the host": {
			resource(vm("web", 2, data("1GiB")), false),
			Observed{Host: workload.Host{Pools: []string{"fast"}, Networks: []string{"lan"}}},
			"pool/bulk does not exist on the host yet",
		},
		"network missing on the host": {
			resource(vm("web", 2), false),
			Observed{Host: workload.Host{Pools: []string{"bulk", "fast"}}},
			"network/lan does not exist on the host yet",
		},
		"kept volume on create": {
			resource(vm("web", 2, data("1GiB")), false),
			Observed{Host: workload.Host{Pools: []string{"bulk", "fast"}, Networks: []string{"lan"}, Volumes: []string{"bulk/web-data"}}},
			"data volume bulk/web-data already exists",
		},
		"kept volume on a new disk": {
			resource(vm("web", 2, data("1GiB")), false),
			Observed{VMs: []workload.VMStatus{observed(vm("web", 2), workload.OwnerGitOps, false)}, Host: workload.Host{Pools: []string{"bulk", "fast"}, Networks: []string{"lan"}, Volumes: []string{"bulk/web-data"}}},
			"data volume bulk/web-data already exists",
		},
		"refused change": {
			resource(vm("web", 2, workload.Disk{Name: "data", Size: "1GiB", Pool: "bulk"}), false),
			Observed{VMs: []workload.VMStatus{observed(vm("web", 2, data("5GiB")), workload.OwnerGitOps, false)}, Host: host()},
			"disks cannot shrink",
		},
		"protected and restarted": {
			resource(vm("web", 4), true),
			Observed{VMs: []workload.VMStatus{protectedRunning}, Host: host()},
			"protected and running",
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := mustPlan(t, doc(tc.res), tc.obs)
			it := only(t, p)
			if it.Action != ActionUnreconcilable || !strings.Contains(strings.Join(it.Reasons, "\n"), tc.want) {
				t.Fatalf("item %+v", it)
			}
			if it.Restarts || it.Deletes || len(it.Changes) != 0 || p.Safe() || !p.Converged() {
				t.Errorf("an unreconcilable item must not apply anything: %+v", it)
			}
		})
	}
}

func TestProtectedVMs(t *testing.T) {
	cur := observed(vm("web", 2), workload.OwnerGitOps, true)
	cur.Protected = true

	// Removed from the configuration: not deleted.
	p := mustPlan(t, doc(), Observed{VMs: []workload.VMStatus{cur}, Host: host()})
	if it := only(t, p); it.Action != ActionUnreconcilable || it.Deletes || !strings.Contains(it.Reasons[0], "protected") {
		t.Errorf("delete: %+v", it)
	}
	// Unprotecting while restarting: the flag change still applies, the
	// restart does not; the plan has not converged.
	p = mustPlan(t, doc(resource(vm("web", 4), false)), Observed{VMs: []workload.VMStatus{cur}, Host: host()})
	it := only(t, p)
	if it.Action != ActionUnreconcilable || it.Protect == nil || *it.Protect || len(it.Changes) != 0 || p.Converged() {
		t.Errorf("unprotect: %+v", it)
	}
	// A stopped protected VM is updated: nothing is restarted.
	cur.Status = "Stopped"
	p = mustPlan(t, doc(resource(vm("web", 4), true)), Observed{VMs: []workload.VMStatus{cur}, Host: host()})
	if it := only(t, p); it.Action != ActionUpdate || it.Restarts {
		t.Errorf("stopped: %+v", it)
	}
}

func TestLimitsIncludeCLIVMs(t *testing.T) {
	d := doc(resource(vm("web", 2), false), resource(vm("db", 1), false))
	d.Host.Limits = ir.Limits{CPU: 4}
	obs := Observed{VMs: []workload.VMStatus{
		observed(vm("manual", 2), workload.OwnerCLI, true),
		observed(vm("db", 1), workload.OwnerGitOps, true),
	}, Host: host()}
	p := mustPlan(t, d, obs)
	it := only(t, p)
	if it.Resource != "vm/web" || it.Action != ActionUnreconcilable || !strings.Contains(it.Reasons[0], "5 CPUs in total exceed the project's CPU limit (4)") {
		t.Errorf("item %+v", it)
	}
	// Deleting a Git VM frees the limit for a new one.
	d = doc(resource(vm("web", 2), false))
	d.Host.Limits = ir.Limits{Instances: 2}
	obs.VMs = append(obs.VMs, observed(vm("old", 1), workload.OwnerGitOps, false))
	obs.VMs = slices.DeleteFunc(obs.VMs, func(v workload.VMStatus) bool { return v.Spec.Name == "db" })
	p = mustPlan(t, d, obs)
	if len(p.Unreconcilable()) != 0 || len(p.Items) != 2 {
		t.Errorf("plan %+v", p.Items)
	}
}

func TestPlanID(t *testing.T) {
	d := doc(resource(vm("web", 2), false))
	obs := Observed{VMs: []workload.VMStatus{observed(vm("manual", 1), workload.OwnerCLI, true)}, Host: workload.Host{Pools: []string{"fast", "bulk"}, Networks: []string{"lan"}}}
	a, b := mustPlan(t, d, obs), mustPlan(t, d, obs)
	if a.ID == "" || a.ID != b.ID {
		t.Fatalf("unstable ID %q %q", a.ID, b.ID)
	}
	if obs.Host.Pools[0] != "fast" {
		t.Error("planning modified the observation")
	}
	changed := []func() (*ir.Document, Observed){
		func() (*ir.Document, Observed) { return doc(resource(vm("web", 3), false)), obs },
		func() (*ir.Document, Observed) { return doc(resource(vm("web", 2), true)), obs },
		func() (*ir.Document, Observed) {
			o := obs
			o.VMs = []workload.VMStatus{observed(vm("manual", 1), workload.OwnerCLI, false)}
			return d, o
		},
		func() (*ir.Document, Observed) {
			o := obs
			o.Host.Volumes = []string{"bulk/x"}
			return d, o
		},
	}
	for i, f := range changed {
		dd, oo := f()
		if mustPlan(t, dd, oo).ID == a.ID {
			t.Errorf("change %d kept the ID", i)
		}
	}
}

func TestInvalidInput(t *testing.T) {
	v := observed(vm("web", 1), workload.OwnerGitOps, false)
	if _, err := Make(doc(), Observed{VMs: []workload.VMStatus{v, v}}); err == nil {
		t.Error("duplicate VM accepted")
	}
	r := resource(vm("web", 1), false)
	r.VM = nil
	if _, err := Make(doc(r), Observed{}); err == nil {
		t.Error("unparsed resource accepted")
	}
}
