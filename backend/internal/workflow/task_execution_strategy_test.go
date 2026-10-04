package workflow_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// independentPlanTasks are two tasks that each write one specific, different
// file: the classifier can name what both of them touch and prove the two sets
// are disjoint, which is exactly the evidence smart-parallel worktrees require.
func independentPlanTasks() []workflowcore.TaskScopeInput {
	return []workflowcore.TaskScopeInput{
		scopeInput("wft-a", 1, "Classifier", "Add backend/internal/workflow/task_graph.go.", []string{"Classifier is pure."}, nil),
		scopeInput("wft-b", 2, "Board", "Update backend/internal/workflow/board.go.", []string{"Board renders."}, nil),
	}
}

func classify(tasks []workflowcore.TaskScopeInput) workflowcore.TaskGraph {
	return workflowcore.ClassifyTaskGraph(workflowcore.TaskGraphInput{WorkflowRunID: "wf-1", Tasks: tasks})
}

func placementFor(t *testing.T, placements map[string]workflowcore.TaskExecutionPlacement, id string) workflowcore.TaskExecutionPlacement {
	t.Helper()
	p, ok := placements[id]
	if !ok {
		t.Fatalf("no placement selected for %s", id)
	}
	return p
}

// Tasks the classifier proved independent, with write sets specific enough to
// have proved it, keep the strategy the project asked for.
func TestSmartParallelKeepsIndependentTasksParallel(t *testing.T) {
	graph := workflowcore.ApplyTaskExecutionStrategies(domain.ExecutionSmartParallelWorktrees, classify(independentPlanTasks()))
	for _, id := range []string{"wft-a", "wft-b"} {
		scope := graph.Scopes[id]
		if scope.ExecutionMode != domain.ExecutionSmartParallelWorktrees {
			t.Fatalf("%s execution mode=%q, want smart_parallel_worktrees", id, scope.ExecutionMode)
		}
		if scope.ExecutionModeDowngrade != nil {
			t.Fatalf("%s was downgraded with nothing to downgrade for: %+v", id, scope.ExecutionModeDowngrade)
		}
		// The scheduling strategy is the classifier's, and parallel worktrees
		// must not have disturbed it.
		if scope.ExecutionStrategy != domain.WorkflowTaskExecutionParallel {
			t.Fatalf("%s scheduling strategy=%q, want parallel", id, scope.ExecutionStrategy)
		}
	}
}

// A probable write conflict is a fact about the plan, not a doubt about it, so
// it demotes all the way to serial: a private worktree removes the physical
// collision but both tasks still have to land in some order.
func TestSmartParallelSerializesAConflictingPair(t *testing.T) {
	tasks := []workflowcore.TaskScopeInput{
		scopeInput("wft-a", 1, "Add a field", "Add a field to backend/internal/domain/workflow_plan.go.", []string{"The field is persisted."}, nil),
		scopeInput("wft-b", 2, "Add a constant", "Add a constant to backend/internal/domain/workflow_plan.go.", []string{"The constant is exported."}, nil),
	}
	placements := workflowcore.SelectTaskExecutionStrategies(domain.ExecutionSmartParallelWorktrees, classify(tasks))
	for id, partner := range map[string]string{"wft-a": "wft-b", "wft-b": "wft-a"} {
		p := placementFor(t, placements, id)
		if p.Mode != domain.ExecutionIsolatedWorktree {
			t.Fatalf("%s mode=%q, want isolated_worktree", id, p.Mode)
		}
		if !p.Serial {
			t.Fatalf("%s was left free to run concurrently with %s", id, partner)
		}
		if p.Downgrade == nil {
			t.Fatalf("%s was downgraded without recording a reason", id)
		}
		if p.Downgrade.Reason != string(workflowcore.TaskStrategyReasonWriteConflict) {
			t.Fatalf("%s downgrade reason=%q, want %q", id, p.Downgrade.Reason, workflowcore.TaskStrategyReasonWriteConflict)
		}
		if p.Downgrade.PolicyVersion != workflowcore.TaskStrategyPolicyVersion {
			t.Fatalf("%s downgrade policy version=%q, want %q", id, p.Downgrade.PolicyVersion, workflowcore.TaskStrategyPolicyVersion)
		}
		if p.Downgrade.From != domain.ExecutionSmartParallelWorktrees || p.Downgrade.To != domain.ExecutionIsolatedWorktree {
			t.Fatalf("%s downgrade=%+v, want smart_parallel_worktrees -> isolated_worktree", id, p.Downgrade)
		}
		if len(p.Downgrade.Conflicts) != 1 || p.Downgrade.Conflicts[0] != partner {
			t.Fatalf("%s downgrade conflicts=%+v, want [%s]", id, p.Downgrade.Conflicts, partner)
		}
		if !strings.Contains(p.Downgrade.Detail, partner) {
			t.Fatalf("%s downgrade detail does not name the sibling it collides with: %q", id, p.Downgrade.Detail)
		}
	}
}

