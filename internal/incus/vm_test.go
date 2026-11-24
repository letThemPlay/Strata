package incus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/letthemplay/Strata/internal/workload"
)

// fakeVMServer is an in-memory Incus with instances and custom volumes.
type fakeVMServer struct {
	mu        sync.Mutex
	instances map[string]map[string]any
	volumes   map[string]bool // "pool/name"
	// failCreate makes instance creation fail.
	failCreate bool
	// tamper changes the stored instance after creation.
	tamper func(map[string]any)
	// noStart leaves the status unchanged on start.
	noStart bool
	deletes []string
	// calls records state changes, puts and deletes in order.
	calls []string
	// failPut makes instance updates fail.
	failPut bool
	// lastSource is the source of the last instance creation.
	lastSource map[string]any
}

func reply(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	if code >= 400 {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": v, "error_code": code})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "metadata": v})
}

func (f *fakeVMServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.Method == http.MethodGet && p == "/1.0/instances":
		list := []map[string]any{}
		for _, i := range f.instances {
			list = append(list, i)
		}
		reply(w, 200, list)
	case r.Method == http.MethodPost && p == "/1.0/instances":
		if f.failCreate {
			reply(w, 400, "Failed creating instance from image")
			return
		}
		name := body["name"].(string)
		f.lastSource, _ = body["source"].(map[string]any)
		inst := map[string]any{"name": name, "type": body["type"], "status": "Stopped", "config": body["config"], "devices": body["devices"]}
		if f.tamper != nil {
			f.tamper(inst)
		}
		f.instances[name] = inst
		reply(w, 200, nil)
	case strings.HasPrefix(p, "/1.0/instances/") && strings.HasSuffix(p, "/state") && r.Method == http.MethodPut:
		name := strings.TrimSuffix(strings.TrimPrefix(p, "/1.0/instances/"), "/state")
		f.calls = append(f.calls, body["action"].(string))
		if !f.noStart {
			f.instances[name]["status"] = map[string]string{"start": "Running", "stop": "Stopped", "restart": "Running"}[body["action"].(string)]
		}
		reply(w, 200, nil)
	case strings.HasPrefix(p, "/1.0/instances/") && r.Method == http.MethodPut:
		name := strings.TrimPrefix(p, "/1.0/instances/")
		f.calls = append(f.calls, "put")
		if f.failPut {
			reply(w, 400, "Invalid device")
			return
		}
		f.instances[name]["config"], f.instances[name]["devices"] = body["config"], body["devices"]
		reply(w, 200, nil)
	case strings.HasPrefix(p, "/1.0/instances/") && r.Method == http.MethodDelete:
		delete(f.instances, strings.TrimPrefix(p, "/1.0/instances/"))
		f.calls = append(f.calls, "delete")
		reply(w, 200, nil)
	case strings.HasPrefix(p, "/1.0/instances/") && r.Method == http.MethodGet:
		inst, ok := f.instances[strings.TrimPrefix(p, "/1.0/instances/")]
		if !ok {
			reply(w, 404, "Instance not found")
			return
		}
		reply(w, 200, inst)
	case strings.HasPrefix(p, "/1.0/storage-pools/") && strings.Contains(p, "/volumes/custom"):
		parts := strings.Split(strings.TrimPrefix(p, "/1.0/storage-pools/"), "/")
		pool := parts[0]
		switch r.Method {
		case http.MethodPost:
			f.volumes[pool+"/"+body["name"].(string)] = true
			reply(w, 200, nil)
		case http.MethodGet:
			if f.volumes[pool+"/"+parts[3]] {
				reply(w, 200, map[string]any{})
			} else {
				reply(w, 404, "Storage volume not found")
			}
		case http.MethodDelete:
			delete(f.volumes, pool+"/"+parts[3])
			f.deletes = append(f.deletes, pool+"/"+parts[3])
			reply(w, 200, nil)
		}
	default:
		reply(w, 404, "not found: "+r.Method+" "+p)
	}
}

