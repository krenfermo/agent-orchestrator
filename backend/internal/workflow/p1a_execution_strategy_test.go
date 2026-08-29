package workflow_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1a_execution_strategy_test.go — P1-A's durability matrix.
//
// Every test here answers one question: does the strategy a run was created
// with survive the thing that used to lose it? Restarts, recovery, child
// creation, repeated reconciliation, and a caller asking for something the
// run's frozen strategy forbids.

func explicitStrategy(t *testing.T, s domain.ExecutionStrategy) domain.ExecutionStrategySelection {
	t.Helper()
	sel := domain.SelectExecutionStrategy(domain.RequestedExecutionStrategy(s), domain.ExecutionStrategySignals{}, time.Now().UTC())
	if sel.Effective != s || !sel.Chosen() {
		t.Fatalf("explicit selection for %s = %+v", s, sel)
	}
	return sel
}

func strategyOf(t *testing.T, store *crashStore, runID string) domain.ExecutionStrategySelection {
	t.Helper()
	return runPolicy(t, store, runID).Strategy
}

// ---------------------------------------------------------------------------
// 1-3: an explicitly chosen strategy survives a daemon restart, for all three.
// ---------------------------------------------------------------------------

func TestExplicitStrategyPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		strategy domain.ExecutionStrategy
		create   func(*workflowcore.Coordinator, domain.ExecutionStrategySelection) (workflowcore.RunDetail, error)
	}{
		{domain.ExecutionStrategyTask, func(c *workflowcore.Coordinator, sel domain.ExecutionStrategySelection) (workflowcore.RunDetail, error) {
			return c.CreateTaskRun(ctx, workflowcore.TaskRunRequest{ProjectID: "p", Objective: "Fix the typo", Strategy: sel})
		}},
		{domain.ExecutionStrategyAutonomous, func(c *workflowcore.Coordinator, sel domain.ExecutionStrategySelection) (workflowcore.RunDetail, error) {
			return c.CreateObjectiveRunWithStrategy(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual, sel)
		}},
		{domain.ExecutionStrategyMaster, func(c *workflowcore.Coordinator, sel domain.ExecutionStrategySelection) (workflowcore.RunDetail, error) {
			return c.CreateObjectiveRunWithStrategy(ctx, "p", "Rebuild billing", domain.WorkflowPlanApprovalManual, sel)
		}},
	} {
		t.Run(string(tc.strategy), func(t *testing.T) {
			store, reboot := newCrashFixture(t, validMasterPlan())
			sel := explicitStrategy(t, tc.strategy)
			created, err := tc.create(reboot(), sel)
			if err != nil {
				t.Fatal(err)
			}
			runID := created.Run.ID

			// The strategy must be on disk from the creation write itself --
			// not added by some later step a crash could skip.
			if got := strategyOf(t, store, runID); got.Effective != tc.strategy || !got.Chosen() {
				t.Fatalf("at creation: strategy = %+v, want explicit %s", got, tc.strategy)
			}

			// Three restarts, each with a full reconciliation pass. A restart
			// is exactly where the old implicit-flag model lost the answer.
			for pass := 1; pass <= 3; pass++ {
				c := reboot()
				if err := c.Reconcile(ctx); err != nil {
					t.Fatalf("pass %d: Reconcile: %v", pass, err)
				}
				got := strategyOf(t, store, runID)
				if got.Effective != tc.strategy {
					t.Fatalf("pass %d: strategy drifted to %s, want %s", pass, got.Effective, tc.strategy)
				}
				if got.Source != domain.ExecutionStrategyExplicit {
					t.Fatalf("pass %d: source = %s, want explicit -- recovery must never relabel a chosen strategy", pass, got.Source)
				}
				resolved, err := c.EffectiveStrategy(ctx, runID)
				if err != nil || resolved.Effective != tc.strategy {
					t.Fatalf("pass %d: EffectiveStrategy = %+v err=%v, want %s", pass, resolved, err, tc.strategy)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 7/8: the decision is persisted with its provenance, and nothing recomputes
// it later. The selection below carries a reason today's policy would never
// pair with `task`, so a run that still has it after three reconciliations
// proves the value was read back rather than re-derived.
// ---------------------------------------------------------------------------

func TestRecordedDecisionIsNeverRecomputed(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	seeded := domain.ExecutionStrategySelection{
		Effective:     domain.ExecutionStrategyTask,
		Source:        domain.ExecutionStrategyPolicy,
		PolicyVersion: "v0-historic",
		Reason:        domain.ExecutionStrategyReasonMultiWorkstream,
		Signals:       domain.ExecutionStrategySignals{MultiWorkstream: true},
		At:            time.Now().UTC(),
	}
	created, err := reboot().CreateTaskRun(ctx, workflowcore.TaskRunRequest{ProjectID: "p", Objective: "Bounded change", Strategy: seeded})
	if err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 3; pass++ {
		if err := reboot().Reconcile(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		got := strategyOf(t, store, created.Run.ID)
		if got.Effective != domain.ExecutionStrategyTask || got.PolicyVersion != "v0-historic" ||
			got.Reason != domain.ExecutionStrategyReasonMultiWorkstream || !got.Signals.MultiWorkstream {
			t.Fatalf("pass %d: recorded decision was rewritten: %+v", pass, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 9: legacy runs. A run created before P1-A carries no selection at all; boot
// reconciliation maps it from the durable facts it does have, and says the
// answer is a reading of history rather than somebody's choice.
// ---------------------------------------------------------------------------

func TestLegacyRunsAreMappedAndMarkedRecovered(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())

	objective, err := reboot().CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatal(err)
	}
	task, err := reboot().CreateRun(ctx, "p", "Bounded change")
	if err != nil {
		t.Fatal(err)
	}

	// Strip both runs back to a pre-P1-A snapshot: the exact bytes a run
	// created by an older daemon has on disk.
	legacy, err := json.Marshal(domain.DefaultWorkflowPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{objective.Run.ID, task.Run.ID} {
		if ok, uerr := store.UpdateWorkflowRunPolicySnapshot(ctx, id, string(legacy), time.Now().UTC()); uerr != nil || !ok {
			t.Fatalf("seed legacy snapshot for %s: ok=%v err=%v", id, ok, uerr)
		}
		if strategyOf(t, store, id).Recorded() {
			t.Fatalf("%s still has a recorded strategy; the fixture did not reproduce a legacy run", id)
		}
	}

	if err := reboot().Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	objectiveStrategy := strategyOf(t, store, objective.Run.ID)
	if objectiveStrategy.Effective != domain.ExecutionStrategyAutonomous ||
		objectiveStrategy.Source != domain.ExecutionStrategyRecovered ||
		objectiveStrategy.Reason != domain.ExecutionStrategyReasonLegacyPlannedRun {
		t.Fatalf("legacy objective mapped to %+v, want recovered/autonomous", objectiveStrategy)
	}
	// Deliberately NOT master: no legacy row records that anybody chose a
	// large initiative, and inventing that would be inventing history.
	taskStrategy := strategyOf(t, store, task.Run.ID)
	if taskStrategy.Effective != domain.ExecutionStrategyTask ||
		taskStrategy.Source != domain.ExecutionStrategyRecovered ||
		taskStrategy.Reason != domain.ExecutionStrategyReasonLegacySingleTaskRun {
		t.Fatalf("legacy single-task run mapped to %+v, want recovered/task", taskStrategy)
	}

	// And the mapping is stable: once recorded it is never re-derived.
	for pass := 2; pass <= 3; pass++ {
		if err := reboot().Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if got := strategyOf(t, store, objective.Run.ID); got != objectiveStrategy {
			t.Fatalf("pass %d: recovered mapping changed: %+v -> %+v", pass, objectiveStrategy, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 10/17/21: TASK genuinely avoids master orchestration, still gets the full
// review/verify chain, and cannot be talked into planning afterwards.
// ---------------------------------------------------------------------------

func TestTaskStrategyNeverPlansAndKeepsReviewAndVerify(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	planner := &staticPlanner{plan: validMasterPlan()}
	c := workflowcore.New(workflowcore.Deps{Store: store, Projects: store, Planner: planner, PlannerContextBuilder: staticContext{}})

	created, err := c.CreateTaskRun(ctx, workflowcore.TaskRunRequest{
		ProjectID: "p", Objective: "Rename the flag", Strategy: explicitStrategy(t, domain.ExecutionStrategyTask),
		AcceptanceCriteria: []string{"The flag is renamed everywhere it is read."},
	})
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Run.ID

	if _, isMaster, err := store.GetWorkflowPlan(ctx, runID); err != nil || isMaster {
		t.Fatalf("a task run owns a master plan row (isMaster=%v err=%v): that is the orchestration it exists to avoid", isMaster, err)
	}
	if planner.calls != 0 {
		t.Fatalf("the planner was invoked %d times for a task run", planner.calls)
	}

	// TASK is not "skip review": the full durable chain is still there.
	kinds := stepsByKind(t, store, runID)
	for _, kind := range []domain.WorkflowStepKind{
		domain.WorkflowStepPlan, domain.WorkflowStepWork, domain.WorkflowStepReview,
		domain.WorkflowStepFix, domain.WorkflowStepVerify, domain.WorkflowStepAdvance,
	} {
		if _, ok := kinds[kind]; !ok {
			t.Fatalf("task run is missing its %s step", kind)
		}
	}
	artifact := childPlanArtifact(t, store, runID)
	if len(artifact.AcceptanceCriteria) != 1 || artifact.AcceptanceCriteria[0] != "The flag is renamed everywhere it is read." {
		t.Fatalf("task acceptance criteria = %v, want the caller's", artifact.AcceptanceCriteria)
	}

	// 21: a later caller -- a stale writer, a resumed wake, a retry -- cannot
	// turn this run into a planned one, and the refusal survives the restart.
	for pass := 1; pass <= 2; pass++ {
		if err := reboot().Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := reboot().GeneratePlan(ctx, runID); err == nil {
			t.Fatalf("pass %d: GeneratePlan succeeded on a task run", pass)
		}
		if got := strategyOf(t, store, runID); got.Effective != domain.ExecutionStrategyTask {
			t.Fatalf("pass %d: strategy became %s", pass, got.Effective)
		}
	}
	if planner.calls != 0 {
		t.Fatalf("the planner ran %d times after the refusals", planner.calls)
	}
}

// 16: a read-only TASK declares its intent durably at creation, which is the
// only thing that lets an unchanged worktree read as success rather than as
// ambiguous_worker_state.
func TestReadOnlyTaskDeclaresWriteIntentAtCreation(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	created, err := reboot().CreateTaskRun(ctx, workflowcore.TaskRunRequest{
		ProjectID: "p", Objective: "Audit the config", Strategy: explicitStrategy(t, domain.ExecutionStrategyTask),
		WriteIntent: domain.WorkflowWriteIntentReadOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := childPlanArtifact(t, store, created.Run.ID)
	if artifact.WriteIntent != domain.WorkflowWriteIntentReadOnly {
		t.Fatalf("write intent = %q, want read_only", artifact.WriteIntent)
	}
	if len(artifact.AcceptanceCriteria) == 0 {
		t.Fatal("declaring only a write intent silently dropped the default acceptance criteria")
	}
	// A mutating task is the conservative default, unchanged.
	mutating, err := reboot().CreateTaskRun(ctx, workflowcore.TaskRunRequest{
		ProjectID: "p", Objective: "Change the config", Strategy: explicitStrategy(t, domain.ExecutionStrategyTask),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := childPlanArtifact(t, store, mutating.Run.ID).WriteIntent; got.ReadOnly() {
		t.Fatalf("undeclared write intent = %q, want the conservative mutating default", got)
	}
}

// ---------------------------------------------------------------------------
// 11/12/13/14/23: the planned strategies. A master objective plans, creates
// durable children, gives each one a bounded inherited strategy, and repeated
// reconciliation neither duplicates a child nor moves a strategy.
// ---------------------------------------------------------------------------

func TestMasterCreatesDurableChildrenWithBoundedInheritedStrategy(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	created, err := reboot().CreateObjectiveRunWithStrategy(ctx, "p", "Build users",
		domain.WorkflowPlanApprovalAuto, explicitStrategy(t, domain.ExecutionStrategyMaster))
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Run.ID
	if _, err := reboot().GeneratePlan(ctx, runID); err != nil {
		t.Fatal(err)
	}

	tasks, err := store.ListWorkflowTasks(ctx, runID)
	if err != nil || len(tasks) == 0 {
		t.Fatalf("ListWorkflowTasks: %d tasks err=%v -- a master objective must produce a durable plan", len(tasks), err)
	}

	childIDs := func() []string {
		t.Helper()
		current, err := store.ListWorkflowTasks(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, task := range current {
			id, ok, ferr := store.FindWorkflowRunByPlannedTask(ctx, task.ID)
			if ferr != nil {
				t.Fatal(ferr)
			}
			if ok {
				ids = append(ids, id)
			}
		}
		return ids
	}

	first := childIDs()
	if len(first) == 0 {
		t.Fatal("a master objective produced no child workstream")
	}

	// Repeated reconciliation must converge: same children, same strategies.
	for pass := 1; pass <= 3; pass++ {
		if err := reboot().Reconcile(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if got := len(childIDs()); got != len(first) {
			t.Fatalf("pass %d: child count %d -> %d; reconcile duplicated children", pass, len(first), got)
		}
		if parent := strategyOf(t, store, runID); parent.Effective != domain.ExecutionStrategyMaster {
			t.Fatalf("pass %d: parent strategy drifted to %s", pass, parent.Effective)
		}
		for _, childID := range first {
			child := strategyOf(t, store, childID)
			if child.Effective == domain.ExecutionStrategyMaster {
				t.Fatalf("pass %d: child %s became master -- recursive decomposition is unbounded", pass, childID)
			}
			if child.Effective != domain.ExecutionStrategyTask || child.Source != domain.ExecutionStrategyInherited {
				t.Fatalf("pass %d: child %s strategy = %+v, want an inherited task", pass, childID, child)
			}
			if child.ParentRunID != runID {
				t.Fatalf("pass %d: child %s names parent %q, want %s", pass, childID, child.ParentRunID, runID)
			}
			if child.Depth > domain.ExecutionStrategyMaxChildDepth {
				t.Fatalf("pass %d: child %s depth = %d, want <= %d", pass, childID, child.Depth, domain.ExecutionStrategyMaxChildDepth)
			}
			// 14/15: the child still proves it inherited its parent's frozen
			// execution policy -- the P0 invariant P1-A must not disturb.
			prov := runPolicy(t, store, childID).Execution.Provenance
			if prov.Source != domain.ExecutionPolicyInherited || prov.ParentRunID != runID {
				t.Fatalf("pass %d: child %s policy provenance = %+v, want inherited from %s", pass, childID, prov, runID)
			}
		}
	}
}

// 23: a restart landing in the middle of strategy-dependent dispatch -- the
// child exists, its binding write was lost -- converges without changing any
// strategy and without a second child.
func TestRestartDuringChildDispatchConvergesWithoutChangingStrategy(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	masterID, childID := dispatchedChildAfterCrash(t, store, reboot)

	before := strategyOf(t, store, childID)
	if before.Effective != domain.ExecutionStrategyTask || before.ParentRunID != masterID {
		t.Fatalf("child strategy at the crash = %+v, want an inherited task of %s", before, masterID)
	}
	for pass := 1; pass <= 3; pass++ {
		if err := reboot().Reconcile(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if got := strategyOf(t, store, childID); got.Effective != before.Effective || got.ParentRunID != before.ParentRunID {
			t.Fatalf("pass %d: child strategy moved: %+v -> %+v", pass, before, got)
		}
	}
}

// 22: ContinueRun -- the wake poller's only entry point and the Continue
// button's -- never alters the strategy it resumes under.
func TestContinueRunPreservesStrategy(t *testing.T) {
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	created, err := reboot().CreateObjectiveRunWithStrategy(ctx, "p", "Build users",
		domain.WorkflowPlanApprovalManual, explicitStrategy(t, domain.ExecutionStrategyMaster))
	if err != nil {
		t.Fatal(err)
	}
	before := strategyOf(t, store, created.Run.ID)
	if _, err := reboot().ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if got := strategyOf(t, store, created.Run.ID); got != before {
		t.Fatalf("ContinueRun changed the strategy: %+v -> %+v", before, got)
	}
}

// 18: approval policy and execution strategy are independent axes. Every
// combination is representable, and neither one moves the other.
func TestApprovalPolicyIsIndependentOfExecutionStrategy(t *testing.T) {
	ctx := context.Background()
	for _, strategy := range []domain.ExecutionStrategy{domain.ExecutionStrategyAutonomous, domain.ExecutionStrategyMaster} {
		for _, mode := range []domain.WorkflowPlanApprovalMode{domain.WorkflowPlanApprovalManual, domain.WorkflowPlanApprovalAuto} {
			store, reboot := newCrashFixture(t, validMasterPlan())
			created, err := reboot().CreateObjectiveRunWithStrategy(ctx, "p", "Build users", mode, explicitStrategy(t, strategy))
			if err != nil {
				t.Fatalf("%s/%s: %v", strategy, mode, err)
			}
			plan, isMaster, err := store.GetWorkflowPlan(ctx, created.Run.ID)
			if err != nil || !isMaster {
				t.Fatalf("%s/%s: no plan row (isMaster=%v err=%v)", strategy, mode, isMaster, err)
			}
			if plan.ApprovalMode != mode {
				t.Fatalf("%s/%s: approval mode = %s -- the strategy changed the approval policy", strategy, mode, plan.ApprovalMode)
			}
			if got := strategyOf(t, store, created.Run.ID); got.Effective != strategy {
				t.Fatalf("%s/%s: strategy = %s -- the approval policy changed the strategy", strategy, mode, got.Effective)
			}
		}
	}
}

// 20: an invalid strategy is refused at the coordinator boundary too, not
// only at the API's. CreateObjectiveRunWithStrategy must never quietly
// upgrade a task into a planned objective.
func TestCreateRefusesMismatchedStrategy(t *testing.T) {
	ctx := context.Background()
	_, reboot := newCrashFixture(t, validMasterPlan())
	if _, err := reboot().CreateObjectiveRunWithStrategy(ctx, "p", "Build users",
		domain.WorkflowPlanApprovalManual, explicitStrategy(t, domain.ExecutionStrategyTask)); err == nil {
		t.Fatal("a task strategy created a planned objective")
	}
	_, err := reboot().CreateTaskRun(ctx, workflowcore.TaskRunRequest{
		ProjectID: "p", Objective: "Build users", Strategy: explicitStrategy(t, domain.ExecutionStrategyMaster),
	})
	if err == nil || !strings.Contains(err.Error(), "master") {
		t.Fatalf("CreateTaskRun with a master strategy: err = %v, want a refusal naming the strategy", err)
	}
}
