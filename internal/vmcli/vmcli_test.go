package vmcli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/letthemplay/Strata/internal/agent/api"
	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/workload"
)

const spec = `{"apiVersion":"vm.strata.dev/v1alpha1","name":"web-01","resources":{"cpu":2,"memory":"1GiB"},
	"image":"debian-13","disks":[{"name":"root","size":"10GiB","pool":"fast"}],
	"networks":[{"name":"eth0","network":"lan"}]}`

type fakeVMs struct {
	mu   sync.Mutex
	vms  map[string]workload.VMStatus
	last string
	fail error
}

func (f *fakeVMs) ListVMs(context.Context) ([]workload.VMStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []workload.VMStatus{}
	for _, v := range f.vms {
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeVMs) GetVM(_ context.Context, name string) (workload.VMStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vms[name]
	if !ok {
		return v, fmt.Errorf("vm %s: %w", name, workload.ErrNotFound)
	}
	return v, nil
}

func (f *fakeVMs) CreateVM(_ context.Context, vm workload.VM, meta workload.Meta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vms[vm.Name] = workload.VMStatus{Spec: vm, Meta: meta, Status: "Stopped", Addresses: []string{"192.0.2.5"}}
	return f.fail
}

func (f *fakeVMs) Host(context.Context) (workload.Host, error) {
	return workload.Host{Pools: []string{"fast"}, Networks: []string{"lan"}, Volumes: []string{}}, f.fail
}

func (f *fakeVMs) VMAction(_ context.Context, name, action string, force bool, timeout int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = fmt.Sprintf("%s %s %v %d", action, name, force, timeout)
	return f.fail
}

func (f *fakeVMs) PlanVM(_ context.Context, vm workload.VM) (workload.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.vms[vm.Name]
	if !ok {
		return workload.Plan{}, fmt.Errorf("vm %s: %w", vm.Name, workload.ErrNotFound)
	}
	return workload.PlanVM(cur.Spec, vm, cur.Status == "Running")
}

func (f *fakeVMs) UpdateVM(ctx context.Context, vm workload.VM, planID string, _ int) (workload.Plan, error) {
	plan, err := f.PlanVM(ctx, vm)
	if err != nil {
		return plan, err
	}
	if plan.ID != planID {
		return workload.Plan{}, fmt.Errorf("vm %s: %w: plan again", vm.Name, workload.ErrConflict)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.vms[vm.Name]
	st.Spec = vm
	f.vms[vm.Name] = st
	return plan, nil
}

func (f *fakeVMs) DeleteVM(_ context.Context, name string, stop bool, _ int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.vms[name]
	if !ok {
		return nil, fmt.Errorf("vm %s: %w", name, workload.ErrNotFound)
	}
	if st.Status == "Running" && !stop {
		return nil, fmt.Errorf("vm %s: %w: it is Running", name, workload.ErrConflict)
	}
	delete(f.vms, name)
	var kept []string
	for _, d := range st.Spec.Disks {
		if d.Name != workload.RootDisk {
			kept = append(kept, d.Pool+"/"+st.Spec.VolumeName(d))
		}
	}
	return kept, nil
}

func serve(t *testing.T) (string, *fakeVMs) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeVMs{vms: map[string]workload.VMStatus{}}
	s := &api.Server{Log: slog.New(slog.DiscardHandler), Provider: func() (api.VMs, func(), error) { return f, func() {}, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return socket, f
}

func vm(t *testing.T, socket string, args ...string) (int, string, string) {
	t.Helper()
	return vmIn(t, socket, "", args...)
}

func vmIn(t *testing.T, socket, input string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("STRATA_AGENT_SOCKET", socket)
	var out, errOut bytes.Buffer
	cmd := cli.Command{Name: "strata", Summary: "test", Subcommands: []cli.Subcommand{Subcommand(strings.NewReader(input))}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code := cmd.Run(ctx, append([]string{"vm"}, args...), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestLifecycle(t *testing.T) {
	socket, f := serve(t)
	file := filepath.Join(t.TempDir(), "web.json")
	if err := os.WriteFile(file, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := vm(t, socket, "create", "-f", file)
	if code != cli.ExitOK || !strings.HasPrefix(out, "create web-01: done (operation ") {
		t.Fatalf("create: %d %q %q", code, out, errOut)
	}
	code, out, _ = vm(t, socket, "list")
	if code != cli.ExitOK || !strings.Contains(out, "web-01") || !strings.Contains(out, "Stopped") || !strings.Contains(out, "192.0.2.5") {
		t.Errorf("list: %d\n%s", code, out)
	}
	code, out, _ = vm(t, socket, "show", "web-01")
	if code != cli.ExitOK || !strings.Contains(out, "Image:     debian-13") || !strings.Contains(out, "pool fast") {
		t.Errorf("show: %d\n%s", code, out)
	}
	if code, out, _ = vm(t, socket, "show", "web-01", "--json"); code != cli.ExitOK || !strings.Contains(out, `"apiVersion": "vm.strata.dev/v1alpha1"`) {
		t.Errorf("show --json: %d\n%s", code, out)
	}
	if code, _, errOut = vm(t, socket, "stop", "web-01", "--force", "--timeout", "5"); code != cli.ExitOK || f.last != "stop web-01 true 5" {
		t.Errorf("stop: %d %q %q", code, f.last, errOut)
	}
	if code, _, _ = vm(t, socket, "start", "web-01"); code != cli.ExitOK || f.last != "start web-01 false 60" {
		t.Errorf("start: %d %q", code, f.last)
	}
}

func TestErrors(t *testing.T) {
	socket, f := serve(t)
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"name":"x"}`), 0o600)
	for name, tc := range map[string]struct {
		args []string
		code int
		want string
	}{
		"invalid spec (local)": {[]string{"create", "-f", bad}, cli.ExitError, "apiVersion: must be"},
		"no file":              {[]string{"create"}, cli.ExitUsage, "-f FILE is required"},
		"missing vm":           {[]string{"show", "nope"}, cli.ExitError, "vm nope: not found"},
		"no name":              {[]string{"start"}, cli.ExitUsage, "expected one VM name"},
		"unknown command":      {[]string{"migrate"}, cli.ExitUsage, `unknown command "migrate"`},
		"delete unconfirmed":   {[]string{"delete", "x"}, cli.ExitUsage, "confirm with --confirm x"},
		"delete wrong name":    {[]string{"delete", "x", "--confirm", "y"}, cli.ExitUsage, "confirm with --confirm x"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, errOut := vm(t, socket, tc.args...)
			if code != tc.code || !strings.Contains(errOut, tc.want) {
				t.Errorf("%d %q", code, errOut)
			}
		})
	}

	f.fail = fmt.Errorf("vm web-01: %w: status Stopped after start", workload.ErrVerify)
	file := filepath.Join(t.TempDir(), "web.json")
	_ = os.WriteFile(file, []byte(spec), 0o600)
	code, _, errOut := vm(t, socket, "create", "-f", file)
	if code != cli.ExitError || !strings.Contains(errOut, "verification failed") {
		t.Errorf("failed operation: %d %q", code, errOut)
	}
}

func TestConnectionErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.sock")
	code, _, errOut := vm(t, missing, "list")
	if code != cli.ExitError || !strings.Contains(errOut, "the Strata agent's API is not running") {
		t.Errorf("missing socket: %d %q", code, errOut)
	}
	if os.Getuid() == 0 {
		t.Skip("root ignores socket permissions")
	}
	socket, _ := serve(t)
	if err := os.Chmod(socket, 0); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = vm(t, socket, "list")
	if code != cli.ExitError || !strings.Contains(errOut, "permission denied (members of the strata-operators group") {
		t.Errorf("no permission: %d %q", code, errOut)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	socket, f := serve(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "web.json")
	_ = os.WriteFile(file, []byte(spec), 0o600)
	if code, _, errOut := vm(t, socket, "create", "-f", file); code != cli.ExitOK {
		t.Fatal(errOut)
	}
	f.vms["web-01"] = workload.VMStatus{Spec: f.vms["web-01"].Spec, Status: "Running"}
	bigger := filepath.Join(dir, "bigger.json")
	_ = os.WriteFile(bigger, []byte(strings.Replace(spec, `"cpu":2`, `"cpu":4`, 1)), 0o600)

	code, out, _ := vmIn(t, socket, "n\n", "update", "-f", bigger)
	if code != cli.ExitError || !strings.Contains(out, "resources.cpu  2 -> 4  restart") ||
		!strings.Contains(out, "stopped gracefully, changed and started again") || !strings.Contains(out, "Nothing was changed.") {
		t.Errorf("declined: %d\n%s", code, out)
	}
	if f.vms["web-01"].Spec.Resources.CPU != 2 {
		t.Error("declined plan was applied")
	}
	if code, out, errOut := vmIn(t, socket, "y\n", "update", "-f", bigger); code != cli.ExitOK || !strings.Contains(out, "update web-01: done") {
		t.Errorf("accepted: %d %s %s", code, out, errOut)
	}
	if f.vms["web-01"].Spec.Resources.CPU != 4 {
		t.Error("plan not applied")
	}
	if code, out, _ := vm(t, socket, "update", "-f", bigger, "--yes"); code != cli.ExitOK || !strings.Contains(out, "No changes for web-01.") {
		t.Errorf("no changes: %d %s", code, out)
	}

	f.vms["web-01"] = workload.VMStatus{Spec: workload.VM{
		APIVersion: workload.VMAPIVersion, Name: "web-01",
		Resources: workload.Resources{CPU: 1, Memory: "1GiB"},
		Disks:     []workload.Disk{{Name: "root", Size: "10GiB", Pool: "fast"}, {Name: "data", Size: "1GiB", Pool: "bulk"}},
	}, Status: "Running"}
	if code, _, errOut := vm(t, socket, "delete", "web-01", "--confirm", "web-01"); code != cli.ExitError || !strings.Contains(errOut, "it is Running") {
		t.Errorf("running: %d %s", code, errOut)
	}
	code, out, errOut := vm(t, socket, "delete", "web-01", "--confirm", "web-01", "--stop")
	if code != cli.ExitOK || !strings.Contains(out, "Data volumes kept (not deleted): bulk/web-01-data") {
		t.Errorf("delete: %d %q %q", code, out, errOut)
	}
}

func TestOwnerAndDrift(t *testing.T) {
	socket, f := serve(t)
	vmSpec, err := workload.ParseVM(strings.NewReader(spec))
	if err != nil {
		t.Fatal(err)
	}
	f.vms["web-01"] = workload.VMStatus{
		Spec: vmSpec, Meta: workload.Meta{Owner: workload.OwnerGitOps, Protected: true}, Status: "Running",
		Drift: []string{`config limits.cpu: "8", want "1"`},
	}
	code, out, _ := vm(t, socket, "list")
	if code != cli.ExitOK || !strings.Contains(out, "OWNER") || !strings.Contains(out, "gitops,protected,drift") {
		t.Errorf("list: %d\n%s", code, out)
	}
	code, out, _ = vm(t, socket, "show", "web-01")
	if code != cli.ExitOK || !strings.Contains(out, "Owner:     gitops\nProtected: true\n") ||
		!strings.Contains(out, "Drift (changed outside Strata):\n  config limits.cpu: \"8\", want \"1\"") {
		t.Errorf("show: %d\n%s", code, out)
	}
	file := filepath.Join(t.TempDir(), "web.json")
	if err := os.WriteFile(file, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := vm(t, socket, "update", "-f", file)
	if code != cli.ExitError || !strings.Contains(errOut, "managed by Git") {
		t.Errorf("update: %d %q", code, errOut)
	}
}
