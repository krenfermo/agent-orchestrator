package controllers_test

// Checkpoint 8P-E13A.1: cancellation, proven at the boundary the user actually
// touches.
//
// The previous checkpoint's cancellation tests all ran against fakes inside the
// workflow package: a fake BranchLocks, a fake wake scheduler, a fake store.
// Every one of them passed while the real system still stranded a branch,
// because the two defects were in exactly the layers those fakes replaced — the
// real SQLite wake upsert, and the ordering of real writes inside CancelRun.
//
// So these tests drive POST /api/v1/workflows/{id}/cancel over the real HTTP
// router, the real workflow service, the real coordinator, the real
// branchlock.Manager and a real SQLite store. Nothing about the branch lock or
// the wake queue is simulated.

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// realBranchLocks is the composition-root adapter from internal/daemon,
// duplicated here because that wiring is unexported. It is the same manager the
// daemon runs.
type realBranchLocks struct{ mgr *branchlock.Manager }

func (w realBranchLocks) Acquire(ctx context.Context, req workflowcore.BranchLockRequest) ([]domain.BranchLock, error) {
	return w.mgr.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: req.ProjectID, RunID: req.RunID, StepID: req.StepID, SessionID: req.SessionID,
	})
}

func (w realBranchLocks) ReleaseRun(ctx context.Context, runID, reason string) (int64, error) {
	return w.mgr.ReleaseRun(ctx, runID, reason)
}

func (w realBranchLocks) HeldByRun(ctx context.Context, runID string) ([]domain.BranchLock, error) {
	return w.mgr.HeldByRun(ctx, runID)
}

func (w realBranchLocks) Renew(ctx context.Context, runID, stepID, sessionID string) {
	w.mgr.Renew(ctx, runID, stepID, sessionID)
}

func (w realBranchLocks) RecoverStale(ctx context.Context, runID string) (int64, error) {
	return w.mgr.RecoverStale(ctx, runID)
}

type realLockClassifier struct{ coord *workflowcore.Coordinator }

func (c realLockClassifier) ClassifyLockOwner(ctx context.Context, run domain.WorkflowRun) (branchlock.OwnerDisposition, error) {
	disp, err := c.coord.ClassifyLockOwner(ctx, run)
	if err != nil {
		return branchlock.OwnerDisposition{}, err
	}
	return branchlock.OwnerDisposition{
		SelfRemediable: disp.SelfRemediable, ProtectsWork: disp.ProtectsWork, Reason: disp.Reason,
	}, nil
}

type cancelFixture struct {
	store *sqlite.Store
	mgr   *branchlock.Manager
	coord *workflowcore.Coordinator
	wake  *wake.Scheduler
	now   time.Time
}

const (
	cancelFixtureRepo   = "/repos/agent-orchestrator"
	cancelFixtureBranch = "feat/engineering-control-center"
)

func newIDSeq(prefix string) func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("%s%d", prefix, n)
	}
}

// newCancelStack wires the production stack over a fresh store, with one
// direct-branch project whose branch is the one the real incident contended.
func newCancelStack(t *testing.T) *cancelFixture {
	t.Helper()
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "agent-orchestrator", Path: cancelFixtureRepo, RegisteredAt: now,
		Config: domain.ProjectConfig{DefaultBranch: cancelFixtureBranch, ExecutionMode: domain.ExecutionDirectBranch},
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "other", Path: "/repos/other", RegisteredAt: now,
		Config: domain.ProjectConfig{DefaultBranch: "main", ExecutionMode: domain.ExecutionDirectBranch},
	}); err != nil {
		t.Fatalf("seed other project: %v", err)
	}

	// Preflight nil: these tests own no real repositories, and the dirty-tree
	// gate is not what is under test here.
	mgr := branchlock.New(branchlock.Deps{Store: store, OwnerToken: "owner-test", NewID: newIDSeq("bl"), Clock: clock})
	wakeSched := wake.New(store, clock, newIDSeq("wk"), wake.Config{})
	coord := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store, SessionFacts: store, ReviewRuns: store,
		BranchLocks: realBranchLocks{mgr: mgr}, WakeScheduler: wakeSched,
		Clock: clock, NewID: newIDSeq("id"),
	})
	mgr.SetClassifier(realLockClassifier{coord: coord})

	return &cancelFixture{store: store, mgr: mgr, coord: coord, wake: wakeSched, now: now}
}

