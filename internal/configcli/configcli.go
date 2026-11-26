// Package configcli implements `strata config`: the GitOps controller's
// status, plans, approvals, history, pinning and adoption, through its
// local API (ADR-0025, ADR-0026).
package configcli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/letthemplay/Strata/internal/agent/api"
	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/controller"
)

// DefaultSocket is the controller's API socket.
const DefaultSocket = "/run/strata/controller.sock"

// Subcommand returns `strata config`; confirmations are read from stdin.
func Subcommand(stdin io.Reader) cli.Subcommand {
	return cli.Subcommand{
		Name:    "config",
		Usage:   "status|sync|plan|apply|history|show|pin|unpin|adopt ...",
		Summary: "Declarative configuration from Git: plans, approvals and history",
		Run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			return run(ctx, args, stdin, stdout, stderr)
		},
	}
}

const usage = `usage: strata config COMMAND [arguments]

Commands:
  status                    The repository, the evaluated commit, the current plan
  sync                      Fetch, verify, evaluate and plan now; show the plan
  plan                      Show the current plan
  apply --plan ID [--allow-delete] [--yes]
                            Approve and apply the current plan (deleting VMs
                            needs --allow-delete; data volumes are kept)
  history [--limit N]       Deployments and operator actions, newest first
  show DEPLOYMENT           One deployment record
  pin COMMIT                Evaluate this signed commit instead of the branch
                            (roll back without changing the repository)
  unpin                     Follow the branch again
  adopt NAME                Manage a VM created with strata vm through Git

Options: --json (status, plan, history, show), --socket PATH.
`

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return cli.ExitUsage
	}
	cmd := args[0]
	fset := flag.NewFlagSet("config "+cmd, flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	asJSON := fset.Bool("json", false, "print JSON")
	socket := fset.String("socket", envOr("STRATA_CONTROLLER_SOCKET", DefaultSocket), "controller API socket")
	planID := fset.String("plan", "", "the plan ID to approve")
	allowDelete := fset.Bool("allow-delete", false, "approve a plan that deletes VMs")
	yes := fset.Bool("yes", false, "do not ask for confirmation")
	limit := fset.Int("limit", 20, "how many records")
	// Flags may come before or after the arguments.
	var positional []string
	rest := args[1:]
	for {
		if err := fset.Parse(rest); err != nil {
			_, _ = fmt.Fprintf(stderr, "strata config %s: %v\n%s", cmd, err, usage)
			return cli.ExitUsage
		}
		if fset.NArg() == 0 {
			break
		}
		positional = append(positional, fset.Arg(0))
		rest = fset.Args()[1:]
	}
	wantArgs := map[string]int{"show": 1, "pin": 1, "adopt": 1}[cmd]
	if len(positional) != wantArgs {
		_, _ = fmt.Fprintf(stderr, "strata config %s: wrong number of arguments\n%s", cmd, usage)
		return cli.ExitUsage
	}
	c := newClient(*socket)
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "strata config %s: %v\n", cmd, err)
		return cli.ExitError
	}
	out := func(v any, text func()) int {
		if *asJSON {
			return printJSON(stdout, v)
		}
		text()
		return cli.ExitOK
	}

	switch cmd {
	case "status", "sync":
		var st controller.Status
		method, path := http.MethodGet, "/api/v1/config"
		if cmd == "sync" {
			method, path = http.MethodPost, "/api/v1/config/sync"
		}
		if err := c.do(ctx, method, path, nil, &st); err != nil {
			return fail(err)
		}
		code := out(st, func() {
			printStatus(stdout, st)
			if cmd == "sync" && st.Plan != nil {
				var p controller.CurrentPlan
				if err := c.do(ctx, http.MethodGet, "/api/v1/config/plan", nil, &p); err == nil {
					_, _ = fmt.Fprintln(stdout)
					printPlan(stdout, p, true)
				}
			}
		})
		if st.Error != "" {
			return cli.ExitError
		}
		return code
	case "plan":
		var p controller.CurrentPlan
		if err := c.do(ctx, http.MethodGet, "/api/v1/config/plan", nil, &p); err != nil {
			return fail(err)
		}
		return out(p, func() { printPlan(stdout, p, true) })
	case "apply":
		return apply(ctx, c, *planID, *allowDelete, *yes, stdin, stdout, stderr)
	case "history":
		var h []controller.DeploymentSummary
		if err := c.do(ctx, http.MethodGet, "/api/v1/deployments?limit="+strconv.Itoa(*limit), nil, &h); err != nil {
			return fail(err)
		}
		return out(h, func() { printHistory(stdout, h) })
	case "show":
		var d controller.Deployment
		if err := c.do(ctx, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(positional[0]), nil, &d); err != nil {
			return fail(err)
		}
		return out(d, func() { printDeployment(stdout, d) })
	case "pin", "unpin", "adopt":
		var st controller.Status
		var err error
		switch cmd {
		case "pin":
			err = c.do(ctx, http.MethodPost, "/api/v1/config/pin", map[string]string{"commit": positional[0]}, &st)
		case "unpin":
			err = c.do(ctx, http.MethodPost, "/api/v1/config/unpin", nil, &st)
		case "adopt":
			err = c.do(ctx, http.MethodPost, "/api/v1/config/adopt/"+url.PathEscape(positional[0]), nil, &st)
		}
		if err != nil {
			return fail(err)
		}
		return out(st, func() {
			printStatus(stdout, st)
			if st.Plan != nil && st.Plan.Items > 0 {
				_, _ = fmt.Fprintln(stdout, "\nSee the plan with `strata config plan`.")
			}
		})
	}
	_, _ = fmt.Fprintf(stderr, "unknown command %q\n%s", cmd, usage)
	return cli.ExitUsage
}

