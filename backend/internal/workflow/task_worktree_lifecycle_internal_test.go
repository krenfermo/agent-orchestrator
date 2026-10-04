package workflow

import (
	stdctx "context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/workspace"
)

// These test the WIRING, not the worktree operations themselves -- those are
// pinned against real git in internal/workspace and internal/worktree. What is
// at stake here is the ordering the workflow side is responsible for:
//
//   - cleanup runs only after the promotion is durably recorded, because a task
//     whose promotion is missing will be integrated again and needs its branch;
//   - the "already promoted" early return finishes an interrupted cleanup
//     instead of skipping it, which is the crash window between the two writes;
//   - a failed or cancelled task's worktree is preserved, not tidied;
//   - boot recovery reconciles worktrees before it advances any run.

// fakeTaskWorkspaces records what the coordinator asked for, in order.
type fakeTaskWorkspaces struct {
	calls []string
	// records is the durable state, keyed by task, as the real manager would
	// hold it.
	records map[string]domain.TaskWorktreeRecord
	// cleanupResult and cleanupErr script Cleanup.
	cleanupResult workspace.CleanupResult
	cleanupErr    error
	markErr       error
	reconcile     workspace.ReconcileReport
	reconcileErr  error
	reconciled    int
}

func newFakeTaskWorkspaces() *fakeTaskWorkspaces {
	return &fakeTaskWorkspaces{records: map[string]domain.TaskWorktreeRecord{}}
}

func (f *fakeTaskWorkspaces) MarkIntegrated(_ stdctx.Context, taskID, sha string) (domain.TaskWorktreeRecord, error) {
	f.calls = append(f.calls, "mark-integrated "+taskID+" "+sha)
	if f.markErr != nil {
		return domain.TaskWorktreeRecord{}, f.markErr
	}
	rec, ok := f.records[taskID]
	if !ok {
		return domain.TaskWorktreeRecord{}, workspace.ErrNoRecord
	}
	rec.IntegratedSHA = sha
	rec.State = domain.TaskWorktreeIntegrated
	f.records[taskID] = rec
	return rec, nil
}

func (f *fakeTaskWorkspaces) Cleanup(_ stdctx.Context, taskID string) (workspace.CleanupResult, error) {
	f.calls = append(f.calls, "cleanup "+taskID)
	if f.cleanupErr != nil {
		return workspace.CleanupResult{}, f.cleanupErr
	}
	rec := f.records[taskID]
	rec.State = domain.TaskWorktreeReleased
	rec.BranchDeleted = f.cleanupResult.BranchDeleted
	f.records[taskID] = rec
	result := f.cleanupResult
	result.Record = rec
	return result, nil
}

func (f *fakeTaskWorkspaces) Preserve(_ stdctx.Context, taskID, reason string) (domain.TaskWorktreeRecord, bool, error) {
	f.calls = append(f.calls, "preserve "+taskID)
	rec, ok := f.records[taskID]
	if !ok {
		return domain.TaskWorktreeRecord{}, false, nil
	}
	rec.State = domain.TaskWorktreePreserved
	rec.Detail = reason
	f.records[taskID] = rec
	return rec, true, nil
}

func (f *fakeTaskWorkspaces) Reconcile(_ stdctx.Context) (workspace.ReconcileReport, error) {
	f.reconciled++
	f.calls = append(f.calls, "reconcile")
	return f.reconcile, f.reconcileErr
}

func newLifecycleCoordinator(t *testing.T) (*Coordinator, *fakeTaskWorkspaces, domain.WorkflowRun, stdctx.Context) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	spaces := newFakeTaskWorkspaces()
	c := New(Deps{
		Store: store, Projects: store, TaskWorkspaces: spaces,
		Clock: func() time.Time { return time.Now().UTC() },
	})
	// A real master run: every checkpoint below is a row on it, and the
	// foreign key is what makes the ledger reads in these tests read the same
	// rows the daemon would.
	parent, err := c.CreateRun(ctx, "p", "the master objective")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return c, spaces, parent.Run, ctx
}

func lifecycleTask() domain.WorkflowTask {
	return domain.WorkflowTask{ID: "task-1", Ordinal: 1, Title: "Build the thing"}
}

