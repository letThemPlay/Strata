package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Store paths for tests (Nix base-32 hashes).
var (
	drv1 = "/nix/store/" + strings.Repeat("a", 32) + "-nixos-system-hv-01.drv"
	out1 = "/nix/store/" + strings.Repeat("b", 32) + "-nixos-system-hv-01"
	drv2 = "/nix/store/" + strings.Repeat("c", 32) + "-nixos-system-hv-01.drv"
	out2 = "/nix/store/" + strings.Repeat("d", 32) + "-nixos-system-hv-01"
	sys1 = SystemEval{DrvPath: drv1, OutPath: out1}
	sys2 = SystemEval{DrvPath: drv2, OutPath: out2}
)

func base(p string) string { return strings.TrimPrefix(p, "/nix/store/") }

// v4Show is `nix derivation show` output of Nix 2.35 for drv building out.
func v4Show(drv, out string) string {
	return `{"derivations":{"` + base(drv) + `":{"name":"x","outputs":{"out":{"path":"` + base(out) + `"}},"version":4}},"version":4}`
}

func TestEvaluatorSystem(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "nix")
	e := &Evaluator{Nix: fake, Home: dir, Timeout: 5 * time.Second}
	// script answers `nix derivation show` with show.
	script := func(show string) {
		t.Helper()
		s := `#!/bin/sh
printf '%s\n' "$@" > ` + dir + `/args
case "$*" in
*"derivation show"*) ` + show + ` ;;
*) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
		if err := os.WriteFile(fake, []byte(s), 0o700); err != nil { //nolint:gosec // An executable test script.
			t.Fatal(err)
		}
	}
	url := "git+file:///r?rev=" + c1

	script(`echo '` + v4Show(drv1, out1) + `'`)
	got, err := e.System(context.Background(), url, "hv-01")
	if err != nil || got != sys1 {
		t.Fatalf("System %+v %v", got, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, want := range []string{
		"derivation\nshow", "eval-cache\nfalse", "--offline", "--no-write-lock-file", "allow-import-from-derivation\nfalse", "pure-eval\ntrue", "substitute\nfalse",
		url + `#nixosConfigurations."hv-01".config.system.build.toplevel`,
	} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	// The installable comes last, after the restrictions.
	if lines := strings.Split(strings.TrimSpace(string(args)), "\n"); !strings.HasSuffix(lines[len(lines)-1], ".config.system.build.toplevel") {
		t.Errorf("installable not last:\n%s", args)
	}

	// The earlier `derivation show` format (full paths) is read too.
	script(`echo '{"` + drv1 + `":{"outputs":{"out":{"path":"` + out1 + `"}}}}'`)
	if got, err := e.System(context.Background(), url, "hv-01"); err != nil || got != sys1 {
		t.Errorf("earlier format: %+v %v", got, err)
	}

	two := `{"derivations":{"` + base(drv1) + `":{"outputs":{"out":{"path":"` + base(out1) + `"}}},"` + base(drv2) + `":{"outputs":{"out":{"path":"` + base(out2) + `"}}}}}`
	for name, c := range map[string]struct{ show, want string }{
		"no output":         {`echo '{"derivations":{"` + base(drv1) + `":{"outputs":{}}}}'`, "not a store path"},
		"two derivations":   {`echo '` + two + `'`, "listed 2 derivations"},
		"none":              {`echo '{"derivations":{}}'`, "listed 0 derivations"},
		"not a derivation":  {`echo '` + v4Show(out1, out2) + `'`, "not a store derivation"},
		"output is a drv":   {`echo '` + v4Show(drv1, drv2) + `'`, "not a store path"},
		"outside the store": {`echo '{"/tmp/x.drv":{"outputs":{"out":{"path":"` + out1 + `"}}}}'`, "not a store derivation"},
		"not json":          {"echo nope", "unexpected output"},
	} {
		script(c.show)
		if _, err := e.System(context.Background(), url, "hv-01"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}

	script("echo 'error: \033[31mboom\033[0m' >&2; exit 1")
	var ee *EvalError
	if _, err := e.System(context.Background(), url, "hv-01"); !errors.As(err, &ee) || ee.Cause() != "error: boom" {
		t.Errorf("evaluation failure: %v", err)
	}
	script("echo 'error: unable to download https://x' >&2; exit 1")
	if _, err := e.System(context.Background(), url, "hv-01"); !errors.Is(err, ErrInputsMissing) {
		t.Errorf("offline: %v", err)
	}
	if _, err := e.System(context.Background(), url, `hv-01"; x`); err == nil || !strings.Contains(err.Error(), "not a valid node ID") {
		t.Errorf("bad node: %v", err)
	}
	script("sleep 10")
	e.Timeout = 100 * time.Millisecond
	if _, err := e.System(context.Background(), url, "hv-01"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("timeout: %v", err)
	}
}

