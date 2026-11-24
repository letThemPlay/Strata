package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/letthemplay/Strata/internal/workload"
)

const spec = `{"apiVersion":"vm.strata.dev/v1alpha1","name":"web-01","resources":{"cpu":1,"memory":"512MiB"},
	"disks":[{"name":"root","size":"8GiB","pool":"fast"}]}`

// fakeVMs records calls; block makes actions wait until released.
type fakeVMs struct {
	mu      sync.Mutex
	vms     map[string]workload.VMStatus
	calls   []string
	fail    error
	block   chan struct{}
	release int
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
		return workload.VMStatus{}, fmt.Errorf("vm %s: %w", name, workload.ErrNotFound)
	}
	return v, nil
}

func (f *fakeVMs) CreateVM(_ context.Context, vm workload.VM) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "create "+vm.Name)
	if f.fail != nil {
		return f.fail
	}
	f.vms[vm.Name] = workload.VMStatus{Spec: vm, Status: "Stopped"}
	return nil
}

func (f *fakeVMs) VMAction(_ context.Context, name, action string, force bool, timeout int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s %s force=%v timeout=%d", action, name, force, timeout))
	return f.fail
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type harness struct {
	client *http.Client
	fake   *fakeVMs
	logs   *lockedBuffer
}

func start(t *testing.T, authorize func(uint32) error, withBackend bool) *harness {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{fake: &fakeVMs{vms: map[string]workload.VMStatus{}}, logs: &lockedBuffer{}}
	s := &Server{Authorize: authorize, Log: slog.New(slog.NewJSONHandler(h.logs, nil))}
	if withBackend {
		s.Provider = func() (VMs, func(), error) {
			h.fake.mu.Lock()
			h.fake.release++
			h.fake.mu.Unlock()
			return h.fake, func() {}, nil
		}
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
	h.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}}
	return h
}