// Uncertainty is not conflict: the task may still run alongside whatever the
// DAG allows, it just does not get a grant that assumes a known write set.
func TestSmartParallelDowngradesUncertainClassification(t *testing.T) {
	tests := []struct {
		name   string
		task   workflowcore.TaskScopeInput
		reason workflowcore.TaskStrategyReason
	}{
		{
			// Nothing in this task's text names a path, so the classifier
			// cannot say what it writes -- and therefore cannot say it does
			// not write what its sibling writes.
			name:   "no write path could be estimated",
			task:   scopeInput("wft-x", 3, "Narrative", "Rewrite the onboarding narrative so it reads well.", []string{"It reads well."}, nil),
			reason: workflowcore.TaskStrategyReasonUnknownWriteSet,
		},
		{
			// A directory in a write set is the classifier saying "somewhere
			// in here, file unknown".
			name:   "write set is only a directory",
			task:   scopeInput("wft-x", 3, "Renderer", "Update everything under frontend/src/renderer to use the new tokens.", []string{"frontend/src/renderer compiles."}, nil),
			reason: workflowcore.TaskStrategyReasonCoarseWriteSet,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			graph := workflowcore.ApplyTaskExecutionStrategies(
				domain.ExecutionSmartParallelWorktrees,
				classify(append(independentPlanTasks(), tc.task)),
			)
			scope := graph.Scopes["wft-x"]
			if scope.ExecutionMode != domain.ExecutionIsolatedWorktree {
				t.Fatalf("execution mode=%q, want isolated_worktree", scope.ExecutionMode)
			}
			if scope.ExecutionModeDowngrade == nil {
				t.Fatalf("uncertain task was downgraded without recording a reason")
			}
			if scope.ExecutionModeDowngrade.Reason != string(tc.reason) {
				t.Fatalf("downgrade reason=%q, want %q", scope.ExecutionModeDowngrade.Reason, tc.reason)
			}
			if scope.ExecutionModeDowngrade.Serial {
				t.Fatalf("uncertainty alone serialized the task: %+v", scope.ExecutionModeDowngrade)
			}
			if len(scope.ExecutionModeDowngrade.Conflicts) != 0 {
				t.Fatalf("uncertainty downgrade named conflicts it did not find: %+v", scope.ExecutionModeDowngrade.Conflicts)
			}
			// The certain siblings are unaffected: one vague task must not
			// cost the whole plan its parallelism.
			for _, id := range []string{"wft-a", "wft-b"} {
				if got := graph.Scopes[id].ExecutionMode; got != domain.ExecutionSmartParallelWorktrees {
					t.Fatalf("%s mode=%q, want smart_parallel_worktrees", id, got)
				}
			}
		})
	}
}

// A write set the run has already OBSERVED is never uncertain -- those paths
// came from a worktree, not from prose -- so an observed scope keeps the grant
// even though it would have been called coarse as an estimate.
func TestObservedWriteSetIsNotUncertain(t *testing.T) {
	graph := workflowcore.TaskGraph{
		Scopes: map[string]domain.WorkflowTaskScope{
			"wft-a": {
				Version:            workflowcore.TaskGraphPolicyVersion,
				Source:             domain.WorkflowTaskScopeObserved,
				WritePaths:         []string{"backend/internal/workflow"},
				ObservedWritePaths: []string{"backend/internal/workflow"},
			},
		},
	}
	graph = workflowcore.ApplyTaskExecutionStrategies(domain.ExecutionSmartParallelWorktrees, graph)
	if got := graph.Scopes["wft-a"].ExecutionMode; got != domain.ExecutionSmartParallelWorktrees {
		t.Fatalf("observed scope mode=%q, want smart_parallel_worktrees", got)
	}
}

