package configcli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/controller"
	"github.com/letthemplay/Strata/internal/planner"
	"github.com/letthemplay/Strata/internal/reconciler"
	"github.com/letthemplay/Strata/internal/workload"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// fakeController serves the controller's API from canned values and
// records requests.
type fakeController struct {
	mu       sync.Mutex
	status   controller.Status
	plan     *controller.CurrentPlan
	deploy   controller.Deployment
	polls    int
	requests []string
}

func (f *fakeController) serve(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "c.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	fail := func(w http.ResponseWriter, code int, msg string) {
		w.WriteHeader(code)
		reply(w, map[string]string{"error": msg})
	}
	record := func(r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI()+" "+strings.TrimSpace(string(body)))
		f.mu.Unlock()
	}
	status := func(w http.ResponseWriter, r *http.Request) { record(r); reply(w, f.status) }
	mux.HandleFunc("GET /api/v1/config", status)
	mux.HandleFunc("POST /api/v1/config/sync", status)
	mux.HandleFunc("POST /api/v1/config/pin", status)
	mux.HandleFunc("POST /api/v1/config/unpin", status)
	mux.HandleFunc("POST /api/v1/config/adopt/{name}", status)
	mux.HandleFunc("GET /api/v1/config/plan", func(w http.ResponseWriter, _ *http.Request) {
		if f.plan == nil {
			fail(w, http.StatusNotFound, "no current plan (the last synchronization failed: refused)")
			return
		}
		reply(w, f.plan)
	})
	mux.HandleFunc("POST /api/v1/config/apply", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		d := f.deploy
		d.State = controller.StatePending
		reply(w, d)
	})
	mux.HandleFunc("GET /api/v1/deployments/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.polls++
		d := f.deploy
		if f.polls < 2 {
			d.State = controller.StateApplying
		}
		f.mu.Unlock()
		if r.PathValue("id") != d.ID {
			fail(w, http.StatusNotFound, "no such deployment")
			return
		}
		reply(w, d)
	})
	mux.HandleFunc("GET /api/v1/deployments", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, []controller.DeploymentSummary{{ID: f.deploy.ID, Kind: "plan", State: "SUCCEEDED", Commit: sha, Items: 2, ApprovedBy: "uid:1000"}})
	})
	srv := &http.Server{Handler: mux} //nolint:gosec // Test server.
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socket
}

