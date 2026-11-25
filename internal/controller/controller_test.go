package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/letthemplay/Strata/internal/gitops"
	"github.com/letthemplay/Strata/internal/workload"
)

// fakeRepo is a repository whose commits and trust are set by the test.
type fakeRepo struct {
	head      string
	fetchErr  error
	untrusted map[string]bool
	locks     map[string]string
	verified  []string
}

func (r *fakeRepo) Fetch(context.Context) (string, error) { return r.head, r.fetchErr }

func (r *fakeRepo) Verify(_ context.Context, sha, lastApplied string) (string, error) {
	r.verified = append(r.verified, sha)
	if r.untrusted[sha] {
		return "", gitops.ErrUntrusted
	}
	// c1 < c2 < c3 in history.
	if lastApplied != "" && sha < lastApplied {
		return "", gitops.ErrRewound
	}
	return "alice", nil
}

func (r *fakeRepo) FileID(_ context.Context, sha, _ string) (string, error) { return r.locks[sha], nil }

func (r *fakeRepo) FlakeURL(sha string) string { return "git+file:///repo?rev=" + sha }

func (r *fakeRepo) Resolve(_ context.Context, ref string) (string, error) {
	for _, c := range []string{c1, c2, c3} {
		if strings.HasPrefix(c, ref) {
			return c, nil
		}
	}
	return "", errors.New("commit not in the fetched history")
}

// fakeEval returns the IR the test sets per commit.
type fakeEval struct {
	irs   map[string][]byte
	err   map[string]error
	calls []string
}

func (e *fakeEval) IR(_ context.Context, url, node string) ([]byte, error) {
	sha := url[strings.Index(url, "rev=")+4:]
	e.calls = append(e.calls, sha+" "+node)
	if err := e.err[sha]; err != nil {
		return nil, err
	}
	return e.irs[sha], nil
}

// fakeNode is the node as the agent shows it to the controller: it
// observes and applies (reconciler.Provider), in memory.
type fakeNode struct {
	mu    sync.Mutex
	vms   []workload.VMStatus
	err   error
	calls []string
}

func (n *fakeNode) ListVMs(context.Context) ([]workload.VMStatus, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.vms), n.err
}

func (n *fakeNode) Host(context.Context) (workload.Host, error) {
	return workload.Host{Pools: []string{"bulk", "fast"}, Networks: []string{"dmz", "lan"}, Volumes: []string{}}, n.err
}

func (n *fakeNode) find(name string) int {
	return slices.IndexFunc(n.vms, func(v workload.VMStatus) bool { return v.Spec.Name == name })
}

func (n *fakeNode) CreateVM(_ context.Context, vm workload.VM, meta workload.Meta) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, "create "+vm.Name)
	n.vms = append(n.vms, workload.VMStatus{Spec: vm, Meta: meta, Status: "Stopped"})
	return nil
}

func (n *fakeNode) VMAction(_ context.Context, name, action string, _ bool, _ int) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, action+" "+name)
	if i := n.find(name); i >= 0 && action == workload.ActionStart {
		n.vms[i].Status = "Running"
	}
	return nil
}

func (n *fakeNode) UpdateVM(_ context.Context, vm workload.VM, planID string, _ int) (workload.Plan, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, "update "+vm.Name)
	i := n.find(vm.Name)
	plan, err := workload.PlanVM(n.vms[i].Spec, vm, n.vms[i].Status == "Running")
	if err != nil || plan.ID != planID {
		return plan, errors.Join(err, workload.ErrConflict)
	}
	n.vms[i].Spec = vm
	return plan, nil
}

func (n *fakeNode) RestoreVM(_ context.Context, name string, _ int) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, "restore "+name)
	n.vms[n.find(name)].Drift = nil
	return nil
}

func (n *fakeNode) SetMeta(_ context.Context, name string, meta workload.Meta) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, fmt.Sprintf("meta %s %s %v", name, meta.Owner, meta.Protected))
	n.vms[n.find(name)].Meta = meta
	return nil
}

func (n *fakeNode) DeleteVM(_ context.Context, name string, _ bool, _ int) ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, "delete "+name)
	n.vms = slices.Delete(n.vms, n.find(name), n.find(name)+1)
	return nil, nil
}

