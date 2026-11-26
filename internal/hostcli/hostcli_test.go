package hostcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/letthemplay/Strata/internal/agent/api"
	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/controller"
)

const (
	sha  = "1111111111111111111111111111111111111111"
	out1 = "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-nixos-system-hv-01"
	out2 = "/nix/store/dddddddddddddddddddddddddddddddd-nixos-system-hv-01"
	drv1 = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-nixos-system-hv-01.drv"
)

// fake is the controller's host API.
type fake struct {
	mu       sync.Mutex
	hc       controller.HostConfiguration
	evalErr  string // reply of POST evaluate, if set
	polls    int
	finished controller.HostEvaluation // what a poll returns once done
	requests []string
}

func succeeded() *controller.HostEvaluation {
	return &controller.HostEvaluation{
		ID: "20261126T100000Z-abcdef", State: controller.HostEvalSucceeded, DurationSeconds: 9.8,
		StartedAt: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC), CompletedAt: time.Date(2026, 11, 26, 10, 0, 10, 0, time.UTC),
		Source:  controller.SourceSnapshot{Commit: sha, Tree: "2222222222222222222222222222222222222222", FlakeLock: "lockblob", Signer: "alice", Node: "hv-01"},
		DrvPath: drv1, OutPath: out1,
	}
}

func (f *fake) serve(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "c.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, []controller.NodeInfo{{ID: "hv-01"}})
	})
	mux.HandleFunc("GET /api/v1/nodes/{node}/configuration", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		if r.PathValue("node") != "hv-01" {
			reply(w, http.StatusNotFound, api.ErrorResponse{Error: `no such node: "` + r.PathValue("node") + `"`, Code: "not_found"})
			return
		}
		f.polls++
		if f.polls >= 2 && f.finished.ID != "" {
			ev := f.finished
			f.hc.Evaluation = &ev
		}
		reply(w, http.StatusOK, f.hc)
	})
	mux.HandleFunc("POST /api/v1/nodes/{node}/configuration/evaluate", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		if f.evalErr != "" {
			reply(w, http.StatusConflict, api.ErrorResponse{Error: f.evalErr, Code: "conflict"})
			return
		}
		ev := controller.HostEvaluation{ID: f.finished.ID, State: controller.HostEvalRunning, Source: f.finished.Source}
		f.hc.Evaluation = &ev
		f.polls = 0
		reply(w, http.StatusAccepted, ev)
	})
	srv := &http.Server{Handler: mux} //nolint:gosec // Test server.
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socket
}