// REGRESSION (the point of this test): selecting direct_branch must leave the
// planning output exactly what it was before the selector existed.
// ClassifyTaskGraph IS the pre-change pipeline in full, so comparing the
// serialized scopes on both sides of ApplyTaskExecutionStrategies is a literal
// byte-for-byte check against the old behavior.
func TestNonSmartModesLeavePlanningOutputByteForByteUnchanged(t *testing.T) {
	tasks := append(independentPlanTasks(),
		scopeInput("wft-c", 3, "Conflict", "Add a constant to backend/internal/workflow/board.go.", []string{"The constant is exported."}, nil),
		scopeInput("wft-d", 4, "Narrative", "Rewrite the onboarding narrative so it reads well.", []string{"It reads well."}, nil),
	)
	before := map[string]string{}
	for id, scope := range classify(tasks).Scopes {
		raw, err := workflowcore.MarshalTaskScope(scope)
		if err != nil {
			t.Fatal(err)
		}
		before[id] = raw
	}

	// "" is every project registered before the execution-mode setting existed.
	for _, mode := range []domain.ExecutionMode{"", domain.ExecutionIsolatedWorktree, domain.ExecutionDirectBranch} {
		t.Run(string("mode="+mode), func(t *testing.T) {
			graph := workflowcore.ApplyTaskExecutionStrategies(mode, classify(tasks))
			for id, scope := range graph.Scopes {
				raw, err := workflowcore.MarshalTaskScope(scope)
				if err != nil {
					t.Fatal(err)
				}
				if raw != before[id] {
					t.Fatalf("%s scope changed under %q:\n before: %s\n  after: %s", id, mode, before[id], raw)
				}
				if strings.Contains(raw, "executionMode") || strings.Contains(raw, "executionModeDowngrade") {
					t.Fatalf("%s persisted a per-task selection under %q: %s", id, mode, raw)
				}
			}
		})
	}
}

// Moving a project OFF smart_parallel_worktrees must clear the downgrades its
// old setting produced, or a task would keep carrying a demotion from a rule
// that no longer applies to it.
func TestLeavingSmartParallelClearsStaleSelections(t *testing.T) {
	tasks := []workflowcore.TaskScopeInput{
		scopeInput("wft-a", 1, "Add a field", "Add a field to backend/internal/domain/workflow_plan.go.", []string{"The field is persisted."}, nil),
		scopeInput("wft-b", 2, "Add a constant", "Add a constant to backend/internal/domain/workflow_plan.go.", []string{"The constant is exported."}, nil),
	}
	graph := workflowcore.ApplyTaskExecutionStrategies(domain.ExecutionSmartParallelWorktrees, classify(tasks))
	if graph.Scopes["wft-a"].ExecutionModeDowngrade == nil {
		t.Fatalf("fixture no longer produces a downgrade")
	}
	graph = workflowcore.ApplyTaskExecutionStrategies(domain.ExecutionDirectBranch, graph)
	for _, id := range []string{"wft-a", "wft-b"} {
		scope := graph.Scopes[id]
		if scope.ExecutionMode != "" || scope.ExecutionModeDowngrade != nil {
			t.Fatalf("%s kept a stale selection: mode=%q downgrade=%+v", id, scope.ExecutionMode, scope.ExecutionModeDowngrade)
		}
	}
}

// ResolveTaskExecutionMode is how every reader must ask the question: an unset
// per-task selection means "use the project's mode", never "unknown".
func TestResolveTaskExecutionModeFallsBackToTheProject(t *testing.T) {
	tests := []struct {
		project domain.ExecutionMode
		scope   domain.WorkflowTaskScope
		want    domain.ExecutionMode
	}{
		{project: "", scope: domain.WorkflowTaskScope{}, want: domain.ExecutionIsolatedWorktree},
		{project: domain.ExecutionDirectBranch, scope: domain.WorkflowTaskScope{}, want: domain.ExecutionDirectBranch},
		{project: domain.ExecutionSmartParallelWorktrees, scope: domain.WorkflowTaskScope{}, want: domain.ExecutionSmartParallelWorktrees},
		{
			project: domain.ExecutionSmartParallelWorktrees,
			scope:   domain.WorkflowTaskScope{ExecutionMode: domain.ExecutionIsolatedWorktree},
			want:    domain.ExecutionIsolatedWorktree,
		},
	}
	for _, tc := range tests {
		if got := domain.ResolveTaskExecutionMode(tc.project, tc.scope); got != tc.want {
			t.Fatalf("project=%q scope=%q -> %q, want %q", tc.project, tc.scope.ExecutionMode, got, tc.want)
		}
	}
}

// newMasterFixtureWithExecutionMode is newMasterFixture with the project
// configured for one execution mode, so plan acceptance can be exercised end to
// end against the setting the selector reads.
func newMasterFixtureWithExecutionMode(t *testing.T, plan workflowcore.MasterPlan, mode domain.ExecutionMode) (*workflowcore.Coordinator, string) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	project := domain.ProjectRecord{
		ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC(),
		Config: domain.ProjectConfig{ExecutionMode: mode},
	}
	if err := store.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	c := workflowcore.New(workflowcore.Deps{Store: store, Projects: store, Planner: &staticPlanner{plan: plan}, PlannerContextBuilder: staticContext{}})
	created, err := c.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	return c, created.Run.ID
}

