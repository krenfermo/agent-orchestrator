package workflow_test

// Checkpoint 8P-D: headless progression -- once kicked off, an autonomous
// master run must advance task dispatch, review, fix, verify, and
// integration promotion purely via wakepoller.Poller.RunDueOnce. Every test
// here calls ApplyExecutionPolicySnapshot exactly once as the kickoff, then
// drives with driveCycles alone; any "out of band" fact injection (review
// verdicts, agent health events) is a direct store/fake write, mirroring
// how `ao review submit`/a real provider outage lands in production -- never
// a GetRun/ContinueRun/GeneratePlan/ApprovePlan call from the test.

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wakepoller"
)

// TestAutonomous_HeadlessProgression_NoGET is Checkpoint 8P-D's central
// multi-task claim (test-matrix items 4 and 8 combined): a 2-task plan
// (task "tests" depends on task "model") drives end to end -- task "tests"
// only gets an ExecutionRunID after task "model" reaches
// WorkflowTaskCompleted, a reviewer is dispatched and approved for each task
// along the way (review is REQUIRED by construction: dispatchReviewStep
// always launches one), and the run eventually reaches
// WorkflowRunCompleted -- all purely via the poller.
func TestAutonomous_HeadlessProgression_NoGET(t *testing.T) {
	fx := newAutonomousFixture(t, twoTaskDependentPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	bDispatchedBeforeACompleted := false
	bEverDispatched := false

	driveCycles(t, fx, 40, func(i int) {
		taskA, okA := taskByPlanStepID(t, fx, created.Run.ID, "model")
		taskB, okB := taskByPlanStepID(t, fx, created.Run.ID, "tests")
		if okB && taskB.ExecutionRunID != nil {
			bEverDispatched = true
			if okA && taskA.State != domain.WorkflowTaskCompleted {
				bDispatchedBeforeACompleted = true
			}
		}
		// Approve whichever child's review is currently open -- the
		// out-of-band `ao review submit` equivalent.
		if _, childID, ok := activeChildRunID(t, fx, created.Run.ID); ok {
			approveOpenReview(t, fx, childID, domain.VerdictApproved)
		}
	})

	if bDispatchedBeforeACompleted {
		t.Fatalf("task B (depends on A) was dispatched before task A completed")
	}
	if !bEverDispatched {
		t.Fatalf("task B never dispatched -- headless progression stalled after task A")
	}
	if fx.launcher.launchCalls < 2 {
		t.Fatalf("reviewer launch calls = %d, want at least 2 (one per task)", fx.launcher.launchCalls)
	}
	run, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", run.State)
	}
	if fx.planner.calls != 1 {
		t.Fatalf("planner calls = %d, want exactly 1", fx.planner.calls)
	}
	taskA, _ := taskByPlanStepID(t, fx, created.Run.ID, "model")
	taskB, _ := taskByPlanStepID(t, fx, created.Run.ID, "tests")
	if taskA.State != domain.WorkflowTaskCompleted || taskB.State != domain.WorkflowTaskCompleted {
		t.Fatalf("tasks not both completed: A=%s B=%s", taskA.State, taskB.State)
	}
}

// TestAutonomous_ChangesRequestedFixLoopThenVerifyThenComplete: the reviewer
// requests changes exactly once; the fix worker must be dispatched into the
// SAME session (MessageSender.Send), and once the workspace shows a genuine
// change (a new observation), the fix cycle resolves, review cycle 2
// approves, verify passes, and the run completes -- all headlessly.
func TestAutonomous_ChangesRequestedFixLoopThenVerifyThenComplete(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	changesRequestedOnce := false
	fixTriggered := false

	driveCycles(t, fx, 40, func(i int) {
		_, childID, ok := activeChildRunID(t, fx, created.Run.ID)
		if !ok {
			return
		}
		if !changesRequestedOnce {
			if approveOpenReview(t, fx, childID, domain.VerdictChangesRequested) {
				changesRequestedOnce = true
			}
			return
		}
		if changesRequestedOnce && !fixTriggered && fx.sender.calls > 0 {
			// Simulate the worker actually producing a genuine change in
			// response to the fix findings, so the fix cycle can resolve.
			fx.ws.obs.Changes = append(fx.ws.obs.Changes, ports.WorkspaceChange{Path: "fix.go", Status: " M"})
			fixTriggered = true
			return
		}
		approveOpenReview(t, fx, childID, domain.VerdictApproved)
	})

	if !changesRequestedOnce {
		t.Fatalf("changes_requested verdict was never applied -- test setup issue")
	}
	if fx.sender.calls == 0 {
		t.Fatalf("MessageSender.Send calls = 0, want at least 1 (fix findings never delivered)")
	}
	run, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", run.State)
	}
	if fx.verifier.calls == 0 {
		t.Fatalf("verifier calls = 0, want at least 1")
	}
}