func host(t *testing.T, socket string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := cli.Command{Name: "strata", Subcommands: []cli.Subcommand{Subcommand()}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code := cmd.Run(ctx, append(append([]string{"host"}, args...), "--socket", socket), &out, &errOut)
	return code, out.String(), errOut.String()
}

func baseStatus() controller.HostConfiguration {
	return controller.HostConfiguration{
		Node: "hv-01", Repository: "ssh://git@h/x.git", Branch: "main", Target: sha, Note: controller.HostNote,
		Observed:           controller.ObservedSystem{Current: out2, Booted: out1},
		ConfigurationDrift: controller.DriftUnknown, DriftReason: "commit 111111111111 has not been evaluated (strata host evaluate)",
		BootState: controller.BootRebootPending, BootReason: "the running system is not the one the host booted",
	}
}

func TestStatusUnevaluated(t *testing.T) {
	f := &fake{hc: baseStatus()}
	code, out, errOut := host(t, f.serve(t), "status")
	if code != cli.ExitOK {
		t.Fatalf("%d %s", code, errOut)
	}
	for _, want := range []string{
		"Node:                hv-01",
		"Target commit:       " + sha,
		"Evaluation:          (none; strata host evaluate)",
		"Desired system:      (unknown: the target commit has no successful evaluation)",
		"Running system:      " + out2,
		"Booted system:       " + out1,
		"Configuration drift: unknown: commit 111111111111 has not been evaluated",
		"Boot state:          reboot-pending: the running system",
		"Note: Strata can evaluate host configurations but does not deploy them yet.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestStatusUnknownObservations(t *testing.T) {
	hc := baseStatus()
	hc.Observed = controller.ObservedSystem{CurrentError: "read /run/current-system: no such file", BootedError: "read /run/booted-system: no such file"}
	hc.BootState, hc.BootReason = controller.BootUnknown, "the running system can't be read"
	hc.Target, hc.Pinned = sha, true
	f := &fake{hc: hc}
	_, out, _ := host(t, f.serve(t), "status")
	for _, want := range []string{
		"Target commit:       " + sha + " (pinned)",
		"Running system:      (unknown: read /run/current-system: no such file)",
		"Boot state:          unknown: the running system can't be read",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestStatusJSON(t *testing.T) {
	hc := baseStatus()
	hc.Evaluation, hc.Desired = succeeded(), succeeded()
	hc.ConfigurationDrift, hc.DriftReason = controller.DriftDiffers, "the host runs a different system than Git declares"
	f := &fake{hc: hc}
	code, out, _ := host(t, f.serve(t), "status", "--json")
	var got controller.HostConfiguration
	if err := json.Unmarshal([]byte(out), &got); code != cli.ExitOK || err != nil {
		t.Fatalf("%d %v\n%s", code, err, out)
	}
	if got.ConfigurationDrift != controller.DriftDiffers || got.BootState != controller.BootRebootPending || got.Desired.OutPath != out1 {
		t.Errorf("json %+v", got)
	}
	// The two properties are separate fields.
	if !strings.Contains(out, `"configurationDrift": "differs"`) || !strings.Contains(out, `"bootState": "reboot-pending"`) {
		t.Errorf("fields:\n%s", out)
	}
}

func TestEvaluateWaits(t *testing.T) {
	hc := baseStatus()
	f := &fake{hc: hc, finished: *succeeded()}
	f.hc.Desired = nil
	socket := f.serve(t)
	// Once finished, the fake's status shows the evaluation (and the
	// desired system the controller would report).
	f.hc.Desired = succeeded()
	f.hc.ConfigurationDrift, f.hc.DriftReason = controller.DriftDiffers, "the host runs a different system than Git declares"
	code, out, errOut := host(t, socket, "evaluate")
	if code != cli.ExitOK {
		t.Fatalf("%d %s\n%s", code, errOut, out)
	}
	for _, want := range []string{
		"Evaluating 111111111111 (evaluation 20261126T100000Z-abcdef)...",
		"Evaluation:          20261126T100000Z-abcdef, of 111111111111: succeeded in 9.8 s",
		"Desired system:      " + out1,
		"  Derivation:        " + drv1,
		"Configuration drift: differs: the host runs a different system than Git declares",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if f.requests[0] != "POST /api/v1/nodes/hv-01/configuration/evaluate" {
		t.Errorf("requests %v", f.requests)
	}
}

func TestEvaluateFailureExitsNonZero(t *testing.T) {
	failed := *succeeded()
	failed.State, failed.Error, failed.DrvPath, failed.OutPath = controller.HostEvalFailed, "commit 111111111111: evaluation failed:\nerror: boom", "", ""
	f := &fake{hc: baseStatus(), finished: failed}
	code, out, _ := host(t, f.serve(t), "evaluate")
	if code != cli.ExitError || !strings.Contains(out, ": failed after") || !strings.Contains(out, "error: boom") {
		t.Errorf("%d\n%s", code, out)
	}
}

func TestEvaluateNoWaitAndErrors(t *testing.T) {
	f := &fake{hc: baseStatus(), finished: *succeeded()}
	socket := f.serve(t)
	if code, out, _ := host(t, socket, "evaluate", "--no-wait"); code != cli.ExitOK || !strings.Contains(out, "started; see strata host status") {
		t.Errorf("no-wait: %d %s", code, out)
	}
	f.evalErr = "a host evaluation is in progress (20261126T100000Z-abcdef)"
	if code, _, errOut := host(t, socket, "evaluate"); code != cli.ExitError || !strings.Contains(errOut, "in progress") {
		t.Errorf("busy: %d %s", code, errOut)
	}
	if code, _, errOut := host(t, socket, "status", "--node", "hv-02"); code != cli.ExitError || !strings.Contains(errOut, "no such node") {
		t.Errorf("unknown node: %d %s", code, errOut)
	}
	if code, _, _ := host(t, socket, "build"); code != cli.ExitUsage {
		t.Errorf("unknown command: %d", code)
	}
	if code, _, _ := host(t, socket, "status", "extra"); code != cli.ExitUsage {
		t.Errorf("extra argument: %d", code)
	}
	if code, _, errOut := host(t, filepath.Join(t.TempDir(), "none.sock"), "status"); code != cli.ExitError || !strings.Contains(errOut, "not running on this host") {
		t.Errorf("no controller: %d %s", code, errOut)
	}
}
