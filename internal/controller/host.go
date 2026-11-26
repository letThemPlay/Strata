package controller

// Host evaluation (M3 slice 5, ADR-0028): the controller evaluates the
// NixOS system of its current target commit and compares it with the
// system the host runs. It is read-only with respect to the host: it
// builds and activates nothing, and it is kept apart from the workload
// pipeline (its own lock, state and errors).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Host evaluation states.
const (
	HostEvalRunning   = "running"
	HostEvalSucceeded = "succeeded"
	HostEvalFailed    = "failed"
)

// Configuration drift: the evaluated desired system against the running
// one.
const (
	DriftInSync  = "in-sync"
	DriftDiffers = "differs"
	DriftUnknown = "unknown"
)

// Boot state: the running system against the booted one. Independent of
// configuration drift.
const (
	BootCurrent       = "current"
	BootRebootPending = "reboot-pending"
	BootUnknown       = "unknown"
)

// HostNote is shown with every host status while host deployment is not
// implemented.
const HostNote = "Strata can evaluate host configurations but does not deploy them yet."

// Errors of host evaluation.
var (
	ErrHostBusy    = errors.New("a host evaluation is in progress")
	ErrUnknownNode = errors.New("no such node")
)

// maxHostResults bounds the cached successful evaluations.
const maxHostResults = 8

// SystemEvaluator evaluates a commit's NixOS system (RemoteEvaluator,
// Evaluator).
type SystemEvaluator interface {
	System(ctx context.Context, flakeURL, node string) (SystemEval, error)
}

// SystemObserver reports the systems the host runs and booted.
type SystemObserver interface {
	Observe() ObservedSystem
}

// ObservedSystem is the running (current) and booted system, as store
// paths, or why one could not be read.
type ObservedSystem struct {
	Current      string `json:"current,omitempty"`
	Booted       string `json:"booted,omitempty"`
	CurrentError string `json:"currentError,omitempty"`
	BootedError  string `json:"bootedError,omitempty"`
}

// RunObserver reads /run/current-system and /run/booted-system (below
// Root, "/" if empty). It only reads the links. In M4 the observation
// moves to the agent (ADR-0029); the status model stays.
type RunObserver struct {
	Root string
}

// Observe implements SystemObserver.
func (o RunObserver) Observe() ObservedSystem {
	root := o.Root
	if root == "" {
		root = "/"
	}
	var obs ObservedSystem
	var err error
	if obs.Current, err = readSystemLink(filepath.Join(root, "run/current-system")); err != nil {
		obs.CurrentError = err.Error()
	}
	if obs.Booted, err = readSystemLink(filepath.Join(root, "run/booted-system")); err != nil {
		obs.BootedError = err.Error()
	}
	return obs
}

// readSystemLink returns the store path a system link points to.
func readSystemLink(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if !storePath.MatchString(target) {
		return "", fmt.Errorf("%s points to %q, not a store path", path, target)
	}
	return target, nil
}

// SourceSnapshot is the immutable source of an evaluation: everything it
// was bound to. The commit fixes the tree and flake.lock; they are
// recorded to make that visible.
type SourceSnapshot struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Pinned     bool   `json:"pinned,omitempty"`
	Commit     string `json:"commit"`
	Tree       string `json:"tree,omitempty"`
	FlakeLock  string `json:"flakeLock,omitempty"`
	Signer     string `json:"signer,omitempty"`
	Node       string `json:"node"`
	// Target is the evaluated attribute.
	Target string `json:"target"`
}

// HostEvaluation is one evaluation of the host's system.
type HostEvaluation struct {
	ID              string         `json:"id"`
	State           string         `json:"state"`
	Source          SourceSnapshot `json:"source"`
	RequestedBy     string         `json:"requestedBy"`
	StartedAt       time.Time      `json:"startedAt"`
	CompletedAt     time.Time      `json:"completedAt,omitzero"`
	DurationSeconds float64        `json:"durationSeconds,omitempty"`
	DrvPath         string         `json:"drvPath,omitempty"`
	OutPath         string         `json:"outPath,omitempty"`
	Error           string         `json:"error,omitempty"`
}

