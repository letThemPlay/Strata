// Package vmcli implements `strata vm`: VM lifecycle through the agent's
// local API (ADR-0020, ADR-0021).
package vmcli

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
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/letthemplay/Strata/internal/agent/api"
	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/workload"
)

// DefaultSocket is the agent's local API socket.
const DefaultSocket = "/run/strata/agent.sock"

// Client talks to the agent's local API.
type Client struct {
	Socket string
	// PollInterval and Timeout bound waiting for operations.
	PollInterval time.Duration
	Timeout      time.Duration
	http         *http.Client
}

func (c *Client) init() {
	if c.http != nil {
		return
	}
	if c.PollInterval == 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.Timeout == 0 {
		c.Timeout = 20 * time.Minute
	}
	socket := c.Socket
	c.http = &http.Client{
		Timeout: time.Minute,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}

// APIError is an error reply of the agent.
type APIError struct {
	Status int
	api.ErrorResponse
}

func (e *APIError) Error() string { return e.ErrorResponse.Error }

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	c.init()
	req, err := http.NewRequestWithContext(ctx, method, "http://strata-agent"+path, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.connectError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	if resp.StatusCode >= 400 {
		e := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(data, &e.ErrorResponse) != nil || e.ErrorResponse.Error == "" {
			e.ErrorResponse.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return e
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("malformed reply: %w", err)
		}
	}
	return nil
}

func (c *Client) connectError(err error) error {
	switch {
	case errors.Is(err, syscall.EACCES):
		return fmt.Errorf("cannot connect to %s: permission denied (members of the %s group may use it; "+
			"log in again after being added)", c.Socket, "strata-operators")
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("cannot connect to %s: the Strata agent's API is not running on this host "+
			"(strata.agent.enable and strata.compute.enable)", c.Socket)
	}
	return fmt.Errorf("cannot connect to %s: %w", c.Socket, err)
}

// ListVMs lists the VMs.
func (c *Client) ListVMs(ctx context.Context) ([]workload.VMStatus, error) {
	var out []workload.VMStatus
	return out, c.do(ctx, http.MethodGet, "/api/v1/vms", nil, &out)
}

// GetVM returns a VM.
func (c *Client) GetVM(ctx context.Context, name string) (workload.VMStatus, error) {
	var out workload.VMStatus
	return out, c.do(ctx, http.MethodGet, "/api/v1/vms/"+url.PathEscape(name), nil, &out)
}

// CreateVM starts creating a VM from a spec and returns the operation.
func (c *Client) CreateVM(ctx context.Context, spec io.Reader) (api.Operation, error) {
	var op api.Operation
	return op, c.do(ctx, http.MethodPost, "/api/v1/vms", spec, &op)
}

// Action starts, stops or restarts a VM and returns the operation.
// timeout < 0 uses the agent's default.
func (c *Client) Action(ctx context.Context, name, action string, force bool, timeout int) (api.Operation, error) {
	q := url.Values{}
	if force {
		q.Set("force", "true")
	}
	if timeout >= 0 {
		q.Set("timeout", strconv.Itoa(timeout))
	}
	var op api.Operation
	path := "/api/v1/vms/" + url.PathEscape(name) + "/" + action
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return op, c.do(ctx, http.MethodPost, path, nil, &op)
}

// Wait polls an operation until it has finished.
func (c *Client) Wait(ctx context.Context, op api.Operation) (api.Operation, error) {
	c.init()
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	for op.Status != api.StatusSucceeded && op.Status != api.StatusFailed {
		select {
		case <-ctx.Done():
			return op, fmt.Errorf("operation %s still %s: %w", op.ID, op.Status, ctx.Err())
		case <-time.After(c.PollInterval):
		}
		if err := c.do(ctx, http.MethodGet, "/api/v1/operations/"+op.ID, nil, &op); err != nil {
			return op, err
		}
	}
	return op, nil
}

// Subcommand returns `strata vm`.
func Subcommand() cli.Subcommand {
	return cli.Subcommand{
		Name:    "vm",
		Usage:   "list|show|create|start|stop|restart ...",
		Summary: "Manage virtual machines on this host",
		Run:     run,
	}
}

const usage = `Usage: strata vm COMMAND [--socket PATH] ...

  list [--json]                       List VMs
  show NAME [--json]                  Show a VM's spec and status
  create -f FILE                      Create a VM from a spec (vm.strata.dev/v1alpha1)
  start NAME                          Start a VM
  stop NAME [--force] [--timeout S]   Stop a VM (graceful; forced after S seconds with --force)
  restart NAME [--timeout S]          Restart a VM
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = fmt.Fprint(stderr, usage)
		if len(args) == 0 {
			return cli.ExitUsage
		}
		return cli.ExitOK
	}
	cmd, args := args[0], args[1:]
	fset := flag.NewFlagSet("vm "+cmd, flag.ContinueOnError)
	fset.SetOutput(stderr)
	socket := fset.String("socket", envOr("STRATA_AGENT_SOCKET", DefaultSocket), "agent API socket")
	asJSON := fset.Bool("json", false, "print JSON")
	file := fset.String("f", "", "VM spec file (- for standard input)")
	force := fset.Bool("force", false, "force stop if the guest does not shut down in time")
	timeout := fset.Int("timeout", -1, "seconds to wait for a graceful stop (default 60)")
	// Flags may come before or after the VM name.
	var positional []string
	for {
		if err := fset.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return cli.ExitOK
			}
			return cli.ExitUsage
		}
		if fset.NArg() == 0 {
			break
		}
		positional = append(positional, fset.Arg(0))
		args = fset.Args()[1:]
	}
	c := &Client{Socket: *socket}
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "strata vm %s: %v\n", cmd, err)
		return cli.ExitError
	}
	needName := func() (string, bool) {
		if len(positional) != 1 {
			_, _ = fmt.Fprintf(stderr, "strata vm %s: expected one VM name\n", cmd)
			return "", false
		}
		return positional[0], true
	}
	finish := func(op api.Operation, err error) int {
		if err != nil {
			return fail(err)
		}
		op, err = c.Wait(ctx, op)
		if err != nil {
			return fail(err)
		}
		if op.Status != api.StatusSucceeded {
			return fail(fmt.Errorf("%s %s: %s (operation %s)", op.Type, op.Resource, op.Error, op.ID))
		}
		_, _ = fmt.Fprintf(stdout, "%s %s: done (operation %s)\n", op.Type, strings.TrimPrefix(op.Resource, "vms/"), op.ID)
		return cli.ExitOK
	}

	switch cmd {
	case "list":
		vms, err := c.ListVMs(ctx)
		if err != nil {
			return fail(err)
		}
		if *asJSON {
			return printJSON(stdout, vms)
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAME\tSTATUS\tCPU\tMEMORY\tADDRESSES")
		for _, v := range vms {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", v.Spec.Name, v.Status, v.Spec.Resources.CPU,
				v.Spec.Resources.Memory, strings.Join(v.Addresses, ","))
		}
		_ = tw.Flush()
		return cli.ExitOK
	case "show":
		name, ok := needName()
		if !ok {
			return cli.ExitUsage
		}
		vm, err := c.GetVM(ctx, name)
		if err != nil {
			return fail(err)
		}
		if *asJSON {
			return printJSON(stdout, vm)
		}
		show(stdout, vm)
		return cli.ExitOK
	case "create":
		if *file == "" || len(positional) != 0 {
			_, _ = fmt.Fprintln(stderr, "strata vm create: -f FILE is required")
			return cli.ExitUsage
		}
		spec, err := readSpec(*file)
		if err != nil {
			return fail(err)
		}
		// Validate locally first for clear messages; the agent validates again.
		if _, err := workload.ParseVM(strings.NewReader(spec)); err != nil {
			return fail(err)
		}
		return finish(c.CreateVM(ctx, strings.NewReader(spec)))
	case workload.ActionStart, workload.ActionStop, workload.ActionRestart:
		name, ok := needName()
		if !ok {
			return cli.ExitUsage
		}
		return finish(c.Action(ctx, name, cmd, *force, *timeout))
	default:
		_, _ = fmt.Fprintf(stderr, "strata vm: unknown command %q\n\n%s", cmd, usage)
		return cli.ExitUsage
	}
}

func readSpec(file string) (string, error) {
	var data []byte
	var err error
	if file == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	} else {
		data, err = os.ReadFile(file) //nolint:gosec // The user's own spec file.
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", file, err)
	}
	return string(data), nil
}

func show(w io.Writer, v workload.VMStatus) {
	s := v.Spec
	_, _ = fmt.Fprintf(w, "Name:      %s\nStatus:    %s\nCPU:       %d\nMemory:    %s\n", s.Name, v.Status, s.Resources.CPU, s.Resources.Memory)
	image := s.Image
	if image == "" {
		image = "(none: empty VM)"
	}
	_, _ = fmt.Fprintf(w, "Image:     %s\nAutostart: %v\nDisks:\n", image, s.Autostart)
	for _, d := range s.Disks {
		_, _ = fmt.Fprintf(w, "  %-10s %8s  pool %s\n", d.Name, d.Size, d.Pool)
	}
	if len(s.Networks) > 0 {
		_, _ = fmt.Fprintln(w, "Networks:")
		for _, n := range s.Networks {
			_, _ = fmt.Fprintf(w, "  %-10s network %s\n", n.Name, n.Network)
		}
	}
	if len(v.Addresses) > 0 {
		_, _ = fmt.Fprintf(w, "Addresses: %s\n", strings.Join(v.Addresses, ", "))
	}
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
