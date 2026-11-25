package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/letthemplay/Strata/internal/ir"
	"github.com/letthemplay/Strata/internal/planner"
	"github.com/letthemplay/Strata/internal/workload"
)

// Repository is the configuration repository (internal/gitops.Repo).
type Repository interface {
	Fetch(ctx context.Context) (string, error)
	Verify(ctx context.Context, sha, lastApplied string) (string, error)
	FileID(ctx context.Context, sha, path string) (string, error)
	FlakeURL(sha string) string
}

// IREvaluator evaluates a commit's IR (Evaluator).
type IREvaluator interface {
	IR(ctx context.Context, flakeURL, node string) ([]byte, error)
}

// Node observes the node through the agent (internal/agent/client).
type Node interface {
	ListVMs(ctx context.Context) ([]workload.VMStatus, error)
	Host(ctx context.Context) (workload.Host, error)
}

// Status is what `strata config status` shows.
type Status struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Node       string `json:"node"`
	AutoApply  string `json:"autoApply"`
	// Head is the branch head at the last fetch.
	Head string `json:"head,omitempty"`
	// Evaluated is the commit whose IR is current, and who signed it.
	Evaluated   string `json:"evaluated,omitempty"`
	Signer      string `json:"signer,omitempty"`
	FlakeLock   string `json:"flakeLock,omitempty"`
	LastApplied string `json:"lastApplied,omitempty"`
	LastSync    string `json:"lastSync,omitempty"`
	// Error is the last synchronization's error, if it failed.
	Error string       `json:"error,omitempty"`
	Plan  *PlanSummary `json:"plan,omitempty"`
	Drift []string     `json:"drift,omitempty"`
}

// PlanSummary summarizes the current plan.
type PlanSummary struct {
	ID             string `json:"id"`
	Commit         string `json:"commit"`
	Items          int    `json:"items"`
	Restarts       bool   `json:"restarts"`
	Deletes        bool   `json:"deletes"`
	Unreconcilable int    `json:"unreconcilable"`
	Safe           bool   `json:"safe"`
	Converged      bool   `json:"converged"`
	MadeAt         string `json:"madeAt"`
}

// CurrentPlan is the plan for the evaluated commit against the node as
// last observed.
type CurrentPlan struct {
	Commit string       `json:"commit"`
	Signer string       `json:"signer"`
	MadeAt time.Time    `json:"madeAt"`
	Plan   planner.Plan `json:"plan"`
}

// evaluated is a commit whose IR was evaluated and parsed.
type evaluated struct {
	commit, signer, flakeLock string
	doc                       *ir.Document
}

// Controller synchronizes the configuration repository and plans it
// against the node. In M3 slice 3a it does not apply plans.
type Controller struct {
	Config Config
	Repo   Repository
	Eval   IREvaluator
	Node   Node
	Log    *slog.Logger
	// Now returns the current time (tests).
	Now func() time.Time

	syncMu sync.Mutex // one synchronization at a time

	mu          sync.Mutex
	head        string
	cache       *evaluated
	lastGood    *evaluated // last commit that evaluated successfully
	lastApplied string
	lastSync    time.Time
	lastErr     error
	current     *CurrentPlan
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Sync fetches the branch and, for a new head, verifies, evaluates and
// parses it; then it plans the evaluated commit against the node. A
// commit that fails verification is never evaluated. The result is
// available from Status and Plan.
func (c *Controller) Sync(ctx context.Context) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	err := c.sync(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSync = c.now()
	c.lastErr = err
	if err != nil {
		c.log().WarnContext(ctx, "sync failed", "error", err)
	}
}

func (c *Controller) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log
}

func (c *Controller) sync(ctx context.Context) error {
	head, err := c.Repo.Fetch(ctx)
	if err != nil {
		// The current plan stays: it describes the node against the last
		// verified commit.
		return fmt.Errorf("fetch: %w", err)
	}
	c.mu.Lock()
	c.head = head
	cache, lastGood, lastApplied := c.cache, c.lastGood, c.lastApplied
	c.mu.Unlock()

	if cache == nil || cache.commit != head {
		e, err := c.evaluate(ctx, head, lastApplied, lastGood)
		c.mu.Lock()
		if err != nil {
			// Never keep a plan of a commit other than the head.
			c.cache, c.current = nil, nil
			c.mu.Unlock()
			return err
		}
		c.cache, c.lastGood = e, e
		c.mu.Unlock()
		cache = e
		c.log().InfoContext(ctx, "evaluated", "commit", head, "signer", e.signer, "resources", len(e.doc.Resources))
	}

	obs, err := observe(ctx, c.Node)
	if err != nil {
		return err
	}
	plan, err := planner.Make(cache.doc, obs)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	c.mu.Lock()
	c.current = &CurrentPlan{Commit: cache.commit, Signer: cache.signer, MadeAt: c.now().UTC(), Plan: plan}
	c.mu.Unlock()
	return nil
}