// TestAutonomous_CapacityWaitResumesHeadlessly: both worker harnesses are in
// cooldown at kickoff time, so task dispatch must park in a durable
// worker_capacity wait; once Claude Code's cooldown lifts, the poller alone
// (no test-driven GetRun/ContinueRun) must resume dispatch.
func TestAutonomous_CapacityWaitResumesHeadlessly(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	reset := fx.clk.Now().Add(45 * time.Minute)
	profileByHarness := map[domain.AgentHarness]domain.ProviderProfileID{
		domain.HarnessClaudeCode: "prof-claude-1",
		domain.HarnessCodex:      "prof-codex-1",
	}
	for _, h := range []domain.AgentHarness{domain.HarnessClaudeCode, domain.HarnessCodex} {
		if _, err := fx.store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
			ID: "ahe-" + string(h), Harness: h, UserID: "user-1", ProviderProfileID: profileByHarness[h], State: domain.AgentHealthCooldown,
			Reason: "test", CooldownUntil: &reset, CreatedAt: fx.clk.Now(),
		}); err != nil {
			t.Fatalf("RecordAgentHealthEvent(%s): %v", h, err)
		}
	}

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	// Drive exactly one cycle: plan generation + auto-approval + first task
	// dispatch all chain synchronously off the single kickoff wake, parking
	// the child on capacity before any retry has consumed the work step's
	// dispatch-attempt budget (policy default maxWorkProviderAttempts=3 --
	// driving further cycles before recovering capacity would burn that
	// budget via the child's own backoff-scheduled retries and land the run
	// in needs_attention, which is a real, unrelated policy gate, not what
	// this test is proving).
	driveCycles(t, fx, 1, nil)
	task, ok := taskByPlanStepID(t, fx, created.Run.ID, "only")
	if !ok || task.ExecutionRunID == nil {
		t.Fatalf("expected task to have a child run created (parked on capacity), task=%+v ok=%v", task, ok)
	}
	next, err := fx.wake.NextForRun(ctx, domain.WorkflowRunID(*task.ExecutionRunID))
	if err != nil || next == nil || next.Reason != "worker_capacity" {
		t.Fatalf("expected a durable worker_capacity wake for the child run, got %+v err=%v", next, err)
	}
	if len(fx.spawner.calls) != 0 {
		t.Fatalf("spawner calls = %d, want 0 before capacity recovers", len(fx.spawner.calls))
	}

	if _, err := fx.store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-recovered", Harness: domain.HarnessClaudeCode, UserID: "user-1", ProviderProfileID: "prof-claude-1", State: domain.AgentHealthAvailable,
		Reason: "recovered", CreatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent(recovered): %v", err)
	}

	driveCycles(t, fx, 3, nil)
	if len(fx.spawner.calls) == 0 {
		t.Fatalf("spawner calls = 0 after recovery, want at least 1 (poller must resume dispatch headlessly)")
	}
}

// TestAutonomous_FallbackUseNextAvailable is a regression proof that
// failover.go's existing fallback behavior still works under autonomous/
// headless driving: Claude Code unavailable, Codex configured second in
// WorkerPriority under FallbackUseNextAvailable -- Codex must end up
// selected and the run must continue, not wait forever.
func TestAutonomous_FallbackUseNextAvailable(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())
	// Claude Code being unavailable would also block the reviewer role under
	// the default ReviewIndependenceRequireDifferentProvider (codex worker
	// would then need a claude reviewer, which is unavailable too) -- allow
	// same-provider review fallback so this test isolates the WORKER
	// fallback claim it's actually about, not an unrelated reviewer wait.
	if _, err := fx.store.UpsertUserExecutionPolicy(ctx, domain.UserExecutionPolicy{
		ID:     "policy-user-1",
		UserID: "user-1", Version: domain.UserExecutionPolicyVersion, AutonomousMode: true,
		PlannerPriority:          []domain.ProviderProfileID{"prof-claude-1"},
		WorkerPriority:           []domain.ProviderProfileID{"prof-claude-1", "prof-codex-1"},
		ReviewerPriority:         []domain.ProviderProfileID{"prof-codex-1", "prof-claude-1"},
		DecisionResolverPriority: []domain.ProviderProfileID{"prof-codex-1", "prof-claude-1"},
		FallbackBehavior:         domain.FallbackUseNextAvailable,
		ReviewIndependence:       domain.ReviewIndependenceAllowSameProviderFallback,
		CreatedAt:                fx.clk.Now(), UpdatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatalf("seed execution policy override: %v", err)
	}

	if _, err := fx.store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-claude-down", Harness: domain.HarnessClaudeCode, UserID: "user-1", ProviderProfileID: "prof-claude-1", State: domain.AgentHealthUnavailable,
		Reason: "test", CreatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent: %v", err)
	}

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	driveCycles(t, fx, 10, func(i int) {
		if _, childID, ok := activeChildRunID(t, fx, created.Run.ID); ok {
			approveOpenReview(t, fx, childID, domain.VerdictApproved)
		}
	})

	if len(fx.spawner.calls) == 0 {
		t.Fatalf("spawner never called -- fallback never dispatched")
	}
	if fx.spawner.calls[0].Harness != domain.HarnessCodex {
		t.Fatalf("dispatched harness = %q, want codex (fallback past unavailable claude-code)", fx.spawner.calls[0].Harness)
	}
	run, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed (fallback must not block the run forever)", run.State)
	}
}

