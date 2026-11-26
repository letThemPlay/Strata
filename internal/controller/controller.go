package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/letthemplay/Strata/internal/ir"
	"github.com/letthemplay/Strata/internal/planner"
	"github.com/letthemplay/Strata/internal/reconciler"
	"github.com/letthemplay/Strata/internal/workload"
)

// Repository is the configuration repository (internal/gitops.Repo).
type Repository interface {
	Fetch(ctx context.Context) (string, error)
	Verify(ctx context.Context, sha, lastApplied string) (string, error)
	FileID(ctx context.Context, sha, path string) (string, error)
	FlakeURL(sha string) string
	Resolve(ctx context.Context, ref string) (string, error)
}

// IREvaluator evaluates a commit's IR (RemoteEvaluator, Evaluator).
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
	// Pinned is the commit evaluated instead of the branch head.
	Pinned string `json:"pinned,omitempty"`
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
	// Awaiting is the deployment awaiting approval; Running the one in
	// progress.
	Awaiting *DeploymentSummary `json:"awaiting,omitempty"`
	Running  *DeploymentSummary `json:"running,omitempty"`
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

// DeploymentSummary is a line of `strata config history`.
type DeploymentSummary struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	State       string    `json:"state"`
	Commit      string    `json:"commit,omitempty"`
	Subject     string    `json:"subject,omitempty"`
	PlanID      string    `json:"planId,omitempty"`
	ApprovedBy  string    `json:"approvedBy,omitempty"`
	Items       int       `json:"items"`
	CreatedAt   time.Time `json:"createdAt"`
	CompletedAt time.Time `json:"completedAt,omitzero"`
	Error       string    `json:"error,omitempty"`
}

func summarize(d *Deployment) *DeploymentSummary {
	s := &DeploymentSummary{
		ID: d.ID, Kind: d.Kind, State: d.State, Commit: d.Commit, Subject: d.Subject, PlanID: d.PlanID,
		ApprovedBy: d.ApprovedBy, CreatedAt: d.CreatedAt, CompletedAt: d.CompletedAt, Error: d.Error,
	}
	if d.Plan != nil {
		s.Items = len(d.Plan.Items)
	}
	return s
}

// CurrentPlan is the plan for the evaluated commit against the node as
// last observed.
type CurrentPlan struct {
	Commit string       `json:"commit"`
	Signer string       `json:"signer"`
	MadeAt time.Time    `json:"madeAt"`
	Plan   planner.Plan `json:"plan"`
	// Deployment is the record awaiting approval of this plan.
	Deployment string `json:"deployment,omitempty"`
}

// evaluated is a commit whose IR was evaluated and parsed.
type evaluated struct {
	commit, signer, flakeLock string
	pinned                    bool
	doc                       *ir.Document
}

// Errors of Apply, Pin and Adopt.
var (
	ErrNoPlan        = errors.New("no current plan")
	ErrNotAwaiting   = errors.New("no plan with this ID awaiting approval")
	ErrAllowDelete   = errors.New("the plan deletes VMs")
	ErrBusy          = errors.New("a deployment is in progress")
	ErrInvalidAction = errors.New("invalid request")
)

// Controller synchronizes the configuration repository, plans it against
// the node and applies approved plans (ADR-0025, ADR-0026).
type Controller struct {
	Config Config
	Repo   Repository
	Eval   IREvaluator
	Node   Node
	// Exec applies plans (the agent, as the controller).
	Exec  reconciler.Provider
	Store *Store
	// Version is the controller's Strata version, recorded in deployments.
	Version string
	Log     *slog.Logger
	// Now returns the current time (tests).
	Now func() time.Time

	// syncMu serializes synchronizations, deployments and operator
	// actions.
	syncMu sync.Mutex

	mu          sync.Mutex
	base        context.Context // deployments run in it (canceled on shutdown)
	head        string
	cache       *evaluated
	lastGood    *evaluated // last commit that evaluated successfully
	state       State
	lastSync    time.Time
	lastErr     error
	current     *CurrentPlan
	records     []*Deployment
	awaiting    *Deployment
	running     *Deployment
	deployments sync.WaitGroup
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Controller) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log
}

