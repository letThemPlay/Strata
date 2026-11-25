package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/letthemplay/Strata/internal/agent/api"
	"github.com/letthemplay/Strata/internal/workload"
)

// node is an in-memory api.VMs.
type node struct {
	mu    sync.Mutex
	vms   map[string]workload.VMStatus
	calls []string
	fail  string
}

func (n *node) log(c string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, c)
	if n.fail != "" && strings.HasPrefix(c, n.fail) {
		return errors.New("simulated failure")
	}
	return nil
}

func (n *node) ListVMs(context.Context) ([]workload.VMStatus, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []workload.VMStatus{}
	for _, v := range n.vms {
		out = append(out, v)
	}
	return out, nil
}

func (n *node) GetVM(_ context.Context, name string) (workload.VMStatus, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.vms[name]
	if !ok {
		return v, workload.ErrNotFound
	}
	return v, nil
}

func (n *node) Host(context.Context) (workload.Host, error) {
	return workload.Host{Pools: []string{"fast"}, Networks: []string{}, Volumes: []string{}}, nil
}

func (n *node) CreateVM(_ context.Context, vm workload.VM, meta workload.Meta) error {
	if err := n.log(fmt.Sprintf("create %s %s %v", vm.Name, meta.Owner, meta.Protected)); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.vms[vm.Name] = workload.VMStatus{Spec: vm, Meta: meta, Status: "Stopped"}
	return nil
}

func (n *node) VMAction(_ context.Context, name, action string, force bool, timeout int) error {
	return n.log(fmt.Sprintf("%s %s %v %d", action, name, force, timeout))
}

func (n *node) PlanVM(ctx context.Context, vm workload.VM) (workload.Plan, error) {
	cur, err := n.GetVM(ctx, vm.Name)
	if err != nil {
		return workload.Plan{}, err
	}
	return workload.PlanVM(cur.Spec, vm, false)
}

func (n *node) UpdateVM(ctx context.Context, vm workload.VM, planID string, _ int) (workload.Plan, error) {
	p, err := n.PlanVM(ctx, vm)
	if err != nil {
		return p, err
	}
	if p.ID != planID {
		return p, workload.ErrConflict
	}
	return p, n.log("update " + vm.Name)
}

func (n *node) DeleteVM(_ context.Context, name string, stop bool, _ int) ([]string, error) {
	return []string{"fast/" + name + "-data"}, n.log(fmt.Sprintf("delete %s %v", name, stop))
}

func (n *node) RestoreVM(_ context.Context, name string, _ int) error {
	return n.log("restore " + name)
}

func (n *node) SetMeta(_ context.Context, name string, meta workload.Meta) error {
	return n.log(fmt.Sprintf("meta %s %s %v", name, meta.Owner, meta.Protected))
}

func serve(t *testing.T, n *node) Provider {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	s := &api.Server{
		Log: slog.New(slog.DiscardHandler), IsController: func(uint32) bool { return true },
		Provider: func() (api.VMs, func(), error) { return n, func() {}, nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return Provider{Client: &Client{Socket: socket, PollInterval: 5 * time.Millisecond}}
}

func vm(cpu int) workload.VM {
	return workload.VM{
		APIVersion: workload.VMAPIVersion, Name: "web", Resources: workload.Resources{CPU: cpu, Memory: "1GiB"},
		Disks: []workload.Disk{{Name: "root", Size: "8GiB", Pool: "fast"}},
	}
}

func TestProvider(t *testing.T) {
	n := &node{vms: map[string]workload.VMStatus{}}
	p := serve(t, n)
	ctx := context.Background()
	if err := p.CreateVM(ctx, vm(1), workload.Meta{Owner: workload.OwnerGitOps, Protected: true}); err != nil {
		t.Fatal(err)
	}
	if err := p.VMAction(ctx, "web", workload.ActionStart, false, 0); err != nil {
		t.Fatal(err)
	}
	plan, _ := workload.PlanVM(vm(1), vm(2), false)
	if got, err := p.UpdateVM(ctx, vm(2), plan.ID, 30); err != nil || got.ID != plan.ID {
		t.Fatalf("update %+v %v", got, err)
	}
	if err := p.RestoreVM(ctx, "web", 30); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta(ctx, "web", workload.Meta{Owner: workload.OwnerGitOps}); err != nil {
		t.Fatal(err)
	}
	kept, err := p.DeleteVM(ctx, "web", true, 30)
	if err != nil || len(kept) != 1 || kept[0] != "fast/web-data" {
		t.Fatalf("delete %v %v", kept, err)
	}
	want := "create web gitops true,start web false 0,update web,restore web,meta web gitops false,delete web true"
	if got := strings.Join(n.calls, ","); got != want {
		t.Errorf("calls\n%s\nwant\n%s", got, want)
	}
	vms, err := p.ListVMs(ctx)
	if err != nil || len(vms) != 1 {
		t.Errorf("list %v %v", vms, err)
	}
	if h, err := p.Host(ctx); err != nil || h.Pools[0] != "fast" {
		t.Errorf("host %+v %v", h, err)
	}
}

func TestProviderReportsFailedOperations(t *testing.T) {
	n := &node{vms: map[string]workload.VMStatus{}, fail: "create"}
	p := serve(t, n)
	err := p.CreateVM(context.Background(), vm(1), workload.Meta{Owner: workload.OwnerCLI})
	if err == nil || !strings.Contains(err.Error(), "simulated failure") {
		t.Errorf("err %v", err)
	}
}
