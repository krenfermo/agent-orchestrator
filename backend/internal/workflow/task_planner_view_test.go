package workflow_test

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// scopeJSON is the durable shape a task row actually carries, so these tests
// exercise the same unmarshal path the daemon does rather than a struct the
// database never sees.
func scopeJSON(t *testing.T, scope domain.WorkflowTaskScope) string {
	t.Helper()
	raw, err := workflowcore.MarshalTaskScope(scope)
	if err != nil {
		t.Fatalf("marshal scope: %v", err)
	}
	return raw
}

func task(id string, ordinal int64, state domain.WorkflowTaskState, deps ...string) domain.WorkflowTask {
	return domain.WorkflowTask{
		ID:            id,
		PlanStepID:    "step-" + id,
		WorkflowRunID: "run-1",
		Ordinal:       ordinal,
		Title:         "task " + id,
		State:         state,
		Dependencies:  deps,
		ScopeJSON:     "{}",
	}
}

func viewByTask(views []workflowcore.TaskPlannerView) map[string]workflowcore.TaskPlannerView {
	out := map[string]workflowcore.TaskPlannerView{}
	for _, v := range views {
		out[v.TaskID] = v
	}
	return out
}

func relationship(a, b string, rel domain.WorkflowTaskRelation) domain.WorkflowTaskRelationship {
	if a > b {
		a, b = b, a
	}
	return domain.WorkflowTaskRelationship{WorkflowRunID: "run-1", TaskID: a, RelatedTaskID: b, Relation: rel}
}

// The seven labels the Board is required to be able to show, each from the
// durable rows that make it true. They are asserted together because the
// vocabulary's value is that it is exhaustive: a status that can be produced
// but never rendered, or rendered but never produced, is the failure mode.
func TestDeriveTaskPlannerViewsCoversEveryStatus(t *testing.T) {
	t.Parallel()

	waitingDeps := task("dep-waiter", 4, domain.WorkflowTaskBlocked, "a")
	waitingDeps.ScopeJSON = scopeJSON(t, domain.WorkflowTaskScope{WaitingReason: domain.WorkflowTaskWaitingDependency})
	waitingConflict := task("conflict-waiter", 5, domain.WorkflowTaskBlocked)
	waitingConflict.ScopeJSON = scopeJSON(t, domain.WorkflowTaskScope{WaitingReason: domain.WorkflowTaskWaitingConflict})

	readyRun := "child-ready"
	ready := task("ready", 6, domain.WorkflowTaskRunning)
	ready.ExecutionRunID = &readyRun

	integratingRun := "child-integrating"
	integrating := task("integrating", 7, domain.WorkflowTaskRunning)
	integrating.ExecutionRunID = &integratingRun

	tasks := []domain.WorkflowTask{
		task("a", 1, domain.WorkflowTaskRunning),
		task("b", 2, domain.WorkflowTaskRunning),
		task("done", 3, domain.WorkflowTaskCompleted),
		waitingDeps,
		waitingConflict,
		ready,
		integrating,
		task("parked", 8, domain.WorkflowTaskNeedsAttention),
	}

	views := workflowcore.DeriveTaskPlannerViews(workflowcore.TaskPlannerInput{
		ProjectMode: domain.ExecutionSmartParallelWorktrees,
		Tasks:       tasks,
		Integrations: []workflowcore.TaskIntegrationRecord{
			{TaskID: "done", Outcome: string(integration.OutcomeIntegrated), Strategy: string(integration.StrategyFastForward)},
			// An intent with no result after it: the Coordinator is in the lane.
			{TaskID: "integrating", Outcome: string(integration.OutcomeAttempting)},
			{TaskID: "parked", Outcome: string(integration.OutcomeNeedsAttention),
				AttentionReason: string(integration.ReasonMergeConflict), ConflictFiles: []string{"backend/a.go"}},
		},
		// "ready" and "integrating" both finished their execution run; only the
		// second one has reached the lane.
		ChildRunCompleted: map[string]bool{"ready": true, "integrating": true},
	})

	got := viewByTask(views)
	want := map[string]workflowcore.TaskPlannerStatus{
		"a":               workflowcore.TaskPlannerRunningInParallel,
		"b":               workflowcore.TaskPlannerRunningInParallel,
		"done":            workflowcore.TaskPlannerIntegrated,
		"dep-waiter":      workflowcore.TaskPlannerWaitingForDependency,
		"conflict-waiter": workflowcore.TaskPlannerWaitingForConflict,
		"ready":           workflowcore.TaskPlannerReadyToIntegrate,
		"integrating":     workflowcore.TaskPlannerIntegrating,
		"parked":          workflowcore.TaskPlannerConflict,
	}
	for id, expected := range want {
		if got[id].Status != expected {
			t.Errorf("task %s: status = %q, want %q", id, got[id].Status, expected)
		}
	}

	// Every label in the exported vocabulary was actually produced above.
	produced := map[workflowcore.TaskPlannerStatus]bool{}
	for _, v := range views {
		produced[v.Status] = true
	}
	for _, status := range workflowcore.TaskPlannerStatuses {
		if !produced[status] {
			t.Errorf("status %q is in TaskPlannerStatuses but this fixture never produced it", status)
		}
	}

	// The conflict carries the detail a person needs to act on it, not just the
	// word "conflict".
	parked := got["parked"]
	if parked.Integration == nil {
		t.Fatal("parked task lost its integration record")
	}
	if parked.Integration.AttentionReason != string(integration.ReasonMergeConflict) {
		t.Errorf("attention reason = %q", parked.Integration.AttentionReason)
	}
	if len(parked.Integration.ConflictFiles) != 1 || parked.Integration.ConflictFiles[0] != "backend/a.go" {
		t.Errorf("conflict files = %v", parked.Integration.ConflictFiles)
	}
}