// stoppedHolder reproduces wf-3220567f: a run that owns the branch and is
// parked in needs_attention on an exhausted fix budget.
func (f *cancelFixture) stoppedHolder(t *testing.T, projectID string) string {
	t.Helper()
	ctx := context.Background()
	created, err := f.coord.CreateRun(ctx, projectID, "the workflow holding the branch")
	if err != nil {
		t.Fatalf("CreateRun holder: %v", err)
	}
	if _, err := f.mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: domain.ProjectID(projectID), RunID: created.Run.ID}); err != nil {
		t.Fatalf("acquire branch: %v", err)
	}
	if _, err := f.store.UpdateWorkflowRunState(ctx, created.Run.ID, domain.WorkflowRunPending, domain.WorkflowRunNeedsAttention, f.now); err != nil {
		t.Fatalf("park holder: %v", err)
	}
	if _, err := f.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-holder-" + created.Run.ID, WorkflowRunID: created.Run.ID, ProjectID: projectID,
		NextAction: "the review/fix budget is exhausted", DurablePhase: workflowcore.ReasonFixBudgetExhausted,
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: f.now,
	}); err != nil {
		t.Fatalf("record holder stop: %v", err)
	}
	return created.Run.ID
}

func (f *cancelFixture) heldLock(t *testing.T, runID string) (domain.BranchLock, bool) {
	t.Helper()
	locks, err := f.store.ListHeldBranchLocksByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListHeldBranchLocksByRun: %v", err)
	}
	if len(locks) == 0 {
		return domain.BranchLock{}, false
	}
	return locks[0], true
}

func (f *cancelFixture) lockRow(t *testing.T, id string) domain.BranchLock {
	t.Helper()
	lock, ok, err := f.store.GetBranchLock(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("GetBranchLock(%q): %v (found=%v)", id, err, ok)
	}
	return lock
}

// A) The real incident: cancelling the stopped owner from the API frees the
// branch, durably and with a truthful reason.
func TestCancelOverHTTPReleasesTheHeldBranchLock(t *testing.T) {
	fx := newCancelStack(t)
	srv := newWorkflowTestServer(t, workflowsvc.New(fx.coord))
	holder := fx.stoppedHolder(t, "agent-orchestrator")
	lock, ok := fx.heldLock(t, holder)
	if !ok {
		t.Fatal("fixture did not reach the state under test: holder owns no lock")
	}

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/"+holder+"/cancel", "")
	if status != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", status, body)
	}

	run, _, err := fx.store.GetWorkflowRun(context.Background(), holder)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.State != domain.WorkflowRunCancelled {
		t.Fatalf("run state = %q, want cancelled", run.State)
	}
	after := fx.lockRow(t, lock.ID)
	if after.State != domain.BranchLockReleased {
		t.Fatalf("lock state = %q, want released", after.State)
	}
	if after.ReleasedAt == nil {
		t.Fatal("released_at is NULL on a released lock")
	}
	if after.ReleaseReason == "" {
		t.Fatal("release_reason is empty: a release must say why it happened")
	}
	// And the branch is genuinely free for the next run.
	if _, err := fx.mgr.Acquire(context.Background(), branchlock.AcquireRequest{
		ProjectID: "agent-orchestrator", RunID: "wf-successor",
	}); err != nil {
		t.Fatalf("acquire after cancellation: %v", err)
	}
}

// B) The legacy impossible state, repaired at startup: a terminal run whose
// lock was never released (every such row on disk today), and a second pass
// that must change nothing.
func TestStartupReconcileReleasesALegacyTerminalOwnersLockIdempotently(t *testing.T) {
	fx := newCancelStack(t)
	ctx := context.Background()
	holder := fx.stoppedHolder(t, "agent-orchestrator")
	lock, _ := fx.heldLock(t, holder)
	// Terminal run, lock still held: exactly the row shape the field report
	// describes. Written directly, bypassing CancelRun, because the point is a
	// row that predates the release path.
	if _, err := fx.store.UpdateWorkflowRunState(ctx, holder, domain.WorkflowRunNeedsAttention, domain.WorkflowRunCancelled, fx.now); err != nil {
		t.Fatalf("terminate holder: %v", err)
	}

	result, err := fx.mgr.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Released != 1 {
		t.Fatalf("reconcile = %#v, want the legacy lock released", result)
	}
	if fx.lockRow(t, lock.ID).State != domain.BranchLockReleased {
		t.Fatal("legacy lock still held after reconciliation")
	}

	second, err := fx.mgr.Reconcile(ctx)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if second.Released != 0 || second.Adopted != 0 || second.Kept != 0 {
		t.Fatalf("second reconcile = %#v, want a no-op", second)
	}
}