// apply shows the plan, asks for confirmation, approves it and follows
// the deployment to its end.
func apply(ctx context.Context, c *client, planID string, allowDelete, yes bool, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "strata config apply: %v\n", err)
		return cli.ExitError
	}
	if planID == "" {
		_, _ = fmt.Fprint(stderr, "strata config apply: --plan ID is required (see strata config plan)\n")
		return cli.ExitUsage
	}
	var p controller.CurrentPlan
	if err := c.do(ctx, http.MethodGet, "/api/v1/config/plan", nil, &p); err != nil {
		return fail(err)
	}
	if p.Plan.ID != planID {
		return fail(fmt.Errorf("plan %s is not the current plan (it is %s); review it with strata config plan", planID, p.Plan.ID))
	}
	printPlan(stdout, p, false)
	if p.Plan.Deletes() && !allowDelete {
		return fail(errors.New("the plan deletes VMs (their root disks; data volumes are kept): add --allow-delete to approve it"))
	}
	if !yes {
		_, _ = fmt.Fprint(stdout, "Apply this plan? [y/N] ")
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			_, _ = fmt.Fprintln(stdout, "Not applied.")
			return cli.ExitError
		}
	}
	var d controller.Deployment
	if err := c.do(ctx, http.MethodPost, "/api/v1/config/apply", map[string]any{"planId": planID, "allowDelete": allowDelete}, &d); err != nil {
		return fail(err)
	}
	_, _ = fmt.Fprintf(stdout, "Deployment %s: %s\n", d.ID, d.State)
	last := d.State
	for !d.Finished() {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(c.poll):
		}
		if err := c.do(ctx, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(d.ID), nil, &d); err != nil {
			return fail(err)
		}
		if d.State != last {
			_, _ = fmt.Fprintf(stdout, "  %s\n", d.State)
			last = d.State
		}
	}
	_, _ = fmt.Fprintln(stdout)
	printDeployment(stdout, d)
	if d.State != controller.StateSucceeded {
		return cli.ExitError
	}
	return cli.ExitOK
}

