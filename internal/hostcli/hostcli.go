// Package hostcli implements `strata host`: the host's configuration
// status and its evaluation, through the controller's local API
// (ADR-0027, ADR-0028). Strata evaluates host configurations; it does not
// deploy them yet (M4).
package hostcli

import (
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
	"strings"
	"syscall"
	"time"

	"github.com/letthemplay/Strata/internal/agent/api"
	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/controller"
)

// DefaultSocket is the controller's API socket.
const DefaultSocket = "/run/strata/controller.sock"

// Subcommand returns `strata host`.
func Subcommand() cli.Subcommand {
	return cli.Subcommand{
		Name:    "host",
		Usage:   "status|evaluate ...",
		Summary: "The host's NixOS configuration: evaluation, drift and boot state (no deployment yet)",
		Run:     run,
	}
}

const usage = `usage: strata host COMMAND [arguments]

Commands:
  status                    The host's configuration: the latest evaluation,
                            the desired and running systems, configuration
                            drift and boot state
  evaluate [--no-wait]      Evaluate the NixOS system of the commit the
                            controller targets (the branch head, or the pinned
                            commit). Builds and activates nothing.

Options: --json (status, evaluate), --node NODE, --socket PATH.
Strata can evaluate host configurations but does not deploy them yet.
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return cli.ExitUsage
	}
	cmd := args[0]
	fset := flag.NewFlagSet("host "+cmd, flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	asJSON := fset.Bool("json", false, "print JSON")
	socket := fset.String("socket", envOr("STRATA_CONTROLLER_SOCKET", DefaultSocket), "controller API socket")
	node := fset.String("node", "", "the node (default: the controller's only node)")
	noWait := fset.Bool("no-wait", false, "do not wait for the evaluation to finish")
	if err := fset.Parse(args[1:]); err != nil {
		_, _ = fmt.Fprintf(stderr, "strata host %s: %v\n%s", cmd, err, usage)
		return cli.ExitUsage
	}
	if fset.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "strata host %s: unexpected arguments\n%s", cmd, usage)
		return cli.ExitUsage
	}
	c := newClient(*socket)
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "strata host %s: %v\n", cmd, err)
		return cli.ExitError
	}
	switch cmd {
	case "status", "evaluate":
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n%s", cmd, usage)
		return cli.ExitUsage
	}
	name, err := c.node(ctx, *node)
	if err != nil {
		return fail(err)
	}
	path := "/api/v1/nodes/" + url.PathEscape(name) + "/configuration"
	var hc controller.HostConfiguration
	switch cmd {
	case "status":
		if err := c.do(ctx, http.MethodGet, path, &hc); err != nil {
			return fail(err)
		}
	case "evaluate":
		var ev controller.HostEvaluation
		if err := c.do(ctx, http.MethodPost, path+"/evaluate", &ev); err != nil {
			return fail(err)
		}
		if *noWait {
			if *asJSON {
				return printJSON(stdout, ev)
			}
			_, _ = fmt.Fprintf(stdout, "Evaluation %s of %s started; see strata host status.\n", ev.ID, short(ev.Source.Commit))
			return cli.ExitOK
		}
		if !*asJSON {
			_, _ = fmt.Fprintf(stdout, "Evaluating %s (evaluation %s)...\n", short(ev.Source.Commit), ev.ID)
		}
		if hc, err = c.wait(ctx, path, ev.ID); err != nil {
			return fail(err)
		}
	}
	if *asJSON {
		if code := printJSON(stdout, hc); code != cli.ExitOK {
			return code
		}
	} else {
		printStatus(stdout, hc)
	}
	if cmd == "evaluate" && (hc.Evaluation == nil || hc.Evaluation.State != controller.HostEvalSucceeded) {
		return cli.ExitError
	}
	return cli.ExitOK
}

// printStatus prints a host configuration status. Drift and boot state are
// separate: one compares Git with the running system, the other the
// running system with the booted one.
func printStatus(w io.Writer, hc controller.HostConfiguration) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("Node:                %s\n", hc.Node)
	p("Repository:          %s (branch %s)\n", hc.Repository, hc.Branch)
	switch {
	case hc.Target == "":
		p("Target commit:       (none: nothing fetched yet; strata config sync)\n")
	case hc.Pinned:
		p("Target commit:       %s (pinned)\n", hc.Target)
	default:
		p("Target commit:       %s\n", hc.Target)
	}
	if ev := hc.Evaluation; ev == nil {
		p("Evaluation:          (none; strata host evaluate)\n")
	} else {
		switch ev.State {
		case controller.HostEvalRunning:
			p("Evaluation:          %s, of %s: running since %s\n", ev.ID, short(ev.Source.Commit), ev.StartedAt.Format(time.RFC3339))
		case controller.HostEvalSucceeded:
			p("Evaluation:          %s, of %s: succeeded in %.1f s (%s)\n", ev.ID, short(ev.Source.Commit), ev.DurationSeconds, ev.CompletedAt.Format(time.RFC3339))
		default:
			p("Evaluation:          %s, of %s: %s after %.1f s\n", ev.ID, short(ev.Source.Commit), ev.State, ev.DurationSeconds)
			p("  Error:             %s\n", indent(ev.Error, "                     "))
		}
	}
	if d := hc.Desired; d != nil {
		p("Desired system:      %s\n", d.OutPath)
		p("  Commit:            %s (signed by %s)\n", d.Source.Commit, orNone(d.Source.Signer))
		p("  Tree, flake.lock:  %s, %s\n", d.Source.Tree, orNone(d.Source.FlakeLock))
		p("  Derivation:        %s\n", d.DrvPath)
	} else {
		p("Desired system:      (unknown: the target commit has no successful evaluation)\n")
	}
	p("Running system:      %s\n", observed(hc.Observed.Current, hc.Observed.CurrentError))
	p("Booted system:       %s\n", observed(hc.Observed.Booted, hc.Observed.BootedError))
	p("\n")
	p("Configuration drift: %s\n", withReason(hc.ConfigurationDrift, hc.DriftReason))
	p("Boot state:          %s\n", withReason(hc.BootState, hc.BootReason))
	p("\nNote: %s\n", hc.Note)
}

func observed(path, err string) string {
	if path == "" {
		return "(unknown: " + err + ")"
	}
	return path
}

func withReason(state, reason string) string {
	if reason == "" {
		return state
	}
	return state + ": " + reason
}

func indent(s, prefix string) string { return strings.ReplaceAll(s, "\n", "\n"+prefix) }

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
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
		Timeout: time.Minute,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}}
}

// node returns name, or the controller's only node.
func (c *client) node(ctx context.Context, name string) (string, error) {
	if name != "" {
		return name, nil
	}
	var nodes []controller.NodeInfo
	if err := c.do(ctx, http.MethodGet, "/api/v1/nodes", &nodes); err != nil {
		return "", err
	}
	if len(nodes) != 1 {
		return "", fmt.Errorf("the controller manages %d nodes: choose one with --node", len(nodes))
	}
	return nodes[0].ID, nil
}

// wait polls the configuration until evaluation id has finished.
func (c *client) wait(ctx context.Context, path, id string) (controller.HostConfiguration, error) {
	for {
		var hc controller.HostConfiguration
		if err := c.do(ctx, http.MethodGet, path, &hc); err != nil {
			return hc, err
		}
		switch ev := hc.Evaluation; {
		case ev == nil || ev.ID != id:
			return hc, fmt.Errorf("evaluation %s is no longer the latest one", id)
		case ev.State != controller.HostEvalRunning:
			return hc, nil
		}
		select {
		case <-ctx.Done():
			return hc, fmt.Errorf("waiting for evaluation %s: %w", id, ctx.Err())
		case <-time.After(c.poll):
		}
	}
}

func (c *client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://strata-controller"+path, http.NoBody)
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
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
