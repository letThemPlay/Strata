// Package reconciler applies approved node plans (internal/planner)
// through a workload provider, then verifies that the node converged
// (M3 slice 2).
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/letthemplay/Strata/internal/ir"
	"github.com/letthemplay/Strata/internal/planner"
	"github.com/letthemplay/Strata/internal/workload"
)

// Provider is what the reconciler needs from the node: the agent's API
// for the controller (slice 3), a mock in tests. Every change is verified
// by the provider before it returns (ADR-0020).
type Provider interface {
	ListVMs(ctx context.Context) ([]workload.VMStatus, error)
	Host(ctx context.Context) (workload.Host, error)
	CreateVM(ctx context.Context, vm workload.VM, meta workload.Meta) error
	VMAction(ctx context.Context, name, action string, force bool, timeoutSeconds int) error
	UpdateVM(ctx context.Context, vm workload.VM, planID string, stopTimeout int) (workload.Plan, error)
	RestoreVM(ctx context.Context, name string, stopTimeout int) error
	SetMeta(ctx context.Context, name string, meta workload.Meta) error
	DeleteVM(ctx context.Context, name string, stop bool, stopTimeout int) ([]string, error)
}

// Item results.
const (
	StatusSucceeded    = "succeeded"
	StatusFailed       = "failed"
	StatusSkipped      = "skipped"       // unreconcilable: not applied
	StatusNotAttempted = "not-attempted" // after an earlier failure
)

// Errors returned by Apply.
var (
	// ErrStale: the node or the desired state changed since the plan
	// was made; nothing was applied.
	ErrStale = errors.New("the plan is stale")
	// ErrNotConverged: the changes were applied, but planning again
	// still finds something to do.
	ErrNotConverged = errors.New("the node did not converge")
)

// Phases reported through Options.OnPhase.
const (
	PhaseApplying  = "applying"
	PhaseVerifying = "verifying"
)

// Options control how plans are applied.
type Options struct {
	// StopTimeout is how long a graceful stop may take (seconds).
	StopTimeout int
	// OnPhase, if set, is called when applying starts ("applying") and
	// when the result is verified ("verifying").
	OnPhase func(phase string)
}

// ItemResult is what happened to one plan item.
type ItemResult struct {
	Resource    string   `json:"resource"`
	Action      string   `json:"action"`
	Status      string   `json:"status"`
	Error       string   `json:"error,omitempty"`
	KeptVolumes []string `json:"keptVolumes,omitempty"`
}

// Result is the outcome of applying a plan.
type Result struct {
	PlanID string       `json:"planId"`
	Items  []ItemResult `json:"items"`
	// After is the plan made from the node after applying: only
	// unreconcilable items when converged.
	After     *planner.Plan `json:"after,omitempty"`
	Converged bool          `json:"converged"`
}

// Observe reads what the node has.
func Observe(ctx context.Context, p Provider) (planner.Observed, error) {
	vms, err := p.ListVMs(ctx)
	if err != nil {
		return planner.Observed{}, fmt.Errorf("list VMs: %w", err)
	}
	host, err := p.Host(ctx)
	if err != nil {
		return planner.Observed{}, fmt.Errorf("host: %w", err)
	}
	return planner.Observed{VMs: vms, Host: host}, nil
}

// Plan observes the node and plans doc against it.
func Plan(ctx context.Context, p Provider, doc *ir.Document) (planner.Plan, error) {
	obs, err := Observe(ctx, p)
	if err != nil {
		return planner.Plan{}, err
	}
	plan, err := planner.Make(doc, obs)
	if err != nil {
		return planner.Plan{}, fmt.Errorf("plan: %w", err)
	}
	return plan, nil
}

