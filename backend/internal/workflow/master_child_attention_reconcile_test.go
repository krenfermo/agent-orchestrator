package workflow_test

// The incident, from ~/.ao/data on 2026-08-22:
//
//	parent (master, autonomous)  state=needs_attention
//	                             attention reason=child_needs_attention
//	task 1's child               state=running
//
// Both rows are individually truthful and jointly impossible. The task detail
// page showed the child running; the plan row showed "TE NECESITA — Motivo:
// child_needs_attention"; every later task stayed blocked; and nothing short of
// a human pressing Continue on the PARENT would ever have unstuck it.
//
// child_needs_attention is a MIRROR of another run's state, not a decision of
// the parent's own. Two things made it historical instead of current:
//
//  1. it is registered as a human-owned reason, so maybeScheduleAutonomousHeartbeat
//     stopped rescheduling the parent's heartbeat the moment it was written —
//     and that heartbeat is the only thing that re-enters reconcileMasterTasks,
//     so nothing was ever going to look at the child again; and
//  2. the clear was reached only from inside reconcileMasterTasksOnce's "this
//     child can advance" branch, i.e. only on a pass that (1) had already made
//     impossible.
//
// These tests drive the real autonomous stack — real sqlite store, real
// wake.Scheduler, real wakepoller, real notify.Manager — and, for the recovery
// half, use ONLY the daemon's own poller: no GetRun from a browser and no
// Continue from a person.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// TestParentReconcilesStaleChildAttentionWhenTheChildResumes is the incident
// itself, start to finish: parent running, task 1's child stops on a human
// decision, the parent mirrors it, the child resumes on its own, and the parent
// must return to running and carry the plan to completion with nobody touching
// it — without duplicating a task, a child run, a notification or a wake.
func TestParentReconcilesStaleChildAttentionWhenTheChildResumes(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())

	taskID, childID := dispatchedChild(t, fx, masterID)
	spawnsAtStop := len(fx.spawner.calls)

	// 1-3. The child stops on something only a person can resolve, and the
	// next poller pass mirrors that onto the parent.
	parkOnHumanDecision(t, fx, childID)
	driveUntil(t, fx, 6, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child; the fixture never reached the state under test")
	}
	// Checkpoint 8P-E.24: ONE notification per real incident. The child is the
	// run that actually stopped and the one whose notification names a reason a
	// person can act on; the parent's mirror is the same incident seen from one
	// level up, and emailing about both is two messages for one problem.
	if got := fx.emails.countOfType(domain.NotificationTaskNeedsAttention); got != 1 {
		t.Fatalf("task needs-attention notifications while the stop was real = %d, want exactly 1", got)
	}
	if got := fx.emails.countOfType(domain.NotificationWorkflowNeedsAttention); got != 0 {
		t.Fatalf("parent mirror produced %d objective-level notifications, want 0: the child already reported this same incident", got)
	}

	// 4. The child recovers by itself — the branch queue clears, the retry
	// lands, the worker comes back. Nothing else changes.
	resumeChild(t, fx, childID)

	// 5-6. Only the daemon's poller from here. No GetRun, no Continue.
	driveUntil(t, fx, 8, func() bool { return !mirroredChildStop(t, fx, masterID) })

	master, _, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(master): %v", err)
	}
	if master.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("master is still %q after its child resumed: the mirrored stop is still historical, not current", master.State)
	}
	detail, err := fx.coord.GetRun(ctx, masterID)
	if err != nil {
		t.Fatalf("GetRun(master): %v", err)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	if life.AttentionReason == workflowcore.ReasonChildNeedsAttention {
		t.Fatalf("master still reports %q to the Board after its child resumed: %#v", life.AttentionReason, life)
	}
	if life.Attention == workflowcore.AttentionHuman {
		t.Fatalf("master asks for a decision nobody has to make: %#v", life)
	}

	// The clear is one auditable event, not one per poll.
	if got := countCheckpointPhase(t, fx, masterID, "attention_cleared"); got != 1 {
		t.Fatalf("attention_cleared checkpoints on the master = %d, want exactly 1", got)
	}
	// History is intact: the notification raised while the condition was real
	// is still there, and no second one was invented.
	if got := fx.emails.countOfType(domain.NotificationTaskNeedsAttention); got != 1 {
		t.Fatalf("task needs-attention notifications after the recovery = %d, want still 1", got)
	}
	if got := fx.emails.countOfType(domain.NotificationWorkflowNeedsAttention); got != 0 {
		t.Fatalf("the recovery invented %d objective-level notifications, want 0", got)
	}
	if got := pendingWakeCount(t, fx, masterID); got > 1 {
		t.Fatalf("pending wake entries for the master = %d, want at most 1", got)
	}

	// 7-9. The child carries on and finishes, and the parent advances to task
	// 2 and completes the plan.
	//
	// The one human act in this whole test happens HERE and only here, on the
	// CHILD: the person does what the notification asked (reconnect the
	// reviewer's provider) and continues the run that actually stopped. The
	// parent is never touched — it had already reconciled itself above, off the
	// poller alone, which is the entire point.
	if _, err := fx.coord.ContinueRun(ctx, childID); err != nil {
		t.Fatalf("ContinueRun(child): %v", err)
	}
	driveCycles(t, fx, 40, func(int) {
		if _, active, ok := activeChildRunID(t, fx, masterID); ok {
			approveOpenReview(t, fx, active, domain.VerdictApproved)
		}
	})

	master, _, err = fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(master): %v", err)
	}
	if master.State != domain.WorkflowRunCompleted {
		t.Fatalf("master state = %q, want completed: the plan never finished after the recovery", master.State)
	}
	taskA, _ := taskByPlanStepID(t, fx, masterID, "model")
	taskB, _ := taskByPlanStepID(t, fx, masterID, "tests")
	if taskA.State != domain.WorkflowTaskCompleted || taskB.State != domain.WorkflowTaskCompleted {
		t.Fatalf("tasks not both completed: A=%s B=%s", taskA.State, taskB.State)
	}
	if taskB.ExecutionRunID == nil {
		t.Fatal("task 2 was never dispatched: the parent never advanced past the recovered task")
	}

	// 11. No duplicates anywhere: the stopped task kept its identity and its
	// one child run, and no extra worker was spawned for it.
	if taskA.ID != taskID {
		t.Fatalf("task 1 id = %q, want the original %q: the plan was re-materialized", taskA.ID, taskID)
	}
	if taskA.ExecutionRunID == nil || *taskA.ExecutionRunID != childID {
		t.Fatalf("task 1's execution run = %v, want the original %q: a duplicate child run was created", taskA.ExecutionRunID, childID)
	}
	if len(fx.spawner.calls) < spawnsAtStop {
		t.Fatalf("spawner calls went backwards (%d -> %d)", spawnsAtStop, len(fx.spawner.calls))
	}
	if got := countCheckpointPhase(t, fx, masterID, workflowcore.ReasonChildNeedsAttention); got != 1 {
		t.Fatalf("child_needs_attention checkpoints on the master = %d, want exactly 1 (one occurrence, not one per poll)", got)
	}
}