// A successful integration ends with the worktree recorded as integrated, then
// cleaned up, then accounted for on the run's own ledger.
//
// The ORDER is the assertion. Marking first is what makes the cleanup
// recoverable: a crash between the two leaves a record that says "this landed
// at <sha>, finish tidying up", where a crash after a cleanup that was never
// recorded would leave nothing at all.
func TestFinishTaskWorktreeMarksIntegratedThenCleansUpThenRecords(t *testing.T) {
	c, spaces, run, ctx := newLifecycleCoordinator(t)
	spaces.records["task-1"] = domain.TaskWorktreeRecord{
		TaskID: "task-1", Path: "/ao/worktrees/task-1", Branch: "ao/wf-master/task-1",
	}
	spaces.cleanupResult = workspace.CleanupResult{WorktreeRemoved: true, BranchDeleted: true}

	c.finishTaskWorktree(ctx, run, lifecycleTask(), "abc123def456")

	want := []string{"mark-integrated task-1 abc123def456", "cleanup task-1"}
	if strings.Join(spaces.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", spaces.calls, want)
	}
	cleanups, err := c.ListTaskWorktreeCleanups(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleanups) != 1 {
		t.Fatalf("recorded %d cleanups, want 1", len(cleanups))
	}
	got := cleanups[0]
	if got.TaskID != "task-1" || got.IntegratedSHA != "abc123def456" {
		t.Fatalf("record = %+v", got)
	}
	if !got.WorktreeRemoved || !got.BranchDeleted {
		t.Fatalf("record does not account for the removal and the branch delete: %+v", got)
	}
	if got.Branch != "ao/wf-master/task-1" || got.WorktreePath != "/ao/worktrees/task-1" {
		t.Fatalf("record does not name what was removed: %+v", got)
	}
	if got.Preserved {
		t.Fatal("a successful cleanup was recorded as a preservation")
	}
}

// A cleanup that cannot finish is recorded as outstanding, not as done, and
// never as a failure of the integration.
//
// The work is on the target. A directory that outlived it is untidy, and
// turning that into a run failure would stop a plan over housekeeping -- so the
// row says exactly what is left and boot reconciliation picks it up.
func TestFinishTaskWorktreeRecordsADeferredCleanupRatherThanFailing(t *testing.T) {
	c, spaces, run, ctx := newLifecycleCoordinator(t)
	spaces.records["task-1"] = domain.TaskWorktreeRecord{TaskID: "task-1", Branch: "ao/wf-master/task-1"}
	spaces.cleanupErr = errors.New("worktree contains modified files")

	c.finishTaskWorktree(ctx, run, lifecycleTask(), "abc123def456")

	cleanups, err := c.ListTaskWorktreeCleanups(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleanups) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(cleanups))
	}
	if cleanups[0].Error == "" {
		t.Fatalf("a deferred cleanup was recorded as if it had succeeded: %+v", cleanups[0])
	}
	if cleanups[0].BranchDeleted {
		t.Fatal("a failed cleanup claimed the branch was deleted")
	}
}

// A branch that could not be proved safe to delete is recorded with the reason,
// because that reason names work AO deliberately refused to throw away. It is
// the row a person reads when they wonder why an ao/* branch is still there.
func TestFinishTaskWorktreeRecordsWhyABranchWasKept(t *testing.T) {
	c, spaces, run, ctx := newLifecycleCoordinator(t)
	spaces.records["task-1"] = domain.TaskWorktreeRecord{TaskID: "task-1", Branch: "ao/wf-master/task-1"}
	spaces.cleanupResult = workspace.CleanupResult{
		WorktreeRemoved: true,
		BranchKept:      "ao/wf-master/task-1 holds work that exists nowhere else",
	}

	c.finishTaskWorktree(ctx, run, lifecycleTask(), "abc123def456")

	cleanups, _ := c.ListTaskWorktreeCleanups(ctx, run.ID)
	if len(cleanups) != 1 || cleanups[0].BranchKept == "" {
		t.Fatalf("records = %+v, want one naming the kept branch", cleanups)
	}
	if cleanups[0].BranchDeleted {
		t.Fatal("a kept branch was recorded as deleted")
	}
}

// A direct-branch task has no AO worktree, so there is nothing to clean up and
// nothing to say about it. The absence must not produce a ledger row -- a plan
// full of "nothing to clean up" checkpoints buries the ones that matter.
func TestFinishTaskWorktreeIsSilentForATaskWithNoWorktree(t *testing.T) {
	c, spaces, run, ctx := newLifecycleCoordinator(t)
	// No record seeded: MarkIntegrated answers ErrNoRecord.

	c.finishTaskWorktree(ctx, run, lifecycleTask(), "abc123def456")

	if len(spaces.calls) != 1 {
		t.Fatalf("calls = %v, want only the mark attempt", spaces.calls)
	}
	cleanups, _ := c.ListTaskWorktreeCleanups(ctx, run.ID)
	if len(cleanups) != 0 {
		t.Fatalf("recorded %+v for a task with no AO worktree", cleanups)
	}
}

// A failed or cancelled task's worktree is preserved, durably, with the reason.
func TestPreserveTaskWorktreeRecordsTheDecisionToKeepTheWork(t *testing.T) {
	c, spaces, run, ctx := newLifecycleCoordinator(t)
	spaces.records["task-1"] = domain.TaskWorktreeRecord{
		TaskID: "task-1", Path: "/ao/worktrees/task-1", Branch: "ao/wf-master/task-1",
	}

	c.preserveTaskWorktree(ctx, run, lifecycleTask(), "task 1 (Build the thing) was cancelled before its work could be integrated")

	if got := spaces.records["task-1"].State; got != domain.TaskWorktreePreserved {
		t.Fatalf("state = %q, want preserved", got)
	}
	cleanups, _ := c.ListTaskWorktreeCleanups(ctx, run.ID)
	if len(cleanups) != 1 || !cleanups[0].Preserved {
		t.Fatalf("records = %+v, want one preservation", cleanups)
	}
	if !strings.Contains(cleanups[0].Reason, "cancelled") {
		t.Fatalf("reason = %q, want it to say why the work is being kept", cleanups[0].Reason)
	}
	if cleanups[0].Branch != "ao/wf-master/task-1" {
		t.Fatalf("record does not name the branch being kept: %+v", cleanups[0])
	}
}