// Apply applies the plan with ID planID: it plans again and refuses
// (ErrStale, nothing applied) unless the plan still has that ID, so what
// is applied is what was approved. Items are applied in this order:
// lifecycle changes, restores, updates, deletions, creations; the
// lifecycle change of an unreconcilable item is applied, the rest of it is
// skipped. Apply stops at the first failure. Finally it plans again: if
// anything but unreconcilable items remains, it returns ErrNotConverged
// with the result.
func Apply(ctx context.Context, p Provider, doc *ir.Document, planID string, opt Options) (Result, error) {
	plan, err := Plan(ctx, p, doc)
	if err != nil {
		return Result{}, err
	}
	if plan.ID != planID {
		return Result{}, fmt.Errorf("%w: plan %s was approved, the node's plan is now %s; plan and approve again", ErrStale, planID, plan.ID)
	}
	report := func(p string) {
		if opt.OnPhase != nil {
			opt.OnPhase(p)
		}
	}
	report(PhaseApplying)
	res := Result{PlanID: planID, Items: []ItemResult{}}
	specs := map[string]workload.VM{}
	for _, r := range doc.Resources {
		specs[r.ID] = *r.VM
	}

	items := slices.Clone(plan.Items)
	slices.SortStableFunc(items, func(a, b planner.Item) int { return phase(a) - phase(b) })
	var failure error
	for _, it := range items {
		r := ItemResult{Resource: it.Resource, Action: it.Action}
		switch {
		case failure != nil:
			r.Status = StatusNotAttempted
		case it.Action == planner.ActionUnreconcilable && it.Protect == nil:
			r.Status = StatusSkipped
			r.Error = strings.Join(it.Reasons, "; ")
		default:
			kept, err := apply(ctx, p, it, specs[it.Resource], opt)
			r.KeptVolumes = kept
			if err != nil {
				failure = fmt.Errorf("%s %s: %w", it.Action, it.Resource, err)
				r.Status, r.Error = StatusFailed, err.Error()
			} else {
				r.Status = StatusSucceeded
			}
		}
		res.Items = append(res.Items, r)
	}
	if failure != nil {
		return res, failure
	}

	report(PhaseVerifying)
	after, err := Plan(ctx, p, doc)
	if err != nil {
		return res, fmt.Errorf("verify: %w", err)
	}
	res.After = &after
	res.Converged = after.Converged()
	if !res.Converged {
		var left []string
		for _, it := range after.Items {
			if it.Action != planner.ActionUnreconcilable || it.Protect != nil {
				left = append(left, it.Action+" "+it.Resource)
			}
		}
		return res, fmt.Errorf("%w: still to do: %s", ErrNotConverged, strings.Join(left, ", "))
	}
	return res, nil
}

// phase orders items: lifecycle changes, restores, updates, deletions
// (which free project limits), creations.
func phase(it planner.Item) int {
	return map[string]int{
		planner.ActionLifecycle:      0,
		planner.ActionUnreconcilable: 0, // only its lifecycle change is applied
		planner.ActionRestore:        1,
		planner.ActionUpdate:         2,
		planner.ActionDelete:         3,
		planner.ActionCreate:         4,
	}[it.Action]
}

// apply applies one item. It returns the kept data volumes of a deletion.
func apply(ctx context.Context, p Provider, it planner.Item, spec workload.VM, opt Options) ([]string, error) {
	name := strings.TrimPrefix(it.Resource, ir.TypeVM+"/")
	gitops := func(protected bool) workload.Meta {
		return workload.Meta{Owner: workload.OwnerGitOps, Protected: protected}
	}
	switch it.Action {
	case planner.ActionCreate:
		protected := it.Protect != nil && *it.Protect
		if err := p.CreateVM(ctx, spec, gitops(protected)); err != nil {
			return nil, err //nolint:wrapcheck // Provider errors name the VM and step.
		}
		// Declared VMs run once created; afterwards their power state is
		// the operator's (M3 does not declare it).
		return nil, p.VMAction(ctx, name, workload.ActionStart, false, 0) //nolint:wrapcheck // As above.
	case planner.ActionDelete:
		return p.DeleteVM(ctx, name, true, opt.StopTimeout) //nolint:wrapcheck // As above.
	}
	if it.Action == planner.ActionRestore {
		if err := p.RestoreVM(ctx, name, opt.StopTimeout); err != nil {
			return nil, err //nolint:wrapcheck // As above.
		}
	}
	if len(it.Changes) > 0 {
		if _, err := p.UpdateVM(ctx, spec, it.VMPlanID, opt.StopTimeout); err != nil {
			return nil, err //nolint:wrapcheck // As above.
		}
	}
	if it.Protect != nil {
		if err := p.SetMeta(ctx, name, gitops(*it.Protect)); err != nil {
			return nil, err //nolint:wrapcheck // As above.
		}
	}
	return nil, nil
}