// TestParentKeepsChildAttentionWhileTheChildStillNeedsAHuman is the other half
// of the rule: reconciling the mirror must not become "clear it".
func TestParentKeepsChildAttentionWhileTheChildStillNeedsAHuman(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)

	parkOnHumanDecision(t, fx, childID)
	driveUntil(t, fx, 6, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child")
	}

	// Many more passes over the same unchanged condition.
	driveCycles(t, fx, 10, nil)

	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent cleared a mirror of a child that is still waiting on a person")
	}
	child, _, err := fx.store.GetWorkflowRun(ctx, childID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(child): %v", err)
	}
	if child.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("child state = %q, want needs_attention: the fixture healed the child under the test", child.State)
	}
	// Idempotent: one occurrence, one notification, however many passes.
	if got := countCheckpointPhase(t, fx, masterID, workflowcore.ReasonChildNeedsAttention); got != 1 {
		t.Fatalf("child_needs_attention checkpoints = %d, want exactly 1 across %d reconcile passes", got, 10)
	}
	if got := fx.emails.countOfType(domain.NotificationTaskNeedsAttention); got != 1 {
		t.Fatalf("task needs-attention notifications = %d, want exactly 1", got)
	}
	if got := fx.emails.countOfType(domain.NotificationWorkflowNeedsAttention); got != 0 {
		t.Fatalf("parent mirror produced %d objective-level notifications, want 0", got)
	}
	if got := pendingWakeCount(t, fx, masterID); got > 1 {
		t.Fatalf("pending wake entries for the master = %d, want at most 1", got)
	}
}

