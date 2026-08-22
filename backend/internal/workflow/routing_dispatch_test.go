package workflow_test

// Checkpoint 8L integration tests: worker dispatch through ExecutionRouter
// when capacity is unavailable, restart/reconcile resumption, and routing
// decision durability (checkpoint spec tests #5/#15/#16).

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// TestWorkDispatch_BothProvidersCooldownWaitsForCapacityWithZeroSpawn covers
// test requirement #5: when both known harnesses are in a durable cooldown
// (with a still-future CooldownUntil, so genuinely not eligible — see
// domain.AgentHealth.Available), the work step must never spawn, the run
// must move to waiting (never needs_attention, never a synthetic failure),
// and the routing decision persisted must explain why.
func TestWorkDispatch_BothProvidersCooldownWaitsForCapacityWithZeroSpawn(t *testing.T) {
	spawner := &harnessAwareSpawner{}
	c, store, clk := newCoordinatorWithSwitcher(spawner, nil)
	ctx := context.Background()

	future := clk.Now().Add(time.Hour)
	for _, h := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
			ID: "ahe-" + string(h), Harness: h, State: domain.AgentHealthCooldown,
			Reason: "test", CooldownUntil: &future, CreatedAt: clk.Now(),
		}); err != nil {
			t.Fatalf("RecordAgentHealthEvent(%s): %v", h, err)
		}
	}

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if detail.Run.State != domain.WorkflowRunWaiting {
		t.Fatalf("run state = %q, want waiting", detail.Run.State)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawner calls = %v, want zero (no eligible provider)", spawner.calls)
	}
	work := workStepFrom(detail)
	if work.Step.SessionID != nil {
		t.Fatalf("work step has a session despite waiting_for_capacity")
	}

	// The routing_decision checkpoint must explain the wait.
	cps, err := store.ListWorkflowCheckpoints(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	foundWaiting := false
	for _, cp := range cps {
		if cp.DurablePhase != "routing_decision" {
			continue
		}
		decision, ok := workflowcore.DecodeRoutingDecisionForTest(cp.RetryState)
		if !ok {
			t.Fatalf("routing_decision checkpoint did not decode: %+v", cp)
		}
		if decision.Waiting {
			foundWaiting = true
			if decision.PolicyVersion != domain.RoutingPolicyVersion {
				t.Fatalf("policy version = %q, want %q", decision.PolicyVersion, domain.RoutingPolicyVersion)
			}
		}
	}
	if !foundWaiting {
		t.Fatalf("no waiting routing_decision checkpoint found among %d checkpoints", len(cps))
	}
}

// TestWorkDispatch_CapacityRestoredAfterRestartLaunchesExactlyOneWorker
// covers test requirement #16: a run stuck at waiting_for_capacity survives
// a daemon restart, and once capacity becomes eligible again, Reconcile
// dispatches the worker exactly once — never a duplicate spawn.
func TestWorkDispatch_CapacityRestoredAfterRestartLaunchesExactlyOneWorker(t *testing.T) {
	spawner1 := &harnessAwareSpawner{}
	c1, store, clk := newCoordinatorWithSwitcher(spawner1, nil)
	ctx := context.Background()

	future := clk.Now().Add(time.Hour)
	for _, h := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
			ID: "ahe-pre-" + string(h), Harness: h, State: domain.AgentHealthCooldown,
			Reason: "test", CooldownUntil: &future, CreatedAt: clk.Now(),
		}); err != nil {
			t.Fatalf("RecordAgentHealthEvent(%s): %v", h, err)
		}
	}

	created, err := c1.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c1.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if detail.Run.State != domain.WorkflowRunWaiting {
		t.Fatalf("run state = %q, want waiting before capacity is restored", detail.Run.State)
	}
	if len(spawner1.calls) != 0 {
		t.Fatalf("spawner1 calls = %v, want zero before restart", spawner1.calls)
	}

	// Capacity for claude-code (the normal-complexity preferred worker)
	// recovers to available — the production mechanism for this is a fresh
	// success/cooldown-expiry event, never a manual DB mutation of state.
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-recovered", Harness: domain.HarnessClaudeCode, State: domain.AgentHealthAvailable,
		Reason: "recovered", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent(recovered): %v", err)
	}

	// Simulate a daemon restart: a fresh Coordinator over the same durable
	// store (the exact pattern recovery_boundaries_test.go already uses).
	spawner2 := &harnessAwareSpawner{}
	c2 := workflowcore.New(workflowcore.Deps{
		Store:   store,
		Spawner: spawner2,
		Clock:   clk.Now,
		NewID:   func() string { return "restart-id" },
	})
	if err := c2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, err := c2.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	work := workStepFrom(got)
	if work.Step.SessionID == nil {
		t.Fatalf("work step has no session after capacity was restored and Reconcile ran")
	}
	if len(spawner2.calls) != 1 {
		t.Fatalf("spawner2 calls = %v, want exactly 1 (no duplicate dispatch)", spawner2.calls)
	}
	if spawner2.calls[0] != domain.HarnessClaudeCode {
		t.Fatalf("spawned harness = %q, want claude-code (the recovered preferred provider)", spawner2.calls[0])
	}
}

