package workflow_test

// Cancel-and-archive: the Board action that retires a workflow the incident
// behind it has outlived, without deleting a row.
//
// Every test here is written against the shape that produced the requirement:
// a master run parked in needs_attention (child_failed,
// master_integration_promotion_failed, a cancelled child chain), which is not
// terminal and therefore never aged out of the Board's completion retention.

import (
	stdctx "context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// --- fakeStore's ArchiveStore half -------------------------------------------------
//
// Kept here rather than in workflow_test.go so the archive surface reads as one
// unit. Mirrors the real store exactly on the two rules that matter: the marker
// is write-once (a second archive keeps the original timestamp) and it is only
// ever set on a terminal run.

func (f *fakeStore) ArchiveWorkflowRun(_ stdctx.Context, id string, now time.Time) (bool, error) {
	run, ok := f.runs[id]
	if !ok || run.ArchivedAt != nil || !run.State.Terminal() {
		return false, nil
	}
	run.ArchivedAt = &now
	run.UpdatedAt = now
	f.runs[id] = run
	return true, nil
}

func (f *fakeStore) ListChildWorkflowRuns(_ stdctx.Context, parentRunID string) ([]domain.WorkflowRun, error) {
	var out []domain.WorkflowRun
	for _, run := range f.runs {
		if run.ParentWorkflowID != nil && *run.ParentWorkflowID == parentRunID {
			out = append(out, run)
		}
	}
	sortRunsByID(out)
	return out, nil
}

func (f *fakeStore) ListArchivedWorkflowRuns(_ stdctx.Context, projectID string, limit int) ([]domain.WorkflowRun, error) {
	var out []domain.WorkflowRun
	for _, run := range f.runs {
		if run.ProjectID != projectID || run.ParentWorkflowID != nil || run.ArchivedAt == nil {
			continue
		}
		out = append(out, run)
	}
	sortRunsByID(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func sortRunsByID(runs []domain.WorkflowRun) {
	for i := 1; i < len(runs); i++ {
		for j := i; j > 0 && runs[j].ID < runs[j-1].ID; j-- {
			runs[j], runs[j-1] = runs[j-1], runs[j]
		}
	}
}

// --- fixture -----------------------------------------------------------------------

type archiveFixture struct {
	c     *workflowcore.Coordinator
	store *fakeStore
	locks *fakeBranchLocks
	wakes *fakeWakeScheduler
	now   time.Time
}

func newArchiveFixture(t *testing.T) *archiveFixture {
	t.Helper()
	store := newFakeStore()
	locks := newFakeBranchLocks()
	wakes := newFakeWakeScheduler()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:         store,
		Projects:      fakeProjects{"proj": directProject("proj", contendedRepo, contendedBranch)},
		BranchLocks:   locks,
		WakeScheduler: wakes,
		Clock:         func() time.Time { return now },
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("arch%d", idSeq)
		},
	})
	return &archiveFixture{c: c, store: store, locks: locks, wakes: wakes, now: now}
}

// seedRun creates a run and parks it in the requested state, which is how every
// stale Board card in the report was found: not terminal, not moving.
func (f *archiveFixture) seedRun(t *testing.T, objective string, state domain.WorkflowRunState) string {
	t.Helper()
	detail, err := f.c.CreateRun(stdctx.Background(), "proj", objective)
	if err != nil {
		t.Fatalf("CreateRun %q: %v", objective, err)
	}
	f.park(t, detail.Run.ID, state)
	return detail.Run.ID
}

func (f *archiveFixture) park(t *testing.T, runID string, state domain.WorkflowRunState) {
	t.Helper()
	if state == domain.WorkflowRunPending {
		return
	}
	// pending has no direct edge to completed/failed — the run state machine
	// requires it to have run first. Walk the same path a real run takes.
	path := []domain.WorkflowRunState{state}
	if state == domain.WorkflowRunCompleted || state == domain.WorkflowRunFailed {
		path = []domain.WorkflowRunState{domain.WorkflowRunRunning, state}
	}
	for _, next := range path {
		run := f.store.runs[runID]
		if run.State == next {
			continue
		}
		moved, err := f.store.UpdateWorkflowRunState(stdctx.Background(), runID, run.State, next, f.now)
		if err != nil || !moved {
			t.Fatalf("park run %s as %s (via %s): moved=%v err=%v", runID, state, next, moved, err)
		}
	}
}

