package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/providerruntime"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// seedOwnerAndClaudeProfile inserts a real User + a Claude Code
// ProviderProfile for them (Checkpoint 8P-C.1's fixtures need real FK rows
// for scoped agent_health_events, same as capacity_scope_internal_test.go's
// helper in the internal package).
func seedOwnerAndClaudeProfile(t *testing.T, store *sqlite.Store, userID domain.UserID, profileID domain.ProviderProfileID, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.InsertUser(ctx, domain.User{
		ID: userID, DisplayName: string(userID), Email: string(userID) + "@example.com", Username: string(userID),
		PasswordHash: "x", Status: domain.UserStatusActive, Role: domain.UserRoleMember, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed user %s: %v", userID, err)
	}
	if _, err := store.InsertProviderProfile(ctx, domain.ProviderProfile{
		ID: profileID, UserID: userID, Provider: "anthropic", Harness: domain.HarnessClaudeCode, DisplayName: "Claude",
		Enabled: true, AuthState: domain.ProviderAuthStateAuthenticated, AuthMethod: domain.AuthMethodCLIBootstrap,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed profile %s: %v", profileID, err)
	}
}

// TestMasterTaskChild_OwnerPropagatedFromParent is Checkpoint 8P-C.1's core
// proof: a master task's child run durably inherits owner_user_id from the
// parent master run -- never guessed from the current request, never left
// NULL -- and the exact same providerruntime/capacity mechanisms 8P-B.1/
// 8P-C already use then resolve correctly against it with no special-case
// parent fallback.
func TestMasterTaskChild_OwnerPropagatedFromParent(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}

	userA, userB := domain.UserID("master-user-a"), domain.UserID("master-user-b")
	profA, profB := domain.ProviderProfileID("master-prof-a"), domain.ProviderProfileID("master-prof-b")
	seedOwnerAndClaudeProfile(t, store, userA, profA, now)
	seedOwnerAndClaudeProfile(t, store, userB, profB, now)

	planner := &staticPlanner{plan: validMasterPlan()}
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store, Planner: planner, PlannerContextBuilder: staticContext{},
		ProviderProfiles: store, ExecutionPolicies: store,
	})

	created, err := c.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalAuto)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	// Mirrors httpd's stampOwner: the master run is owned by A the same way
	// the HTTP controller stamps it right after creation.
	if ok, err := store.SetWorkflowRunOwner(ctx, created.Run.ID, userA); err != nil || !ok {
		t.Fatalf("stamp master owner: ok=%v err=%v", ok, err)
	}

	detail, err := c.GeneratePlan(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	if detail.Plan.Status != domain.WorkflowPlanApproved || len(detail.Tasks) != 2 || detail.Tasks[0].ExecutionRunID == nil {
		t.Fatalf("expected auto-approved plan with a dispatched first task, got %+v", detail)
	}
	childID := *detail.Tasks[0].ExecutionRunID

	// --- 1: child owner == parent owner, durably persisted (not just in
	// the frozen policy snapshot). ---
	owner, err := store.GetWorkflowRunOwner(ctx, childID)
	if err != nil {
		t.Fatalf("GetWorkflowRunOwner(child): %v", err)
	}
	if owner == nil || *owner != userA {
		t.Fatalf("child owner = %v, want %q", owner, userA)
	}

	// --- 5/18/21: runtime isolation resolves the child to A's own profile,
	// never B's -- the exact 8P-B.1 mechanism, no special parent fallback. ---
	resolver := &providerruntime.Resolver{Owners: store, Profiles: store, DataDir: t.TempDir(), TrustedLocal: false}
	env, resolvedOwner, resolvedProfile, err := resolver.Resolve(ctx, childID, domain.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("Resolve(child): %v", err)
	}
	if resolvedOwner != userA {
		t.Fatalf("resolved owner = %q, want %q", resolvedOwner, userA)
	}
	if resolvedProfile != profA {
		t.Fatalf("resolved profile = %q, want %q (never B's)", resolvedProfile, profA)
	}
	if env == nil {
		t.Fatal("expected a non-nil isolated runtime env for the child's resolved owner")
	}

	// --- 6/22: capacity scoping -- B's Claude cooldown must never affect
	// A's child routing, and vice versa. ---
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-b-cooldown", Harness: domain.HarnessClaudeCode, UserID: userB, ProviderProfileID: profB,
		State: domain.AgentHealthCooldown, Reason: "rate_limited", CreatedAt: now,
	}); err != nil {
		t.Fatalf("record B cooldown: %v", err)
	}
	healthA, ok, err := store.GetAgentHealthScoped(ctx, domain.HarnessClaudeCode, userA, profA)
	if err != nil {
		t.Fatalf("GetAgentHealthScoped(A): %v", err)
	}
	if ok && healthA.State == domain.AgentHealthCooldown {
		t.Fatalf("A's scoped health leaked B's cooldown: %+v", healthA)
	}

	// --- 10: a later reconcile pass (standing in for "daemon restarted,
	// GetRun/GeneratePlan re-entered the same durable state") must not
	// duplicate the child or lose/change its owner -- stampChildOwnership
	// is idempotent and re-runs on every entry into dispatchMasterTask. ---
	again, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun (second pass): %v", err)
	}
	if again.Tasks[0].ExecutionRunID == nil || *again.Tasks[0].ExecutionRunID != childID {
		t.Fatalf("second pass changed/duplicated the child run: got %v, want %q", again.Tasks[0].ExecutionRunID, childID)
	}
	ownerAfter, err := store.GetWorkflowRunOwner(ctx, childID)
	if err != nil {
		t.Fatalf("GetWorkflowRunOwner(child) after second pass: %v", err)
	}
	if ownerAfter == nil || *ownerAfter != userA {
		t.Fatalf("child owner after second pass = %v, want %q (no NULL, no alternate owner)", ownerAfter, userA)
	}
}
