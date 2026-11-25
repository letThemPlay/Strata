// Package configcli implements `strata config`: the GitOps controller's
// status and plans, through its local API (ADR-0025).
package configcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/letthemplay/Strata/internal/agent/api"
	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/controller"
)

// DefaultSocket is the controller's API socket.
const DefaultSocket = "/run/strata/controller.sock"

// Subcommand returns `strata config`.
func Subcommand() cli.Subcommand {
	return cli.Subcommand{
		Name:    "config",
		Usage:   "status|sync|plan [--json]",
		Summary: "Declarative configuration from Git: status and plans",
		Run:     run,
	}
}

const usage = `usage: strata config COMMAND [--json]

Commands:
  status   The repository, the evaluated commit and the current plan
  sync     Fetch, verify, evaluate and plan now; then show the plan
  plan     Show the current plan
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return cli.ExitUsage
	}
	cmd := args[0]
	fset := flag.NewFlagSet("config "+cmd, flag.ContinueOnError)
	fset.SetOutput(stderr)
	asJSON := fset.Bool("json", false, "print JSON")
	socket := fset.String("socket", envOr("STRATA_CONTROLLER_SOCKET", DefaultSocket), "controller API socket")
	if err := fset.Parse(args[1:]); err != nil || fset.NArg() != 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return cli.ExitUsage
	}
	c := newClient(*socket)
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "strata config %s: %v\n", cmd, err)
		return cli.ExitError
	}
	switch cmd {
	case "status", "sync":
		var st controller.Status
		method, path := http.MethodGet, "/api/v1/config"
		if cmd == "sync" {
			method, path = http.MethodPost, "/api/v1/config/sync"
		}
		if err := c.do(ctx, method, path, &st); err != nil {
			return fail(err)
		}
		if *asJSON {
			return printJSON(stdout, st)
		}
		printStatus(stdout, st)
		if cmd == "sync" && st.Plan != nil {
			var p controller.CurrentPlan
			if err := c.do(ctx, http.MethodGet, "/api/v1/config/plan", &p); err == nil {
				_, _ = fmt.Fprintln(stdout)
				printPlan(stdout, p)
			}
		}
		if st.Error != "" {
			return cli.ExitError
		}
		return cli.ExitOK
	case "plan":
		var p controller.CurrentPlan
		if err := c.do(ctx, http.MethodGet, "/api/v1/config/plan", &p); err != nil {
			return fail(err)
		}
		if *asJSON {
			return printJSON(stdout, p)
		}
		printPlan(stdout, p)
		return cli.ExitOK
	}
	_, _ = fmt.Fprintf(stderr, "unknown command %q\n%s", cmd, usage)
	return cli.ExitUsage
}

func printStatus(w io.Writer, s controller.Status) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("Repository:  %s (branch %s)\n", s.Repository, s.Branch)
	p("Node:        %s\n", s.Node)
	p("Head:        %s\n", orNone(s.Head))
	if s.Evaluated != "" {
		p("Evaluated:   %s (signed by %s)\n", s.Evaluated, s.Signer)
	} else {
		p("Evaluated:   (none)\n")
	}
	p("Last applied: %s\n", orNone(s.LastApplied))
	p("Last sync:   %s\n", orNone(s.LastSync))
	if s.Error != "" {
		p("Error:       %s\n", s.Error)
	}
	switch {
	case s.Plan == nil:
		p("Plan:        (none)\n")
	case s.Plan.Items == 0:
		p("Plan:        %s: in sync, nothing to do\n", s.Plan.ID)
	default:
		var notes []string
		if s.Plan.Restarts {
			notes = append(notes, "restarts VMs")
		}
		if s.Plan.Deletes {
			notes = append(notes, "deletes VMs")
		}
		if s.Plan.Unreconcilable > 0 {
			notes = append(notes, fmt.Sprintf("%d cannot be applied", s.Plan.Unreconcilable))
		}
		if len(notes) == 0 {
			notes = append(notes, "safe")
		}
		p("Plan:        %s: %d items (%s)\n", s.Plan.ID, s.Plan.Items, strings.Join(notes, ", "))
	}
	if len(s.Drift) > 0 {
		p("Drift:       %s (changed outside Strata)\n", strings.Join(s.Drift, ", "))
	}
}

func printPlan(w io.Writer, cp controller.CurrentPlan) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	plan := cp.Plan
	p("Plan %s for node %s, commit %s (signed by %s):\n", plan.ID, plan.Node, short(cp.Commit), cp.Signer)
	if len(plan.Items) == 0 {
		p("  nothing to do: the node matches the configuration\n")
	}
	for _, it := range plan.Items {
		var notes []string
		if it.Restarts {
			notes = append(notes, "restarts the VM")
		}
		if it.Deletes {
			notes = append(notes, "deletes the VM and its root disk")
		}
		if it.Protect != nil {
			notes = append(notes, fmt.Sprintf("protected: %v", *it.Protect))
		}
		p("  %-15s %s", it.Action, it.Resource)
		if len(notes) > 0 {
			p("  (%s)", strings.Join(notes, "; "))
		}
		p("\n")
		for _, c := range it.Changes {
			line := c.Field
			if c.From != "" || c.To != "" {
				line += fmt.Sprintf(": %s -> %s", orNone(c.From), orNone(c.To))
			}
			if c.Note != "" {
				line += " (" + c.Note + ")"
			}
			p("      %s\n", line)
		}
		for _, d := range it.Drift {
			p("      restore %s\n", d)
		}
		for _, v := range it.KeptVolumes {
			p("      keeps data volume %s\n", v)
		}
		for _, r := range it.Reasons {
			p("      cannot apply: %s\n", r)
		}
	}
	if len(plan.InSync) > 0 {
		p("In sync: %s\n", strings.Join(plan.InSync, ", "))
	}
	if len(plan.Unmanaged) > 0 {
		p("Not managed through Git (created with strata vm): %s\n", strings.Join(plan.Unmanaged, ", "))
	}
	if len(plan.Items) > 0 && !plan.Converged() {
		p("Applying plans is not available yet (M3 slice 3b).\n")
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func printJSON(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return cli.ExitError
	}
	return cli.ExitOK
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// client talks to the controller's API.
type client struct {
	socket string
	http   *http.Client
}

func newClient(socket string) *client {
	return &client{socket: socket, http: &http.Client{
		// A synchronization evaluates Nix and can take minutes.
		Timeout: 15 * time.Minute,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}}
}

func (c *client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://strata-controller"+path, nil)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, syscall.EACCES):
			return fmt.Errorf("cannot connect to %s: permission denied (members of the strata-operators group may use it)", c.socket)
		case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
			return fmt.Errorf("cannot connect to %s: the Strata controller is not running on this host (strata.gitops.enable)", c.socket)
		}
		return fmt.Errorf("cannot connect to %s: %w", c.socket, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	if resp.StatusCode >= 400 {
		var e api.ErrorResponse
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return errors.New(e.Error)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("malformed reply: %w", err)
	}
	return nil
}