// evaluate verifies and evaluates a commit.
func (c *Controller) evaluate(ctx context.Context, sha, lastApplied string, lastGood *evaluated) (*evaluated, error) {
	signer, err := c.Repo.Verify(ctx, sha, lastApplied)
	if err != nil {
		return nil, fmt.Errorf("refused: %w", err)
	}
	lock, err := c.Repo.FileID(ctx, sha, "flake.lock")
	if err != nil {
		return nil, fmt.Errorf("commit %s: %w", sha, err)
	}
	data, err := c.Eval.IR(ctx, c.Repo.FlakeURL(sha), c.Config.Node)
	if err != nil {
		if lastGood != nil && lastGood.flakeLock != lock {
			detail := err.Error()
			var ee *EvalError
			if errors.As(err, &ee) {
				detail = ee.Detail
			}
			return nil, fmt.Errorf("commit %s changes flake.lock: the host configuration changed; apply it with nixos-rebuild first. Nix reported:\n%s", short(sha), detail)
		}
		return nil, fmt.Errorf("commit %s: %w", short(sha), err)
	}
	doc, err := ir.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("commit %s: invalid IR: %w", short(sha), err)
	}
	if doc.Node != c.Config.Node {
		return nil, fmt.Errorf("commit %s: the IR is for node %q, this is %q", short(sha), doc.Node, c.Config.Node)
	}
	return &evaluated{commit: sha, signer: signer, flakeLock: lock, doc: doc}, nil
}

func observe(ctx context.Context, n Node) (planner.Observed, error) {
	vms, err := n.ListVMs(ctx)
	if err != nil {
		return planner.Observed{}, fmt.Errorf("observe the node: %w", err)
	}
	host, err := n.Host(ctx)
	if err != nil {
		return planner.Observed{}, fmt.Errorf("observe the node: %w", err)
	}
	return planner.Observed{VMs: vms, Host: host}, nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Status returns the controller's state.
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Status{
		Repository: c.Config.Repository, Branch: c.Config.Branch, Node: c.Config.Node, AutoApply: c.Config.AutoApply,
		Head: c.head, LastApplied: c.lastApplied,
	}
	if !c.lastSync.IsZero() {
		s.LastSync = c.lastSync.UTC().Format(time.RFC3339)
	}
	if c.lastErr != nil {
		s.Error = c.lastErr.Error()
	}
	if c.cache != nil {
		s.Evaluated, s.Signer, s.FlakeLock = c.cache.commit, c.cache.signer, c.cache.flakeLock
	}
	if p := c.current; p != nil {
		s.Plan = &PlanSummary{
			ID: p.Plan.ID, Commit: p.Commit, Items: len(p.Plan.Items), Restarts: p.Plan.Restarts(), Deletes: p.Plan.Deletes(),
			Unreconcilable: len(p.Plan.Unreconcilable()), Safe: p.Plan.Safe(), Converged: p.Plan.Converged(),
			MadeAt: p.MadeAt.Format(time.RFC3339),
		}
		for _, it := range p.Plan.Items {
			if it.Action == planner.ActionRestore {
				s.Drift = append(s.Drift, it.Resource)
			}
		}
	}
	return s
}

// ErrNoPlan reports that there is no current plan.
var ErrNoPlan = errors.New("no current plan")

// Plan returns the current plan.
func (c *Controller) Plan() (CurrentPlan, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil {
		msg := "no current plan"
		if c.lastErr != nil {
			msg += ": the last synchronization failed: " + c.lastErr.Error()
		}
		return CurrentPlan{}, fmt.Errorf("%w (%s)", ErrNoPlan, msg)
	}
	return *c.current, nil
}

// Run synchronizes now and then every interval until ctx is canceled.
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(c.Config.Interval())
	defer t.Stop()
	for {
		c.Sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