// A single running task is not running in parallel, however parallel the plan
// permits it to be. The label reports the moment, not the permission.
func TestDeriveTaskPlannerViewsLoneRunnerIsNotParallel(t *testing.T) {
	t.Parallel()

	views := workflowcore.DeriveTaskPlannerViews(workflowcore.TaskPlannerInput{
		ProjectMode: domain.ExecutionSmartParallelWorktrees,
		Tasks: []domain.WorkflowTask{
			task("a", 1, domain.WorkflowTaskRunning),
			task("b", 2, domain.WorkflowTaskEligible),
		},
	})
	got := viewByTask(views)
	if got["a"].Status != "" {
		t.Errorf("lone runner status = %q, want empty", got["a"].Status)
	}
	if got["b"].Status != "" {
		t.Errorf("undispatched task status = %q, want empty", got["b"].Status)
	}
}

// A completed task is integrated even with no ledger row of its own: the
// promotion is recorded before the state transition, so "completed" already
// proves the work landed. Runs planned before the ledger existed must not read
// as unfinished forever.
func TestDeriveTaskPlannerViewsCompletedWithoutLedgerRowIsIntegrated(t *testing.T) {
	t.Parallel()

	views := workflowcore.DeriveTaskPlannerViews(workflowcore.TaskPlannerInput{
		Tasks: []domain.WorkflowTask{task("a", 1, domain.WorkflowTaskCompleted)},
	})
	if views[0].Status != workflowcore.TaskPlannerIntegrated {
		t.Errorf("status = %q, want integrated", views[0].Status)
	}
}

// Only the newest ledger row describes where a task's work is now. A task that
// conflicted, was resumed and then landed must read as integrated.
func TestDeriveTaskPlannerViewsUsesNewestIntegrationRow(t *testing.T) {
	t.Parallel()

	views := workflowcore.DeriveTaskPlannerViews(workflowcore.TaskPlannerInput{
		Tasks: []domain.WorkflowTask{task("a", 1, domain.WorkflowTaskRunning)},
		Integrations: []workflowcore.TaskIntegrationRecord{
			{TaskID: "a", Outcome: string(integration.OutcomeNeedsAttention), AttentionReason: string(integration.ReasonMergeConflict)},
			{TaskID: "a", Outcome: string(integration.OutcomeAttempting)},
			{TaskID: "a", Outcome: string(integration.OutcomeIntegrated), Strategy: string(integration.StrategyRebaseFastForward), TargetAfterSHA: "cafe"},
		},
	})
	if views[0].Status != workflowcore.TaskPlannerIntegrated {
		t.Fatalf("status = %q, want integrated", views[0].Status)
	}
	if views[0].Integration.Strategy != string(integration.StrategyRebaseFastForward) {
		t.Errorf("strategy = %q, want the newest row's", views[0].Integration.Strategy)
	}
	if views[0].Integration.TargetAfterSHA != "cafe" {
		t.Errorf("targetAfterSha = %q", views[0].Integration.TargetAfterSHA)
	}
}