// TestHumanContinueOnTheChildClearsTheParentMirror: the person does the thing
// the notification asked them to do — on the CHILD, which is where the decision
// actually was — and the parent must reconcile itself. Nobody should have to
// press Continue a second time on the parent just to clear derived state.
func TestHumanContinueOnTheChildClearsTheParentMirror(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)

	parkOnHumanDecision(t, fx, childID)
	driveUntil(t, fx, 6, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child")
	}

	// The human decision, taken: Continue on the child run.
	if _, err := fx.coord.ContinueRun(ctx, childID); err != nil {
		t.Fatalf("ContinueRun(child): %v", err)
	}
	if child, _, err := fx.store.GetWorkflowRun(ctx, childID); err != nil {
		t.Fatalf("GetWorkflowRun(child): %v", err)
	} else if child.State == domain.WorkflowRunNeedsAttention {
		// ContinueRun is not obliged to leave the run "running" — but it must
		// leave it out of the stop, or there is nothing for the parent to
		// reconcile and this test proves nothing.
		resumeChild(t, fx, childID)
	}

	driveUntil(t, fx, 8, func() bool { return !mirroredChildStop(t, fx, masterID) })
	if mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent still mirrors child_needs_attention after the child was continued")
	}
}

// TestDaemonRestartHealsAStaleParentMirror: the incident's rows are already on
// disk when AO starts. Boot reconcile must repair them, with no HTTP traffic
// and no human at all.
func TestDaemonRestartHealsAStaleParentMirror(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	taskID, childID := dispatchedChild(t, fx, masterID)

	parkOnHumanDecision(t, fx, childID)
	driveUntil(t, fx, 6, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child")
	}
	resumeChild(t, fx, childID)
	spawnsBefore := len(fx.spawner.calls)

	// A brand-new Coordinator over the same durable rows: the daemon restart.
	restarted := newAutonomousCoordinator(fx.store, fx.clk, fx.spawner, fx.planner, fx.ws, fx.launcher, fx.verifier, fx.sender, fx.wake, fx.emails)
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}

	if mirroredChildStop(t, fx, masterID) {
		t.Fatal("boot reconcile left the parent parked on a mirror of a child that is running")
	}
	// Idempotent across the restart: no second reconcile pass, no second
	// child, no second worker, no second notification.
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if got := countCheckpointPhase(t, fx, masterID, "attention_cleared"); got != 1 {
		t.Fatalf("attention_cleared checkpoints = %d, want exactly 1 across two boot reconciles", got)
	}
	if len(fx.spawner.calls) != spawnsBefore {
		t.Fatalf("spawner calls = %d, want unchanged %d across two boot reconciles", len(fx.spawner.calls), spawnsBefore)
	}
	task, _ := taskByPlanStepID(t, fx, masterID, "model")
	if task.ID != taskID || task.ExecutionRunID == nil || *task.ExecutionRunID != childID {
		t.Fatalf("task/child identity changed across restart: task=%q child=%v", task.ID, task.ExecutionRunID)
	}
	if got := fx.emails.countOfType(domain.NotificationTaskNeedsAttention); got != 1 {
		t.Fatalf("task needs-attention notifications = %d, want still 1 after the restart", got)
	}
}