// serveEvalOnce runs ServeEval on request and returns the raw reply.
func serveEvalOnce(t *testing.T, e *Evaluator, request string) evalReply {
	t.Helper()
	var out bytes.Buffer
	rw := struct {
		io.Reader
		io.Writer
	}{strings.NewReader(request), &out}
	if err := ServeEval(context.Background(), rw, e, "/r"); err != nil {
		t.Fatal(err)
	}
	var reply evalReply
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
		t.Fatalf("reply %q: %v", out.String(), err)
	}
	return reply
}

func TestServeEvalKinds(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "nix")
	s := `#!/bin/sh
case "$*" in
*"config.strata.ir"*) echo '{"apiVersion":"ir"}' ;;
*"derivation show"*"toplevel"*) echo '` + v4Show(drv1, out1) + `' ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(fake, []byte(s), 0o700); err != nil { //nolint:gosec // An executable test script.
		t.Fatal(err)
	}
	e := &Evaluator{Nix: fake, Home: dir, Timeout: 5 * time.Second}
	req := func(kind string) string {
		k := ""
		if kind != "-" {
			k = `,"kind":"` + kind + `"`
		}
		return `{"commit":"` + c1 + `","node":"hv-01"` + k + `}`
	}
	// No kind is IR (the existing protocol); so is "ir".
	for _, kind := range []string{"-", "", "ir"} {
		if r := serveEvalOnce(t, e, req(kind)); string(r.IR) != `{"apiVersion":"ir"}` || r.System != nil || r.Error != "" {
			t.Errorf("kind %q: %+v", kind, r)
		}
	}
	if r := serveEvalOnce(t, e, req("system")); r.System == nil || *r.System != sys1 || r.IR != nil {
		t.Errorf("system: %+v", r)
	}
	// An unknown kind is refused, never evaluated as another kind.
	for _, kind := range []string{"toplevel", "IR", "system "} {
		if r := serveEvalOnce(t, e, req(kind)); !strings.Contains(r.Error, "unknown evaluation kind") || r.IR != nil || r.System != nil {
			t.Errorf("kind %q: %+v", kind, r)
		}
	}
}

func TestRemoteEvaluatorSystem(t *testing.T) {
	socket := evalService(t, `case "$*" in