func golden(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../ir/testdata/standalone.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

const (
	c1 = "1111111111111111111111111111111111111111"
	c2 = "2222222222222222222222222222222222222222"
	c3 = "3333333333333333333333333333333333333333"
)

func newController(t *testing.T) (*Controller, *fakeRepo, *fakeEval, *fakeNode) {
	t.Helper()
	repo := &fakeRepo{head: c1, untrusted: map[string]bool{}, locks: map[string]string{c1: "lock1", c2: "lock1", c3: "lock1"}}
	ev := &fakeEval{irs: map[string][]byte{c1: golden(t), c2: golden(t), c3: golden(t)}, err: map[string]error{}}
	node := &fakeNode{}
	c := &Controller{
		Config: Config{Node: "hv-01", Repository: "ssh://git@h/x.git", Branch: "main", IntervalSeconds: 60, AutoApply: "none", TrustedSigners: []string{"k"}, EvalTimeoutSeconds: 60},
		Repo:   repo, Eval: ev, Node: node, Exec: node, Store: &Store{Dir: t.TempDir()}, Version: "test",
		Now: func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) },
	}
	return c, repo, ev, node
}

func TestSyncPlansTheHead(t *testing.T) {
	c, _, ev, _ := newController(t)
	c.Sync(context.Background())
	st := c.Status()
	if st.Error != "" || st.Head != c1 || st.Evaluated != c1 || st.Signer != "alice" || st.FlakeLock != "lock1" || st.LastSync == "" {
		t.Fatalf("status %+v", st)
	}
	if st.Plan == nil || st.Plan.Items != 3 || st.Plan.Commit != c1 || !st.Plan.Safe {
		t.Errorf("plan %+v", st.Plan)
	}
	p, err := c.Plan()
	if err != nil || p.Commit != c1 || p.Signer != "alice" || len(p.Plan.Items) != 3 || p.Plan.Items[0].Action != "create" {
		t.Errorf("plan %+v %v", p, err)
	}
	// An unchanged head is planned again (drift, node changes) but not
	// evaluated again.
	c.Sync(context.Background())
	if len(ev.calls) != 1 || ev.calls[0] != c1+" hv-01" {
		t.Errorf("evaluations %v", ev.calls)
	}
}

func TestUntrustedCommitIsNeverEvaluated(t *testing.T) {
	c, repo, ev, _ := newController(t)
	c.Sync(context.Background())
	repo.head = c2
	repo.untrusted[c2] = true
	c.Sync(context.Background())
	st := c.Status()
	if !strings.Contains(st.Error, "refused") || !strings.Contains(st.Error, "not signed by a trusted key") {
		t.Errorf("error %q", st.Error)
	}
	if len(ev.calls) != 1 {
		t.Errorf("the untrusted commit was evaluated: %v", ev.calls)
	}
	if st.Plan != nil || st.Evaluated != "" {
		t.Errorf("a plan of another commit was kept: %+v", st)
	}
	if _, err := c.Plan(); !errors.Is(err, ErrNoPlan) || !strings.Contains(err.Error(), "not signed") {
		t.Errorf("Plan: %v", err)
	}
}

func TestFlakeLockChangeIsAHostChange(t *testing.T) {
	c, repo, ev, _ := newController(t)
	c.Sync(context.Background())
	repo.head, repo.locks[c2] = c2, "lock2"
	ev.err[c2] = ErrInputsMissing
	c.Sync(context.Background())
	if st := c.Status(); !strings.Contains(st.Error, "changes flake.lock: the host configuration changed; apply it with nixos-rebuild first") {
		t.Errorf("error %q", st.Error)
	}
}

func TestEvaluationErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		ir   []byte
		err  error
		want string
	}{
		"evaluation": {nil, errors.New("evaluation failed:\nassertion failed"), "assertion failed"},
		"invalid IR": {[]byte(`{"apiVersion": "x"}`), nil, "invalid IR"},
		"other node": {bytes.Replace(golden(t), []byte(`"node": "hv-01"`), []byte(`"node": "hv-02"`), 1), nil, `the IR is for node "hv-02"`},
	} {
		t.Run(name, func(t *testing.T) {
			c, _, ev, _ := newController(t)
			ev.irs[c1], ev.err[c1] = tc.ir, tc.err
			c.Sync(context.Background())
			if st := c.Status(); !strings.Contains(st.Error, tc.want) || st.Plan != nil {
				t.Errorf("status %+v", st)
			}
		})
	}
}

func TestFetchFailureKeepsThePlan(t *testing.T) {
	c, repo, _, _ := newController(t)
	c.Sync(context.Background())
	repo.fetchErr = errors.New("connection refused")
	c.Sync(context.Background())
	st := c.Status()
	if !strings.Contains(st.Error, "fetch: connection refused") || st.Plan == nil || st.Evaluated != c1 {
		t.Errorf("status %+v", st)
	}
}

