package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/worktree"
)

// The lifecycle manager persists through this Store, so the store must
// actually satisfy the interface the manager declares. Asserting it here keeps
// the two from drifting without importing storage into the manager package.
var _ worktree.Store = (*sqlite.Store)(nil)

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
