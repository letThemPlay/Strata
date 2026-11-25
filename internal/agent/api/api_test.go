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

func (f *fakeVMs) CreateVM(_ context.Context, vm workload.VM, meta workload.Meta) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "create "+vm.Name)
	if f.fail != nil {
		return f.fail
	}
	f.vms[vm.Name] = workload.VMStatus{Spec: vm, Meta: meta, Status: "Stopped"}
	return nil
}

func (f *fakeVMs) Host(context.Context) (workload.Host, error) {
	return workload.Host{Pools: []string{"fast"}, Networks: []string{"lan"}, Volumes: []string{"fast/web-01-data"}}, f.fail
}

func (f *fakeVMs) VMAction(_ context.Context, name, action string, force bool, timeout int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s %s force=%v timeout=%d", action, name, force, timeout))
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
		"invalid spec":       {http.MethodPost, "/api/v1/vms", `{"name":"x"}`, 400, "apiVersion"},
		"unknown field":      {http.MethodPost, "/api/v1/vms", strings.Replace(spec, `"name"`, `"raw":1,"name"`, 1), 400, "unknown field"},
		"too large":          {http.MethodPost, "/api/v1/vms", `{"name":"` + strings.Repeat("x", 70<<10) + `"}`, 400, "too large"},
		"missing vm":         {http.MethodGet, "/api/v1/vms/nope", "", 404, "not_found"},
		"unknown action":     {http.MethodPost, "/api/v1/vms/web-01/explode", "", 404, "unknown action"},
		"bad timeout":        {http.MethodPost, "/api/v1/vms/web-01/stop?timeout=-1", "", 400, "timeout must be"},
		"missing op":         {http.MethodGet, "/api/v1/operations/nope", "", 404, "no such operation"},
		"delete unconfirmed": {http.MethodDelete, "/api/v1/vms/web-01", "", 400, "confirm with its name"},
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

func create(t *testing.T, h *harness, body string) {
	t.Helper()
	code, out := h.do(t, http.MethodPost, "/api/v1/vms", body)
	if code != http.StatusAccepted {
		t.Fatalf("create: %d %s", code, out)
	}
	var op Operation
	_ = json.Unmarshal(out, &op)
	if done := h.wait(t, op.ID); done.Status != StatusSucceeded {
		t.Fatalf("create: %+v", done)
	}
}

func TestUpdatePlanAndApply(t *testing.T) {
	h := start(t, allowAll, true)
	create(t, h, spec)
	bigger := strings.Replace(spec, `"cpu":1`, `"cpu":2`, 1)

	code, out := h.do(t, http.MethodPut, "/api/v1/vms/web-01?plan=true", bigger)
	var plan workload.Plan
	if code != http.StatusOK || json.Unmarshal(out, &plan) != nil || len(plan.Changes) != 1 || plan.Changes[0].Field != "resources.cpu" {
		t.Fatalf("plan: %d %s", code, out)
	}
	if code, out := h.do(t, http.MethodPut, "/api/v1/vms/web-01", bigger); code != 400 || !strings.Contains(string(out), "?plan=true") {
		t.Errorf("no mode: %d %s", code, out)
	}
	if code, out := h.do(t, http.MethodPut, "/api/v1/vms/other?plan=true", bigger); code != 400 || !strings.Contains(string(out), `not \"other\"`) {
		t.Errorf("name mismatch: %d %s", code, out)
	}

	// A stale plan ID fails the operation with a conflict.
	_, out = h.do(t, http.MethodPut, "/api/v1/vms/web-01?confirm=0000", bigger)
	var op Operation
	_ = json.Unmarshal(out, &op)
	if done := h.wait(t, op.ID); done.Status != StatusFailed || !strings.Contains(done.Error, "plan again") {
		t.Errorf("stale: %+v", done)
	}

	_, out = h.do(t, http.MethodPut, "/api/v1/vms/web-01?confirm="+plan.ID, bigger)
	_ = json.Unmarshal(out, &op)
	done := h.wait(t, op.ID)
	if done.Status != StatusSucceeded || done.Type != "update" || done.Result == nil {
		t.Errorf("apply: %+v", done)
	}
	if h.fake.vms["web-01"].Spec.Resources.CPU != 2 {
		t.Error("not applied")
	}

	// A refused change is a 422 at planning time.
	shrunk := strings.Replace(spec, `"8GiB"`, `"4GiB"`, 1)
	if code, out := h.do(t, http.MethodPut, "/api/v1/vms/web-01?plan=true", shrunk); code != 422 || !strings.Contains(string(out), "cannot shrink") {
		t.Errorf("refused: %d %s", code, out)
	}
}

func TestDelete(t *testing.T) {
	h := start(t, allowAll, true)
	create(t, h, strings.Replace(spec, `]}`, `,{"name":"data","size":"1GiB","pool":"bulk"}]}`, 1))
	h.fake.vms["web-01"] = workload.VMStatus{Spec: h.fake.vms["web-01"].Spec, Status: "Running"}

	if code, out := h.do(t, http.MethodDelete, "/api/v1/vms/web-01?confirm=web-02", ""); code != 400 || !strings.Contains(string(out), "confirm=web-01") {
		t.Errorf("wrong name: %d %s", code, out)
	}
	_, out := h.do(t, http.MethodDelete, "/api/v1/vms/web-01?confirm=web-01", "")
	var op Operation
	_ = json.Unmarshal(out, &op)
	if done := h.wait(t, op.ID); done.Status != StatusFailed || !strings.Contains(done.Error, "conflict") {
		t.Errorf("running: %+v", done)
	}
	_, out = h.do(t, http.MethodDelete, "/api/v1/vms/web-01?confirm=web-01&stop=true", "")
	_ = json.Unmarshal(out, &op)
	done := h.wait(t, op.ID)
	if done.Status != StatusSucceeded || !strings.Contains(fmt.Sprint(done.Result), "bulk/web-01-data") {
		t.Errorf("delete: %+v", done)
	}
}

func TestCLICreatesAreOwnedByTheCLI(t *testing.T) {
	h := start(t, allowAll, true)
	_, body := h.do(t, http.MethodPost, "/api/v1/vms", spec)
	var op Operation
	_ = json.Unmarshal(body, &op)
	h.wait(t, op.ID)
	if st := h.fake.vms["web-01"]; st.Owner != workload.OwnerCLI || st.Protected {
		t.Errorf("meta %+v", st.Meta)
	}
}

func TestGitManagedVMsRefuseChanges(t *testing.T) {
	h := start(t, allowAll, true)
	vm, err := workload.ParseVM(strings.NewReader(spec))
	if err != nil {
		t.Fatal(err)
	}
	h.fake.vms["web-01"] = workload.VMStatus{Spec: vm, Meta: workload.Meta{Owner: workload.OwnerGitOps}, Status: "Running"}
	bigger := strings.Replace(spec, `"cpu":1`, `"cpu":2`, 1)

	code, body := h.do(t, http.MethodPut, "/api/v1/vms/web-01?plan=true", bigger)
	if code != http.StatusConflict || !strings.Contains(string(body), "git_managed") || !strings.Contains(string(body), "configuration repository") {
		t.Errorf("plan: %d %s", code, body)
	}
	for _, req := range [][2]string{
		{http.MethodPut, "/api/v1/vms/web-01?confirm=anything"},
		{http.MethodDelete, "/api/v1/vms/web-01?confirm=web-01&stop=true"},
	} {
		code, body := h.do(t, req[0], req[1], bigger)
		var op Operation
		if code != http.StatusAccepted || json.Unmarshal(body, &op) != nil {
			t.Fatalf("%s %s: %d %s", req[0], req[1], code, body)
		}
		if done := h.wait(t, op.ID); done.Status != StatusFailed || !strings.Contains(done.Error, "managed by Git") {
			t.Errorf("%s: %+v", req[0], done)
		}
	}
	if _, ok := h.fake.vms["web-01"]; !ok || h.fake.vms["web-01"].Spec.Resources.CPU != 1 {
		t.Error("the Git-managed VM was changed")
	}
	// Lifecycle actions stay allowed.
	code, body = h.do(t, http.MethodPost, "/api/v1/vms/web-01/stop", "")
	var op Operation
	if code != http.StatusAccepted || json.Unmarshal(body, &op) != nil {
		t.Fatalf("stop: %d %s", code, body)
	}
	if done := h.wait(t, op.ID); done.Status != StatusSucceeded {
		t.Errorf("stop: %+v", done)
	}
}

func TestHost(t *testing.T) {
	h := start(t, allowAll, true)
	code, body := h.do(t, http.MethodGet, "/api/v1/host", "")
	if code != http.StatusOK || string(body) != `{"pools":["fast"],"networks":["lan"],"volumes":["fast/web-01-data"]}`+"\n" {
		t.Errorf("host: %d %s", code, body)
	}
	h = start(t, allowAll, false)
	if code, _ := h.do(t, http.MethodGet, "/api/v1/host", ""); code != http.StatusServiceUnavailable {
		t.Errorf("no backend: %d", code)
	}
}