func TestDriftIsReportedWithoutANewCommit(t *testing.T) {
	c, _, _, node := newController(t)
	c.Sync(context.Background())
	var doc struct {
		Resources []struct {
			Spec workload.VM `json:"spec"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(golden(t), &doc); err != nil {
		t.Fatal(err)
	}
	for _, r := range doc.Resources {
		st := workload.VMStatus{Spec: r.Spec, Meta: workload.Meta{Owner: workload.OwnerGitOps}, Status: "Running"}
		if r.Spec.Name == "db-01" {
			st.Protected = true
		}
		if r.Spec.Name == "web-01" {
			st.Drift = []string{"config limits.cpu"}
		}
		node.vms = append(node.vms, st)
	}
	c.Sync(context.Background())
	st := c.Status()
	if len(st.Drift) != 1 || st.Drift[0] != "vm/web-01" || st.Plan.Safe {
		t.Errorf("status %+v %+v", st, st.Plan)
	}
}

func TestObserveFailure(t *testing.T) {
	c, _, _, node := newController(t)
	node.err = errors.New("agent down")
	c.Sync(context.Background())
	if st := c.Status(); !strings.Contains(st.Error, "observe the node: agent down") {
		t.Errorf("status %+v", st)
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	good := `{"node":"hv-01","repository":"ssh://git@h/x.git","branch":"main","intervalSeconds":300,"autoApply":"none",
		"trustedSigners":["ssh-ed25519 AAAA a"],"knownHostsFile":"/kh","sshKey":"gitops-ssh-key","evalTimeoutSeconds":300}`
	path := filepath.Join(dir, "c.json")
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil || c.Interval() != 5*time.Minute || c.SSHKey != "gitops-ssh-key" {
		t.Fatalf("%+v %v", c, err)
	}
	for name, edit := range map[string]func(*Config){
		"node":       func(c *Config) { c.Node = "HV" },
		"interval":   func(c *Config) { c.IntervalSeconds = 1 },
		"autoApply":  func(c *Config) { c.AutoApply = "all" },
		"signers":    func(c *Config) { c.TrustedSigners = nil },
		"credential": func(c *Config) { c.SSHKey = "../etc/shadow" },
		"timeout":    func(c *Config) { c.EvalTimeoutSeconds = 0 },
	} {
		cc := c
		edit(&cc)
		if cc.Check() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Replace(good, `"node"`, `"extra":1,"node"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestEvaluator(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "nix")
	write := func(script string) {
		t.Helper()
		if err := os.WriteFile(fake, []byte("#!/bin/sh\n"+script), 0o700); err != nil { //nolint:gosec // An executable test script.
			t.Fatal(err)
		}
	}
	e := &Evaluator{Nix: fake, Home: dir, Timeout: 5 * time.Second}

	write(`printf '%s\n' "$@" > ` + dir + `/args; echo '{"apiVersion":"x"}'`)
	out, err := e.IR(context.Background(), "git+file:///r?rev="+c1, "hv-01")
	if err != nil || string(out) != "{\"apiVersion\":\"x\"}\n" {
		t.Fatalf("IR %q %v", out, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, want := range []string{
		"--offline", "--no-write-lock-file", "allow-import-from-derivation\nfalse", "pure-eval\ntrue",
		"git+file:///r?rev=" + c1 + `#nixosConfigurations."hv-01".config.strata.ir`,
	} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}

	write("echo 'error: \033[31massertion failed\033[0m: bad pool' >&2; exit 1")
	if _, err := e.IR(context.Background(), "u", "hv-01"); err == nil || !strings.Contains(err.Error(), "assertion failed: bad pool") || strings.Contains(err.Error(), "\033") {
		t.Errorf("failure: %v", err)
	}
	write("echo 'error: cannot fetch input in offline mode' >&2; exit 1")
	if _, err := e.IR(context.Background(), "u", "hv-01"); !errors.Is(err, ErrInputsMissing) {
		t.Errorf("offline: %v", err)
	}
	write("sleep 10")
	e.Timeout = 100 * time.Millisecond
	if _, err := e.IR(context.Background(), "u", "hv-01"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("timeout: %v", err)
	}
	if _, err := e.IR(context.Background(), "u", `x" or true`); err == nil {
		t.Error("invalid node accepted")
	}
}

func TestServer(t *testing.T) {
	c, _, _, _ := newController(t)
	socket := filepath.Join(t.TempDir(), "c.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	denied := false
	s := &Server{Controller: c, Authorize: func(uint32) error {
		if denied {
			return errors.New("uid 1000 is not in group strata-operators")
		}
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}}
	do := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, "http://c"+path, nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := do(http.MethodGet, "/api/v1/config/plan"); code != http.StatusNotFound || !strings.Contains(body, "no current plan") {
		t.Errorf("plan before sync: %d %s", code, body)
	}
	if code, body := do(http.MethodPost, "/api/v1/config/sync"); code != http.StatusOK || !strings.Contains(body, `"evaluated":"`+c1) {
		t.Errorf("sync: %d %s", code, body)
	}
	if code, body := do(http.MethodGet, "/api/v1/config"); code != http.StatusOK || !strings.Contains(body, `"signer":"alice"`) {
		t.Errorf("status: %d %s", code, body)
	}
	if code, body := do(http.MethodGet, "/api/v1/config/plan"); code != http.StatusOK || !strings.Contains(body, `"resource":"vm/db-01"`) {
		t.Errorf("plan: %d %s", code, body)
	}
	denied = true
	if code, body := do(http.MethodGet, "/api/v1/config"); code != http.StatusForbidden || !strings.Contains(body, "not in group") {
		t.Errorf("denied: %d %s", code, body)
	}
}

func TestNixError(t *testing.T) {
	in := "warning: could not read HEAD\nremote: Counting objects:  6% (1/16)\rremote: done\n" +
		"error:\n       … while calling the 'seq' builtin\n\n       error: \x1b[35munable to download\x1b[0m 'https://x'"
	got := nixError(in)
	if strings.Contains(got, "remote:") || strings.Contains(got, "warning") || !strings.HasPrefix(got, "error:") || strings.Contains(got, "\x1b") {
		t.Errorf("nixError: %q", got)
	}
	if !offlineFailure.MatchString(got) {
		t.Error("a download failure was not recognized")
	}
}

// evalService serves ServeEval on a socket, as systemd does with
// Accept=yes, with the fake nix script.
func evalService(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "nix")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"+script), 0o700); err != nil { //nolint:gosec // An executable test script.
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "eval.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_ = ServeEval(context.Background(), conn, &Evaluator{Nix: fake, Home: dir, Timeout: 5 * time.Second}, "/var/lib/strata-controller/repo.git")
			_ = conn.Close()
		}
	}()
	return socket
}

func TestRemoteEvaluator(t *testing.T) {
	socket := evalService(t, `case "$*" in *"git+file:///var/lib/strata-controller/repo.git?rev=`+c1+`#nixosConfigurations.\"hv-01\".config.strata.ir"*) echo '{"apiVersion":"ok"}';; *) echo "unexpected: $*" >&2; exit 1;; esac`)
	r := &RemoteEvaluator{Socket: socket, Timeout: 5 * time.Second}
	out, err := r.IR(context.Background(), "git+file:///anything?rev="+c1, "hv-01")
	if err != nil || strings.TrimSpace(string(out)) != `{"apiVersion":"ok"}` {
		t.Fatalf("IR %q %v", out, err)
	}
	// Only a commit is sent: the service evaluates its own repository.
	if _, err := r.IR(context.Background(), "git+file:///x?ref=main", "hv-01"); err == nil {
		t.Error("a URL without a commit was accepted")
	}
	if _, err := r.IR(context.Background(), "git+file:///x?rev="+c1, `hv-01"; x`); err == nil || !strings.Contains(err.Error(), "not a valid node ID") {
		t.Errorf("bad node: %v", err)
	}
}

func TestRemoteEvaluatorErrors(t *testing.T) {
	socket := evalService(t, "echo 'error: unable to download https://github.com/x' >&2; exit 1")
	r := &RemoteEvaluator{Socket: socket, Timeout: 5 * time.Second}
	_, err := r.IR(context.Background(), "git+file:///x?rev="+c1, "hv-01")
	var ee *EvalError
	if !errors.As(err, &ee) || !ee.InputsMissing || !errors.Is(err, ErrInputsMissing) || !strings.Contains(ee.Detail, "unable to download") {
		t.Errorf("err %v", err)
	}
	if _, err := (&RemoteEvaluator{Socket: filepath.Join(t.TempDir(), "none"), Timeout: time.Second}).IR(context.Background(), "x?rev="+c1, "hv-01"); err == nil ||
		!strings.Contains(err.Error(), "evaluation service") {
		t.Errorf("no service: %v", err)
	}
}

func TestServeEvalRefusesBadRequests(t *testing.T) {
	for _, req := range []string{`{"commit":"HEAD","node":"hv-01"}`, `{"commit":"` + c1 + `","node":"x y"}`} {
		var out bytes.Buffer
		rw := struct {
			io.Reader
			io.Writer
		}{strings.NewReader(req), &out}
		if err := ServeEval(context.Background(), rw, &Evaluator{Nix: "/bin/false"}, "/r"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), `"error"`) || strings.Contains(out.String(), `"ir"`) {
			t.Errorf("%s: %s", req, out.String())
		}
	}
}
