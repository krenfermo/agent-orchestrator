package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/workspace"
)

// The lifecycle manager persists through this Store, so the store must
// actually satisfy the interface the manager declares. Asserting it here keeps
// the two from drifting without importing storage into the manager package.
var _ workspace.Store = (*sqlite.Store)(nil)

func seedTaskForWorktree(t *testing.T, s *sqlite.Store, projectID, runID, taskID string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	seedProject(t, s, projectID)
	run := sampleWorkflowRun(projectID, runID, now)
	if _, _, err := s.CreateWorkflowRun(ctx, run, sampleWorkflowSteps(runID, now)); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := s.InsertWorkflowTasks(ctx, []domain.WorkflowTask{{
		ID: taskID, WorkflowRunID: runID, PlanStepID: runID + "-step-1", Ordinal: 1,
		Title: "task", Description: "d", AcceptanceCriteriaJSON: "[]", VerifyJSON: "[]",
		State: domain.WorkflowTaskEligible, CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

// Every field on the record is there because it cannot be re-derived later, so
// the round trip has to prove every field survives -- including the dependency
// pins, which are the ones with a JSON hop in the middle.
func TestTaskWorktreeRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	seedTaskForWorktree(t, s, "proj", "wf-1", "task-1", now)

	rec := domain.TaskWorktreeRecord{
		WorkflowRunID: "wf-1", TaskID: "task-1", ProjectID: "proj",
		RepoPath: "/repos/proj", Path: "/data/worktrees/proj/wf-1/task-1",
		Branch: "ao/wf-1/task-1", TargetBranch: "main", BaseSHA: "base1",
		// Deliberately out of order: the store sorts, so two runs that
		// resolved the same dependencies store identical JSON.
		Dependencies: []domain.TaskWorktreeDependency{
			{TaskID: "task-2", SHA: "sha2"},
			{TaskID: "task-0", SHA: "sha0"},
		},
		ExecutionMode: domain.ExecutionSmartParallelWorktrees,
		State:         domain.TaskWorktreeCreating,
		CreatedAt:     now, UpdatedAt: now,
	}
	if err := s.UpsertTaskWorktree(ctx, rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, found, err := s.GetTaskWorktree(ctx, "task-1")
	if err != nil || !found {
		t.Fatalf("get = %v, %v", found, err)
	}
	want := rec
	want.Dependencies = []domain.TaskWorktreeDependency{{TaskID: "task-0", SHA: "sha0"}, {TaskID: "task-2", SHA: "sha2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %#v\nwant %#v", got, want)
	}

	// A retry updates the task's one row rather than adding a second the
	// caller cannot tell apart, and created_at keeps saying when the worktree
	// first appeared.
	released := now.Add(time.Minute)
	rec.State = domain.TaskWorktreeReleased
	rec.UpdatedAt = released
	rec.ReleasedAt = &released
	rec.CreatedAt = released // ignored by the upsert
	if err := s.UpsertTaskWorktree(ctx, rec); err != nil {
		t.Fatalf("upsert released: %v", err)
	}
	rows, err := s.ListTaskWorktreesByRun(ctx, "wf-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 per task", len(rows))
	}
	if rows[0].State != domain.TaskWorktreeReleased || rows[0].ReleasedAt == nil {
		t.Fatalf("row = %#v, want released with a timestamp", rows[0])
	}
	if !rows[0].CreatedAt.Equal(now) {
		t.Fatalf("created_at = %v, want the original %v", rows[0].CreatedAt, now)
	}
	if rows[0].Branch != "ao/wf-1/task-1" || rows[0].BaseSHA != "base1" {
		t.Fatalf("released row lost its branch/base: %#v", rows[0])
	}
}

// A task with no worktree -- a direct-branch task, or one never started -- is
// a normal absent answer, not an error.
func TestGetTaskWorktreeAbsent(t *testing.T) {
	s := newTestStore(t)
	_, found, err := s.GetTaskWorktree(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("get absent: %v", err)
	}
	if found {
		t.Fatal("found = true, want false")
	}
}

// A state outside the typed vocabulary is refused with the offending value
// named, rather than surfacing as a CHECK violation that only names the table.
func TestUpsertTaskWorktreeRejectsUnknownState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	seedTaskForWorktree(t, s, "proj", "wf-1", "task-1", now)

	err := s.UpsertTaskWorktree(ctx, domain.TaskWorktreeRecord{
		WorkflowRunID: "wf-1", TaskID: "task-1", ProjectID: "proj", RepoPath: "/repos/proj",
		Path: "/w/p", Branch: "ao/wf-1/task-1", TargetBranch: "main", BaseSHA: "base1",
		ExecutionMode: domain.ExecutionIsolatedWorktree, State: "half-created",
		CreatedAt: now, UpdatedAt: now,
	})
	if err == nil || !strings.Contains(err.Error(), "half-created") {
		t.Fatalf("err = %v, want the unknown state named", err)
	}
}

// The two cleanup facts have to survive the round trip like every other field:
// integrated_sha is what authorizes deleting a branch, and branch_deleted is
// what stops a reconcile pass from looking for one that is already gone.
func TestTaskWorktreeCleanupFactsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	seedTaskForWorktree(t, s, "proj", "wf-1", "task-1", now)

	released := now.Add(time.Minute)
	rec := domain.TaskWorktreeRecord{
		WorkflowRunID: "wf-1", TaskID: "task-1", ProjectID: "proj",
		RepoPath: "/repos/proj", Path: "/data/worktrees/proj/wf-1/task-1",
		Branch: "ao/wf-1/task-1", TargetBranch: "main", BaseSHA: "base1",
		ExecutionMode: domain.ExecutionIsolatedWorktree,
		State:         domain.TaskWorktreeReleased,
		IntegratedSHA: "landed1",
		BranchDeleted: true,
		CreatedAt:     now, UpdatedAt: released, ReleasedAt: &released,
	}
	if err := s.UpsertTaskWorktree(ctx, rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, ok, err := s.GetTaskWorktree(ctx, "task-1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.IntegratedSHA != "landed1" || !got.BranchDeleted {
		t.Fatalf("cleanup facts = %q/%v, want landed1/true", got.IntegratedSHA, got.BranchDeleted)
	}
	if got.State != domain.TaskWorktreeReleased {
		t.Fatalf("state = %q, want released", got.State)
	}
}

// ListUnfinishedTaskWorktrees is what boot reconciliation reads, so its
// predicate IS the definition of "still AO's problem".
//
// The two ends are the interesting ones. A released record whose branch is
// still there is NOT finished -- its checkout is gone but its commits are not,
// and something has to go back and delete that branch. A preserved record IS
// finished, permanently and on purpose: it is a decision to keep the work, and
// listing it would invite a later pass to re-decide.
func TestListUnfinishedTaskWorktreesSelectsWhatStillNeedsWork(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	type seed struct {
		task          string
		state         domain.TaskWorktreeState
		branchDeleted bool
		want          bool
	}
	seeds := []seed{
		{"task-1", domain.TaskWorktreeCreating, false, true},
		{"task-2", domain.TaskWorktreeActive, false, true},
		{"task-3", domain.TaskWorktreeIntegrated, false, true},
		{"task-4", domain.TaskWorktreeReleased, false, true},
		{"task-5", domain.TaskWorktreeReleased, true, false},
		{"task-6", domain.TaskWorktreePreserved, false, false},
		{"task-7", domain.TaskWorktreeFailed, false, false},
	}
	seedProject(t, s, "proj")
	run := sampleWorkflowRun("proj", "wf-1", now)
	if _, _, err := s.CreateWorkflowRun(ctx, run, sampleWorkflowSteps("wf-1", now)); err != nil {
		t.Fatalf("create run: %v", err)
	}
	var tasks []domain.WorkflowTask
	for i, sd := range seeds {
		tasks = append(tasks, domain.WorkflowTask{
			ID: sd.task, WorkflowRunID: "wf-1", PlanStepID: "wf-1-step-" + sd.task, Ordinal: int64(i + 1),
			Title: "task", Description: "d", AcceptanceCriteriaJSON: "[]", VerifyJSON: "[]",
			State: domain.WorkflowTaskEligible, CreatedAt: now, UpdatedAt: now,
		})
	}
	if err := s.InsertWorkflowTasks(ctx, tasks); err != nil {
		t.Fatalf("insert tasks: %v", err)
	}
	for _, sd := range seeds {
		if err := s.UpsertTaskWorktree(ctx, domain.TaskWorktreeRecord{
			WorkflowRunID: "wf-1", TaskID: sd.task, ProjectID: "proj",
			RepoPath: "/repos/proj", Path: "/data/worktrees/" + sd.task,
			Branch: "ao/wf-1/" + sd.task, TargetBranch: "main", BaseSHA: "base1",
			ExecutionMode: domain.ExecutionIsolatedWorktree,
			State:         sd.state, BranchDeleted: sd.branchDeleted,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert %s: %v", sd.task, err)
		}
	}

	rows, err := s.ListUnfinishedTaskWorktrees(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listed := map[string]bool{}
	for _, row := range rows {
		listed[row.TaskID] = true
	}
	for _, sd := range seeds {
		if listed[sd.task] != sd.want {
			t.Fatalf("task %s (%s, branchDeleted=%v) listed=%v, want %v",
				sd.task, sd.state, sd.branchDeleted, listed[sd.task], sd.want)
		}
	}
	if !strings.HasPrefix(rows[0].TaskID, "task-") {
		t.Fatalf("first row = %+v", rows[0])
	}
}
