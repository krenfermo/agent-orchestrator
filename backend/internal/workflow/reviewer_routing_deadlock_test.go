package workflow_test

// Checkpoint 8P-E.13A.4 end-to-end regression: an authenticated, enabled,
// reviewer-capable Codex profile that the stored UserExecutionPolicy never
// lists must still be dispatched as the independent reviewer, headlessly.

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// seedUserWithClaudeOnlyPolicy reproduces the exact stored state found in
// ~/.ao/data: two real profiles (Claude Code and Codex, both enabled,
// authenticated, reviewer + capacity_telemetry capable) but a
// user_execution_policies row whose every priority list names ONLY the Claude
// Code profile -- because it was written before Codex was ever connected.
//
// independence selects which review-independence mode the stored row carries:
// the real row said allow_same_provider_fallback (which high-risk complexity
// overrides on its own -- see
// TestRouteExecution_CompletionNeverRelaxesHighRiskIndependence), while
// require_different_provider is the mode that forbids a same-provider reviewer
// at EVERY complexity, and is therefore what an end-to-end test can assert
// against without having to manufacture high-risk risk facts.
func seedUserWithClaudeOnlyPolicy(t *testing.T, store *sqlite.Store, userID domain.UserID, claudeProfile, codexProfile domain.ProviderProfileID, independence domain.ReviewIndependence, now time.Time) {
	t.Helper()
	seedUser(t, store, userID, claudeProfile, codexProfile, true, now)
	if _, err := store.UpsertUserExecutionPolicy(context.Background(), domain.UserExecutionPolicy{
		ID:     domain.UserExecutionPolicyID("policy-" + string(userID)),
		UserID: userID, Version: domain.UserExecutionPolicyVersion, AutonomousMode: true,
		PlannerPriority:          []domain.ProviderProfileID{claudeProfile},
		WorkerPriority:           []domain.ProviderProfileID{claudeProfile},
		ReviewerPriority:         []domain.ProviderProfileID{claudeProfile},
		DecisionResolverPriority: []domain.ProviderProfileID{claudeProfile},
		FallbackBehavior:         domain.FallbackUseNextAvailable,
		ReviewIndependence:       independence,
		CreatedAt:                now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed claude-only execution policy: %v", err)
	}
}

// Regression A/B (end to end): with the deadlocking policy in place, the run
// must still reach a reviewer, that reviewer must be Codex (independent of the
// Claude Code implementer), and it must happen with no human action -- no
// manual Codex session, no policy edit, no approval.
func TestAutonomous_UnlistedCodexProfileStillReviewsIndependently(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUserWithClaudeOnlyPolicy(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", domain.ReviewIndependenceRequireDifferentProvider, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	driveCycles(t, fx, 25, nil)

	if fx.launcher.launchCalls == 0 {
		t.Fatalf("reviewer never launched: the run is still deadlocked in waiting_for_capacity")
	}
	if got := fx.launcher.lastReq.Harness; got != domain.ReviewerCodex {
		t.Fatalf("reviewer harness = %q, want codex (independent of the claude-code implementer)", got)
	}
}

// Regression C/E (end to end): when the only independent reviewer is genuinely
// unavailable, the run must WAIT rather than review Claude's work with Claude
// -- priority completion must not become a back door around independence --
// and the wait must not spew duplicate checkpoints.
func TestAutonomous_UnavailableIndependentReviewerWaitsWithoutCheckpointSpam(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUserWithClaudeOnlyPolicy(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", domain.ReviewIndependenceRequireDifferentProvider, fx.clk.Now())

	// Codex is durably observed unavailable for this owner's connection: a
	// real, recorded fact, which always beats any probe (probes only ever run
	// against an unknown state).
	if _, err := fx.store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-codex-down", Harness: domain.HarnessCodex, UserID: "user-1", ProviderProfileID: "prof-codex-1",
		State: domain.AgentHealthUnavailable, Reason: "provider outage", CreatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent: %v", err)
	}

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	driveCycles(t, fx, 25, nil)

	if fx.launcher.launchCalls != 0 {
		t.Fatalf("reviewer launched %d time(s) with %q: require_different_provider must never fall back to the implementer's own provider",
			fx.launcher.launchCalls, fx.launcher.lastReq.Harness)
	}

	// The wait is on record, but exactly once per distinct decision -- not
	// once per poll cycle.
	waits := 0
	for _, childID := range childRunIDs(t, fx, created.Run.ID) {
		cps, err := fx.store.ListWorkflowCheckpoints(ctx, childID)
		if err != nil {
			t.Fatalf("ListWorkflowCheckpoints: %v", err)
		}
		for _, cp := range cps {
			if cp.NextAction == "waiting_for_capacity: role=reviewer" {
				waits++
			}
		}
	}
	if waits > 2 {
		t.Fatalf("reviewer waiting_for_capacity checkpoints = %d across 25 poll cycles, want at most 2 (one durable record per distinct decision)", waits)
	}
}

// childRunIDs lists every child (task execution) run of a master run.
func childRunIDs(t *testing.T, fx *autonomousFixture, masterID string) []string {
	t.Helper()
	tasks, err := fx.store.ListWorkflowTasks(context.Background(), masterID)
	if err != nil {
		t.Fatalf("ListWorkflowTasks: %v", err)
	}
	var out []string
	for _, task := range tasks {
		if task.ExecutionRunID != nil {
			out = append(out, *task.ExecutionRunID)
		}
	}
	return out
}
