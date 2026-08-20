package workflow

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// seedUserAndProfile inserts the real User/ProviderProfile rows a scoped
// agent_health_events row's foreign keys require -- capacity scoping
// (Checkpoint 8P-C) is only meaningful against real owned connections.
func seedUserAndProfile(t *testing.T, store *sqlite.Store, userID domain.UserID, profileID domain.ProviderProfileID, now time.Time) {
	t.Helper()
	if _, err := store.InsertUser(t.Context(), domain.User{
		ID: userID, DisplayName: string(userID), Email: string(userID) + "@example.com", Username: string(userID),
		PasswordHash: "x", Status: domain.UserStatusActive, Role: domain.UserRoleMember, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed user %s: %v", userID, err)
	}
	if _, err := store.InsertProviderProfile(t.Context(), domain.ProviderProfile{
		ID: profileID, UserID: userID, Provider: "anthropic", Harness: domain.HarnessClaudeCode, DisplayName: "Claude",
		Enabled: true, AuthState: domain.ProviderAuthStateAuthenticated, AuthMethod: domain.AuthMethodCLIBootstrap,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed profile %s: %v", profileID, err)
	}
}

// TestCapacityScope_CrossUserIsolation is Checkpoint 8P-C's core capacity
// rescoping proof: User A's Claude cooldown must never appear in User B's
// capacity view, even though both own a profile for the exact same harness
// (claude-code). Mirrors the checkpoint brief's explicit E2E F scenario.
func TestCapacityScope_CrossUserIsolation(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := t.Context()
	now := time.Now().UTC()

	userA, userB := domain.UserID("user-a"), domain.UserID("user-b")
	profA := domain.ProviderProfileID("profile-a-claude")
	profB := domain.ProviderProfileID("profile-b-claude")
	seedUserAndProfile(t, store, userA, profA, now)
	seedUserAndProfile(t, store, userB, profB, now)

	c := New(Deps{Store: store, Projects: store})

	// User A's Claude profile goes into cooldown...
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-a", Harness: domain.HarnessClaudeCode, UserID: userA, ProviderProfileID: profA,
		State: domain.AgentHealthCooldown, Reason: "rate_limited", CreatedAt: now,
	}); err != nil {
		t.Fatalf("record A's cooldown: %v", err)
	}
	// ...User B's own Claude profile has no recorded event at all (unknown,
	// which is eligible per 8H's own "unknown defaults to available" rule).

	healthA, err := c.agentHealth(ctx, domain.HarnessClaudeCode, healthScope{userID: userA, profileID: profA})
	if err != nil {
		t.Fatalf("agentHealth A: %v", err)
	}
	if healthA.State != domain.AgentHealthCooldown {
		t.Fatalf("user A health = %v, want cooldown", healthA.State)
	}

	healthB, err := c.agentHealth(ctx, domain.HarnessClaudeCode, healthScope{userID: userB, profileID: profB})
	if err != nil {
		t.Fatalf("agentHealth B: %v", err)
	}
	if healthB.State != domain.AgentHealthUnknown {
		t.Fatalf("user B health = %v, want unknown (never A's cooldown)", healthB.State)
	}
	if !healthB.Available(now) {
		t.Fatalf("user B must remain available: A's cooldown leaked across users")
	}

	// capacitySnapshotForProfiles (what routing actually consults) confirms
	// the same isolation at the map level.
	snapshotA := c.capacitySnapshotForProfiles(ctx, userA, map[domain.ProviderProfileID]domain.ProviderProfile{
		profA: {ID: profA, Harness: domain.HarnessClaudeCode},
	})
	snapshotB := c.capacitySnapshotForProfiles(ctx, userB, map[domain.ProviderProfileID]domain.ProviderProfile{
		profB: {ID: profB, Harness: domain.HarnessClaudeCode},
	})
	if snapshotA[profA] != domain.CapacityCooldown {
		t.Fatalf("snapshot A = %v, want cooldown", snapshotA[profA])
	}
	if snapshotB[profB] == domain.CapacityCooldown {
		t.Fatalf("snapshot B leaked A's cooldown: %v", snapshotB[profB])
	}

	// Recording a failure for A must never touch B's row, and vice versa --
	// each write is scoped by (harness, userID, profileID), matching the
	// unique combination the migration's index enforces reads against.
	c.recordAgentHealthFailure(ctx, domain.HarnessClaudeCode, healthScope{userID: userA, profileID: profA}, ProviderFailureClassification{Class: domain.WorkflowErrorRateLimited}, now.Add(time.Minute))
	healthBAfter, err := c.agentHealth(ctx, domain.HarnessClaudeCode, healthScope{userID: userB, profileID: profB})
	if err != nil {
		t.Fatalf("agentHealth B after A's second failure: %v", err)
	}
	if healthBAfter.State != domain.AgentHealthUnknown {
		t.Fatalf("user B health after A's second failure = %v, want still unknown", healthBAfter.State)
	}
}