// TestAutonomous_RestartNoDuplicateWork: a "daemon restart" (a fresh
// Coordinator over the SAME store, reusing the same planner instance to
// simulate the same underlying planner adapter) must never regenerate an
// already-approved plan or create a second child run for a task already in
// flight.
func TestAutonomous_RestartNoDuplicateWork(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")
	driveCycles(t, fx, 4, nil)

	task, ok := taskByPlanStepID(t, fx, created.Run.ID, "only")
	if !ok || task.ExecutionRunID == nil {
		t.Fatalf("expected task dispatched before restart, task=%+v ok=%v", task, ok)
	}
	spawnCallsBefore := len(fx.spawner.calls)
	plannerCallsBefore := fx.planner.calls

	// Simulate a daemon restart: a brand-new Coordinator + Poller, same
	// store/planner/spawner/wake scheduler underneath.
	restarted := newAutonomousCoordinator(fx.store, fx.clk, fx.spawner, fx.planner, fx.ws, fx.launcher, fx.verifier, fx.sender, fx.wake, fx.emails)
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}
	if fx.planner.calls != plannerCallsBefore {
		t.Fatalf("planner calls after restart Reconcile = %d, want unchanged %d (plan already approved)", fx.planner.calls, plannerCallsBefore)
	}
	if len(fx.spawner.calls) != spawnCallsBefore {
		t.Fatalf("spawner calls after restart Reconcile = %d, want unchanged %d", len(fx.spawner.calls), spawnCallsBefore)
	}

	restartedPoller := wakepoller.New(fx.wake, restarted, wakepoller.Config{Clock: fx.clk.Now})
	for i := 0; i < 6; i++ {
		fx.clk.Advance(90 * time.Second)
		if _, childID, ok := activeChildRunID(t, fx, created.Run.ID); ok {
			approveOpenReview(t, fx, childID, domain.VerdictApproved)
		}
		if _, err := restartedPoller.RunDueOnce(ctx); err != nil {
			t.Fatalf("RunDueOnce (post-restart) cycle %d: %v", i, err)
		}
	}

	if fx.planner.calls != plannerCallsBefore {
		t.Fatalf("planner calls after post-restart driving = %d, want unchanged %d", fx.planner.calls, plannerCallsBefore)
	}
	taskAfter, _ := taskByPlanStepID(t, fx, created.Run.ID, "only")
	if taskAfter.ExecutionRunID == nil || *taskAfter.ExecutionRunID != *task.ExecutionRunID {
		t.Fatalf("task's ExecutionRunID changed across restart: before=%v after=%v", task.ExecutionRunID, taskAfter.ExecutionRunID)
	}
	if len(fx.spawner.calls) != spawnCallsBefore {
		t.Fatalf("spawner calls after post-restart driving = %d, want still %d (no duplicate child dispatch)", len(fx.spawner.calls), spawnCallsBefore)
	}

	// No duplicate integration promotion: at most one
	// master_integration_promotion checkpoint for the (single) completed
	// task.
	checkpoints, err := fx.store.ListWorkflowCheckpoints(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	promotions := 0
	for _, cp := range checkpoints {
		if cp.DurablePhase == "master_integration_promotion" {
			promotions++
		}
	}
	if promotions > 1 {
		t.Fatalf("master_integration_promotion checkpoints = %d, want at most 1", promotions)
	}
}