// TestTerminalChildFailureIsNeverClearedAsARecovery: a failure is not a
// recovery. The parent must follow the failure lifecycle, and the mirror must
// never be read as "the child came back".
func TestTerminalChildFailureIsNeverClearedAsARecovery(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)

	parkOnHumanDecision(t, fx, childID)
	driveUntil(t, fx, 6, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child")
	}

	// The child ends terminally instead of recovering.
	mustTransitionRun(t, fx, childID, domain.WorkflowRunNeedsAttention, domain.WorkflowRunRunning)
	mustTransitionRun(t, fx, childID, domain.WorkflowRunRunning, domain.WorkflowRunFailed)

	driveCycles(t, fx, 8, nil)

	master, _, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(master): %v", err)
	}
	if master.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("master state = %q, want needs_attention: a terminally failed child was reconciled away", master.State)
	}
	if countCheckpointPhase(t, fx, masterID, "attention_cleared") != 0 {
		t.Fatal("the parent un-parked itself over a terminally failed child")
	}
	if countCheckpointPhase(t, fx, masterID, workflowcore.ReasonChildFailed) == 0 {
		t.Fatal("the parent never recorded the child's failure as its own stop reason")
	}
	task, _ := taskByPlanStepID(t, fx, masterID, "model")
	if task.State != domain.WorkflowTaskFailed {
		t.Fatalf("task state = %q, want failed", task.State)
	}
}

// TestUnrelatedParentAttentionIsNeverClearedByChildReconcile: the reconciliation
// owns exactly one reason. A parent stopped on anything else — here, an
// exhausted fix budget of its own — stays stopped no matter what its children
// are doing.
func TestUnrelatedParentAttentionIsNeverClearedByChildReconcile(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)

	// The child is fine; the parent is not, for a reason of its own.
	master, _, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(master): %v", err)
	}
	mustTransitionRun(t, fx, masterID, master.State, domain.WorkflowRunNeedsAttention)
	writeStopCheckpoint(t, fx, masterID, workflowcore.ReasonFixBudgetExhausted,
		"the reviewer still requests changes after every allowed fix cycle")

	driveCycles(t, fx, 8, nil)
	if _, err := fx.coord.GetRun(ctx, masterID); err != nil {
		t.Fatalf("GetRun(master): %v", err)
	}

	master, _, err = fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(master): %v", err)
	}
	if master.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("master state = %q, want needs_attention: an unrelated stop was cleared by the child reconcile", master.State)
	}
	if countCheckpointPhase(t, fx, masterID, "attention_cleared") != 0 {
		t.Fatal("the child reconcile un-parked a parent stopped for a reason of its own")
	}
	if newestCheckpointPhase(t, fx, masterID) != workflowcore.ReasonFixBudgetExhausted {
		t.Fatalf("newest master checkpoint = %q, want the unrelated stop reason to still stand", newestCheckpointPhase(t, fx, masterID))
	}
	if child, _, cerr := fx.store.GetWorkflowRun(ctx, childID); cerr == nil && child.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("the fixture stopped the child too; this test no longer isolates the parent's own reason")
	}
}

// ---- helpers ---------------------------------------------------------------

// startAutonomousObjective is the three lines every test above opens with: a
// real autonomous master run, owned and policy-stamped, plan approved.
func startAutonomousObjective(t *testing.T, plan workflowcore.MasterPlan) (*autonomousFixture, context.Context, string) {
	t.Helper()
	fx := newAutonomousFixture(t, plan)
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())
	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")
	return fx, ctx, created.Run.ID
}

// dispatchedChild drives the poller until task 1 has a child run, and returns
// both ids.
func dispatchedChild(t *testing.T, fx *autonomousFixture, masterID string) (taskID, childID string) {
	t.Helper()
	driveUntil(t, fx, 6, func() bool {
		_, _, ok := activeChildRunID(t, fx, masterID)
		return ok
	})
	taskID, childID, ok := activeChildRunID(t, fx, masterID)
	if !ok {
		t.Fatal("no child run was dispatched; the fixture never reached the state under test")
	}
	return taskID, childID
}

// driveUntil runs poller cycles until done() or the budget runs out. It never
// fails on exhaustion: the caller asserts what it actually wanted.
func driveUntil(t *testing.T, fx *autonomousFixture, maxCycles int, done func() bool) {
	t.Helper()
	for i := 0; i < maxCycles; i++ {
		if done() {
			return
		}
		driveCycles(t, fx, 1, nil)
	}
}