// HostConfiguration is the host's configuration status
// (GET /api/v1/nodes/{node}/configuration).
type HostConfiguration struct {
	Node       string `json:"node"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	// Target is the commit the controller targets now (the branch head, or
	// the pinned commit).
	Target string `json:"target,omitempty"`
	Pinned bool   `json:"pinned,omitempty"`
	// Evaluation is the latest evaluation, of any commit and outcome.
	Evaluation *HostEvaluation `json:"evaluation,omitempty"`
	// Desired is the successful evaluation of Target, if there is one.
	Desired            *HostEvaluation `json:"desired,omitempty"`
	Observed           ObservedSystem  `json:"observed"`
	ConfigurationDrift string          `json:"configurationDrift"`
	DriftReason        string          `json:"driftReason,omitempty"`
	BootState          string          `json:"bootState"`
	BootReason         string          `json:"bootReason,omitempty"`
	Note               string          `json:"note"`
}

// NodeInfo is a node the controller manages (GET /api/v1/nodes).
type NodeInfo struct {
	ID string `json:"id"`
}

// Nodes returns the nodes: this one.
func (c *Controller) Nodes() []NodeInfo { return []NodeInfo{{ID: c.Config.Node}} }

// CheckNode returns ErrUnknownNode unless node is this one.
func (c *Controller) CheckNode(node string) error {
	if node != c.Config.Node {
		return fmt.Errorf("%w: %q (this controller manages %q)", ErrUnknownNode, node, c.Config.Node)
	}
	return nil
}

func (c *Controller) systemTarget() string {
	return `nixosConfigurations."` + c.Config.Node + `".config.system.build.toplevel`
}

// hostTarget is the commit host evaluation uses, as sync does: the pinned
// commit (no forward-only check), else the branch head. c.mu is held.
func (c *Controller) hostTarget() (commit string, pinned bool, lastApplied string) {
	if c.state.Pinned != "" {
		return c.state.Pinned, true, ""
	}
	return c.head, false, c.state.LastApplied
}

// EvaluateHost starts an evaluation of the current target's system and
// returns it (running). One runs at a time. It reads the controller's
// state but never changes it, and never takes the workload sync lock.
func (c *Controller) EvaluateHost(uid uint32) (HostEvaluation, error) {
	if c.SystemEval == nil {
		return HostEvaluation{}, fmt.Errorf("%w: host evaluation is not configured", ErrInvalidAction)
	}
	c.hostMu.Lock()
	defer c.hostMu.Unlock()
	if l := c.hostLatest; l != nil && l.State == HostEvalRunning {
		return *l, fmt.Errorf("%w (%s)", ErrHostBusy, l.ID)
	}
	c.mu.Lock()
	commit, pinned, lastApplied := c.hostTarget()
	lastGoodLock := ""
	if c.lastGood != nil {
		lastGoodLock = c.lastGood.flakeLock
	}
	base := c.base
	c.mu.Unlock()
	if commit == "" {
		return HostEvaluation{}, fmt.Errorf("%w: no commit has been fetched yet (strata config sync)", ErrInvalidAction)
	}
	now := c.now().UTC()
	id, err := NewID(now)
	if err != nil {
		return HostEvaluation{}, err
	}
	ev := &HostEvaluation{
		ID: id, State: HostEvalRunning, RequestedBy: fmt.Sprintf("uid:%d", uid), StartedAt: now,
		Source: SourceSnapshot{
			Repository: c.Config.Repository, Branch: c.Config.Branch, Pinned: pinned,
			Commit: commit, Node: c.Config.Node, Target: c.systemTarget(),
		},
	}
	c.hostLatest = ev
	if base == nil {
		base = context.Background()
	}
	c.hostEvals.Add(1)
	go c.evaluateHost(base, id, ev.Source, lastApplied, lastGoodLock)
	return *ev, nil
}

// evaluateHost runs an evaluation and records its result.
func (c *Controller) evaluateHost(ctx context.Context, id string, src SourceSnapshot, lastApplied, lastGoodLock string) {
	defer c.hostEvals.Done()
	sys, err := c.runHostEvaluation(ctx, &src, lastApplied, lastGoodLock)
	if err == nil {
		// The evaluation is bound to the commit it started with: if the
		// controller's target moved meanwhile, it describes the past.
		c.mu.Lock()
		now, _, _ := c.hostTarget()
		c.mu.Unlock()
		if now != src.Commit {
			err = fmt.Errorf("the target changed during the evaluation (%s -> %s); evaluate again", short(src.Commit), short(now))
		}
	}
	done := c.now().UTC()
	c.hostMu.Lock()
	defer c.hostMu.Unlock()
	ev := c.hostLatest
	if ev == nil || ev.ID != id {
		return
	}
	ev.Source, ev.CompletedAt = src, done
	ev.DurationSeconds = done.Sub(ev.StartedAt).Round(time.Millisecond).Seconds()
	if err != nil {
		ev.State, ev.Error = HostEvalFailed, err.Error()
		c.log().WarnContext(ctx, "host evaluation failed", "evaluation", id, "commit", src.Commit, "error", err)
		return
	}
	ev.State, ev.DrvPath, ev.OutPath = HostEvalSucceeded, sys.DrvPath, sys.OutPath
	ok := *ev
	c.hostResults = append(c.hostResults, &ok)
	if len(c.hostResults) > maxHostResults {
		c.hostResults = c.hostResults[len(c.hostResults)-maxHostResults:]
	}
	c.log().InfoContext(ctx, "host evaluated", "evaluation", id, "commit", src.Commit, "outPath", sys.OutPath,
		"seconds", ev.DurationSeconds)
}

// runHostEvaluation verifies the commit as sync does, completes the
// snapshot and evaluates the system of exactly that commit.
func (c *Controller) runHostEvaluation(ctx context.Context, src *SourceSnapshot, lastApplied, lastGoodLock string) (SystemEval, error) {
	signer, err := c.Repo.Verify(ctx, src.Commit, lastApplied)
	if err != nil {
		return SystemEval{}, fmt.Errorf("refused: %w", err)
	}
	lock, err := c.Repo.FileID(ctx, src.Commit, "flake.lock")
	if err != nil {
		return SystemEval{}, fmt.Errorf("commit %s: %w", short(src.Commit), err)
	}
	tree, err := c.Repo.Tree(ctx, src.Commit)
	if err != nil {
		return SystemEval{}, fmt.Errorf("commit %s: %w", short(src.Commit), err)
	}
	src.Signer, src.FlakeLock, src.Tree = signer, lock, tree
	// The URL names the commit: the evaluation can't follow a moving head.
	sys, err := c.SystemEval.System(ctx, c.Repo.FlakeURL(src.Commit), src.Node)
	if err != nil {
		var ee *EvalError
		if errors.As(err, &ee) {
			c.log().WarnContext(ctx, "host evaluation: nix failed", "commit", src.Commit, "nix", ee.Detail)
			if lastGoodLock != "" && lastGoodLock != lock {
				return SystemEval{}, fmt.Errorf("commit %s changes flake.lock: its inputs are not on this host, and Strata does not fetch them yet (M4); apply it with nixos-rebuild first. Nix reported:\n%s",
					short(src.Commit), ee.Cause())
			}
		}
		return SystemEval{}, fmt.Errorf("commit %s: %w", short(src.Commit), err)
	}
	if err := sys.Validate(); err != nil {
		return SystemEval{}, err
	}
	return sys, nil
}

// HostStatus returns the host's configuration status: the latest
// evaluation, the desired system of the current target, the observed
// systems, configuration drift and boot state.
func (c *Controller) HostStatus() HostConfiguration {
	c.mu.Lock()
	target, pinned, _ := c.hostTarget()
	c.mu.Unlock()
	hc := HostConfiguration{
		Node: c.Config.Node, Repository: c.Config.Repository, Branch: c.Config.Branch,
		Target: target, Pinned: pinned, Note: HostNote,
	}
	if c.Observer != nil {
		hc.Observed = c.Observer.Observe()
	} else {
		hc.Observed = ObservedSystem{CurrentError: "not observed", BootedError: "not observed"}
	}
	c.hostMu.Lock()
	if l := c.hostLatest; l != nil {
		latest := *l
		hc.Evaluation = &latest
	}
	for i := len(c.hostResults) - 1; i >= 0; i-- {
		if r := c.hostResults[i]; r.Source.Commit == target && r.Source.Node == c.Config.Node && r.Source.Target == c.systemTarget() {
			desired := *r
			hc.Desired = &desired
			break
		}
	}
	c.hostMu.Unlock()
	hc.ConfigurationDrift, hc.DriftReason = configurationDrift(target, hc.Desired, hc.Evaluation, hc.Observed)
	hc.BootState, hc.BootReason = bootState(hc.Observed)
	return hc
}

// configurationDrift compares the desired system of the target with the
// running one.
func configurationDrift(target string, desired, latest *HostEvaluation, obs ObservedSystem) (string, string) {
	switch {
	case target == "":
		return DriftUnknown, "no commit has been fetched yet"
	case desired == nil && latest != nil && latest.Source.Commit == target && latest.State == HostEvalRunning:
		return DriftUnknown, "the evaluation of " + short(target) + " is in progress"
	case desired == nil && latest != nil && latest.Source.Commit == target && latest.State == HostEvalFailed:
		return DriftUnknown, "the evaluation of " + short(target) + " failed"
	case desired == nil:
		return DriftUnknown, "commit " + short(target) + " has not been evaluated (strata host evaluate)"
	case obs.Current == "":
		return DriftUnknown, "the running system can't be read: " + obs.CurrentError
	case desired.OutPath == obs.Current:
		return DriftInSync, ""
	}
	return DriftDiffers, "the host runs a different system than Git declares"
}

// bootState compares the running system with the booted one.
func bootState(obs ObservedSystem) (string, string) {
	switch {
	case obs.Current == "":
		return BootUnknown, "the running system can't be read: " + obs.CurrentError
	case obs.Booted == "":
		return BootUnknown, "the booted system can't be read: " + obs.BootedError
	case obs.Current == obs.Booted:
		return BootCurrent, ""
	}
	return BootRebootPending, "the running system is not the one the host booted (a configuration was activated without a reboot); the kernel in use is the booted system's"
}
