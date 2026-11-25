package controller

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/letthemplay/Strata/internal/planner"
	"github.com/letthemplay/Strata/internal/reconciler"
)

// Deployment states (CLAUDE.md §9). A plan is recorded when it is
// proposed (AWAITING_APPROVAL); one that is replaced before approval is
// CANCELLED.
const (
	StateAwaitingApproval = "AWAITING_APPROVAL"
	StatePending          = "PENDING"
	StateValidating       = "VALIDATING"
	StatePlanning         = "PLANNING"
	StateApplying         = "APPLYING"
	StateVerifying        = "VERIFYING"
	StateSucceeded        = "SUCCEEDED"
	StateFailed           = "FAILED"
	StateCancelled        = "CANCELLED"
)

// Record kinds: a plan (a deployment), or an operator action.
const (
	KindPlan  = "plan"
	KindPin   = "pin"
	KindUnpin = "unpin"
	KindAdopt = "adopt"
)

// Transition is a state change of a deployment.
type Transition struct {
	State string    `json:"state"`
	At    time.Time `json:"at"`
	Note  string    `json:"note,omitempty"`
}

// Deployment is the record of a plan (or of an operator action): what was
// planned from which commit, who approved it, and what happened (CLAUDE.md
// §8: repository, commit, flake lock, Strata version, node, time, result).
type Deployment struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Node       string `json:"node"`
	Commit     string `json:"commit,omitempty"`
	Signer     string `json:"signer,omitempty"`
	// FlakeLock is the Git blob ID of the commit's flake.lock.
	FlakeLock string `json:"flakeLock,omitempty"`
	// Pinned: the commit was pinned rather than the branch head.
	Pinned bool `json:"pinned,omitempty"`
	// StrataVersion is the controller's; IRGenerator the version that
	// evaluated the configuration.
	StrataVersion string        `json:"strataVersion"`
	IRGenerator   string        `json:"irGenerator,omitempty"`
	PlanID        string        `json:"planId,omitempty"`
	Plan          *planner.Plan `json:"plan,omitempty"`
	// ApprovedBy is "uid:N" of the approver, or "auto".
	ApprovedBy string `json:"approvedBy,omitempty"`
	// Subject is the pinned commit or the adopted VM.
	Subject     string             `json:"subject,omitempty"`
	CreatedAt   time.Time          `json:"createdAt"`
	CompletedAt time.Time          `json:"completedAt,omitzero"`
	Transitions []Transition       `json:"transitions"`
	Result      *reconciler.Result `json:"result,omitempty"`
	Error       string             `json:"error,omitempty"`
}

// Finished reports whether the deployment is in a final state.
func (d *Deployment) Finished() bool {
	return d.State == StateSucceeded || d.State == StateFailed || d.State == StateCancelled
}

// InProgress reports whether the deployment was approved and is running.
func (d *Deployment) InProgress() bool { return !d.Finished() && d.State != StateAwaitingApproval }

// transition records a state change.
func (d *Deployment) transition(state, note string, at time.Time) {
	d.State = state
	d.Transitions = append(d.Transitions, Transition{State: state, At: at, Note: note})
	if d.Finished() {
		d.CompletedAt = at
	}
}

// State is what the controller keeps across restarts.
type State struct {
	LastApplied string `json:"lastApplied,omitempty"`
	Pinned      string `json:"pinned,omitempty"`
}

// Store keeps deployment records (one JSON file each) and the state in a
// directory (M3 decision 7: files until PostgreSQL arrives in M5).
type Store struct {
	Dir string
}

var idPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}$`)

// NewID returns a new deployment ID, sortable by time.
func NewID(at time.Time) (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("deployment ID: %w", err)
	}
	return at.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:]), nil
}

func (s *Store) deployments() string { return filepath.Join(s.Dir, "deployments") }

// writeAtomic writes data to path through a temporary file and a rename.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	_, werr := tmp.Write(data)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Save writes a deployment record.
func (s *Store) Save(d *Deployment) error {
	if !idPattern.MatchString(d.ID) {
		return fmt.Errorf("deployment ID %q is not valid", d.ID)
	}
	if err := os.MkdirAll(s.deployments(), 0o700); err != nil {
		return fmt.Errorf("deployments: %w", err)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("encode deployment %s: %w", d.ID, err)
	}
	return writeAtomic(filepath.Join(s.deployments(), d.ID+".json"), append(data, '\n'))
}

// Load reads every deployment record, oldest first.
func (s *Store) Load() ([]*Deployment, error) {
	entries, err := os.ReadDir(s.deployments())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("deployments: %w", err)
	}
	var out []*Deployment
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !idPattern.MatchString(id) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.deployments(), e.Name()))
		if err != nil {
			return nil, fmt.Errorf("deployment %s: %w", id, err)
		}
		var d Deployment
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("deployment %s: %w", id, err)
		}
		out = append(out, &d)
	}
	slices.SortFunc(out, func(a, b *Deployment) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// Prune removes the oldest finished records beyond the newest keep
// finished ones. Records awaiting approval or in progress are never
// removed. keep <= 0 keeps everything.
func (s *Store) Prune(all []*Deployment, keep int) ([]*Deployment, error) {
	if keep <= 0 {
		return all, nil
	}
	var finished []*Deployment
	for _, d := range all {
		if d.Finished() {
			finished = append(finished, d)
		}
	}
	if len(finished) <= keep {
		return all, nil
	}
	drop := map[string]bool{}
	for _, d := range finished[:len(finished)-keep] {
		if err := os.Remove(filepath.Join(s.deployments(), d.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return all, fmt.Errorf("prune deployment %s: %w", d.ID, err)
		}
		drop[d.ID] = true
	}
	return slices.DeleteFunc(slices.Clone(all), func(d *Deployment) bool { return drop[d.ID] }), nil
}

// LoadState reads the controller's state (empty if there is none yet).
func (s *Store) LoadState() (State, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("state: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("state: %w", err)
	}
	return st, nil
}

// SaveState writes the controller's state.
func (s *Store) SaveState(st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	return writeAtomic(filepath.Join(s.Dir, "state.json"), append(data, '\n'))
}