// Plan acceptance is where the selection happens, so the strategy and any
// downgrade must be on the task row the moment the plan is accepted -- not
// derived later by whoever schedules it.
func TestAcceptedPlanPersistsSelectedExecutionStrategy(t *testing.T) {
	c, runID := newMasterFixtureWithExecutionMode(t, conflictingMasterPlan(), domain.ExecutionSmartParallelWorktrees)
	detail, err := c.GeneratePlan(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Tasks) != 3 {
		t.Fatalf("tasks=%d, want 3", len(detail.Tasks))
	}
	byStep := map[string]domain.WorkflowTaskScope{}
	for _, task := range detail.Tasks {
		scope, err := workflowcore.UnmarshalTaskScope(task.ScopeJSON)
		if err != nil {
			t.Fatal(err)
		}
		byStep[task.PlanStepID] = scope
	}
	// "model" and "roles" both declare backend/internal/domain/user.go and
	// neither depends on the other: the one pair in this plan that collides.
	for _, step := range []string{"model", "roles"} {
		scope := byStep[step]
		if scope.ExecutionMode != domain.ExecutionIsolatedWorktree {
			t.Fatalf("%s mode=%q, want isolated_worktree", step, scope.ExecutionMode)
		}
		if scope.ExecutionModeDowngrade == nil || !scope.ExecutionModeDowngrade.Serial {
			t.Fatalf("%s conflicting task was not serialized: %+v", step, scope.ExecutionModeDowngrade)
		}
		if scope.ExecutionModeDowngrade.Reason != string(workflowcore.TaskStrategyReasonWriteConflict) {
			t.Fatalf("%s downgrade reason=%q", step, scope.ExecutionModeDowngrade.Reason)
		}
	}
	// "tests" writes its own file and only depends on "model": a dependency
	// edge is an ordering, not a downgrade.
	tests := byStep["tests"]
	if tests.ExecutionMode != domain.ExecutionSmartParallelWorktrees || tests.ExecutionModeDowngrade != nil {
		t.Fatalf("tests mode=%q downgrade=%+v, want an undowngraded smart_parallel_worktrees task", tests.ExecutionMode, tests.ExecutionModeDowngrade)
	}
}

// The same plan accepted by a direct-branch project persists exactly the scopes
// it persisted before this selector existed: no strategy, no downgrade.
func TestAcceptedPlanUnderDirectBranchPersistsNoSelection(t *testing.T) {
	direct, directRun := newMasterFixtureWithExecutionMode(t, conflictingMasterPlan(), domain.ExecutionDirectBranch)
	unset, unsetRun := newMasterFixtureWithExecutionMode(t, conflictingMasterPlan(), "")
	ctx := context.Background()
	directDetail, err := direct.GeneratePlan(ctx, directRun)
	if err != nil {
		t.Fatal(err)
	}
	unsetDetail, err := unset.GeneratePlan(ctx, unsetRun)
	if err != nil {
		t.Fatal(err)
	}
	unsetByStep := scopesByPlanStep(unsetDetail.Tasks)
	directByStep := scopesByPlanStep(directDetail.Tasks)
	for step, raw := range directByStep {
		if strings.Contains(raw, "executionMode") {
			t.Fatalf("direct-branch task %s persisted a per-task selection: %s", step, raw)
		}
		if got := unsetByStep[step]; got != raw {
			t.Fatalf("direct-branch task %s scope diverged from the pre-setting behavior:\n unset: %s\ndirect: %s", step, got, raw)
		}
	}
}

// scopesByPlanStep keys each task's scope JSON by its stable plan step id, with
// every generated task id inside the JSON rewritten to the step id it belongs
// to. Task ids are freshly generated per run, so comparing two runs' raw scopes
// would compare their uuids; the plan step ids are what both runs actually
// agree on.
func scopesByPlanStep(tasks []domain.WorkflowTask) map[string]string {
	stepByTaskID := make(map[string]string, len(tasks))
	for _, task := range tasks {
		stepByTaskID[task.ID] = task.PlanStepID
	}
	out := make(map[string]string, len(tasks))
	for _, task := range tasks {
		raw := task.ScopeJSON
		for id, step := range stepByTaskID {
			raw = strings.ReplaceAll(raw, id, step)
		}
		out[task.PlanStepID] = raw
	}
	return out
}
