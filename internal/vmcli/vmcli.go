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
	"os"
	"strings"
	"text/tabwriter"

	"github.com/letthemplay/Strata/internal/agent/api"
	"github.com/letthemplay/Strata/internal/agent/client"
	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/workload"
)

// Subcommand returns `strata vm`; confirmations are read from stdin.
func Subcommand(stdin io.Reader) cli.Subcommand {
	return cli.Subcommand{
		Name:    "vm",
		Usage:   "list|show|create|update|start|stop|restart|delete ...",
		Summary: "Manage virtual machines on this host",
		Run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			return run(ctx, args, stdin, stdout, stderr)
		},
	}
}

const usage = `Usage: strata vm COMMAND [--socket PATH] ...

  list [--json]                       List VMs
  show NAME [--json]                  Show a VM's spec and status
  create -f FILE                      Create a VM from a spec (vm.strata.dev/v1alpha1)
  start NAME                          Start a VM
  stop NAME [--force] [--timeout S]   Stop a VM (graceful; forced after S seconds with --force)
  restart NAME [--timeout S]          Restart a VM
  update -f FILE [--yes]              Show the plan for a changed spec and apply it after confirmation
  delete NAME --confirm NAME [--stop] Delete a VM and its root disk; data volumes are kept
`

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
	socket := fset.String("socket", envOr("STRATA_AGENT_SOCKET", client.DefaultSocket), "agent API socket")
	asJSON := fset.Bool("json", false, "print JSON")
	file := fset.String("f", "", "VM spec file (- for standard input)")
	force := fset.Bool("force", false, "force stop if the guest does not shut down in time")
	timeout := fset.Int("timeout", -1, "seconds to wait for a graceful stop (default 60)")
	yes := fset.Bool("yes", false, "apply the plan without asking")
	confirm := fset.String("confirm", "", "the VM's name, to confirm deleting it")
	stop := fset.Bool("stop", false, "stop a running VM before deleting it")
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
	c := &client.Client{Socket: *socket}
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
		_, _ = fmt.Fprintln(tw, "NAME\tSTATUS\tOWNER\tCPU\tMEMORY\tADDRESSES")
		for _, v := range vms {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", v.Spec.Name, v.Status, ownerLabel(v), v.Spec.Resources.CPU,
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
	case "update":
		if *file == "" || len(positional) != 0 {
			_, _ = fmt.Fprintln(stderr, "strata vm update: -f FILE is required")
			return cli.ExitUsage
		}
		spec, err := readSpec(*file)
		if err != nil {
			return fail(err)
		}
		vm, err := workload.ParseVM(strings.NewReader(spec))
		if err != nil {
			return fail(err)
		}
		plan, err := c.Plan(ctx, vm.Name, spec)
		if err != nil {
			return fail(err)
		}
		printPlan(stdout, plan)
		if plan.Empty() {
			return cli.ExitOK
		}
		if !*yes && !ask(stdin, stdout, "Apply? [y/N] ") {
			_, _ = fmt.Fprintln(stdout, "Nothing was changed.")
			return cli.ExitError
		}
		return finish(c.Apply(ctx, vm.Name, spec, plan.ID))
	case "delete":
		name, ok := needName()
		if !ok {
			return cli.ExitUsage
		}
		if *confirm != name {
			_, _ = fmt.Fprintf(stderr, "strata vm delete: deleting %s removes its root disk; confirm with --confirm %s\n", name, name)
			return cli.ExitUsage
		}
		op, err := c.Delete(ctx, name, *stop)
		if code := finish(op, err); code != cli.ExitOK {
			return code
		}
		op, _ = c.Wait(ctx, op)
		var kept api.KeptVolumes
		if data, err := json.Marshal(op.Result); err == nil {
			_ = json.Unmarshal(data, &kept)
		}
		if len(kept.KeptVolumes) > 0 {
			_, _ = fmt.Fprintf(stdout, "Data volumes kept (not deleted): %s\n", strings.Join(kept.KeptVolumes, ", "))
		}
		return cli.ExitOK
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

func printPlan(w io.Writer, p workload.Plan) {
	state := "stopped"
	if p.Running {
		state = "running"
	}
	if p.Empty() {
		_, _ = fmt.Fprintf(w, "No changes for %s.\n", p.VM)
		return
	}
	_, _ = fmt.Fprintf(w, "Plan for %s (%s), plan %s:\n", p.VM, state, p.ID)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, ch := range p.Changes {
		change := ch.From + " -> " + ch.To
		switch {
		case ch.From == "":
			change = "+ " + ch.To
		case ch.To == "":
			change = "- " + ch.From
		}
		var notes []string
		if ch.Restart && p.Running {
			notes = append(notes, "restart")
		}
		if ch.Note != "" {
			notes = append(notes, ch.Note)
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", ch.Field, change, strings.Join(notes, "; "))
	}
	_ = tw.Flush()
	if p.Restart {
		_, _ = fmt.Fprintln(w, "The VM will be stopped gracefully, changed and started again.")
	}
}

func ask(stdin io.Reader, stdout io.Writer, question string) bool {
	_, _ = fmt.Fprint(stdout, question)
	var answer string
	if _, err := fmt.Fscanln(stdin, &answer); err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
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
	_, _ = fmt.Fprintf(w, "Owner:     %s\nProtected: %v\n", v.Owner, v.Protected)
	if len(v.Drift) > 0 {
		_, _ = fmt.Fprintln(w, "Drift (changed outside Strata):")
		for _, d := range v.Drift {
			_, _ = fmt.Fprintf(w, "  %s\n", d)
		}
	}
}

// ownerLabel is the owner column of `strata vm list`: the owner, marked
// when protected or changed outside Strata.
func ownerLabel(v workload.VMStatus) string {
	l := v.Owner
	if v.Protected {
		l += ",protected"
	}
	if len(v.Drift) > 0 {
		l += ",drift"
	}
	return l
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
