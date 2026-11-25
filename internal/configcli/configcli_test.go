package configcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/controller"
	"github.com/letthemplay/Strata/internal/planner"
	"github.com/letthemplay/Strata/internal/workload"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func serve(t *testing.T, st controller.Status, plan *controller.CurrentPlan) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "c.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, _ *http.Request) { reply(w, st) })
	mux.HandleFunc("POST /api/v1/config/sync", func(w http.ResponseWriter, _ *http.Request) { reply(w, st) })
	mux.HandleFunc("GET /api/v1/config/plan", func(w http.ResponseWriter, _ *http.Request) {
		if plan == nil {
			w.WriteHeader(http.StatusNotFound)
			reply(w, map[string]string{"error": "no current plan (the last synchronization failed: refused)"})
			return
		}
		reply(w, plan)
	})
	srv := &http.Server{Handler: mux} //nolint:gosec // Test server.
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socket
}

func config(t *testing.T, socket string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := cli.Command{Name: "strata", Subcommands: []cli.Subcommand{Subcommand()}}
	code := cmd.Run(context.Background(), append(append([]string{"config"}, args...), "--socket", socket), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestStatusAndPlan(t *testing.T) {
	yes := true
	plan := &controller.CurrentPlan{Commit: sha, Signer: "alice", Plan: planner.Plan{
		ID: "abc", Node: "hv-01",
		Items: []planner.Item{
			{Resource: "vm/db", Action: planner.ActionUpdate, Restarts: true, Changes: []workload.Change{{Field: "resources.cpu", From: "2", To: "4", Restart: true}}},
			{Resource: "vm/new", Action: planner.ActionCreate, Protect: &yes},
			{Resource: "vm/web", Action: planner.ActionUnreconcilable, Reasons: []string{"created with `strata vm`"}},
		},
		InSync: []string{"vm/ok"}, Unmanaged: []string{"manual"},
	}}
	st := controller.Status{
		Repository: "ssh://git@h/x.git", Branch: "main", Node: "hv-01", Head: sha, Evaluated: sha, Signer: "alice",
		Plan: &controller.PlanSummary{ID: "abc", Items: 3, Restarts: true, Unreconcilable: 1},
	}
	socket := serve(t, st, plan)

	code, out, _ := config(t, socket, "status")
	for _, want := range []string{"Repository:  ssh://git@h/x.git (branch main)", "Evaluated:   " + sha + " (signed by alice)", "Plan:        abc: 3 items (restarts VMs, 1 cannot be applied)"} {
		if code != cli.ExitOK || !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = config(t, socket, "plan")
	for _, want := range []string{
		"Plan abc for node hv-01, commit 0123456789ab (signed by alice):",
		"update          vm/db  (restarts the VM)", "resources.cpu: 2 -> 4",
		"create          vm/new  (protected: true)",
		"cannot apply: created with `strata vm`",
		"In sync: vm/ok", "Not managed through Git (created with strata vm): manual",
	} {
		if code != cli.ExitOK || !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if code, out, _ = config(t, socket, "sync"); code != cli.ExitOK || !strings.Contains(out, "Plan abc for node") {
		t.Errorf("sync: %d\n%s", code, out)
	}
	if code, out, _ = config(t, socket, "status", "--json"); code != cli.ExitOK || !strings.Contains(out, `"evaluated": "`+sha) {
		t.Errorf("json: %d\n%s", code, out)
	}
}

func TestFailedSync(t *testing.T) {
	socket := serve(t, controller.Status{Repository: "r", Branch: "main", Error: "refused: commit 0123 not signed by a trusted key"}, nil)
	code, out, _ := config(t, socket, "sync")
	if code != cli.ExitError || !strings.Contains(out, "Error:       refused") || !strings.Contains(out, "Plan:        (none)") {
		t.Errorf("sync: %d\n%s", code, out)
	}
	code, _, errOut := config(t, socket, "plan")
	if code != cli.ExitError || !strings.Contains(errOut, "no current plan") {
		t.Errorf("plan: %d %s", code, errOut)
	}
}

func TestUsageAndConnection(t *testing.T) {
	if code, _, _ := config(t, "/nonexistent", "frobnicate"); code != cli.ExitUsage {
		t.Errorf("unknown command: %d", code)
	}
	code, _, errOut := config(t, filepath.Join(t.TempDir(), "missing.sock"), "status")
	if code != cli.ExitError || !strings.Contains(errOut, "the Strata controller is not running on this host") {
		t.Errorf("missing socket: %d %s", code, errOut)
	}
}