// Observed paths replace estimated ones once a task has run. A reader must not
// be left holding both and deciding which is true.
func TestDeriveTaskPlannerViewsObservedWriteScopeWins(t *testing.T) {
	t.Parallel()

	tk := task("a", 1, domain.WorkflowTaskCompleted)
	tk.ScopeJSON = scopeJSON(t, domain.WorkflowTaskScope{
		Source:             domain.WorkflowTaskScopeObserved,
		WritePaths:         []string{"backend/guessed"},
		ObservedWritePaths: []string{"backend/internal/workflow/board.go"},
		Packages:           []string{"backend/internal/workflow"},
	})

	views := workflowcore.DeriveTaskPlannerViews(workflowcore.TaskPlannerInput{Tasks: []domain.WorkflowTask{tk}})
	scope := views[0].WriteScope
	if scope.Source != domain.WorkflowTaskScopeObserved {
		t.Errorf("source = %q", scope.Source)
	}
	if len(scope.WritePaths) != 1 || scope.WritePaths[0] != "backend/internal/workflow/board.go" {
		t.Errorf("writePaths = %v, want the observed set", scope.WritePaths)
	}
	if len(scope.Packages) != 1 {
		t.Errorf("packages = %v", scope.Packages)
	}
}

// The worktree/branch half: what AO created for a task, read back onto the
// view a card renders.
func TestDeriveTaskPlannerViewsCarriesWorktreeAndBranch(t *testing.T) {
	t.Parallel()

	views := workflowcore.DeriveTaskPlannerViews(workflowcore.TaskPlannerInput{
		ProjectMode: domain.ExecutionSmartParallelWorktrees,
		Tasks: []domain.WorkflowTask{
			task("a", 1, domain.WorkflowTaskRunning),
			task("direct", 2, domain.WorkflowTaskRunning),
		},
		Worktrees: []domain.TaskWorktreeRecord{{
			TaskID:       "a",
			Path:         "/data/worktrees/p/run/a",
			Branch:       "ao/run-a",
			TargetBranch: "main",
			BaseSHA:      "beef",
			State:        domain.TaskWorktreeActive,
		}},
	})
	got := viewByTask(views)
	wt := got["a"].Worktree
	if wt == nil {
		t.Fatal("task a has no worktree view")
	}
	if wt.Branch != "ao/run-a" || wt.TargetBranch != "main" || wt.BaseSHA != "beef" {
		t.Errorf("worktree = %+v", *wt)
	}
	if wt.State != domain.TaskWorktreeActive {
		t.Errorf("state = %q", wt.State)
	}
	// A task with no AO worktree reports none rather than an empty one, so
	// "direct-branch" and "worktree we failed to read" stay distinguishable.
	if got["direct"].Worktree != nil {
		t.Errorf("direct-branch task got a worktree view: %+v", *got["direct"].Worktree)
	}
}

// Strategy, dependency ordering and integration ordering all survive the
// projection, and the integration order is the wider of the two.
func TestDeriveTaskPlannerViewsCarriesStrategyAndDependencies(t *testing.T) {
	t.Parallel()

	tk := task("b", 2, domain.WorkflowTaskEligible, "a")
	tk.ScopeJSON = scopeJSON(t, domain.WorkflowTaskScope{
		ExecutionStrategy:       domain.WorkflowTaskExecutionSerialized,
		IntegrationDependencies: []string{"a", "z"},
		ExecutionMode:           domain.ExecutionIsolatedWorktree,
		ExecutionModeDowngrade: &domain.WorkflowTaskExecutionDowngrade{
			From: domain.ExecutionSmartParallelWorktrees, To: domain.ExecutionIsolatedWorktree,
			Serial: true, Reason: "probable_write_conflict", Conflicts: []string{"z"},
		},
	})

	views := workflowcore.DeriveTaskPlannerViews(workflowcore.TaskPlannerInput{
		ProjectMode: domain.ExecutionSmartParallelWorktrees,
		Tasks:       []domain.WorkflowTask{task("a", 1, domain.WorkflowTaskCompleted), tk, task("z", 3, domain.WorkflowTaskEligible)},
	})
	got := viewByTask(views)["b"]
	if got.ExecutionStrategy != domain.WorkflowTaskExecutionSerialized {
		t.Errorf("strategy = %q", got.ExecutionStrategy)
	}
	// The per-task selection wins over the project's own mode.
	if got.ExecutionMode != domain.ExecutionIsolatedWorktree {
		t.Errorf("execution mode = %q, want the downgraded per-task one", got.ExecutionMode)
	}
	if got.Downgrade == nil || !got.Downgrade.Serial {
		t.Fatalf("downgrade = %+v", got.Downgrade)
	}
	if len(got.Dependencies) != 1 || got.Dependencies[0] != "a" {
		t.Errorf("dependencies = %v", got.Dependencies)
	}
	if len(got.IntegrationDependencies) != 2 {
		t.Errorf("integrationDependencies = %v, want the wider set", got.IntegrationDependencies)
	}

	// A task that recorded no per-task selection falls back to the project's.
	if mode := viewByTask(views)["z"].ExecutionMode; mode != domain.ExecutionSmartParallelWorktrees {
		t.Errorf("unselected task mode = %q, want the project's", mode)
	}
}

