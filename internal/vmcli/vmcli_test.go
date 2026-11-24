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

func (f *fakeVMs) CreateVM(_ context.Context, vm workload.VM) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vms[vm.Name] = workload.VMStatus{Spec: vm, Status: "Stopped", Addresses: []string{"192.0.2.5"}}
	return f.fail
}

func (f *fakeVMs) VMAction(_ context.Context, name, action string, force bool, timeout int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = fmt.Sprintf("%s %s %v %d", action, name, force, timeout)
	return f.fail
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
	t.Setenv("STRATA_AGENT_SOCKET", socket)
	var out, errOut bytes.Buffer
	cmd := cli.Command{Name: "strata", Summary: "test", Subcommands: []cli.Subcommand{Subcommand()}}
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
		"delete not in 4a":     {[]string{"delete", "x"}, cli.ExitUsage, `unknown command "delete"`},
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
