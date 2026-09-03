package workflow

import (
	stdctx "context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// decision_resolver_workspace_internal_test.go — the defect that made an
// auto_resolvable question on a worktree-placed run unresolvable forever.
//
// A step's checkpoints are append-only, and several phases legitimately record
// no workspace. `worker_blocked` is one of them — and it is precisely the phase
// a step is in when a worker pauses on a question. Reading "the latest
// checkpoint" therefore handed the resolver a blank worktree path and parked the
// question on a capacity wait for a workspace AO had already written down.

func seedResolverRun(t *testing.T) (*Coordinator, stdctx.Context, domain.WorkflowRun, domain.WorkflowStep) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Unix(1740000000, 0).UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	run, steps, err := store.CreateWorkflowRun(ctx, domain.WorkflowRun{
		ID: "wf-resolver", ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunRunning, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now,
	}, []domain.WorkflowStep{
		{
			ID: "step-work", WorkflowRunID: "wf-resolver", Kind: domain.WorkflowStepWork,
			Ordinal: 1, State: domain.WorkflowStepRunning, CreatedAt: now, UpdatedAt: now,
		},
		// A second real step, so the "another step's checkout" case exercises a
		// step that genuinely exists rather than a dangling foreign key.
		{
			ID: "step-other", WorkflowRunID: "wf-resolver", Kind: domain.WorkflowStepWork,
			Ordinal: 2, State: domain.WorkflowStepPending, CreatedAt: now, UpdatedAt: now,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := New(Deps{Store: store, Projects: store, Clock: func() time.Time { return now }})
	return c, ctx, run, steps[0]
}

func writeResolverCheckpoint(t *testing.T, c *Coordinator, run domain.WorkflowRun, stepID, phase, branch, worktree string, at time.Time) {
	t.Helper()
	id := stepID
	if _, err := c.store.CreateWorkflowCheckpoint(stdctx.Background(), domain.WorkflowCheckpoint{
		ID: "wfc-" + phase, WorkflowRunID: run.ID, WorkflowStepID: &id, ProjectID: run.ProjectID,
		Branch: branch, WorktreePath: worktree, DurablePhase: phase,
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: at,
	}); err != nil {
		t.Fatalf("write %s checkpoint: %v", phase, err)
	}
}

func TestResolverWorkspace_FindsTheWorkspaceBehindABlankLatestCheckpoint(t *testing.T) {
	c, ctx, run, step := seedResolverRun(t)
	base := time.Unix(1740000000, 0).UTC()
	const worktree = "/tmp/worktrees/slugtools-1"

	writeResolverCheckpoint(t, c, run, step.ID, "worker_dispatched", "ao/feat", worktree, base)
	// The phase a step is in when its worker pauses on a question. It records no
	// workspace, and it is the LATEST row.
	writeResolverCheckpoint(t, c, run, step.ID, "worker_blocked", "", "", base.Add(time.Minute))

	stepID := domain.WorkflowStepID(step.ID)
	branch, path, _ := c.resolverWorkspaceForQuestion(ctx, domain.WorkflowQuestion{
		WorkflowRunID: domain.WorkflowRunID(run.ID), WorkflowStepID: &stepID,
	})
	if path != worktree {
		t.Fatalf("worktree = %q, want %q — the resolver must read the workspace AO already recorded, not the blank latest row", path, worktree)
	}
	if branch != "ao/feat" {
		t.Fatalf("branch = %q, want ao/feat", branch)
	}
}

func TestResolverWorkspace_PrefersTheLatestRecordedWorkspace(t *testing.T) {
	// A step that moved workspace mid-flight resolves against where it is NOW,
	// not against the first place it ever ran.
	c, ctx, run, step := seedResolverRun(t)
	base := time.Unix(1740000000, 0).UTC()

	writeResolverCheckpoint(t, c, run, step.ID, "worker_dispatched", "ao/old", "/tmp/old", base)
	writeResolverCheckpoint(t, c, run, step.ID, "worker_observed", "ao/new", "/tmp/new", base.Add(time.Minute))
	writeResolverCheckpoint(t, c, run, step.ID, "worker_blocked", "", "", base.Add(2*time.Minute))

	stepID := domain.WorkflowStepID(step.ID)
	branch, path, _ := c.resolverWorkspaceForQuestion(ctx, domain.WorkflowQuestion{
		WorkflowRunID: domain.WorkflowRunID(run.ID), WorkflowStepID: &stepID,
	})
	if path != "/tmp/new" || branch != "ao/new" {
		t.Fatalf("got %q/%q, want the most recently recorded workspace", branch, path)
	}
}

func TestResolverWorkspace_ReturnsNothingWhenNothingRecordedOne(t *testing.T) {
	// The pre-existing capacity-wait behaviour is preserved exactly: a step that
	// never recorded a workspace still yields none, so dispatch falls through to
	// the placement lookup and then to the wait. Nothing is guessed.
	c, ctx, run, step := seedResolverRun(t)
	base := time.Unix(1740000000, 0).UTC()
	writeResolverCheckpoint(t, c, run, step.ID, "routing_decision", "", "", base)

	stepID := domain.WorkflowStepID(step.ID)
	_, path, _ := c.resolverWorkspaceForQuestion(ctx, domain.WorkflowQuestion{
		WorkflowRunID: domain.WorkflowRunID(run.ID), WorkflowStepID: &stepID,
	})
	if path != "" {
		t.Fatalf("worktree = %q, want empty — a workspace nobody recorded must not be invented", path)
	}
}

func TestResolverWorkspace_IgnoresOtherStepsWorkspaces(t *testing.T) {
	// Another step's checkout is not this question's workspace. Handing the
	// resolver the wrong tree would have it answer about code the asking worker
	// is not editing.
	c, ctx, run, step := seedResolverRun(t)
	base := time.Unix(1740000000, 0).UTC()
	writeResolverCheckpoint(t, c, run, "step-other", "worker_dispatched", "ao/other", "/tmp/other", base)
	writeResolverCheckpoint(t, c, run, step.ID, "worker_blocked", "", "", base.Add(time.Minute))

	stepID := domain.WorkflowStepID(step.ID)
	_, path, _ := c.resolverWorkspaceForQuestion(ctx, domain.WorkflowQuestion{
		WorkflowRunID: domain.WorkflowRunID(run.ID), WorkflowStepID: &stepID,
	})
	if path != "" {
		t.Fatalf("worktree = %q, want empty — another step's checkout is not this question's workspace", path)
	}
}

func TestResolverWorkspace_NoStepMeansNoWorkspace(t *testing.T) {
	c, ctx, _, _ := seedResolverRun(t)
	if _, path, _ := c.resolverWorkspaceForQuestion(ctx, domain.WorkflowQuestion{}); path != "" {
		t.Fatalf("worktree = %q, want empty", path)
	}
}