// parkOnHumanDecision drives the child into a GENUINE human-owned stop rather
// than seeding one: the reviewer's credentials are rejected, which
// classifyReviewerLaunchFailure names ReasonReviewerAuthInvalid — a permanent,
// non-retryable stop with a real HumanAction, produced by the real dispatch
// path. Seeding the checkpoint directly is not equivalent: the child's own poll
// writes newer observation checkpoints, and stopReason reads the NEWEST one.
func parkOnHumanDecision(t *testing.T, fx *autonomousFixture, childID string) {
	t.Helper()
	fx.launcher.launchErr = ports.ErrChatAuthRequired
	driveUntil(t, fx, 12, func() bool {
		run, ok, err := fx.store.GetWorkflowRun(context.Background(), childID)
		return err == nil && ok && run.State == domain.WorkflowRunNeedsAttention
	})
	run, ok, err := fx.store.GetWorkflowRun(context.Background(), childID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(child): %v (found=%v)", err, ok)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("child state = %q, want needs_attention: the reviewer auth failure never parked it", run.State)
	}
}

// resumeChild is the child coming back — the credential is reconnected, the
// retry lands, the worker returns. needs_attention -> running is the only
// forward transition the domain allows out of a stop, which is exactly what a
// real resume does.
func resumeChild(t *testing.T, fx *autonomousFixture, childID string) {
	t.Helper()
	fx.launcher.launchErr = nil
	child, ok, err := fx.store.GetWorkflowRun(context.Background(), childID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(child): %v (found=%v)", err, ok)
	}
	if child.State != domain.WorkflowRunNeedsAttention {
		return
	}
	mustTransitionRun(t, fx, childID, domain.WorkflowRunNeedsAttention, domain.WorkflowRunRunning)
}

func mustTransitionRun(t *testing.T, fx *autonomousFixture, runID string, from, to domain.WorkflowRunState) {
	t.Helper()
	if from == to {
		return
	}
	moved, err := fx.store.UpdateWorkflowRunState(context.Background(), runID, from, to, fx.clk.Now())
	if err != nil {
		t.Fatalf("UpdateWorkflowRunState(%s: %s -> %s): %v", runID, from, to, err)
	}
	if !moved {
		t.Fatalf("UpdateWorkflowRunState(%s: %s -> %s) matched no row", runID, from, to)
	}
}

var stopCheckpointSeq int

func writeStopCheckpoint(t *testing.T, fx *autonomousFixture, runID, reason, detail string) {
	t.Helper()
	ctx := context.Background()
	run, ok, err := fx.store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): %v (found=%v)", runID, err, ok)
	}
	stopCheckpointSeq++
	fx.clk.Advance(time.Second)
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-seeded-stop-" + strconv.Itoa(stopCheckpointSeq),
		WorkflowRunID:  runID,
		ProjectID:      run.ProjectID,
		NextAction:     detail,
		DurablePhase:   reason,
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      fx.clk.Now(),
	}); err != nil {
		t.Fatalf("CreateWorkflowCheckpoint(%s, %s): %v", runID, reason, err)
	}
}

// mirroredChildStop is the parent's durable state as the Board reads it, WITHOUT
// going through GetRun: the run row says stopped and the newest checkpoint names
// the mirror. Deliberately store-only, so the recovery half of these tests can
// prove the poller alone healed it.
func mirroredChildStop(t *testing.T, fx *autonomousFixture, masterID string) bool {
	t.Helper()
	run, ok, err := fx.store.GetWorkflowRun(context.Background(), masterID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(master): %v (found=%v)", err, ok)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		return false
	}
	return newestCheckpointPhase(t, fx, masterID) == workflowcore.ReasonChildNeedsAttention
}

func newestCheckpointPhase(t *testing.T, fx *autonomousFixture, runID string) string {
	t.Helper()
	cps, err := fx.store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var newest domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if !found || !cp.CreatedAt.Before(newest.CreatedAt) {
			newest, found = cp, true
		}
	}
	if !found {
		return ""
	}
	return newest.DurablePhase
}

func countCheckpointPhase(t *testing.T, fx *autonomousFixture, runID, phase string) int {
	t.Helper()
	cps, err := fx.store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

func pendingWakeCount(t *testing.T, fx *autonomousFixture, runID string) int {
	t.Helper()
	rows, err := fx.store.ListPendingWorkflowWakeSchedulesByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListPendingWorkflowWakeSchedulesByRun: %v", err)
	}
	return len(rows)
}