// Init loads the state and records. A deployment found in progress was
// interrupted (the controller stopped): it is marked FAILED, and the next
// synchronization plans again from the node as it is. A plan awaiting
// approval is CANCELLED: plans are made again after a restart.
func (c *Controller) Init(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.base = ctx
	if c.Store == nil {
		return nil
	}
	st, err := c.Store.LoadState()
	if err != nil {
		return err
	}
	records, err := c.Store.Load()
	if err != nil {
		return err
	}
	c.state, c.records = st, records
	for _, d := range records {
		note := ""
		switch {
		case d.State == StateAwaitingApproval:
			d.transition(StateCancelled, "the controller restarted; the plan is made again", c.now().UTC())
			note = "cancelled"
		case d.InProgress():
			d.Error = "interrupted: the controller stopped during the deployment; the node is planned again"
			d.transition(StateFailed, d.Error, c.now().UTC())
			note = "interrupted"
		}
		if note != "" {
			if err := c.Store.Save(d); err != nil {
				return err
			}
			c.log().WarnContext(ctx, "deployment "+note, "deployment", d.ID)
		}
	}
	return nil
}

// Sync fetches the branch and, for a new target commit (the pinned commit,
// or the branch head), verifies, evaluates and parses it; then it plans
// the evaluated commit against the node and records a new plan as
// awaiting approval. A commit that fails verification is never evaluated.
// With automatic application of safe plans, a safe plan is applied.
func (c *Controller) Sync(ctx context.Context) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	c.syncLocked(ctx)
}