// seedChild attaches a child run to a master, the way a master's task dispatch
// does: same project, parent link, own steps.
func (f *archiveFixture) seedChild(t *testing.T, parentID, objective string, state domain.WorkflowRunState) string {
	t.Helper()
	id := "child-" + objective
	parent := parentID
	run := domain.WorkflowRun{
		ID: id, ProjectID: "proj", Objective: objective,
		State: domain.WorkflowRunPending, PolicyVersion: "v1", PolicySnapshot: "{}",
		CreatedAt: f.now, UpdatedAt: f.now, ParentWorkflowID: &parent,
	}
	steps := []domain.WorkflowStep{{
		ID: id + "-work", WorkflowRunID: id, Kind: domain.WorkflowStepWork, Ordinal: 1,
		State: domain.WorkflowStepRunning, CreatedAt: f.now, UpdatedAt: f.now, ArtifactJSON: "{}",
	}}
	if _, _, err := f.store.CreateWorkflowRun(stdctx.Background(), run, steps); err != nil {
		t.Fatalf("seed child %s: %v", id, err)
	}
	f.park(t, id, state)
	return id
}

func (f *archiveFixture) boardIDs(t *testing.T) []string {
	t.Helper()
	entries, err := f.c.ProjectBoard(stdctx.Background(), "proj", 30*time.Minute)
	if err != nil {
		t.Fatalf("ProjectBoard: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Run.ID)
	}
	return out
}

func containsRunID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func (f *archiveFixture) holdBranch(runID string) {
	key := domain.BranchLockKey(contendedRepo, contendedBranch)
	f.locks.held[key] = domain.BranchLock{
		ID: "bl-" + runID, LockKey: key, ProjectID: "proj",
		RepoPath: contendedRepo, Branch: contendedBranch,
		WorkflowRunID: runID, State: domain.BranchLockHeld, AcquiredAt: f.now,
	}
}

// --- A ------------------------------------------------------------------------------

// TestCancelAndArchiveRemovesNeedsAttentionMasterFromBoard is the headline case:
// the master is parked in needs_attention because its only child already ended
// badly, so nothing about the run will ever change again on its own — and yet
// the Board showed it indefinitely, because needs_attention is not terminal.
func TestCancelAndArchiveRemovesNeedsAttentionMasterFromBoard(t *testing.T) {
	f := newArchiveFixture(t)
	master := f.seedRun(t, "master with a dead child", domain.WorkflowRunNeedsAttention)
	f.seedChild(t, master, "already-failed", domain.WorkflowRunFailed)

	if got := f.boardIDs(t); !containsRunID(got, master) {
		t.Fatalf("precondition: expected the stale master on the Board, got %v", got)
	}

	detail, err := f.c.CancelAndArchiveRun(stdctx.Background(), master)
	if err != nil {
		t.Fatalf("CancelAndArchiveRun: %v", err)
	}
	if detail.Run.State != domain.WorkflowRunCancelled {
		t.Fatalf("run state = %s, want cancelled", detail.Run.State)
	}
	if !detail.Run.Archived() {
		t.Fatal("run reports itself as not archived after cancel-and-archive")
	}
	if got := f.boardIDs(t); containsRunID(got, master) {
		t.Fatalf("archived master still on the active Board: %v", got)
	}
}

// --- B ------------------------------------------------------------------------------

// TestCancelAndArchiveCascadesToNonTerminalChildren proves the cascade: a child
// left running would keep its branch and keep writing to the repository under an
// objective its parent has been retired from.
func TestCancelAndArchiveCascadesToNonTerminalChildren(t *testing.T) {
	f := newArchiveFixture(t)
	master := f.seedRun(t, "master with a live child", domain.WorkflowRunNeedsAttention)
	running := f.seedChild(t, master, "still-running", domain.WorkflowRunRunning)
	waiting := f.seedChild(t, master, "still-waiting", domain.WorkflowRunWaiting)
	done := f.seedChild(t, master, "already-completed", domain.WorkflowRunCompleted)

	if _, err := f.c.CancelAndArchiveRun(stdctx.Background(), master); err != nil {
		t.Fatalf("CancelAndArchiveRun: %v", err)
	}

	for _, id := range []string{running, waiting} {
		if got := f.store.runs[id].State; got != domain.WorkflowRunCancelled {
			t.Errorf("child %s state = %s, want cancelled", id, got)
		}
	}
	// A child that already finished is history, not something to re-terminate:
	// rewriting completed to cancelled would falsify the record of work that
	// really did complete.
	if got := f.store.runs[done].State; got != domain.WorkflowRunCompleted {
		t.Errorf("completed child %s was rewritten to %s", done, got)
	}
	// No orphan may still be holding a step open.
	for _, id := range []string{running, waiting} {
		for _, step := range f.store.steps[id] {
			if !step.State.Terminal() {
				t.Errorf("child %s step %s left in %s", id, step.ID, step.State)
			}
		}
	}
}

// --- C ------------------------------------------------------------------------------

// TestCancelAndArchiveReleasesBranchLocks pins the branch-lock guarantee for
// both the parent and a child holder, through the canonical lifecycle — the
// renderer never touches the lock table.
func TestCancelAndArchiveReleasesBranchLocks(t *testing.T) {
	t.Run("held by the master", func(t *testing.T) {
		f := newArchiveFixture(t)
		master := f.seedRun(t, "holds the branch", domain.WorkflowRunNeedsAttention)
		f.holdBranch(master)

		if _, err := f.c.CancelAndArchiveRun(stdctx.Background(), master); err != nil {
			t.Fatalf("CancelAndArchiveRun: %v", err)
		}
		held, err := f.locks.HeldByRun(stdctx.Background(), master)
		if err != nil {
			t.Fatalf("HeldByRun: %v", err)
		}
		if len(held) != 0 {
			t.Fatalf("branch still locked to the archived run: %+v", held)
		}
	})

	t.Run("held by a child", func(t *testing.T) {
		f := newArchiveFixture(t)
		master := f.seedRun(t, "master whose child holds the branch", domain.WorkflowRunNeedsAttention)
		child := f.seedChild(t, master, "branch-holder", domain.WorkflowRunRunning)
		f.holdBranch(child)

		if _, err := f.c.CancelAndArchiveRun(stdctx.Background(), master); err != nil {
			t.Fatalf("CancelAndArchiveRun: %v", err)
		}
		held, err := f.locks.HeldByRun(stdctx.Background(), child)
		if err != nil {
			t.Fatalf("HeldByRun: %v", err)
		}
		if len(held) != 0 {
			t.Fatalf("child kept the branch after its parent was archived: %+v", held)
		}
	})

	// A run that was ALREADY terminal never enters CancelRun, so its lock has
	// to be reclaimed by the archive path itself — otherwise the exact state
	// this feature exists to clean up (a dead run still owning a branch) would
	// survive the cleanup.
	t.Run("held by an already-terminal run", func(t *testing.T) {
		f := newArchiveFixture(t)
		stopped := f.seedRun(t, "failed but still holding", domain.WorkflowRunNeedsAttention)
		f.park(t, stopped, domain.WorkflowRunFailed)
		f.holdBranch(stopped)

		if _, err := f.c.CancelAndArchiveRun(stdctx.Background(), stopped); err != nil {
			t.Fatalf("CancelAndArchiveRun: %v", err)
		}
		held, err := f.locks.HeldByRun(stdctx.Background(), stopped)
		if err != nil {
			t.Fatalf("HeldByRun: %v", err)
		}
		if len(held) != 0 {
			t.Fatalf("terminal run kept the branch after archiving: %+v", held)
		}
		if f.store.runs[stopped].State != domain.WorkflowRunFailed {
			t.Fatalf("archiving rewrote a terminal run's state to %s", f.store.runs[stopped].State)
		}
		if !f.store.runs[stopped].Archived() {
			t.Fatal("an already-terminal run was not archived")
		}
	})
}

// --- D ------------------------------------------------------------------------------

// TestCancelAndArchiveCancelsPendingWakes proves nothing resurrects the run: a
// wake left pending would fire later and hand the run back to the dispatcher.
func TestCancelAndArchiveCancelsPendingWakes(t *testing.T) {
	f := newArchiveFixture(t)
	ctx := stdctx.Background()
	master := f.seedRun(t, "master with a pending retry", domain.WorkflowRunNeedsAttention)
	child := f.seedChild(t, master, "child-with-wake", domain.WorkflowRunWaiting)
	for _, id := range []string{master, child} {
		if _, err := f.wakes.Schedule(ctx, domain.WorkflowRunID(id), nil, wake.ReasonBranchLock, nil); err != nil {
			t.Fatalf("seed wake for %s: %v", id, err)
		}
	}

	if _, err := f.c.CancelAndArchiveRun(ctx, master); err != nil {
		t.Fatalf("CancelAndArchiveRun: %v", err)
	}

	for _, id := range []string{master, child} {
		next, err := f.wakes.NextForRun(ctx, domain.WorkflowRunID(id))
		if err != nil {
			t.Fatalf("NextForRun %s: %v", id, err)
		}
		if next != nil {
			t.Fatalf("run %s still has an open wake after archiving: %+v", id, next)
		}
	}
}

// --- E ------------------------------------------------------------------------------

// TestCancelAndArchiveIsIdempotent covers the two ways this gets called twice:
// a double click, and a retried API request.
func TestCancelAndArchiveIsIdempotent(t *testing.T) {
	f := newArchiveFixture(t)
	ctx := stdctx.Background()
	master := f.seedRun(t, "archive me twice", domain.WorkflowRunNeedsAttention)
	f.seedChild(t, master, "live-child", domain.WorkflowRunRunning)

	first, err := f.c.CancelAndArchiveRun(ctx, master)
	if err != nil {
		t.Fatalf("first CancelAndArchiveRun: %v", err)
	}
	second, err := f.c.CancelAndArchiveRun(ctx, master)
	if err != nil {
		t.Fatalf("second CancelAndArchiveRun: %v", err)
	}

	if second.Run.State != domain.WorkflowRunCancelled {
		t.Fatalf("state after second call = %s, want cancelled", second.Run.State)
	}
	if first.Run.ArchivedAt == nil || second.Run.ArchivedAt == nil ||
		!first.Run.ArchivedAt.Equal(*second.Run.ArchivedAt) {
		t.Fatalf("archive timestamp moved: %v -> %v", first.Run.ArchivedAt, second.Run.ArchivedAt)
	}

	// Exactly one cancellation is recorded, not two: a second audit entry would
	// read as a second, separate incident.
	audits := 0
	for _, cp := range f.store.checkpoints[master] {
		if cp.DurablePhase == workflowcore.ArchivedRunCheckpointPhase {
			audits++
		}
	}
	if audits != 1 {
		t.Fatalf("audit checkpoints = %d, want exactly 1", audits)
	}
}

// --- F ------------------------------------------------------------------------------

// TestCancelAndArchiveRetainsHistory is the non-destructiveness guarantee:
// archiving hides a run from one view and removes nothing.
func TestCancelAndArchiveRetainsHistory(t *testing.T) {
	f := newArchiveFixture(t)
	ctx := stdctx.Background()
	master := f.seedRun(t, "keep my history", domain.WorkflowRunNeedsAttention)
	child := f.seedChild(t, master, "history-child", domain.WorkflowRunRunning)
	stepsBefore := len(f.store.steps[master])
	childStepsBefore := len(f.store.steps[child])

	if _, err := f.c.CancelAndArchiveRun(ctx, master); err != nil {
		t.Fatalf("CancelAndArchiveRun: %v", err)
	}

	if _, ok, err := f.store.GetWorkflowRun(ctx, master); err != nil || !ok {
		t.Fatalf("master run row is gone: ok=%v err=%v", ok, err)
	}
	if _, ok, err := f.store.GetWorkflowRun(ctx, child); err != nil || !ok {
		t.Fatalf("child run row is gone: ok=%v err=%v", ok, err)
	}
	if got := len(f.store.steps[master]); got != stepsBefore {
		t.Fatalf("master steps = %d, want %d retained", got, stepsBefore)
	}
	if got := len(f.store.steps[child]); got != childStepsBefore {
		t.Fatalf("child steps = %d, want %d retained", got, childStepsBefore)
	}

	cps, err := f.store.ListWorkflowCheckpoints(ctx, master)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var audit *domain.WorkflowCheckpoint
	for i := range cps {
		if cps[i].DurablePhase == workflowcore.ArchivedRunCheckpointPhase {
			audit = &cps[i]
		}
	}
	if audit == nil {
		t.Fatal("no durable cancellation audit checkpoint was written")
	}
	if audit.NextAction == "" {
		t.Fatal("audit checkpoint records no explanation of what happened")
	}
	// The run detail is still fully readable after archiving — that is what
	// makes the archived view an inspection surface rather than a tombstone.
	detail, err := f.c.GetRun(ctx, master)
	if err != nil {
		t.Fatalf("GetRun after archiving: %v", err)
	}
	if len(detail.Steps) != stepsBefore {
		t.Fatalf("GetRun returned %d steps, want %d", len(detail.Steps), stepsBefore)
	}
}

// --- G ------------------------------------------------------------------------------

// TestArchivedRunAppearsInBoardHistory covers "Mostrar archivados": the run left
// the active lane, it did not leave the product.
func TestArchivedRunAppearsInBoardHistory(t *testing.T) {
	f := newArchiveFixture(t)
	ctx := stdctx.Background()
	archived := f.seedRun(t, "retired workflow", domain.WorkflowRunNeedsAttention)
	active := f.seedRun(t, "live workflow", domain.WorkflowRunRunning)

	if _, err := f.c.CancelAndArchiveRun(ctx, archived); err != nil {
		t.Fatalf("CancelAndArchiveRun: %v", err)
	}

	history, err := f.c.ProjectBoardHistory(ctx, "proj", 100)
	if err != nil {
		t.Fatalf("ProjectBoardHistory: %v", err)
	}
	ids := make([]string, 0, len(history))
	for _, e := range history {
		ids = append(ids, e.Run.ID)
	}
	if !containsRunID(ids, archived) {
		t.Fatalf("archived run missing from history: %v", ids)
	}
	if containsRunID(ids, active) {
		t.Fatalf("history contains a run that was never archived: %v", ids)
	}
	for _, e := range history {
		if e.Run.ID == archived && e.Run.ArchivedAt == nil {
			t.Fatal("history entry carries no archive timestamp")
		}
	}
}

// --- H ------------------------------------------------------------------------------

// TestActiveWorkflowsAreNeverHiddenByArchiving is the safety half: this feature
// must not be a way to make a running workflow disappear while it keeps working.
func TestActiveWorkflowsAreNeverHiddenByArchiving(t *testing.T) {
	f := newArchiveFixture(t)
	ctx := stdctx.Background()
	stale := f.seedRun(t, "stale one", domain.WorkflowRunNeedsAttention)
	for _, state := range []domain.WorkflowRunState{
		domain.WorkflowRunPending, domain.WorkflowRunRunning, domain.WorkflowRunWaiting,
	} {
		f.seedRun(t, "active "+string(state), state)
	}

	if _, err := f.c.CancelAndArchiveRun(ctx, stale); err != nil {
		t.Fatalf("CancelAndArchiveRun: %v", err)
	}

	board := f.boardIDs(t)
	if containsRunID(board, stale) {
		t.Fatalf("archived run still on the Board: %v", board)
	}
	if len(board) != 3 {
		t.Fatalf("Board has %d cards (%v), want the 3 untouched active workflows", len(board), board)
	}
	for _, run := range f.store.runs {
		if run.ID == stale || run.ParentWorkflowID != nil {
			continue
		}
		if run.Archived() {
			t.Fatalf("run %s (%s) was archived as collateral", run.ID, run.State)
		}
	}
}

// A child is not a Board card, so archiving one would hide nothing while
// retiring a run its parent still reports on. The action is refused.
func TestCancelAndArchiveRefusesAChildRun(t *testing.T) {
	f := newArchiveFixture(t)
	master := f.seedRun(t, "the parent", domain.WorkflowRunNeedsAttention)
	child := f.seedChild(t, master, "the child", domain.WorkflowRunRunning)

	_, err := f.c.CancelAndArchiveRun(stdctx.Background(), child)
	if !errors.Is(err, workflowcore.ErrInvalid) {
		t.Fatalf("archiving a child returned %v, want ErrInvalid", err)
	}
	if f.store.runs[child].Archived() {
		t.Fatal("child was archived despite the refusal")
	}
}

func TestCancelAndArchiveUnknownRunIsNotFound(t *testing.T) {
	f := newArchiveFixture(t)
	_, err := f.c.CancelAndArchiveRun(stdctx.Background(), "wf-does-not-exist")
	if !errors.Is(err, workflowcore.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