func config(t *testing.T, socket, input string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := cli.Command{Name: "strata", Subcommands: []cli.Subcommand{Subcommand(strings.NewReader(input))}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code := cmd.Run(ctx, append(append([]string{"config"}, args...), "--socket", socket), &out, &errOut)
	return code, out.String(), errOut.String()
}

func samplePlan(deletes bool) *controller.CurrentPlan {
	yes := true
	items := []planner.Item{
		{Resource: "vm/db", Action: planner.ActionUpdate, Restarts: true, Changes: []workload.Change{{Field: "resources.cpu", From: "2", To: "4", Restart: true}}},
		{Resource: "vm/new", Action: planner.ActionCreate, Protect: &yes},
		{Resource: "vm/web", Action: planner.ActionUnreconcilable, Reasons: []string{"created with `strata vm`"}},
	}
	if deletes {
		items = append(items, planner.Item{Resource: "vm/old", Action: planner.ActionDelete, Deletes: true, KeptVolumes: []string{"fast/old-data"}})
	}
	return &controller.CurrentPlan{Commit: sha, Signer: "alice", Plan: planner.Plan{
		ID: "abc", Node: "hv-01", Items: items, InSync: []string{"vm/ok"}, Unmanaged: []string{"manual"},
	}}
}

func newFake() *fakeController {
	return &fakeController{
		status: controller.Status{
			Repository: "ssh://git@h/x.git", Branch: "main", Node: "hv-01", Head: sha, Evaluated: sha, Signer: "alice", AutoApply: "none",
			Plan:     &controller.PlanSummary{ID: "abc", Items: 3, Restarts: true, Unreconcilable: 1},
			Awaiting: &controller.DeploymentSummary{ID: "20261125T120000Z-abcdef", PlanID: "abc"},
		},
		plan: samplePlan(false),
		deploy: controller.Deployment{
			ID: "20261125T120000Z-abcdef", Kind: "plan", State: controller.StateSucceeded, Commit: sha, Signer: "alice",
			StrataVersion: "0.1.0", PlanID: "abc", ApprovedBy: "uid:1000",
			Result: &reconciler.Result{Items: []reconciler.ItemResult{{Resource: "vm/new", Action: "create", Status: "succeeded"}}},
		},
	}
}

func TestStatusAndPlan(t *testing.T) {
	socket := newFake().serve(t)
	code, out, _ := config(t, socket, "", "status")
	for _, want := range []string{
		"Repository:   ssh://git@h/x.git (branch main)", "Evaluated:    " + sha + " (signed by alice)",
		"Plan:         abc: 3 items (restarts VMs, 1 cannot be applied)", "approve with: strata config apply --plan abc",
	} {
		if code != cli.ExitOK || !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = config(t, socket, "", "plan")
	for _, want := range []string{
		"Plan abc for node hv-01, commit 0123456789ab (signed by alice):",
		"update          vm/db  (restarts the VM)", "resources.cpu: 2 -> 4",
		"create          vm/new  (protected: true)", "cannot apply: created with `strata vm`",
		"In sync: vm/ok", "Not managed through Git (created with strata vm): manual",
		"Approve with: strata config apply --plan abc\n",
	} {
		if code != cli.ExitOK || !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if code, out, _ = config(t, socket, "", "status", "--json"); code != cli.ExitOK || !strings.Contains(out, `"evaluated": "`+sha) {
		t.Errorf("json: %d\n%s", code, out)
	}
}

func TestApply(t *testing.T) {
	f := newFake()
	socket := f.serve(t)
	code, out, errOut := config(t, socket, "y\n", "apply", "--plan", "abc")
	if code != cli.ExitOK || !strings.Contains(out, "Apply this plan? [y/N]") || !strings.Contains(out, "Deployment 20261125T120000Z-abcdef: PENDING") ||
		!strings.Contains(out, "  SUCCEEDED") || !strings.Contains(out, "succeeded      create          vm/new") {
		t.Fatalf("apply: %d %s\n%s", code, errOut, out)
	}
	// Already approving: no "Approve with" hint.
	if strings.Contains(out, "Approve with") {
		t.Errorf("apply repeats the approval hint:\n%s", out)
	}
	if got := f.requests[len(f.requests)-1]; got != `POST /api/v1/config/apply {"allowDelete":false,"planId":"abc"}` {
		t.Errorf("request %s", got)
	}
	// Declined: nothing is approved.
	n := len(f.requests)
	if code, out, _ := config(t, socket, "n\n", "apply", "--plan", "abc"); code != cli.ExitError || !strings.Contains(out, "Not applied.") || len(f.requests) != n {
		t.Errorf("declined: %d %s", code, out)
	}
	// Another plan ID: refused before asking.
	if code, _, errOut := config(t, socket, "y\n", "apply", "--plan", "old"); code != cli.ExitError || !strings.Contains(errOut, "is not the current plan (it is abc)") {
		t.Errorf("stale: %d %s", code, errOut)
	}
	if code, _, _ := config(t, socket, "", "apply"); code != cli.ExitUsage {
		t.Errorf("no --plan: %d", code)
	}
}

func TestApplyDeletionNeedsAllowDelete(t *testing.T) {
	f := newFake()
	f.plan = samplePlan(true)
	socket := f.serve(t)
	code, out, errOut := config(t, socket, "y\n", "apply", "--plan", "abc")
	if code != cli.ExitError || !strings.Contains(errOut, "add --allow-delete") || !strings.Contains(out, "keeps data volume fast/old-data") {
		t.Errorf("without: %d %s\n%s", code, errOut, out)
	}
	if code, _, errOut := config(t, socket, "", "apply", "--plan", "abc", "--allow-delete", "--yes"); code != cli.ExitOK {
		t.Errorf("with: %d %s", code, errOut)
	}
	if got := f.requests[len(f.requests)-1]; !strings.Contains(got, `"allowDelete":true`) {
		t.Errorf("request %s", got)
	}
}

func TestHistoryShowPinAdopt(t *testing.T) {
	f := newFake()
	socket := f.serve(t)
	code, out, _ := config(t, socket, "", "history", "--limit", "5")
	if code != cli.ExitOK || !strings.Contains(out, "20261125T120000Z-abcdef  plan  SUCCEEDED  0123456789ab  2      uid:1000") {
		t.Errorf("history: %d\n%s", code, out)
	}
	code, out, _ = config(t, socket, "", "show", "20261125T120000Z-abcdef")
	if code != cli.ExitOK || !strings.Contains(out, "Deployment 20261125T120000Z-abcdef (plan): ") || !strings.Contains(out, "Approved:   uid:1000") {
		t.Errorf("show: %d\n%s", code, out)
	}
	if code, _, errOut := config(t, socket, "", "show", "nope"); code != cli.ExitError || !strings.Contains(errOut, "no such deployment") {
		t.Errorf("show unknown: %d %s", code, errOut)
	}
	if code, _, _ := config(t, socket, "", "pin", "0123456"); code != cli.ExitOK {
		t.Errorf("pin: %d", code)
	}
	if code, _, _ := config(t, socket, "", "adopt", "web-01"); code != cli.ExitOK {
		t.Errorf("adopt: %d", code)
	}
	if code, _, _ := config(t, socket, "", "unpin"); code != cli.ExitOK {
		t.Errorf("unpin: %d", code)
	}
	reqs := strings.Join(f.requests, "\n")
	for _, want := range []string{"GET /api/v1/deployments?limit=5", `POST /api/v1/config/pin {"commit":"0123456"}`, "POST /api/v1/config/adopt/web-01", "POST /api/v1/config/unpin"} {
		if !strings.Contains(reqs, want) {
			t.Errorf("requests lack %q:\n%s", want, reqs)
		}
	}
	if code, _, _ := config(t, socket, "", "pin"); code != cli.ExitUsage {
		t.Errorf("pin without commit: %d", code)
	}
}

func TestFailedSync(t *testing.T) {
	f := newFake()
	f.status = controller.Status{Repository: "r", Branch: "main", Error: "refused: commit 0123 not signed by a trusted key"}
	f.plan = nil
	socket := f.serve(t)
	code, out, _ := config(t, socket, "", "sync")
	if code != cli.ExitError || !strings.Contains(out, "Error:        refused") || !strings.Contains(out, "Plan:         (none)") {
		t.Errorf("sync: %d\n%s", code, out)
	}
	code, _, errOut := config(t, socket, "", "plan")
	if code != cli.ExitError || !strings.Contains(errOut, "no current plan") {
		t.Errorf("plan: %d %s", code, errOut)
	}
}

func TestUsageAndConnection(t *testing.T) {
	if code, _, _ := config(t, "/nonexistent", "", "frobnicate"); code != cli.ExitUsage {
		t.Errorf("unknown command: %d", code)
	}
	code, _, errOut := config(t, filepath.Join(t.TempDir(), "missing.sock"), "", "status")
	if code != cli.ExitError || !strings.Contains(errOut, "the Strata controller is not running on this host") {
		t.Errorf("missing socket: %d %s", code, errOut)
	}
}

func TestItems(t *testing.T) {
	if got := items(1) + ", " + items(0) + ", " + items(3); got != "1 item, 0 items, 3 items" {
		t.Error(got)
	}
}

func TestStatusPlanSummary(t *testing.T) {
	for want, plan := range map[string]controller.PlanSummary{
		"Plan:         p: nothing to do\n":                                        {ID: "p", Converged: true},
		"Plan:         p: 1 item, none can be applied (see strata config plan)\n": {ID: "p", Items: 1, Unreconcilable: 1, Converged: true},
		"Plan:         p: 2 items (restarts VMs, 1 cannot be applied)\n":          {ID: "p", Items: 2, Unreconcilable: 1, Restarts: true},
		"Plan:         p: 1 item (safe)\n":                                        {ID: "p", Items: 1, Safe: true},
	} {
		var b strings.Builder
		printStatus(&b, controller.Status{Plan: &plan})
		if !strings.Contains(b.String(), want) {
			t.Errorf("want %q in:\n%s", want, b.String())
		}
	}
}