*"derivation show"*"repo.git?rev=`+c1+`#nixosConfigurations.\"hv-01\".config.system.build.toplevel"*) echo '`+v4Show(drv1, out1)+`' ;;
*) echo "unexpected: $*" >&2; exit 1 ;;
esac`)
	r := &RemoteEvaluator{Socket: socket, Timeout: 5 * time.Second}
	if got, err := r.System(context.Background(), "git+file:///x?rev="+c1, "hv-01"); err != nil || got != sys1 {
		t.Fatalf("System %+v %v", got, err)
	}
	if _, err := r.System(context.Background(), "git+file:///x?ref=main", "hv-01"); err == nil {
		t.Error("a URL without a commit was accepted")
	}

	// The reply is checked again by the controller.
	for name, reply := range map[string]string{
		"no system":    `{}`,
		"bad path":     `{"system":{"drvPath":"/tmp/x.drv","outPath":"` + out1 + `"}}`,
		"service said": `{"error":"unknown evaluation kind \"system\""}`,
	} {
		sock := fakeService(t, reply)
		if _, err := (&RemoteEvaluator{Socket: sock, Timeout: time.Second}).System(context.Background(), "x?rev="+c1, "hv-01"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// fakeService answers every connection with reply, after reading the
// request.
func fakeService(t *testing.T, reply string) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "s.sock")
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
			var req evalRequest
			_ = json.NewDecoder(conn).Decode(&req)
			_, _ = io.WriteString(conn, reply+"\n")
			_ = conn.Close()
		}
	}()
	return socket
}

func TestConfigurationDriftAndBootState(t *testing.T) {
	desired := &HostEvaluation{State: HostEvalSucceeded, Source: SourceSnapshot{Commit: c1}, OutPath: out1}
	running := &HostEvaluation{State: HostEvalRunning, Source: SourceSnapshot{Commit: c1}}
	failed := &HostEvaluation{State: HostEvalFailed, Source: SourceSnapshot{Commit: c1}}
	at := func(cur, boot string) ObservedSystem {
		o := ObservedSystem{Current: cur, Booted: boot}
		if cur == "" {
			o.CurrentError = "no link"
		}
		if boot == "" {
			o.BootedError = "no link"
		}
		return o
	}
	for name, c := range map[string]struct {
		target          string
		desired, latest *HostEvaluation
		obs             ObservedSystem
		drift, boot     string
	}{
		"in sync, booted":            {c1, desired, desired, at(out1, out1), DriftInSync, BootCurrent},
		"in sync, reboot pending":    {c1, desired, desired, at(out1, out2), DriftInSync, BootRebootPending},
		"differs, booted":            {c1, desired, desired, at(out2, out2), DriftDiffers, BootCurrent},
		"differs, reboot pending":    {c1, desired, desired, at(out2, out1), DriftDiffers, BootRebootPending},
		"not evaluated":              {c1, nil, nil, at(out1, out1), DriftUnknown, BootCurrent},
		"evaluation running":         {c1, nil, running, at(out1, out1), DriftUnknown, BootCurrent},
		"evaluation failed":          {c1, nil, failed, at(out1, out1), DriftUnknown, BootCurrent},
		"no commit":                  {"", nil, nil, at(out1, out1), DriftUnknown, BootCurrent},
		"running system unreadable":  {c1, desired, desired, at("", out1), DriftUnknown, BootUnknown},
		"booted system unreadable":   {c1, desired, desired, at(out1, ""), DriftInSync, BootUnknown},
		"nothing readable":           {c1, desired, desired, at("", ""), DriftUnknown, BootUnknown},
		"in sync, booted unreadable": {c1, desired, desired, at(out1, ""), DriftInSync, BootUnknown},
	} {
		drift, reason := configurationDrift(c.target, c.desired, c.latest, c.obs)
		boot, bootReason := bootState(c.obs)
		if drift != c.drift || boot != c.boot {
			t.Errorf("%s: drift %s (%s), boot %s (%s); want %s, %s", name, drift, reason, boot, bootReason, c.drift, c.boot)
		}
		if (drift == DriftUnknown || drift == DriftDiffers) && reason == "" {
			t.Errorf("%s: no reason for %s", name, drift)
		}
	}
}

func TestRunObserver(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := func(name, target string) {
		t.Helper()
		p := filepath.Join(root, "run", name)
		_ = os.Remove(p)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	o := RunObserver{Root: root}
	if obs := o.Observe(); obs.Current != "" || obs.CurrentError == "" || obs.BootedError == "" {
		t.Errorf("missing links: %+v", obs)
	}
	link("current-system", out1)
	link("booted-system", out2)
	if obs := o.Observe(); obs.Current != out1 || obs.Booted != out2 || obs.CurrentError != "" || obs.BootedError != "" {
		t.Errorf("links: %+v", obs)
	}
	link("current-system", "/etc")
	if obs := o.Observe(); obs.Current != "" || !strings.Contains(obs.CurrentError, "not a store path") {
		t.Errorf("not a store path: %+v", obs)
	}
	// The links are only read.
	if target, _ := os.Readlink(filepath.Join(root, "run/booted-system")); target != out2 {
		t.Errorf("booted-system changed: %s", target)
	}
}

// fakeSystem evaluates systems per commit; with block set, it waits.
type fakeSystem struct {
	mu    sync.Mutex
	out   map[string]SystemEval
	err   error
	block chan struct{}
	calls []string
}

func (f *fakeSystem) System(_ context.Context, url, node string) (SystemEval, error) {
	commit := url[strings.Index(url, "rev=")+4:]
	f.mu.Lock()
	f.calls = append(f.calls, commit+" "+node)
	block, out, err := f.block, f.out[commit], f.err
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return out, err
}

type fakeObserver struct{ obs ObservedSystem }

func (o fakeObserver) Observe() ObservedSystem { return o.obs }

// hostController is a synced controller with fake system evaluation; the
// host runs out1.
func hostController(t *testing.T) (*Controller, *fakeRepo, *fakeSystem) {
	t.Helper()
	c, repo, _, _ := newController(t)
	sys := &fakeSystem{out: map[string]SystemEval{c1: sys1, c2: sys2}}
	c.SystemEval, c.Observer = sys, fakeObserver{ObservedSystem{Current: out1, Booted: out1}}
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Sync(context.Background())
	return c, repo, sys
}

func hostEvaluated(t *testing.T, c *Controller) HostConfiguration {
	t.Helper()
	if _, err := c.EvaluateHost(1000); err != nil {
		t.Fatalf("EvaluateHost: %v", err)
	}
	c.Wait()
	return c.HostStatus()
}

func TestEvaluateHost(t *testing.T) {
	c, repo, sys := hostController(t)
	before := c.HostStatus()
	if before.ConfigurationDrift != DriftUnknown || before.BootState != BootCurrent || before.Evaluation != nil || before.Note != HostNote {
		t.Fatalf("before: %+v", before)
	}
	hc := hostEvaluated(t, c)
	ev := hc.Evaluation
	if ev == nil || ev.State != HostEvalSucceeded || ev.DrvPath != drv1 || ev.OutPath != out1 || ev.RequestedBy != "uid:1000" || ev.CompletedAt.IsZero() {
		t.Fatalf("evaluation %+v", ev)
	}
	want := SourceSnapshot{
		Repository: c.Config.Repository, Branch: c.Config.Branch, Commit: c1, Tree: "f" + c1[1:], FlakeLock: "lock1",
		Signer: "alice", Node: "hv-01", Target: `nixosConfigurations."hv-01".config.system.build.toplevel`,
	}
	if ev.Source != want {
		t.Errorf("source %+v, want %+v", ev.Source, want)
	}
	if hc.Desired == nil || hc.Desired.OutPath != out1 || hc.ConfigurationDrift != DriftInSync || hc.Target != c1 {
		t.Errorf("status %+v", hc)
	}
	if len(sys.calls) != 1 || sys.calls[0] != c1+" hv-01" {
		t.Errorf("calls %v", sys.calls)
	}

	// A new commit is the target: the earlier result no longer says
	// anything about it.
	repo.head = c2
	c.Sync(context.Background())
	if hc := c.HostStatus(); hc.ConfigurationDrift != DriftUnknown || hc.Desired != nil || !strings.Contains(hc.DriftReason, "not been evaluated") {
		t.Errorf("after a new head: %+v", hc)
	}
	if hc := hostEvaluated(t, c); hc.ConfigurationDrift != DriftDiffers || hc.Desired.OutPath != out2 {
		t.Errorf("c2: %+v", hc)
	}
}

func TestEvaluateHostFailuresAreNotCached(t *testing.T) {
	c, _, sys := hostController(t)
	sys.err = &EvalError{Detail: "error:\n  … while evaluating\n\n  error: boom"}
	hc := hostEvaluated(t, c)
	if hc.Evaluation.State != HostEvalFailed || !strings.Contains(hc.Evaluation.Error, "boom") || strings.Contains(hc.Evaluation.Error, "while evaluating") {
		t.Errorf("failed: %+v", hc.Evaluation)
	}
	if hc.Desired != nil || hc.ConfigurationDrift != DriftUnknown || !strings.Contains(hc.DriftReason, "failed") {
		t.Errorf("a failure was taken as a result: %+v", hc)
	}
	// A later success is used; a later failure doesn't erase it.
	sys.err = nil
	hostEvaluated(t, c)
	sys.err = errors.New("service down")
	hc = hostEvaluated(t, c)
	if hc.Evaluation.State != HostEvalFailed || hc.Desired == nil || hc.Desired.OutPath != out1 || hc.ConfigurationDrift != DriftInSync {
		t.Errorf("after success then failure: %+v", hc)
	}
}

func TestEvaluateHostRefusals(t *testing.T) {
	// Untrusted commit: refused before evaluation.
	c, repo, sys := hostController(t)
	repo.untrusted[c1] = true
	hc := hostEvaluated(t, c)
	if hc.Evaluation.State != HostEvalFailed || !strings.Contains(hc.Evaluation.Error, "refused") || len(sys.calls) != 0 {
		t.Errorf("untrusted: %+v, calls %v", hc.Evaluation, sys.calls)
	}

	// No commit fetched yet.
	c2c, _, _, _ := newController(t)
	c2c.SystemEval = &fakeSystem{}
	if _, err := c2c.EvaluateHost(1000); !errors.Is(err, ErrInvalidAction) {
		t.Errorf("no head: %v", err)
	}
	// Not configured.
	c3, _, _, _ := newController(t)
	if _, err := c3.EvaluateHost(1000); !errors.Is(err, ErrInvalidAction) {
		t.Errorf("not configured: %v", err)
	}
}

func TestEvaluateHostFlakeLockChange(t *testing.T) {
	c, repo, sys := hostController(t)
	missing := &EvalError{Detail: "error: unable to download https://github.com/x", InputsMissing: true}
	// c2 changes flake.lock; its inputs are not on the host, so the IR
	// fails too and c1 stays the last commit that evaluated.
	repo.locks[c2] = "lock2"
	c.Eval.(*fakeEval).err[c2] = missing
	repo.head = c2
	c.Sync(context.Background())
	sys.err = missing
	hc := hostEvaluated(t, c)
	if hc.Target != c2 || !strings.Contains(hc.Evaluation.Error, "changes flake.lock") || !strings.Contains(hc.Evaluation.Error, "nixos-rebuild") ||
		!strings.Contains(hc.Evaluation.Error, "unable to download") {
		t.Errorf("flake.lock change: %+v", hc.Evaluation)
	}
}

func TestEvaluateHostTargetChangesDuringEvaluation(t *testing.T) {
	c, _, sys := hostController(t)
	sys.block = make(chan struct{})
	if _, err := c.EvaluateHost(1000); err != nil {
		t.Fatal(err)
	}
	// A second request while one runs is refused, and leaves it alone.
	if ev, err := c.EvaluateHost(1001); !errors.Is(err, ErrHostBusy) || ev.State != HostEvalRunning || ev.RequestedBy != "uid:1000" {
		t.Errorf("busy: %+v %v", ev, err)
	}
	if hc := c.HostStatus(); hc.ConfigurationDrift != DriftUnknown || !strings.Contains(hc.DriftReason, "in progress") {
		t.Errorf("running: %+v", hc)
	}
	c.mu.Lock()
	c.head = c2 // a sync fetched a new head meanwhile
	c.mu.Unlock()
	close(sys.block)
	c.Wait()
	hc := c.HostStatus()
	if hc.Evaluation.State != HostEvalFailed || !strings.Contains(hc.Evaluation.Error, "target changed") || hc.Evaluation.OutPath != "" {
		t.Errorf("moved target: %+v", hc.Evaluation)
	}
	// Nothing was cached for either commit.
	c.mu.Lock()
	c.head = c1
	c.mu.Unlock()
	if hc := c.HostStatus(); hc.Desired != nil {
		t.Errorf("cached: %+v", hc.Desired)
	}
}

func TestEvaluateHostConcurrentRequests(t *testing.T) {
	c, _, _ := hostController(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	started, busy := 0, 0
	for i := range uint32(16) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.EvaluateHost(1000 + i)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				started++
			case errors.Is(err, ErrHostBusy):
				busy++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
		_ = c.HostStatus()
	}
	wg.Wait()
	c.Wait()
	if started+busy != 16 || started == 0 {
		t.Errorf("started %d busy %d", started, busy)
	}
	if hc := c.HostStatus(); hc.Evaluation.State != HostEvalSucceeded || hc.ConfigurationDrift != DriftInSync {
		t.Errorf("final: %+v", hc)
	}
}

// Host evaluation never changes the workload pipeline: its status, plan,
// records and state stay as they were, whatever the outcome.
func TestHostEvaluationLeavesWorkloadsAlone(t *testing.T) {
	c, _, sys := hostController(t)
	status, plan, history := c.Status(), mustPlan(t, c), c.History(0)
	state, err := c.Store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []error{&EvalError{Detail: "error: boom"}, nil} {
		sys.err = e
		hostEvaluated(t, c)
		if got := c.Status(); !reflect.DeepEqual(got, status) {
			t.Errorf("status changed:\n%+v\n%+v", got, status)
		}
		if got := mustPlan(t, c); !reflect.DeepEqual(got, plan) {
			t.Errorf("plan changed")
		}
		if got := c.History(0); !reflect.DeepEqual(got, history) {
			t.Errorf("history changed: %+v", got)
		}
		if got, _ := c.Store.LoadState(); got != state {
			t.Errorf("state changed: %+v", got)
		}
	}
}

func mustPlan(t *testing.T, c *Controller) CurrentPlan {
	t.Helper()
	p, err := c.Plan()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHostAPI(t *testing.T) {
	c, _, sys := hostController(t)
	do := serveController(t, c)
	if code, body := do(http.MethodGet, "/api/v1/nodes"); code != http.StatusOK || strings.TrimSpace(body) != `[{"id":"hv-01"}]` {
		t.Errorf("nodes: %d %s", code, body)
	}
	for _, p := range []string{"/api/v1/nodes/hv-02/configuration", "/api/v1/nodes/x/configuration/evaluate"} {
		method := http.MethodGet
		if strings.HasSuffix(p, "evaluate") {
			method = http.MethodPost
		}
		if code, body := do(method, p); code != http.StatusNotFound || !strings.Contains(body, "no such node") {
			t.Errorf("%s: %d %s", p, code, body)
		}
	}
	if code, body := do(http.MethodGet, "/api/v1/nodes/hv-01/configuration"); code != http.StatusOK ||
		!strings.Contains(body, `"configurationDrift":"unknown"`) || !strings.Contains(body, `"bootState":"current"`) {
		t.Errorf("configuration: %d %s", code, body)
	}
	sys.block = make(chan struct{})
	if code, body := do(http.MethodPost, "/api/v1/nodes/hv-01/configuration/evaluate"); code != http.StatusAccepted || !strings.Contains(body, `"state":"running"`) {
		t.Errorf("evaluate: %d %s", code, body)
	}
	if code, body := do(http.MethodPost, "/api/v1/nodes/hv-01/configuration/evaluate"); code != http.StatusConflict {
		t.Errorf("busy: %d %s", code, body)
	}
	close(sys.block)
	c.Wait()
	var hc HostConfiguration
	code, body := do(http.MethodGet, "/api/v1/nodes/hv-01/configuration")
	if err := json.Unmarshal([]byte(body), &hc); code != http.StatusOK || err != nil || hc.ConfigurationDrift != DriftInSync || hc.Desired.OutPath != out1 {
		t.Errorf("after: %d %s %v", code, body, err)
	}
}

// serveController serves c's API on a socket and returns a request
// function (status, body).
func serveController(t *testing.T, c *Controller) func(method, path string) (int, string) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "c.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Controller: c}
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
	return func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, "http://c"+path, nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
}