// TestContinueRunDispatchesReview_CodexWorkerRoutesToClaudeReviewer covers
// test requirement #7 (checkpoint spec E2E: Codex worker -> Claude Code
// reviewer). Claude Code's capacity is put in cooldown before StartRun, so
// ExecutionRouter's normal-complexity preference (claude-code) is
// unavailable and the worker routes to its fallback, codex — then the real
// review dispatch chain (Coordinator -> InsertReviewRun -> Preflight ->
// Launch, via the exact fakeReviewRuns/fakeReviewerLauncher production call
// path) must independently route the reviewer to the opposite provider,
// claude-code.
func TestContinueRunDispatchesReview_CodexWorkerRoutesToClaudeReviewer(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	future := clk.Now().Add(time.Hour)
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-claude-cooldown", Harness: domain.HarnessClaudeCode, State: domain.AgentHealthCooldown,
		Reason: "test", CooldownUntil: &future, CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent: %v", err)
	}

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got := completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	work := workStepFrom(got)
	attempts, err := store.ListWorkflowAttempts(ctx, work.Step.ID)
	if err != nil || len(attempts) == 0 || attempts[len(attempts)-1].Harness != "codex" {
		t.Fatalf("attempts = %+v, err=%v, want the worker to have routed to codex", attempts, err)
	}

	// Claude Code recovers by review time — the reviewer's cross-provider
	// preference (opposite of the codex worker) is now eligible again.
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-claude-recovered", Harness: domain.HarnessClaudeCode, State: domain.AgentHealthAvailable,
		Reason: "recovered", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent(recovered): %v", err)
	}

	reviewed, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(reviewed)
	if review.Step.ReviewRunID == nil {
		t.Fatalf("review step has no review_run_id after dispatch")
	}
	run, ok := reviewRuns.runs[*review.Step.ReviewRunID]
	if !ok {
		t.Fatalf("review run %s not found", *review.Step.ReviewRunID)
	}
	if run.Harness != domain.ReviewerClaudeCode {
		t.Fatalf("review run harness = %q, want claude-code (cross-provider independence from codex worker)", run.Harness)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launch calls = %d, want exactly 1", launcher.launchCalls)
	}
}

// TestRoutingDecision_UnchangedWaitIsNotRePersisted is Checkpoint
// 8P-E.13A.4's checkpoint-spam regression.
//
// Routing is re-evaluated on every ContinueRun, and a waiting run is driven by
// wake ticks, the autonomous progression heartbeat and reconcileMasterTasks
// alike — several of which can land inside the same second. Each evaluation
// used to append another byte-identical routing_decision checkpoint: in
// ~/.ao/data, wf-35fd1af0 accumulated 68 identical waiting_for_capacity rows,
// three of them within 200ms. An unchanged wait must produce ONE durable
// record, not an unbounded stream, while any material change still writes.
func TestRoutingDecision_UnchangedWaitIsNotRePersisted(t *testing.T) {
	spawner := &harnessAwareSpawner{}
	c, store, clk := newCoordinatorWithSwitcher(spawner, nil)
	ctx := context.Background()

	future := clk.Now().Add(time.Hour)
	for _, h := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
			ID: "ahe-" + string(h), Harness: h, State: domain.AgentHealthCooldown,
			Reason: "test", CooldownUntil: &future, CreatedAt: clk.Now(),
		}); err != nil {
			t.Fatalf("RecordAgentHealthEvent(%s): %v", h, err)
		}
	}

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	countRoutingCheckpoints := func() int {
		cps, err := store.ListWorkflowCheckpoints(ctx, created.Run.ID)
		if err != nil {
			t.Fatalf("ListWorkflowCheckpoints: %v", err)
		}
		n := 0
		for _, cp := range cps {
			if cp.NextAction != "" && cp.RetryState != "" {
				if _, ok := workflowcore.DecodeRoutingDecisionForTest(cp.RetryState); ok {
					n++
				}
			}
		}
		return n
	}

	afterStart := countRoutingCheckpoints()
	if afterStart == 0 {
		t.Fatalf("no routing_decision checkpoint recorded by StartRun")
	}

	// Ten more re-entries with nothing changed: the wait is already on record.
	for i := 0; i < 10; i++ {
		if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
			t.Fatalf("ContinueRun #%d: %v", i, err)
		}
	}
	if got := countRoutingCheckpoints(); got != afterStart {
		t.Fatalf("routing checkpoints = %d after 10 unchanged re-entries, want %d (no duplicates)", got, afterStart)
	}

	// A material change (capacity recovers, so the decision now SELECTS a
	// provider instead of waiting) must still be recorded.
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-recovered", Harness: domain.HarnessClaudeCode, State: domain.AgentHealthAvailable,
		Reason: "recovered", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent(recovered): %v", err)
	}
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun after recovery: %v", err)
	}
	if got := countRoutingCheckpoints(); got <= afterStart {
		t.Fatalf("routing checkpoints = %d, want a new one recorded for the changed decision", got)
	}
}