// A chain orders tasks into successive waves; independent tasks share one.
func TestAssignParallelGroupsFollowsDependencyDepth(t *testing.T) {
	t.Parallel()

	groups := workflowcore.AssignParallelGroups([]domain.WorkflowTask{
		task("a", 1, domain.WorkflowTaskEligible),
		task("b", 2, domain.WorkflowTaskEligible),
		task("c", 3, domain.WorkflowTaskBlocked, "a"),
		task("d", 4, domain.WorkflowTaskBlocked, "c", "b"),
	}, workflowcore.TaskGraphSnapshot{})

	want := map[string]int{"a": 1, "b": 1, "c": 2, "d": 3}
	for id, expected := range want {
		if groups[id] != expected {
			t.Errorf("task %s: group = %d, want %d", id, groups[id], expected)
		}
	}
}

// Two independent tasks that probably write the same region may not share a
// wave, even though nothing orders them.
func TestAssignParallelGroupsSeparatesWriteConflicts(t *testing.T) {
	t.Parallel()

	graph := workflowcore.TaskGraphSnapshot{Relationships: []domain.WorkflowTaskRelationship{
		relationship("a", "b", domain.WorkflowTaskRelationWriteConflict),
		relationship("a", "c", domain.WorkflowTaskRelationIndependent),
		relationship("b", "c", domain.WorkflowTaskRelationIndependent),
	}}
	tasks := []domain.WorkflowTask{
		task("a", 1, domain.WorkflowTaskEligible),
		task("b", 2, domain.WorkflowTaskEligible),
		task("c", 3, domain.WorkflowTaskEligible),
	}

	groups := workflowcore.AssignParallelGroups(tasks, graph)
	if groups["a"] == groups["b"] {
		t.Errorf("conflicting tasks share wave %d", groups["a"])
	}
	if groups["a"] != groups["c"] {
		t.Errorf("independent tasks a=%d c=%d should share a wave", groups["a"], groups["c"])
	}
	// The earlier ordinal keeps the earlier wave, so the grouping is stable
	// under re-derivation rather than depending on map iteration order.
	if groups["a"] != 1 {
		t.Errorf("a = %d, want the first wave", groups["a"])
	}

	// Re-deriving the same plan produces the same answer.
	again := workflowcore.AssignParallelGroups(tasks, graph)
	for id, g := range groups {
		if again[id] != g {
			t.Errorf("task %s: group changed between derivations (%d then %d)", id, g, again[id])
		}
	}

	// And the size travels with the number, so a group of one is legible.
	views := viewByTask(workflowcore.DeriveTaskPlannerViews(workflowcore.TaskPlannerInput{Tasks: tasks, Graph: graph}))
	if views["a"].ParallelGroupSize != 2 {
		t.Errorf("wave 1 size = %d, want 2 (a and c)", views["a"].ParallelGroupSize)
	}
	if views["b"].ParallelGroupSize != 1 {
		t.Errorf("wave 2 size = %d, want 1", views["b"].ParallelGroupSize)
	}
}

// A dependency edge pointing outside the plan cannot order anything, and a
// cycle -- which the planner rejects but a hand-edited row could still hold --
// must not hang the projection.
func TestAssignParallelGroupsToleratesUnknownDepsAndCycles(t *testing.T) {
	t.Parallel()

	groups := workflowcore.AssignParallelGroups([]domain.WorkflowTask{
		task("a", 1, domain.WorkflowTaskEligible, "not-in-this-plan"),
		task("b", 2, domain.WorkflowTaskEligible, "c"),
		task("c", 3, domain.WorkflowTaskEligible, "b"),
	}, workflowcore.TaskGraphSnapshot{})

	if groups["a"] != 1 {
		t.Errorf("task with only an unknown dependency: group = %d, want 1", groups["a"])
	}
	if groups["b"] == 0 || groups["c"] == 0 {
		t.Errorf("cyclic tasks got no group: b=%d c=%d", groups["b"], groups["c"])
	}
}

// A task whose scope JSON will not parse still gets a row. Hiding it would
// make a plan look shorter than it is.
func TestDeriveTaskPlannerViewsToleratesUnparseableScope(t *testing.T) {
	t.Parallel()

	tk := task("a", 1, domain.WorkflowTaskRunning)
	tk.ScopeJSON = "{not json"

	views := workflowcore.DeriveTaskPlannerViews(workflowcore.TaskPlannerInput{Tasks: []domain.WorkflowTask{tk}})
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	if views[0].TaskID != "a" || views[0].ParallelGroup != 1 {
		t.Errorf("view = %+v", views[0])
	}
}