// TestCapacityScope_LegacyGlobalFallbackOnlyWhenUnscoped proves the
// documented precedence rule: a legacy/global row (no user/profile) is only
// ever consulted when no scoped row exists yet for that exact connection --
// it never overrides a scoped row once one exists, and unscoped reads
// (trusted-local/no owner) still see it.
func TestCapacityScope_LegacyGlobalFallbackOnlyWhenUnscoped(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := t.Context()
	now := time.Now().UTC()
	c := New(Deps{Store: store, Projects: store})

	userA := domain.UserID("user-a")
	profA := domain.ProviderProfileID("profile-a-claude")
	seedUserAndProfile(t, store, userA, profA, now)

	// A legacy/global cooldown recorded with no user/profile...
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-legacy", Harness: domain.HarnessClaudeCode, State: domain.AgentHealthCooldown, Reason: "legacy", CreatedAt: now,
	}); err != nil {
		t.Fatalf("record legacy event: %v", err)
	}

	// ...is NOT what a scoped read for user A sees before A has any scoped
	// row of their own -- scoped reads never fall back to legacy/global
	// automatically (that fallback is an explicit, separate compatibility
	// path in resolveExecutionPolicy/resolvedProfiles, not agentHealth
	// itself).
	scopedHealth, err := c.agentHealth(ctx, domain.HarnessClaudeCode, healthScope{userID: userA, profileID: profA})
	if err != nil {
		t.Fatalf("agentHealth scoped: %v", err)
	}
	if scopedHealth.State != domain.AgentHealthCooldown {
		t.Fatalf("scoped read with no scoped row yet = %v, want the documented legacy/global fallback (cooldown)", scopedHealth.State)
	}

	// Once A has their OWN scoped success event, it wins over the
	// legacy/global cooldown -- the scoped fact is always more specific.
	c.recordAgentHealthSuccess(ctx, domain.HarnessClaudeCode, healthScope{userID: userA, profileID: profA}, now.Add(time.Minute))
	scopedHealthAfter, err := c.agentHealth(ctx, domain.HarnessClaudeCode, healthScope{userID: userA, profileID: profA})
	if err != nil {
		t.Fatalf("agentHealth scoped after success: %v", err)
	}
	if scopedHealthAfter.State != domain.AgentHealthAvailable {
		t.Fatalf("scoped read after own success = %v, want available (scoped fact wins over legacy/global)", scopedHealthAfter.State)
	}

	// Unscoped read (trusted-local convention) still sees the legacy row.
	legacyHealth, err := c.agentHealth(ctx, domain.HarnessClaudeCode, healthScope{})
	if err != nil {
		t.Fatalf("agentHealth unscoped: %v", err)
	}
	if legacyHealth.State != domain.AgentHealthCooldown {
		t.Fatalf("unscoped read = %v, want the legacy/global cooldown", legacyHealth.State)
	}
}