func (h *harness) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://agent"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (h *harness) wait(t *testing.T, id string) Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := h.do(t, http.MethodGet, "/api/v1/operations/"+id, "")
		var op Operation
		if code != http.StatusOK || json.Unmarshal(body, &op) != nil {
			t.Fatalf("operation %s: %d %s", id, code, body)
		}
		if op.done() {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s not done: %+v", id, op)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func allowAll(uint32) error { return nil }

func TestCreateAndActions(t *testing.T) {
	h := start(t, allowAll, true)
	code, body := h.do(t, http.MethodPost, "/api/v1/vms", spec)
	if code != http.StatusAccepted {
		t.Fatalf("create: %d %s", code, body)
	}
	var op Operation
	_ = json.Unmarshal(body, &op)
	if op.Type != "create" || op.Resource != "vms/web-01" || op.Status != StatusPending || op.UID != myUID() {
		t.Errorf("operation %+v", op)
	}
	if done := h.wait(t, op.ID); done.Status != StatusSucceeded || done.CompletedAt.IsZero() {
		t.Errorf("done %+v", done)
	}

	code, body = h.do(t, http.MethodGet, "/api/v1/vms/web-01", "")
	if code != http.StatusOK || !strings.Contains(string(body), `"status":"Stopped"`) {
		t.Errorf("get: %d %s", code, body)
	}
	code, body = h.do(t, http.MethodGet, "/api/v1/vms", "")
	if code != http.StatusOK || !strings.Contains(string(body), `"name":"web-01"`) {
		t.Errorf("list: %d %s", code, body)
	}

	_, body = h.do(t, http.MethodPost, "/api/v1/vms/web-01/stop?force=true&timeout=5", "")
	_ = json.Unmarshal(body, &op)
	h.wait(t, op.ID)
	if got := h.fake.calls[len(h.fake.calls)-1]; got != "stop web-01 force=true timeout=5" {
		t.Errorf("call %q", got)
	}

	// The audit log names the caller and the operation.
	if logs := h.logs.String(); !strings.Contains(logs, `"msg":"audit"`) ||
		!strings.Contains(logs, fmt.Sprintf(`"uid":%d`, os.Getuid())) || !strings.Contains(logs, `"operation":"`+op.ID+`"`) {
		t.Errorf("audit log:\n%s", logs)
	}
}

func TestErrors(t *testing.T) {
	h := start(t, allowAll, true)
	for name, tc := range map[string]struct {
		method, path, body string
		code               int
		want               string
	}{
		"invalid spec":    {http.MethodPost, "/api/v1/vms", `{"name":"x"}`, 400, "apiVersion"},
		"unknown field":   {http.MethodPost, "/api/v1/vms", strings.Replace(spec, `"name"`, `"raw":1,"name"`, 1), 400, "unknown field"},
		"too large":       {http.MethodPost, "/api/v1/vms", `{"name":"` + strings.Repeat("x", 70<<10) + `"}`, 400, "too large"},
		"missing vm":      {http.MethodGet, "/api/v1/vms/nope", "", 404, "not_found"},
		"unknown action":  {http.MethodPost, "/api/v1/vms/web-01/explode", "", 404, "unknown action"},
		"bad timeout":     {http.MethodPost, "/api/v1/vms/web-01/stop?timeout=-1", "", 400, "timeout must be"},
		"missing op":      {http.MethodGet, "/api/v1/operations/nope", "", 404, "no such operation"},
		"delete (absent)": {http.MethodDelete, "/api/v1/vms/web-01", "", 405, ""},
	} {
		t.Run(name, func(t *testing.T) {
			code, body := h.do(t, tc.method, tc.path, tc.body)
			if code != tc.code || !strings.Contains(string(body), tc.want) {
				t.Errorf("%d %s", code, body)
			}
		})
	}
}

func TestFailedOperation(t *testing.T) {
	h := start(t, allowAll, true)
	h.fake.fail = fmt.Errorf("vm web-01: %w: status Stopped after start", workload.ErrVerify)
	_, body := h.do(t, http.MethodPost, "/api/v1/vms", spec)
	var op Operation
	_ = json.Unmarshal(body, &op)
	if done := h.wait(t, op.ID); done.Status != StatusFailed || !strings.Contains(done.Error, "verification failed") {
		t.Errorf("done %+v", done)
	}
}

func TestOneOperationPerVM(t *testing.T) {
	h := start(t, allowAll, true)
	h.fake.block = make(chan struct{})
	code, body := h.do(t, http.MethodPost, "/api/v1/vms", spec)
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}
	var op Operation
	_ = json.Unmarshal(body, &op)
	code, body = h.do(t, http.MethodPost, "/api/v1/vms/web-01/start", "")
	if code != http.StatusConflict || !strings.Contains(string(body), "in progress") {
		t.Errorf("concurrent: %d %s", code, body)
	}
	close(h.fake.block)
	h.wait(t, op.ID)
	if code, _ := h.do(t, http.MethodPost, "/api/v1/vms/web-01/start", ""); code != http.StatusAccepted {
		t.Errorf("after: %d", code)
	}
}

func TestAuthorization(t *testing.T) {
	h := start(t, func(uid uint32) error { return fmt.Errorf("uid %d is not in group strata-operators", uid) }, true)
	code, body := h.do(t, http.MethodGet, "/api/v1/vms", "")
	if code != http.StatusForbidden || !strings.Contains(string(body), "not in group strata-operators") {
		t.Errorf("%d %s", code, body)
	}
	if h.fake.release != 0 {
		t.Error("the provider must not be used for an unauthorized caller")
	}
	if !strings.Contains(h.logs.String(), `"status":403`) {
		t.Errorf("refusals must be audited:\n%s", h.logs.String())
	}
}

func TestNoBackend(t *testing.T) {
	h := start(t, allowAll, false)
	code, body := h.do(t, http.MethodGet, "/api/v1/vms", "")
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), "no workload backend") {
		t.Errorf("%d %s", code, body)
	}
}

func TestGroupAuthorizer(t *testing.T) {
	if err := GroupAuthorizer("no-such-group-strata")(0); err != nil {
		t.Errorf("root: %v", err)
	}
	if err := GroupAuthorizer("no-such-group-strata")(myUID()); err == nil && os.Getuid() != 0 {
		t.Error("a missing group must refuse")
	}
	if err := GroupAuthorizer("root")(4000000000); err == nil {
		t.Error("an unknown uid must be refused")
	}
}

// myUID is the test process's uid.
func myUID() uint32 {
	return uint32(os.Getuid()) //nolint:gosec // uids fit in uint32
}
