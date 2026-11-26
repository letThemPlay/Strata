package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/letthemplay/Strata/internal/planner"
	"github.com/letthemplay/Strata/internal/workload"
)

func states(d Deployment) string {
	var s []string
	for _, t := range d.Transitions {
		s = append(s, t.State)
	}
	return strings.Join(s, ",")
}

// applied syncs, approves the current plan as uid 1000 and waits.
func applied(t *testing.T, c *Controller) Deployment {
	t.Helper()
	c.Sync(context.Background())
	p, err := c.Plan()
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.Apply(p.Plan.ID, 1000, true)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	c.Wait()
	got, err := c.Deployment(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestProposalIsRecordedOnce(t *testing.T) {
	c, _, _, _ := newController(t)
	ctx := context.Background()
	c.Sync(ctx)
	c.Sync(ctx)
	h := c.History(0)
	if len(h) != 1 || h[0].State != StateAwaitingApproval || h[0].Kind != KindPlan || h[0].Items != 3 {
		t.Fatalf("history %+v", h)
	}
	p, _ := c.Plan()
	if p.Deployment != h[0].ID || c.Status().Awaiting == nil {
		t.Errorf("plan %+v status %+v", p, c.Status())
	}
	d, err := c.Deployment(h[0].ID)
	if err != nil || d.Commit != c1 || d.Signer != "alice" || d.FlakeLock != "lock1" || d.StrataVersion != "test" ||
		d.IRGenerator != "0.1.0-dev" || d.Node != "hv-01" || d.Repository == "" || d.Plan == nil {
		t.Errorf("record %+v %v", d, err)
	}
}

func TestApplyRecordsTheDeployment(t *testing.T) {
	c, _, _, node := newController(t)
	d := applied(t, c)
	if d.State != StateSucceeded || d.ApprovedBy != "uid:1000" || d.Error != "" {
		t.Fatalf("deployment %+v", d)
	}
	if got := states(d); got != "AWAITING_APPROVAL,PENDING,VALIDATING,PLANNING,APPLYING,VERIFYING,SUCCEEDED" {
		t.Errorf("states %s", got)
	}
	if d.Result == nil || !d.Result.Converged || len(d.Result.Items) != 3 || d.CompletedAt.IsZero() {
		t.Errorf("result %+v", d.Result)
	}
	for _, v := range node.vms {
		if v.Owner != workload.OwnerGitOps || v.Status != "Running" {
			t.Errorf("vm %+v", v)
		}
	}
	st := c.Status()
	if st.LastApplied != c1 || st.Running != nil || st.Awaiting != nil || !st.Plan.Converged {
		t.Errorf("status %+v", st)
	}
	// The state survives a restart.
	saved, err := c.Store.LoadState()
	if err != nil || saved.LastApplied != c1 {
		t.Errorf("state %+v %v", saved, err)
	}
}

// A client polling the record (as the CLI does) sees the new plan as soon
// as the record is final.
func TestFinishedRecordMeansCurrentPlan(t *testing.T) {
	c, _, _, _ := newController(t)
	c.Sync(context.Background())
	p, _ := c.Plan()
	d, err := c.Apply(p.Plan.ID, 1000, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Wait()
	for {
		got, err := c.Deployment(d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Finished() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if st := c.Status(); st.LastApplied != c1 || !st.Plan.Converged || st.Running != nil {
		t.Errorf("status %+v", st)
	}
}

func TestDeleteNeedsAllowDelete(t *testing.T) {
	c, repo, ev, node := newController(t)
	applied(t, c)
	node.mu.Lock()
	for i := range node.vms {
		node.vms[i].Protected = false
	}
	node.mu.Unlock()
	empty := strings.Split(string(golden(t)), `"resources": [`)[0] + `"resources": []}`
	ev.irs[c2] = []byte(empty)
	repo.head = c2
	c.Sync(context.Background())
	p, err := c.Plan()
	if err != nil || !p.Plan.Deletes() {
		t.Fatalf("plan %+v %v", p, err)
	}
	if _, err := c.Apply(p.Plan.ID, 1000, false); !errors.Is(err, ErrAllowDelete) {
		t.Fatalf("delete without --allow-delete: %v", err)
	}
	if d, err := c.Apply(p.Plan.ID, 1000, true); err != nil || d.State != StatePending {
		t.Fatalf("%+v %v", d, err)
	}
	c.Wait()
	if len(node.vms) != 0 {
		t.Errorf("vms %+v", node.vms)
	}
}

func TestStaleApprovalFails(t *testing.T) {
	c, _, _, node := newController(t)
	c.Sync(context.Background())
	p, _ := c.Plan()
	// The node changes after the plan was proposed.
	node.mu.Lock()
	node.vms = append(node.vms, workload.VMStatus{Spec: workload.VM{Name: "manual"}, Meta: workload.Meta{Owner: workload.OwnerCLI}})
	node.mu.Unlock()
	d, err := c.Apply(p.Plan.ID, 1000, false)
	if err != nil {
		t.Fatal(err)
	}
	c.Wait()
	got, _ := c.Deployment(d.ID)
	if got.State != StateFailed || !strings.Contains(got.Error, "changed since plan") || states(got) != "AWAITING_APPROVAL,PENDING,VALIDATING,PLANNING,FAILED" {
		t.Errorf("deployment %+v (%s)", got, states(got))
	}
	if slices.ContainsFunc(node.calls, func(c string) bool { return strings.HasPrefix(c, "create") }) {
		t.Errorf("a stale plan was applied: %v", node.calls)
	}
	// The new plan awaits approval.
	if st := c.Status(); st.Awaiting == nil || st.Awaiting.PlanID == p.Plan.ID {
		t.Errorf("status %+v", st)
	}
}

func TestReplacedPlanIsCancelled(t *testing.T) {
	c, repo, ev, _ := newController(t)
	c.Sync(context.Background())
	first := c.History(0)[0]
	ev.irs[c2] = []byte(strings.Replace(string(golden(t)), `"cpu": 4`, `"cpu": 3`, 1))
	repo.head = c2
	c.Sync(context.Background())
	h := c.History(0)
	if len(h) != 2 || h[1].ID != first.ID || h[1].State != StateCancelled || h[0].State != StateAwaitingApproval || h[0].Commit != c2 {
		t.Errorf("history %+v", h)
	}
	// An unsigned head: the awaiting plan is cancelled, nothing awaits.
	repo.head, repo.untrusted[c3] = c3, true
	c.Sync(context.Background())
	if h := c.History(0); h[0].State != StateCancelled || c.Status().Awaiting != nil {
		t.Errorf("history %+v", h)
	}
}

func TestAutoApplySafePlansOnly(t *testing.T) {
	c, repo, ev, node := newController(t)
	c.Config.AutoApply = "safe"
	c.Sync(context.Background())
	c.Wait()
	h := c.History(0)
	if len(h) != 1 || h[0].State != StateSucceeded || h[0].ApprovedBy != "auto" || len(node.vms) != 3 {
		t.Fatalf("history %+v vms %d", h, len(node.vms))
	}
	// A restart is not safe: it waits for approval.
	ev.irs[c2] = []byte(strings.Replace(string(golden(t)), `"cpu": 2`, `"cpu": 3`, 1))
	repo.head = c2
	c.Sync(context.Background())
	c.Wait()
	if h := c.History(0); h[0].State != StateAwaitingApproval {
		t.Errorf("a restart was applied automatically: %+v", h[0])
	}
}

func TestInitMarksInterruptedDeployments(t *testing.T) {
	c, _, _, _ := newController(t)
	store := c.Store
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for i, state := range []string{StateApplying, StateAwaitingApproval, StateSucceeded} {
		id, _ := NewID(now.Add(time.Duration(i) * time.Second))
		d := &Deployment{ID: id, Kind: KindPlan, CreatedAt: now}
		d.transition(state, "", now)
		if err := store.Save(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveState(State{LastApplied: c1}); err != nil {
		t.Fatal(err)
	}
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := c.History(0)
	if len(h) != 3 || h[2].State != StateFailed || !strings.Contains(h[2].Error, "interrupted") || h[1].State != StateCancelled || h[0].State != StateSucceeded {
		t.Errorf("history %+v", h)
	}
	if c.Status().LastApplied != c1 {
		t.Error("state not loaded")
	}
	reloaded, _ := store.Load()
	if reloaded[0].State != StateFailed {
		t.Errorf("not saved: %+v", reloaded[0])
	}
}

func TestPinAndUnpin(t *testing.T) {
	c, repo, ev, _ := newController(t)
	repo.head = c2
	applied(t, c) // c2 applied
	// c1 is older: following the branch, it would be a rewind; pinned,
	// it is a rollback.
	// (autostart: no restart; db-01 is protected and running.)
	ev.irs[c1] = []byte(strings.Replace(string(golden(t)), `"autostart": true`, `"autostart": false`, 1))
	st, err := c.Pin(context.Background(), c1[:8], 1000)
	if err != nil || st.Pinned != c1 || st.Evaluated != c1 || st.Error != "" || st.Awaiting == nil {
		t.Fatalf("pin: %+v %v", st, err)
	}
	d := applied(t, c)
	if d.State != StateSucceeded || !d.Pinned || d.Commit != c1 {
		t.Fatalf("rollback %+v", d)
	}
	// While pinned, new branch commits are fetched but not planned.
	repo.head = c3
	c.Sync(context.Background())
	if st := c.Status(); st.Head != c3 || st.Evaluated != c1 {
		t.Errorf("pinned status %+v", st)
	}
	st, err = c.Unpin(context.Background(), 1000)
	if err != nil || st.Pinned != "" || st.Evaluated != c3 {
		t.Errorf("unpin: %+v %v", st, err)
	}
	h := c.History(0)
	kinds := []string{}
	for _, d := range h {
		kinds = append(kinds, d.Kind)
	}
	if !slices.Contains(kinds, KindPin) || !slices.Contains(kinds, KindUnpin) {
		t.Errorf("history kinds %v", kinds)
	}
	if _, err := c.Pin(context.Background(), "HEAD", 1000); !errors.Is(err, ErrInvalidAction) {
		t.Errorf("ref name: %v", err)
	}
	repo.untrusted[c2] = true
	if _, err := c.Pin(context.Background(), c2, 1000); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Errorf("untrusted pin: %v", err)
	}
	if _, err := c.Unpin(context.Background(), 1000); !errors.Is(err, ErrInvalidAction) {
		t.Errorf("unpin without pin: %v", err)
	}
}

func TestAdopt(t *testing.T) {
	c, _, _, node := newController(t)
	spec := workload.VM{
		APIVersion: workload.VMAPIVersion, Name: "scratch", Resources: workload.Resources{CPU: 1, Memory: "1GiB"},
		Disks: []workload.Disk{{Name: "root", Size: "10GiB", Pool: "fast"}},
	}
	node.vms = []workload.VMStatus{
		{Spec: spec, Meta: workload.Meta{Owner: workload.OwnerCLI}, Status: "Running"},
		{Spec: workload.VM{Name: "manual"}, Meta: workload.Meta{Owner: workload.OwnerCLI}},
	}
	c.Sync(context.Background())
	if _, err := c.Adopt(context.Background(), "manual", 1000); !errors.Is(err, ErrInvalidAction) || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("undeclared: %v", err)
	}
	if _, err := c.Adopt(context.Background(), "scratch", 1000); err != nil {
		t.Fatal(err)
	}
	if node.vms[0].Owner != workload.OwnerGitOps {
		t.Errorf("owner %+v", node.vms[0])
	}
	p, _ := c.Plan()
	if slices.ContainsFunc(p.Plan.Items, func(it planner.Item) bool { return it.Resource == "vm/scratch" }) {
		t.Errorf("an adopted VM matching its declaration has nothing to do: %+v", p.Plan.Items)
	}
	if _, err := c.Adopt(context.Background(), "scratch", 1000); !errors.Is(err, ErrInvalidAction) {
		t.Errorf("twice: %v", err)
	}
	var adoptions []string
	for _, d := range c.History(0) {
		if d.Kind == KindAdopt {
			adoptions = append(adoptions, d.Subject+" "+d.State)
		}
	}
	if strings.Join(adoptions, ",") != "scratch FAILED,scratch SUCCEEDED,manual FAILED" {
		t.Errorf("adoptions %v", adoptions)
	}
}

func TestPruning(t *testing.T) {
	c, repo, ev, _ := newController(t)
	c.Config.KeepDeployments = 2
	for i, sha := range []string{c1, c2, c3} {
		ev.irs[sha] = []byte(strings.Replace(string(golden(t)), `"cpu": 1,`, `"cpu": `+string(rune('1'+i))+`,`, 1))
		repo.head = sha
		applied(t, c)
	}
	recs, err := c.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	finished := 0
	for _, d := range recs {
		if d.Finished() {
			finished++
		}
	}
	if finished != 2 || len(c.History(0)) != len(recs) {
		t.Errorf("%d finished records kept, %d in history, %d on disk", finished, len(c.History(0)), len(recs))
	}
}

func TestStore(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	if st, err := s.LoadState(); err != nil || st != (State{}) {
		t.Errorf("empty state %+v %v", st, err)
	}
	if recs, err := s.Load(); err != nil || len(recs) != 0 {
		t.Errorf("empty %v %v", recs, err)
	}
	if err := s.Save(&Deployment{ID: "../../etc/x"}); err == nil {
		t.Error("bad ID saved")
	}
	id, err := NewID(time.Now())
	if err != nil || !idPattern.MatchString(id) {
		t.Fatalf("id %q %v", id, err)
	}
}

// A new commit the node already matches is recorded as applied without
// approval, so a later rewind past it is refused.
func TestConvergedCommitIsRecordedAsApplied(t *testing.T) {
	c, repo, _, _ := newController(t)
	applied(t, c)
	repo.head = c2 // declares the same as c1
	c.Sync(context.Background())
	st := c.Status()
	if st.LastApplied != c2 || st.Awaiting != nil || !st.Plan.Converged {
		t.Fatalf("status %+v", st)
	}
	var rec []DeploymentSummary
	for _, d := range c.History(0) {
		if d.Commit == c2 {
			rec = append(rec, d)
		}
	}
	if len(rec) != 1 || rec[0].Kind != KindPlan || rec[0].State != StateSucceeded || rec[0].ApprovedBy != "auto" {
		t.Fatalf("records for c2 %+v", rec)
	}
	d, err := c.Deployment(rec[0].ID)
	if err != nil || len(d.Transitions) != 1 || !strings.Contains(d.Transitions[0].Note, "already matches") || d.Plan == nil {
		t.Errorf("record %+v %v", d, err)
	}
	if saved, err := c.Store.LoadState(); err != nil || saved.LastApplied != c2 {
		t.Errorf("state %+v %v", saved, err)
	}
	// Not recorded again on the next sync.
	n := len(c.History(0))
	c.Sync(context.Background())
	if len(c.History(0)) != n {
		t.Errorf("recorded again: %+v", c.History(0))
	}
	// Rewinding to c1 is now refused.
	repo.head = c1
	c.Sync(context.Background())
	if st := c.Status(); !strings.Contains(st.Error, "does not descend") {
		t.Errorf("rewind accepted: %+v", st)
	}
}

// A first commit that declares nothing is recorded as applied.
func TestEmptyFirstCommitIsRecordedAsApplied(t *testing.T) {
	c, _, ev, _ := newController(t)
	ev.irs[c1] = []byte(strings.Split(string(golden(t)), `"resources": [`)[0] + `"resources": []}`)
	c.Sync(context.Background())
	if st := c.Status(); st.Error != "" || st.LastApplied != c1 || !st.Plan.Converged || st.Awaiting != nil {
		t.Fatalf("status %+v", st)
	}
	if h := c.History(0); len(h) != 1 || h[0].ApprovedBy != "auto" || h[0].State != StateSucceeded {
		t.Errorf("history %+v", h)
	}
}