func newFakeVMs(t *testing.T) (*fakeVMServer, *Client) {
	t.Helper()
	f := &fakeVMServer{instances: map[string]map[string]any{}, volumes: map[string]bool{}}
	return f, NewUnix(serveUnix(t, f.ServeHTTP), "strata", time.Second)
}

func testVM() workload.VM {
	return workload.VM{
		APIVersion: workload.VMAPIVersion,
		Name:       "web-01",
		Resources:  workload.Resources{CPU: 2, Memory: "1GiB"},
		Image:      "debian-13",
		Disks: []workload.Disk{
			{Name: "root", Size: "10GiB", Pool: "fast"},
			{Name: "data", Size: "20GiB", Pool: "bulk"},
		},
		Networks:  []workload.NIC{{Name: "eth0", Network: "lan"}},
		Autostart: true,
		CloudInit: &workload.CloudInit{SSHAuthorizedKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl a@b"}},
	}
}

func TestCreateVM(t *testing.T) {
	f, c := newFakeVMs(t)
	ctx := context.Background()
	if err := c.CreateVM(ctx, testVM()); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	inst := f.instances["web-01"]
	cfg := inst["config"].(map[string]any)
	devices := inst["devices"].(map[string]any)
	if cfg["limits.cpu"] != "2" || cfg["limits.memory"] != "1GiB" || cfg["boot.autostart"] != "true" || cfg[keyManaged] != "true" ||
		cfg["security.nesting"] != "false" {
		t.Errorf("config %v", cfg)
	}
	if ud := cfg["cloud-init.user-data"].(string); !strings.HasPrefix(ud, "#cloud-config\nssh_authorized_keys:\n  - \"ssh-ed25519 ") {
		t.Errorf("cloud-init %q", ud)
	}
	root := devices["root"].(map[string]any)
	data := devices["data"].(map[string]any)
	if root["pool"] != "fast" || root["size"] != "10GiB" || root["path"] != "/" || data["source"] != "web-01-data" {
		t.Errorf("devices %v", devices)
	}
	if !f.volumes["bulk/web-01-data"] {
		t.Error("data volume not created")
	}

	st, err := c.GetVM(ctx, "web-01")
	if err != nil || st.Status != "Stopped" || st.Spec.Name != "web-01" || st.Spec.Disks[1].Pool != "bulk" {
		t.Errorf("GetVM: %+v %v", st, err)
	}
	list, err := c.ListVMs(ctx)
	if err != nil || len(list) != 1 {
		t.Errorf("ListVMs: %+v %v", list, err)
	}

	if err := c.CreateVM(ctx, testVM()); !errors.Is(err, workload.ErrExists) {
		t.Errorf("second create: %v", err)
	}
}

func TestCreateVMRefusesExistingVolume(t *testing.T) {
	f, c := newFakeVMs(t)
	f.volumes["bulk/web-01-data"] = true
	err := c.CreateVM(context.Background(), testVM())
	if !errors.Is(err, workload.ErrExists) || !strings.Contains(err.Error(), "never reused") {
		t.Fatalf("got %v", err)
	}
	if len(f.deletes) != 0 || !f.volumes["bulk/web-01-data"] {
		t.Error("an existing volume must never be removed")
	}
}

func TestCreateVMFailureRemovesOnlyNewVolumes(t *testing.T) {
	f, c := newFakeVMs(t)
	f.failCreate = true
	err := c.CreateVM(context.Background(), testVM())
	if err == nil || !strings.Contains(err.Error(), "Failed creating instance") {
		t.Fatalf("got %v", err)
	}
	if f.volumes["bulk/web-01-data"] || len(f.deletes) != 1 {
		t.Errorf("new volume not cleaned up: %v %v", f.volumes, f.deletes)
	}
}

func TestCreateVMVerifies(t *testing.T) {
	f, c := newFakeVMs(t)
	f.tamper = func(inst map[string]any) {
		inst["config"].(map[string]any)["limits.cpu"] = "8"
		inst["devices"].(map[string]any)["extra"] = map[string]any{"type": "disk"}
	}
	err := c.CreateVM(context.Background(), testVM())
	if !errors.Is(err, workload.ErrVerify) || !strings.Contains(err.Error(), `config limits.cpu: "8", want "2"`) ||
		!strings.Contains(err.Error(), "unexpected device extra") {
		t.Fatalf("got %v", err)
	}
	// The VM exists: its volumes belong to it and are kept.
	if !f.volumes["bulk/web-01-data"] {
		t.Error("volume of an existing VM removed")
	}
}

func TestVMActions(t *testing.T) {
	f, c := newFakeVMs(t)
	ctx := context.Background()
	if err := c.CreateVM(ctx, testVM()); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{workload.ActionStart, workload.ActionRestart, workload.ActionStop} {
		if err := c.VMAction(ctx, "web-01", a, false, 30); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	f.noStart = true
	if err := c.VMAction(ctx, "web-01", workload.ActionStart, false, 30); !errors.Is(err, workload.ErrVerify) {
		t.Errorf("unverified start: %v", err)
	}
	if err := c.VMAction(ctx, "nope", workload.ActionStart, false, 30); !errors.Is(err, workload.ErrNotFound) {
		t.Errorf("missing VM: %v", err)
	}
	if err := c.VMAction(ctx, "web-01", "explode", false, 30); err == nil {
		t.Error("unknown action accepted")
	}
}

func TestUnmanagedInstancesAreInvisible(t *testing.T) {
	f, c := newFakeVMs(t)
	f.instances["manual"] = map[string]any{"name": "manual", "type": "virtual-machine", "status": "Running", "config": map[string]any{}}
	if _, err := c.GetVM(context.Background(), "manual"); !errors.Is(err, workload.ErrNotFound) {
		t.Errorf("GetVM: %v", err)
	}
	if list, _ := c.ListVMs(context.Background()); len(list) != 0 {
		t.Errorf("ListVMs: %v", list)
	}
	if err := c.VMAction(context.Background(), "manual", workload.ActionStop, true, 0); !errors.Is(err, workload.ErrNotFound) {
		t.Errorf("VMAction: %v", err)
	}
}

func createdVM(t *testing.T, running bool) (*fakeVMServer, *Client) {
	t.Helper()
	f, c := newFakeVMs(t)
	if err := c.CreateVM(context.Background(), testVM()); err != nil {
		t.Fatal(err)
	}
	if running {
		f.instances["web-01"]["status"] = "Running"
	}
	f.instances["web-01"]["config"].(map[string]any)["volatile.uuid"] = "keep-me"
	f.calls = nil
	return f, c
}

func bigger() workload.VM {
	vm := testVM()
	vm.Resources = workload.Resources{CPU: 4, Memory: "2GiB"}
	vm.Disks = append(vm.Disks, workload.Disk{Name: "logs", Size: "5GiB", Pool: "bulk"})
	return vm
}

func TestUpdateRunningVM(t *testing.T) {
	f, c := createdVM(t, true)
	ctx := context.Background()
	plan, err := c.PlanVM(ctx, bigger())
	if err != nil || !plan.Restart || plan.Empty() {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if _, err := c.UpdateVM(ctx, bigger(), plan.ID, 30); err != nil {
		t.Fatalf("UpdateVM: %v", err)
	}
	if got := strings.Join(f.calls, ","); got != "stop,put,start" {
		t.Errorf("calls %s", got)
	}
	cfg := f.instances["web-01"]["config"].(map[string]any)
	if cfg["limits.cpu"] != "4" || cfg["volatile.uuid"] != "keep-me" {
		t.Errorf("config %v", cfg)
	}
	if !f.volumes["bulk/web-01-logs"] || f.instances["web-01"]["status"] != "Running" {
		t.Errorf("volume or status: %v %v", f.volumes, f.instances["web-01"]["status"])
	}
	// The stored spec is the new one: planning again changes nothing.
	if again, err := c.PlanVM(ctx, bigger()); err != nil || !again.Empty() {
		t.Errorf("plan after update: %+v %v", again, err)
	}
}

func TestUpdateRefusesStalePlan(t *testing.T) {
	f, c := createdVM(t, false)
	ctx := context.Background()
	plan, _ := c.PlanVM(ctx, bigger())
	f.instances["web-01"]["status"] = "Running" // started since the plan
	_, err := c.UpdateVM(ctx, bigger(), plan.ID, 30)
	if !errors.Is(err, workload.ErrConflict) || !strings.Contains(err.Error(), "plan again") || len(f.calls) != 0 {
		t.Errorf("stale plan: %v, calls %v", err, f.calls)
	}
}

func TestUpdateRefusesDrift(t *testing.T) {
	f, c := createdVM(t, false)
	f.instances["web-01"]["config"].(map[string]any)["limits.cpu"] = "16" // changed with incus directly
	_, err := c.PlanVM(context.Background(), bigger())
	if !errors.Is(err, workload.ErrConflict) || !strings.Contains(err.Error(), "changed outside Strata") {
		t.Errorf("drift: %v", err)
	}
}

func TestUpdateFailureRestores(t *testing.T) {
	f, c := createdVM(t, true)
	f.failPut = true
	ctx := context.Background()
	plan, _ := c.PlanVM(ctx, bigger())
	_, err := c.UpdateVM(ctx, bigger(), plan.ID, 30)
	if err == nil || !strings.Contains(err.Error(), "Invalid device") {
		t.Fatalf("got %v", err)
	}
	if got := strings.Join(f.calls, ","); got != "stop,put,start" {
		t.Errorf("calls %s (the VM must be started again)", got)
	}
	if f.volumes["bulk/web-01-logs"] || !f.volumes["bulk/web-01-data"] {
		t.Errorf("only the new volume is removed: %v", f.volumes)
	}
}

func TestUpdateDetachKeepsVolume(t *testing.T) {
	f, c := createdVM(t, false)
	vm := testVM()
	vm.Disks = vm.Disks[:1]
	plan, _ := c.PlanVM(context.Background(), vm)
	if _, err := c.UpdateVM(context.Background(), vm, plan.ID, 30); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.instances["web-01"]["devices"].(map[string]any)["data"]; ok || !f.volumes["bulk/web-01-data"] {
		t.Errorf("devices %v volumes %v", f.instances["web-01"]["devices"], f.volumes)
	}
	if got := strings.Join(f.calls, ","); got != "put" {
		t.Errorf("a stopped VM is changed without stop/start: %s", got)
	}
}

func TestDeleteVM(t *testing.T) {
	f, c := createdVM(t, true)
	ctx := context.Background()
	if _, err := c.DeleteVM(ctx, "web-01", false, 30); !errors.Is(err, workload.ErrConflict) {
		t.Errorf("running without stop: %v", err)
	}
	kept, err := c.DeleteVM(ctx, "web-01", true, 30)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(kept, ",") != "bulk/web-01-data" || !f.volumes["bulk/web-01-data"] {
		t.Errorf("kept %v volumes %v", kept, f.volumes)
	}
	if got := strings.Join(f.calls, ","); got != "stop,delete" {
		t.Errorf("calls %s", got)
	}
	if _, err := c.DeleteVM(ctx, "web-01", true, 30); !errors.Is(err, workload.ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestCreateFromImageServer(t *testing.T) {
	f, c := newFakeVMs(t)
	vm := testVM()
	vm.ImageServer, vm.Image = "images.linuxcontainers.org", "debian/13/cloud"
	if err := c.CreateVM(context.Background(), vm); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"type": "image", "alias": "debian/13/cloud",
		"server": "https://images.linuxcontainers.org", "protocol": "simplestreams",
	}
	if fmt.Sprint(f.lastSource) != fmt.Sprint(want) {
		t.Errorf("source %v", f.lastSource)
	}
	f2, c2 := newFakeVMs(t)
	if err := c2.CreateVM(context.Background(), testVM()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f2.lastSource) != fmt.Sprint(map[string]any{"type": "image", "alias": "debian-13"}) {
		t.Errorf("local source %v", f2.lastSource)
	}
}