// A task with no AO worktree cannot be "preserved" and must not claim to be.
func TestPreserveTaskWorktreeIsSilentForATaskWithNoWorktree(t *testing.T) {
	c, _, run, ctx := newLifecycleCoordinator(t)

	c.preserveTaskWorktree(ctx, run, lifecycleTask(), "cancelled")

	cleanups, _ := c.ListTaskWorktreeCleanups(ctx, run.ID)
	if len(cleanups) != 0 {
		t.Fatalf("recorded %+v for a task with no AO worktree", cleanups)
	}
}

// promotedHeadSHA reads back where a task's promotion left the integration ref,
// from the ledger rather than from the ref.
//
// It is what lets a cleanup that never ran be finished later: by then the ref
// has usually moved on, so the current head is the wrong answer and the ledger
// row is the only place the right one survives.
func TestPromotedHeadSHAReadsTheTasksOwnPromotionRow(t *testing.T) {
	c, _, run, ctx := newLifecycleCoordinator(t)
	seedPromotion(t, c, ctx, run, "task-1", "sha-target-1")
	seedPromotion(t, c, ctx, run, "task-2", "sha-target-2")

	if got := c.promotedHeadSHA(ctx, run.ID, "task-1"); got != "sha-target-1" {
		t.Fatalf("task-1 head = %q, want sha-target-1", got)
	}
	if got := c.promotedHeadSHA(ctx, run.ID, "task-2"); got != "sha-target-2" {
		t.Fatalf("task-2 head = %q, want sha-target-2", got)
	}
	if got := c.promotedHeadSHA(ctx, run.ID, "task-3"); got != "" {
		t.Fatalf("a task that never promoted reported head %q", got)
	}
}

func seedPromotion(t *testing.T, c *Coordinator, ctx stdctx.Context, run domain.WorkflowRun, taskID, head string) {
	t.Helper()
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + taskID,
		WorkflowRunID:  run.ID,
		ProjectID:      run.ProjectID,
		HeadSHA:        head,
		RetryState:     `{"taskId":"` + taskID + `"}`,
		DurablePhase:   masterIntegrationDurablePhase,
		PayloadVersion: masterIntegrationPayloadVersion,
		CreatedAt:      c.clock(),
	}); err != nil {
		t.Fatalf("seed promotion for %s: %v", taskID, err)
	}
}

// Boot recovery reconciles AO worktrees before it advances a single run.
//
// Everything Reconcile does afterwards reads worktrees and branches, so it has
// to read them in the state the durable records describe rather than whatever
// the crash left on disk.
func TestBootRecoveryReconcilesWorktreesFirst(t *testing.T) {
	c, spaces, _, ctx := newLifecycleCoordinator(t)
	spaces.reconcile = workspace.ReconcileReport{Entries: []workspace.ReconcileEntry{
		{TaskID: "task-1", Action: workspace.ReconcileCleanedUp},
	}}

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if spaces.reconciled != 1 {
		t.Fatalf("worktrees reconciled %d times, want 1", spaces.reconciled)
	}
	if len(spaces.calls) == 0 || spaces.calls[0] != "reconcile" {
		t.Fatalf("calls = %v, want the worktree reconcile first", spaces.calls)
	}
}

// A worktree reconciliation that fails must not stop the daemon booting. The
// worst case is that a directory stays where it is until the next boot, which
// is the same "untidy, never unsafe" trade every branch of this file makes.
func TestBootRecoverySurvivesAFailedWorktreeReconciliation(t *testing.T) {
	c, spaces, _, ctx := newLifecycleCoordinator(t)
	spaces.reconcileErr = errors.New("repository unreadable")

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("a failed worktree reconciliation stopped boot recovery: %v", err)
	}
}

// A coordinator with no worktree lifecycle configured behaves exactly as it did
// before it existed: nothing is cleaned up, nothing is preserved, nothing is
// recorded, and nothing fails.
func TestWorktreeLifecycleIsOptional(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	c := New(Deps{Store: store, Projects: store, Clock: func() time.Time { return time.Now().UTC() }})
	parent, err := c.CreateRun(ctx, "p", "the master objective")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	c.finishTaskWorktree(ctx, parent.Run, lifecycleTask(), "abc123")
	c.preserveTaskWorktree(ctx, parent.Run, lifecycleTask(), "cancelled")
	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile without a worktree lifecycle: %v", err)
	}
	cleanups, _ := c.ListTaskWorktreeCleanups(ctx, parent.Run.ID)
	if len(cleanups) != 0 {
		t.Fatalf("recorded %+v with no worktree lifecycle configured", cleanups)
	}
}