func (c *Controller) syncLocked(ctx context.Context) {
	err := c.sync(ctx)
	c.mu.Lock()
	c.lastSync = c.now()
	c.lastErr = err
	if err != nil {
		c.log().WarnContext(ctx, "sync failed", "error", err)
	}
	auto := c.propose(ctx)
	c.mu.Unlock()
	if auto != nil {
		c.startDeployment(auto, "auto") //nolint:contextcheck // Deployments run in the controller's context, not a request's.
	}
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
	cache, lastGood, st := c.cache, c.lastGood, c.state
	c.mu.Unlock()

	target, lastApplied := head, st.LastApplied
	if st.Pinned != "" {
		// A pinned commit may be older than the last applied one: that is
		// what a rollback is. It must still be signed.
		target, lastApplied = st.Pinned, ""
	}
	if cache == nil || cache.commit != target {
		started := c.now()
		e, err := c.evaluate(ctx, target, lastApplied, lastGood)
		c.mu.Lock()
		if err != nil {
			// Never keep a plan of a commit other than the target.
			c.cache, c.current = nil, nil
			c.mu.Unlock()
			return err
		}
		e.pinned = st.Pinned != ""
		c.cache, c.lastGood = e, e
		c.mu.Unlock()
		cache = e
		c.log().InfoContext(ctx, "evaluated", "commit", target, "signer", e.signer, "resources", len(e.doc.Resources), "pinned", e.pinned,
			"seconds", c.now().Sub(started).Round(time.Millisecond).Seconds())
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

// propose records the current plan as awaiting approval if it has
// something to do and is new, and cancels a replaced one. It returns the
// deployment to apply automatically, if any. c.mu is held.
func (c *Controller) propose(ctx context.Context) *Deployment {
	now := c.now().UTC()
	cur := c.current
	actionable := cur != nil && !cur.Plan.Converged()
	if a := c.awaiting; a != nil && (!actionable || a.PlanID != cur.Plan.ID) {
		note := "replaced by a new plan"
		if !actionable {
			note = "no longer applicable: nothing to do, or the commit could not be planned"
		}
		a.transition(StateCancelled, note, now)
		c.save(ctx, a)
		c.awaiting = nil
	}
	if !actionable {
		if cur != nil && c.running == nil && cur.Commit != c.state.LastApplied {
			c.converged(ctx, cur, now)
		}
		return nil
	}
	if c.awaiting == nil {
		id, err := NewID(now)
		if err != nil {
			c.log().ErrorContext(ctx, "record plan", "error", err)
			return nil
		}
		plan := cur.Plan
		d := &Deployment{
			ID: id, Kind: KindPlan, Repository: c.Config.Repository, Branch: c.Config.Branch, Node: c.Config.Node,
			Commit: cur.Commit, Signer: cur.Signer, FlakeLock: c.cache.flakeLock, Pinned: c.cache.pinned,
			StrataVersion: c.Version, IRGenerator: c.cache.doc.Generator.StrataVersion,
			PlanID: plan.ID, Plan: &plan, CreatedAt: now,
		}
		d.transition(StateAwaitingApproval, "", now)
		c.records = append(c.records, d)
		c.awaiting = d
		c.save(ctx, d)
	}
	cur.Deployment = c.awaiting.ID
	if c.Config.AutoApply == "safe" && cur.Plan.Safe() && c.running == nil {
		d := c.awaiting
		c.awaiting = nil
		return d
	}
	return nil
}

// converged records a new commit whose plan has nothing to do as applied:
// the node already matches it, so there is nothing to approve. The commit
// was verified (signed, descending from the last applied one) when it was
// evaluated; recording it keeps the forward-only check at the commit the
// node matches. c.mu is held.
func (c *Controller) converged(ctx context.Context, cur *CurrentPlan, now time.Time) {
	id, err := NewID(now)
	if err != nil {
		c.log().ErrorContext(ctx, "record plan", "error", err)
		return
	}
	st := c.state
	st.LastApplied = cur.Commit
	if err := c.saveState(st); err != nil {
		c.log().ErrorContext(ctx, "save state", "error", err)
		return
	}
	c.state = st
	plan := cur.Plan
	d := &Deployment{
		ID: id, Kind: KindPlan, Repository: c.Config.Repository, Branch: c.Config.Branch, Node: c.Config.Node,
		Commit: cur.Commit, Signer: cur.Signer, FlakeLock: c.cache.flakeLock, Pinned: c.cache.pinned,
		StrataVersion: c.Version, IRGenerator: c.cache.doc.Generator.StrataVersion,
		PlanID: plan.ID, Plan: &plan, ApprovedBy: "auto", CreatedAt: now,
	}
	d.transition(StateSucceeded, "the node already matches this commit: nothing to apply", now)
	c.records = append(c.records, d)
	c.save(ctx, d)
	cur.Deployment = d.ID
	if c.Store != nil {
		if pruned, err := c.Store.Prune(c.records, c.Config.KeepDeployments); err != nil {
			c.log().ErrorContext(ctx, "prune deployments", "error", err)
		} else {
			c.records = pruned
		}
	}
	c.log().InfoContext(ctx, "commit applied: nothing to do", "deployment", d.ID, "commit", d.Commit)
}

// save writes a record; c.mu is held.
func (c *Controller) save(ctx context.Context, d *Deployment) {
	if c.Store == nil {
		return
	}
	if err := c.Store.Save(d); err != nil {
		c.log().ErrorContext(ctx, "save deployment", "deployment", d.ID, "error", err)
	}
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
		detail := err.Error()
		var ee *EvalError
		if errors.As(err, &ee) {
			detail = ee.Cause()
			// Messages show the cause; the journal keeps Nix's trace.
			c.log().WarnContext(ctx, "evaluation failed", "commit", sha, "nix", ee.Detail)
		}
		if lastGood != nil && lastGood.flakeLock != lock {
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

// Apply approves the plan with ID planID, which must be awaiting approval,
// and starts applying it; it returns the deployment at once. A plan that
// deletes VMs needs allowDelete (M3 slice 3b decision 2).
func (c *Controller) Apply(planID string, uid uint32, allowDelete bool) (Deployment, error) {
	c.mu.Lock()
	d := c.awaiting
	switch {
	case c.running != nil:
		c.mu.Unlock()
		return Deployment{}, fmt.Errorf("%w (%s); wait for it to finish", ErrBusy, c.running.ID)
	case d == nil || d.PlanID != planID:
		current := "there is none"
		if d != nil {
			current = "it is " + d.PlanID
		}
		c.mu.Unlock()
		return Deployment{}, fmt.Errorf("%w: %s (the current plan: %s); run strata config plan", ErrNotAwaiting, planID, current)
	case d.Plan.Deletes() && !allowDelete:
		c.mu.Unlock()
		return Deployment{}, fmt.Errorf("%w: approve it with --allow-delete (root disks are deleted; data volumes are kept)", ErrAllowDelete)
	}
	c.awaiting = nil
	c.mu.Unlock()
	return c.startDeployment(d, fmt.Sprintf("uid:%d", uid)), nil
}

// startDeployment marks d approved and applies it in the background.
func (c *Controller) startDeployment(d *Deployment, approver string) Deployment {
	c.mu.Lock()
	d.ApprovedBy = approver
	d.transition(StatePending, "approved by "+approver, c.now().UTC())
	c.running = d
	ctx := c.base
	if ctx == nil {
		ctx = context.Background()
	}
	c.save(ctx, d)
	snapshot := *d
	c.mu.Unlock()
	c.deployments.Add(1)
	go func() {
		defer c.deployments.Done()
		c.deploy(ctx, d)
	}()
	return snapshot
}

// deploy applies an approved plan: VALIDATING (the commit is still the
// evaluated one and still trusted), PLANNING (the plan is unchanged),
// APPLYING and VERIFYING (reconciler.Apply), then SUCCEEDED or FAILED.
func (c *Controller) deploy(ctx context.Context, d *Deployment) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	step := func(state, note string) {
		c.mu.Lock()
		d.transition(state, note, c.now().UTC())
		c.save(ctx, d)
		c.mu.Unlock()
	}
	// The record becomes final only after the controller has planned
	// again, so whoever sees it finished also sees the current plan.
	final, finalErr := StateSucceeded, error(nil)
	fail := func(err error) { final, finalErr = StateFailed, err }
	defer func() {
		c.mu.Lock()
		c.running = nil
		c.mu.Unlock()
		// Plan again from the node as it is now.
		c.syncLocked(ctx)
		c.mu.Lock()
		if finalErr != nil {
			d.Error = finalErr.Error()
		}
		c.mu.Unlock()
		step(final, "")
		if finalErr != nil {
			c.log().ErrorContext(ctx, "deployment failed", "deployment", d.ID, "error", finalErr)
		} else {
			c.log().InfoContext(ctx, "deployment succeeded", "deployment", d.ID, "commit", d.Commit)
		}
		c.mu.Lock()
		if c.Store != nil {
			if pruned, err := c.Store.Prune(c.records, c.Config.KeepDeployments); err != nil {
				c.log().ErrorContext(ctx, "prune deployments", "error", err)
			} else {
				c.records = pruned
			}
		}
		c.mu.Unlock()
	}()

	step(StateValidating, "")
	c.mu.Lock()
	cache, lastApplied := c.cache, c.state.LastApplied
	c.mu.Unlock()
	if cache == nil || cache.commit != d.Commit {
		fail(errors.New("the evaluated commit changed since the plan was made; plan and approve again"))
		return
	}
	if d.Pinned {
		lastApplied = ""
	}
	if _, err := c.Repo.Verify(ctx, d.Commit, lastApplied); err != nil {
		fail(fmt.Errorf("refused: %w", err))
		return
	}

	step(StatePlanning, "")
	plan, err := reconciler.Plan(ctx, c.Exec, cache.doc)
	if err != nil {
		fail(err)
		return
	}
	if plan.ID != d.PlanID {
		fail(fmt.Errorf("the node or the configuration changed since plan %s was made (it is now %s); plan and approve again", d.PlanID, plan.ID))
		return
	}

	res, err := reconciler.Apply(ctx, c.Exec, cache.doc, d.PlanID, reconciler.Options{
		OnPhase: func(phase string) {
			switch phase {
			case reconciler.PhaseApplying:
				step(StateApplying, "")
			case reconciler.PhaseVerifying:
				step(StateVerifying, "")
			}
		},
	})
	c.mu.Lock()
	d.Result = &res
	c.mu.Unlock()
	if err != nil {
		fail(err)
		return
	}
	c.mu.Lock()
	c.state.LastApplied = d.Commit
	st := c.state
	c.mu.Unlock()
	if c.Store != nil {
		if err := c.Store.SaveState(st); err != nil {
			fail(fmt.Errorf("applied, but the state could not be saved: %w", err))
			return
		}
	}
}

// Wait waits for deployments in progress (tests, shutdown).
func (c *Controller) Wait() { c.deployments.Wait() }

var refPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// record adds a finished operator action to the history; c.mu is held.
func (c *Controller) record(ctx context.Context, kind, subject, commit, approver string, err error) {
	now := c.now().UTC()
	id, idErr := NewID(now)
	if idErr != nil {
		c.log().ErrorContext(ctx, "record", "error", idErr)
		return
	}
	d := &Deployment{
		ID: id, Kind: kind, Repository: c.Config.Repository, Branch: c.Config.Branch, Node: c.Config.Node,
		Commit: commit, Subject: subject, ApprovedBy: approver, StrataVersion: c.Version, CreatedAt: now,
	}
	if err != nil {
		d.Error = err.Error()
		d.transition(StateFailed, "", now)
	} else {
		d.transition(StateSucceeded, "", now)
	}
	c.records = append(c.records, d)
	c.save(ctx, d)
}

func (c *Controller) saveState(st State) error {
	if c.Store == nil {
		return nil
	}
	return c.Store.SaveState(st)
}

// Pin makes the controller evaluate commit ref (a commit ID or a prefix
// of at least 7 hex digits, in the fetched history) instead of the branch
// head: a rollback that does not write to the repository. The commit must
// be signed by a trusted key; it may be older than the last applied one.
func (c *Controller) Pin(ctx context.Context, ref string, uid uint32) (Status, error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	if err := c.notBusy(); err != nil {
		return Status{}, err
	}
	if !refPattern.MatchString(ref) {
		return Status{}, fmt.Errorf("%w: %q is not a commit ID", ErrInvalidAction, ref)
	}
	sha, err := c.Repo.Resolve(ctx, ref)
	if err == nil {
		_, err = c.Repo.Verify(ctx, sha, "")
	}
	c.mu.Lock()
	approver := fmt.Sprintf("uid:%d", uid)
	if err != nil {
		c.record(ctx, KindPin, ref, "", approver, err)
		c.mu.Unlock()
		return Status{}, fmt.Errorf("%w: %w", ErrInvalidAction, err)
	}
	st := c.state
	st.Pinned = sha
	if err := c.saveState(st); err != nil {
		c.mu.Unlock()
		return Status{}, err
	}
	c.state = st
	c.record(ctx, KindPin, sha, sha, approver, nil)
	c.mu.Unlock()
	c.syncLocked(ctx)
	return c.Status(), nil
}

// Unpin returns to the branch head, which must descend from the last
// applied commit.
func (c *Controller) Unpin(ctx context.Context, uid uint32) (Status, error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	if err := c.notBusy(); err != nil {
		return Status{}, err
	}
	c.mu.Lock()
	if c.state.Pinned == "" {
		c.mu.Unlock()
		return Status{}, fmt.Errorf("%w: no commit is pinned", ErrInvalidAction)
	}
	st := c.state
	st.Pinned = ""
	if err := c.saveState(st); err != nil {
		c.mu.Unlock()
		return Status{}, err
	}
	c.state = st
	c.record(ctx, KindUnpin, "", "", fmt.Sprintf("uid:%d", uid), nil)
	c.mu.Unlock()
	c.syncLocked(ctx)
	return c.Status(), nil
}

// Adopt moves a VM created with `strata vm` to Git: it must be declared in
// the evaluated commit and not have drifted. Only its owner changes; the
// next plan shows any difference from the declaration.
func (c *Controller) Adopt(ctx context.Context, name string, uid uint32) (Status, error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	if err := c.notBusy(); err != nil {
		return Status{}, err
	}
	c.mu.Lock()
	cache := c.cache
	c.mu.Unlock()
	err := c.adopt(ctx, cache, name)
	c.mu.Lock()
	commit := ""
	if cache != nil {
		commit = cache.commit
	}
	c.record(ctx, KindAdopt, name, commit, fmt.Sprintf("uid:%d", uid), err)
	c.mu.Unlock()
	if err != nil {
		return Status{}, err
	}
	c.syncLocked(ctx)
	return c.Status(), nil
}

func (c *Controller) adopt(ctx context.Context, cache *evaluated, name string) error {
	if cache == nil {
		return fmt.Errorf("%w: there is no evaluated commit", ErrInvalidAction)
	}
	if !slices.ContainsFunc(cache.doc.Resources, func(r ir.Resource) bool { return r.ID == ir.TypeVM+"/"+name }) {
		return fmt.Errorf("%w: vm %s is not declared in commit %s", ErrInvalidAction, name, short(cache.commit))
	}
	vms, err := c.Node.ListVMs(ctx)
	if err != nil {
		return fmt.Errorf("observe the node: %w", err)
	}
	i := slices.IndexFunc(vms, func(v workload.VMStatus) bool { return v.Spec.Name == name })
	switch {
	case i < 0:
		return fmt.Errorf("%w: vm %s does not exist", ErrInvalidAction, name)
	case vms[i].Owner == workload.OwnerGitOps:
		return fmt.Errorf("%w: vm %s is already managed through Git", ErrInvalidAction, name)
	case len(vms[i].Drift) > 0:
		return fmt.Errorf("%w: vm %s was changed outside Strata; revert that first", ErrInvalidAction, name)
	}
	if err := c.Exec.SetMeta(ctx, name, workload.Meta{Owner: workload.OwnerGitOps, Protected: vms[i].Protected}); err != nil {
		return fmt.Errorf("adopt vm %s: %w", name, err)
	}
	return nil
}

func (c *Controller) notBusy() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running != nil {
		return fmt.Errorf("%w (%s)", ErrBusy, c.running.ID)
	}
	return nil
}

// Status returns the controller's state.
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Status{
		Repository: c.Config.Repository, Branch: c.Config.Branch, Node: c.Config.Node, AutoApply: c.Config.AutoApply,
		Head: c.head, Pinned: c.state.Pinned, LastApplied: c.state.LastApplied,
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
	if c.awaiting != nil {
		s.Awaiting = summarize(c.awaiting)
	}
	if c.running != nil {
		s.Running = summarize(c.running)
	}
	return s
}

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

// History returns the newest records first, at most limit (0: all).
func (c *Controller) History(limit int) []DeploymentSummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []DeploymentSummary{}
	for i := len(c.records) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		out = append(out, *summarize(c.records[i]))
	}
	return out
}

// ErrNoDeployment reports an unknown deployment ID.
var ErrNoDeployment = errors.New("no such deployment")

// Deployment returns a record.
func (c *Controller) Deployment(id string) (Deployment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.records {
		if d.ID == id {
			return *d, nil
		}
	}
	return Deployment{}, fmt.Errorf("%w: %s", ErrNoDeployment, id)
}

// Run synchronizes now and then every interval until ctx is canceled,
// then waits for a deployment in progress.
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(c.Config.Interval())
	defer t.Stop()
	for {
		c.Sync(ctx)
		select {
		case <-ctx.Done():
			c.Wait()
			return
		case <-t.C:
		}
	}
}