// C) The successor, end to end: it is queued on the same repo+branch with a
// branch_lock wake that has ALREADY completed once — the exact shape found in
// ~/.ao/data, and the one the old wake upsert could never revive.
func TestCancelWakesTheSuccessorQueuedOnTheSameBranch(t *testing.T) {
	fx := newCancelStack(t)
	ctx := context.Background()
	srv := newWorkflowTestServer(t, workflowsvc.New(fx.coord))
	holder := fx.stoppedHolder(t, "agent-orchestrator")

	successor, err := fx.coord.CreateRun(ctx, "agent-orchestrator", "the queued successor")
	if err != nil {
		t.Fatalf("CreateRun successor: %v", err)
	}
	if _, err := fx.store.UpdateWorkflowRunState(ctx, successor.Run.ID, domain.WorkflowRunPending, domain.WorkflowRunWaiting, fx.now); err != nil {
		t.Fatalf("park successor: %v", err)
	}
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-branch-wait", WorkflowRunID: successor.Run.ID, ProjectID: "agent-orchestrator",
		Branch: cancelFixtureBranch, WorktreePath: cancelFixtureRepo,
		NextAction:   "waiting_for_branch: held by " + holder,
		DurablePhase: "waiting_for_branch", PayloadVersion: "v1",
		RetryState: `{"branch":"` + cancelFixtureBranch + `","repoPath":"` + cancelFixtureRepo +
			`","heldByWorkflowRunId":"` + holder + `"}`,
		CreatedAt: fx.now,
	}); err != nil {
		t.Fatalf("record successor wait: %v", err)
	}
	sched, err := fx.wake.Schedule(ctx, domain.WorkflowRunID(successor.Run.ID), nil, wake.ReasonBranchLock, nil)
	if err != nil {
		t.Fatalf("schedule successor wake: %v", err)
	}
	if _, err := fx.store.ClaimWorkflowWakeSchedule(ctx, sched.ID, "pending", "poller", fx.now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := fx.store.CompleteWorkflowWakeSchedule(ctx, sched.ID, fx.now); err != nil {
		t.Fatalf("complete: %v", err)
	}

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/"+holder+"/cancel", "")
	if status != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", status, body)
	}

	next, err := fx.wake.NextForRun(ctx, domain.WorkflowRunID(successor.Run.ID))
	if err != nil {
		t.Fatalf("NextForRun: %v", err)
	}
	if next == nil {
		t.Fatal("the successor has no open wake: the freed branch would never be noticed")
	}
	if next.Reason != wake.ReasonBranchLock {
		t.Fatalf("wake reason = %q, want branch_lock", next.Reason)
	}
	if next.ScheduledAt.After(fx.now) {
		t.Fatalf("wake scheduled at %v, want due now (%v): the branch is already free", next.ScheduledAt, fx.now)
	}
	due, err := fx.store.ListDueWorkflowWakeSchedules(ctx, fx.now, fx.now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("ListDueWorkflowWakeSchedules: %v", err)
	}
	if len(due) != 1 || due[0].WorkflowRunID != successor.Run.ID {
		t.Fatalf("due wakes = %+v, want exactly the successor's", due)
	}
	// And on that resume the branch is genuinely available to it.
	if _, err := fx.mgr.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: "agent-orchestrator", RunID: successor.Run.ID,
	}); err != nil {
		t.Fatalf("successor acquire after release: %v", err)
	}
}

// D) Cancelling one workflow must never free somebody else's branch.
func TestCancelLeavesAnotherRunningWorkflowsLockAlone(t *testing.T) {
	fx := newCancelStack(t)
	ctx := context.Background()
	srv := newWorkflowTestServer(t, workflowsvc.New(fx.coord))
	holder := fx.stoppedHolder(t, "agent-orchestrator")

	live, err := fx.coord.CreateRun(ctx, "other", "a genuinely running workflow")
	if err != nil {
		t.Fatalf("CreateRun live: %v", err)
	}
	if _, err := fx.mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "other", RunID: live.Run.ID}); err != nil {
		t.Fatalf("acquire live: %v", err)
	}
	if _, err := fx.store.UpdateWorkflowRunState(ctx, live.Run.ID, domain.WorkflowRunPending, domain.WorkflowRunRunning, fx.now); err != nil {
		t.Fatalf("start live: %v", err)
	}

	if _, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/"+holder+"/cancel", ""); status != http.StatusOK {
		t.Fatalf("cancel status=%d", status)
	}

	if _, ok := fx.heldLock(t, live.Run.ID); !ok {
		t.Fatal("cancelling one workflow released a live workflow's branch lock")
	}
	if _, ok := fx.heldLock(t, holder); ok {
		t.Fatal("the cancelled workflow still holds its branch")
	}
}