func printStatus(w io.Writer, s controller.Status) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("Repository:   %s (branch %s)\n", s.Repository, s.Branch)
	p("Node:         %s\n", s.Node)
	p("Head:         %s\n", orNone(s.Head))
	if s.Pinned != "" {
		p("Pinned:       %s (the branch is not followed; strata config unpin)\n", s.Pinned)
	}
	if s.Evaluated != "" {
		p("Evaluated:    %s (signed by %s)\n", s.Evaluated, s.Signer)
	} else {
		p("Evaluated:    (none)\n")
	}
	p("Last applied: %s\n", orNone(s.LastApplied))
	p("Last sync:    %s\n", orNone(s.LastSync))
	p("Auto-apply:   %s\n", s.AutoApply)
	if s.Error != "" {
		p("Error:        %s\n", s.Error)
	}
	switch {
	case s.Plan == nil:
		p("Plan:         (none)\n")
	case s.Plan.Items == 0:
		p("Plan:         %s: nothing to do\n", s.Plan.ID)
	case s.Plan.Converged:
		// Only items that cannot be applied are left: nothing to approve.
		p("Plan:         %s: %s, none can be applied (see strata config plan)\n", s.Plan.ID, items(s.Plan.Items))
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
		p("Plan:         %s: %s (%s)\n", s.Plan.ID, items(s.Plan.Items), strings.Join(notes, ", "))
	}
	if len(s.Drift) > 0 {
		p("Drift:        %s (changed outside Strata)\n", strings.Join(s.Drift, ", "))
	}
	if s.Awaiting != nil {
		p("Awaiting:     deployment %s, approve with: strata config apply --plan %s\n", s.Awaiting.ID, s.Awaiting.PlanID)
	}
	if s.Running != nil {
		p("Running:      deployment %s (%s)\n", s.Running.ID, s.Running.State)
	}
}

// printPlan prints a plan; hint adds how to approve it.
func printPlan(w io.Writer, cp controller.CurrentPlan, hint bool) {
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
	if hint && !plan.Converged() {
		flag := ""
		if plan.Deletes() {
			flag = " --allow-delete"
		}
		p("Approve with: strata config apply --plan %s%s\n", plan.ID, flag)
	}
}

func printHistory(w io.Writer, h []controller.DeploymentSummary) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKIND\tSTATE\tCOMMIT\tITEMS\tAPPROVED BY\tSUBJECT")
	for _, d := range h {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", d.ID, d.Kind, d.State, short(d.Commit), d.Items, orNone(d.ApprovedBy), d.Subject)
	}
	_ = tw.Flush()
}

func printDeployment(w io.Writer, d controller.Deployment) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("Deployment %s (%s): %s\n", d.ID, d.Kind, d.State)
	p("Repository: %s (branch %s), node %s\n", d.Repository, d.Branch, d.Node)
	if d.Commit != "" {
		pinned := ""
		if d.Pinned {
			pinned = ", pinned"
		}
		p("Commit:     %s (signed by %s%s), flake.lock %s\n", d.Commit, orNone(d.Signer), pinned, orNone(d.FlakeLock))
	}
	p("Strata:     %s", d.StrataVersion)
	if d.IRGenerator != "" {
		p(" (configuration evaluated with %s)", d.IRGenerator)
	}
	p("\n")
	if d.PlanID != "" {
		p("Plan:       %s\n", d.PlanID)
	}
	if d.Subject != "" {
		p("Subject:    %s\n", d.Subject)
	}
	if d.ApprovedBy != "" {
		p("Approved:   %s\n", d.ApprovedBy)
	}
	for _, t := range d.Transitions {
		p("  %s  %s", t.At.Format(time.RFC3339), t.State)
		if t.Note != "" {
			p("  (%s)", t.Note)
		}
		p("\n")
	}
	if d.Result != nil {
		for _, it := range d.Result.Items {
			p("  %-14s %-15s %s", it.Status, it.Action, it.Resource)
			if it.Error != "" {
				p(": %s", it.Error)
			}
			p("\n")
			for _, v := range it.KeptVolumes {
				p("      kept data volume %s\n", v)
			}
		}
	}
	if d.Error != "" {
		p("Error: %s\n", d.Error)
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
	poll   time.Duration
	http   *http.Client
}

func newClient(socket string) *client {
	return &client{socket: socket, poll: time.Second, http: &http.Client{
		// A synchronization evaluates Nix and can take minutes.
		Timeout: 15 * time.Minute,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}}
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://strata-controller"+path, r)
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

// items returns "1 item", "2 items".
func items(n int) string {
	if n == 1 {
		return "1 item"
	}
	return fmt.Sprintf("%d items", n)
}
