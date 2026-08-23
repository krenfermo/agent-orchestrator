package workflow

import (
	stdctx "context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// This file is the regression suite for the self-locking provider-health bug:
// one transient agent_start_failed removed Claude from routing permanently,
// because the failure was persisted as state=unavailable with no cooldown, no
// expiry and no re-evaluation rule, and the only thing that could have cleared
// it -- a successful dispatch -- was exactly what routing then refused to allow.
//
// Every test below asserts a rule from the health policy, and each one is
// written against the durable row shapes the daemon actually writes and reads,
// so a future change to the write side that breaks the read side fails here.

// healthFixture builds a coordinator over a real store with two owned,
// authenticated, reviewer-capable profiles: Claude (anthropic) and Codex
// (openai) -- the exact shape of the real incident's provider set.
func healthFixture(t *testing.T, prober CapacityProber, clock func() time.Time) (*Coordinator, *sqlite.Store, domain.UserID, domain.ProviderProfile, domain.ProviderProfile) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	now := time.Now().UTC()
	if clock != nil {
		now = clock()
	}
	userID := domain.UserID("user-health")
	if _, err := store.InsertUser(t.Context(), domain.User{
		ID: userID, DisplayName: "Health", Email: "health@example.com", Username: string(userID),
		PasswordHash: "x", Status: domain.UserStatusActive, Role: domain.UserRoleMember,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	insert := func(id domain.ProviderProfileID, provider string, harness domain.AgentHarness, name string) domain.ProviderProfile {
		p := domain.ProviderProfile{
			ID: id, UserID: userID, Provider: provider, Harness: harness, DisplayName: name,
			Enabled: true, AuthState: domain.ProviderAuthStateAuthenticated, AuthMethod: domain.AuthMethodCLIBootstrap,
			Capabilities: []domain.ProviderCapability{
				domain.CapabilityWorker, domain.CapabilityReviewer, domain.CapabilityCapacityTelemetry,
			},
			CreatedAt: now, UpdatedAt: now,
		}
		if _, err := store.InsertProviderProfile(t.Context(), p); err != nil {
			t.Fatalf("seed %s profile: %v", name, err)
		}
		return p
	}
	claude := insert("profile-health-claude", "anthropic", domain.HarnessClaudeCode, "Claude")
	codex := insert("profile-health-codex", "openai", domain.HarnessCodex, "Codex")
	c := New(Deps{Store: store, Projects: store, CapacityProber: prober, Clock: clock})
	return c, store, userID, claude, codex
}

// recordHistoricalStaleFailure writes the EXACT durable shape found in
// ~/.ao/data for wf-57f90ff2's Claude profile: the pre-policy writer's
// permanent unavailable with no cooldown at all.
func recordHistoricalStaleFailure(t *testing.T, store *sqlite.Store, userID domain.UserID, p domain.ProviderProfile, at time.Time) {
	t.Helper()
	if _, err := store.RecordAgentHealthEvent(t.Context(), domain.AgentHealthEvent{
		ID: "ahe-historical", Harness: p.Harness, UserID: userID, ProviderProfileID: p.ID,
		State: domain.AgentHealthUnavailable, Reason: "agent_start_failed (unknown)",
		FailureClass: domain.WorkflowErrorAgentStartFailed, CooldownUntil: nil,
		ConsecutiveFailures: 1, CreatedAt: at,
	}); err != nil {
		t.Fatalf("record historical failure: %v", err)
	}
}

// A. One agent_start_failed with no cooldown can never poison a provider
// forever. The row on disk is unchanged; the policy re-reads it as the bounded
// transient failure it always was.
func TestHealthPolicy_StaleAgentStartFailedDoesNotPoisonProviderForever(t *testing.T) {
	now := time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC)
	c, store, userID, claude, _ := healthFixture(t, nil, func() time.Time { return now })
	recordHistoricalStaleFailure(t, store, userID, claude, now.Add(-3*time.Hour))

	health, err := c.agentHealth(t.Context(), claude.Harness, healthScope{userID: userID, profileID: claude.ID})
	if err != nil {
		t.Fatalf("agentHealth: %v", err)
	}
	if health.State != domain.AgentHealthCooldown {
		t.Fatalf("derived state = %q, want a bounded cooldown, not a permanent unavailable", health.State)
	}
	if health.CooldownUntil == nil {
		t.Fatalf("a cooldown with no expiry is exactly the bug: CooldownUntil must be derived")
	}
	if health.EffectiveState(now) != domain.AgentHealthUnknown {
		t.Fatalf("effective state = %q, want unknown once the cooldown has expired", health.EffectiveState(now))
	}
	if !health.Available(now) {
		t.Fatalf("an expired transient cooldown must not block dispatch")
	}
	if !health.ProbeEligible(now) {
		t.Fatalf("an expired transient cooldown must be probe-eligible")
	}
}

// B. That stale observation triggers a real capacity probe on the routing path.
// C. A successful probe returns the provider to available, durably.
func TestHealthPolicy_StaleTransientTriggersProbeAndRecovers(t *testing.T) {
	now := time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC)
	prober := &fakeProber{state: domain.CapacityAvailable, reason: "cli authenticated", ok: true}
	c, store, userID, claude, _ := healthFixture(t, prober, func() time.Time { return now })
	recordHistoricalStaleFailure(t, store, userID, claude, now.Add(-3*time.Hour))

	snapshot := c.capacitySnapshotForProfiles(t.Context(), userID,
		map[domain.ProviderProfileID]domain.ProviderProfile{claude.ID: claude})
	if prober.calls != 1 {
		t.Fatalf("prober calls = %d, want exactly 1: a stale observation must be re-evaluated", prober.calls)
	}
	if snapshot[claude.ID] != domain.CapacityAvailable {
		t.Fatalf("capacity = %q, want available after a conclusive probe", snapshot[claude.ID])
	}
	ev, ok, err := store.GetAgentHealthScoped(t.Context(), claude.Harness, userID, claude.ID)
	if err != nil || !ok {
		t.Fatalf("scoped health after probe: ok=%v err=%v", ok, err)
	}
	if ev.State != domain.AgentHealthAvailable || ev.Reason != "capacity probe: cli authenticated" {
		t.Fatalf("persisted probe conclusion = %q/%q, want a durable available with its evidence", ev.State, ev.Reason)
	}
}

// D. A successful dispatch resets the consecutive-failure streak, so the next
// failure's backoff starts from the bottom again rather than from wherever the
// last incident left it.
func TestHealthPolicy_SuccessfulDispatchResetsFailures(t *testing.T) {
	now := time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC)
	c, store, userID, claude, _ := healthFixture(t, nil, func() time.Time { return now })
	ctx := t.Context()
	scope := healthScope{userID: userID, profileID: claude.ID}

	// Distinct timestamps: agent health is read newest-first, so three failures
	// stamped at the same instant would not be an ordered streak at all.
	for i := 0; i < 3; i++ {
		c.recordAgentHealthFailure(ctx, claude.Harness, scope,
			ProviderFailureClassification{Class: domain.WorkflowErrorAgentStartFailed, Certainty: CertaintyUnknown},
			now.Add(time.Duration(i)*time.Second))
	}
	ev, _, _ := store.GetAgentHealthScoped(ctx, claude.Harness, userID, claude.ID)
	if ev.ConsecutiveFailures != 3 {
		t.Fatalf("consecutive failures = %d, want 3", ev.ConsecutiveFailures)
	}

	c.recordAgentHealthSuccess(ctx, claude.Harness, scope, now.Add(10*time.Second))
	ev, _, _ = store.GetAgentHealthScoped(ctx, claude.Harness, userID, claude.ID)
	if ev.State != domain.AgentHealthAvailable || ev.ConsecutiveFailures != 0 {
		t.Fatalf("after success: state=%q failures=%d, want available/0", ev.State, ev.ConsecutiveFailures)
	}

	after := now.Add(20 * time.Second)
	c.recordAgentHealthFailure(ctx, claude.Harness, scope,
		ProviderFailureClassification{Class: domain.WorkflowErrorAgentStartFailed, Certainty: CertaintyUnknown}, after)
	ev, _, _ = store.GetAgentHealthScoped(ctx, claude.Harness, userID, claude.ID)
	if ev.ConsecutiveFailures != 1 {
		t.Fatalf("post-success failure streak = %d, want 1", ev.ConsecutiveFailures)
	}
	if ev.CooldownUntil == nil || !ev.CooldownUntil.Equal(after.Add(time.Minute)) {
		t.Fatalf("cooldown = %v, want the first-attempt backoff (1m) after a reset streak", ev.CooldownUntil)
	}
}

// E. An auth failure stays unavailable and is never probed: AO does not get to
// decide that a provider's rejection of its credentials has expired, and a
// local CLI reporting "authorized" does not disprove an account-level refusal.
func TestHealthPolicy_AuthFailureRemainsUnavailableAndIsNeverProbed(t *testing.T) {
	now := time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC)
	prober := &fakeProber{state: domain.CapacityAvailable, ok: true}
	c, _, userID, claude, _ := healthFixture(t, prober, func() time.Time { return now })
	ctx := t.Context()

	c.recordAgentHealthFailure(ctx, claude.Harness, healthScope{userID: userID, profileID: claude.ID},
		ProviderFailureClassification{Class: domain.WorkflowErrorAuth, Certainty: CertaintyActual}, now.Add(-30*24*time.Hour))

	health, _ := c.agentHealth(ctx, claude.Harness, healthScope{userID: userID, profileID: claude.ID})
	if health.State != domain.AgentHealthUnavailable || health.Recovery != domain.AgentHealthRecoveryManual {
		t.Fatalf("auth health = %q/%q, want unavailable/manual", health.State, health.Recovery)
	}
	if health.ProbeEligible(now) {
		t.Fatalf("an auth failure must never be probe-eligible, however old it is")
	}
	snapshot := c.capacitySnapshotForProfiles(ctx, userID,
		map[domain.ProviderProfileID]domain.ProviderProfile{claude.ID: claude})
	if snapshot[claude.ID] != domain.CapacityUnavailable {
		t.Fatalf("capacity = %q, want unavailable", snapshot[claude.ID])
	}
	if prober.calls != 0 {
		t.Fatalf("prober calls = %d, want 0", prober.calls)
	}
}

// F. A missing binary stays unavailable until a probe PROVES it is back --
// time alone never clears it, and an indeterminate probe never clears it
// either.
func TestHealthPolicy_BinaryMissingClearsOnlyWhenProven(t *testing.T) {
	now := time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC)
	prober := &fakeProber{ok: false}
	clock := func() time.Time { return now }
	c, _, userID, claude, _ := healthFixture(t, prober, clock)
	ctx := t.Context()
	profiles := map[domain.ProviderProfileID]domain.ProviderProfile{claude.ID: claude}

	c.recordAgentHealthFailure(ctx, claude.Harness, healthScope{userID: userID, profileID: claude.ID},
		ProviderFailureClassification{Class: domain.WorkflowErrorBinaryMissing, Certainty: CertaintyActual}, now.Add(-24*time.Hour))

	// An inconclusive probe leaves it exactly where it was.
	if got := c.capacitySnapshotForProfiles(ctx, userID, profiles); got[claude.ID] != domain.CapacityUnavailable {
		t.Fatalf("capacity = %q after an inconclusive probe, want unavailable", got[claude.ID])
	}
	if prober.calls != 1 {
		t.Fatalf("prober calls = %d, want 1: a missing binary is re-testable", prober.calls)
	}

	// The CLI is reinstalled; the next probe proves it and the provider returns.
	prober.state, prober.ok, prober.reason = domain.CapacityAvailable, true, "cli authenticated"
	c.probeGate.clear(capacityProbeKey{harness: claude.Harness, userID: userID, profile: claude.ID})
	if got := c.capacitySnapshotForProfiles(ctx, userID, profiles); got[claude.ID] != domain.CapacityAvailable {
		t.Fatalf("capacity = %q after a conclusive probe, want available", got[claude.ID])
	}
}

// G. A provider-reported reset is honoured verbatim as the cooldown, in
// preference to AO's own backoff. H. Without one, the backoff is exponential
// and bounded.
func TestHealthPolicy_KnownResetWinsAndUnknownResetBacksOffBounded(t *testing.T) {
	now := time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC)
	c, store, userID, claude, _ := healthFixture(t, nil, func() time.Time { return now })
	ctx := t.Context()
	scope := healthScope{userID: userID, profileID: claude.ID}

	reset := now.Add(47 * time.Minute)
	c.recordAgentHealthFailure(ctx, claude.Harness, scope, ProviderFailureClassification{
		Class: domain.WorkflowErrorRateLimited, Certainty: CertaintyActual, ResetAt: &reset,
	}, now)
	ev, _, _ := store.GetAgentHealthScoped(ctx, claude.Harness, userID, claude.ID)
	if ev.CooldownUntil == nil || !ev.CooldownUntil.Equal(reset) {
		t.Fatalf("cooldown = %v, want the provider's own reported reset %v", ev.CooldownUntil, reset)
	}
	health, _ := c.agentHealth(ctx, claude.Harness, scope)
	if health.EffectiveState(now) != domain.AgentHealthCooldown || health.ProbeEligible(now) {
		t.Fatalf("a live known reset must hold the provider in cooldown and forbid probing")
	}

	// Unknown reset: bounded exponential backoff off the failure streak.
	policy := domain.AgentHealthPolicyForFailure(domain.WorkflowErrorRateLimited)
	prev := time.Duration(0)
	for streak := int64(1); streak <= 10; streak++ {
		d := policy.CooldownFor(streak)
		if d < prev {
			t.Fatalf("streak %d: cooldown shrank from %v to %v", streak, prev, d)
		}
		if d > policy.MaxCooldown {
			t.Fatalf("streak %d: cooldown %v exceeded the %v ceiling", streak, d, policy.MaxCooldown)
		}
		prev = d
	}
	if policy.CooldownFor(1) != 5*time.Minute || policy.CooldownFor(2) != 10*time.Minute {
		t.Fatalf("rate-limit backoff = %v/%v, want 5m then 10m", policy.CooldownFor(1), policy.CooldownFor(2))
	}
	if policy.CooldownFor(1000) != policy.MaxCooldown {
		t.Fatalf("an extreme streak must saturate at the ceiling, got %v", policy.CooldownFor(1000))
	}
}

// J. A cooldown survives a daemon restart with its ORIGINAL deadline: the
// expiry lives in the row, so a restart cannot silently restart the clock (and
// so a long cooldown cannot be escaped by bouncing the daemon).
func TestHealthPolicy_CooldownSurvivesRestartWithoutRestartingItsClock(t *testing.T) {
	now := time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC)
	c, store, userID, claude, _ := healthFixture(t, nil, func() time.Time { return now })
	ctx := t.Context()
	scope := healthScope{userID: userID, profileID: claude.ID}

	c.recordAgentHealthFailure(ctx, claude.Harness, scope,
		ProviderFailureClassification{Class: domain.WorkflowErrorCapacityExhausted, Certainty: CertaintyActual}, now)
	before, _ := c.agentHealth(ctx, claude.Harness, scope)

	// "Restart": a brand-new Coordinator over the same durable store.
	later := now.Add(2 * time.Minute)
	restarted := New(Deps{Store: store, Projects: store, Clock: func() time.Time { return later }})
	after, err := restarted.agentHealth(ctx, claude.Harness, scope)
	if err != nil {
		t.Fatalf("agentHealth after restart: %v", err)
	}
	if after.CooldownUntil == nil || !after.CooldownUntil.Equal(*before.CooldownUntil) {
		t.Fatalf("cooldown after restart = %v, want the original deadline %v", after.CooldownUntil, before.CooldownUntil)
	}
	if after.Available(later) {
		t.Fatalf("a still-live cooldown must keep blocking after a restart")
	}
	if after.Available(now.Add(6*time.Minute)) != true {
		t.Fatalf("the same cooldown must clear on its own once its deadline passes")
	}
}

// K/L/M. The real incident, end to end and provider-agnostic: Codex did the
// work, the review is high-risk so independence forbids a Codex reviewer,
// Claude's only health record is the stale transient failure -- and routing
// must recover to a dispatched Claude reviewer with no human intervention.
func TestHealthPolicy_StaleClaudeRecoversAsIndependentReviewer(t *testing.T) {
	now := time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC)
	prober := &fakeProber{state: domain.CapacityAvailable, reason: "cli authenticated", ok: true}
	c, store, userID, claude, codex := healthFixture(t, prober, func() time.Time { return now })
	ctx := t.Context()
	recordHistoricalStaleFailure(t, store, userID, claude, now.Add(-3*time.Hour))

	profiles := map[domain.ProviderProfileID]domain.ProviderProfile{claude.ID: claude, codex.ID: codex}
	capacity := c.capacitySnapshotForProfiles(ctx, userID, profiles)

	policy := domain.ExecutionPolicySnapshot{
		Version:            domain.UserExecutionPolicyVersion,
		ReviewerPriority:   []domain.ProviderProfileID{claude.ID, codex.ID},
		ReviewIndependence: domain.ReviewIndependenceRequireDifferentProvider,
	}
	decision := RouteExecution(RoutingRequest{
		Role:                       domain.WorkflowRoleReviewer,
		Complexity:                 ComplexityHighRisk,
		CurrentImplementerProvider: "openai", // Codex implemented the work.
		Policy:                     policy,
		EligibleProfiles:           profiles,
		Capacity:                   capacity,
	})
	if decision.Waiting {
		t.Fatalf("reviewer routing still waiting: %+v", decision.ReasonCodes)
	}
	if decision.SelectedProfileID != claude.ID {
		t.Fatalf("selected reviewer = %q, want the independent Claude profile", decision.SelectedProfileID)
	}

	// L: independence is still genuinely enforced, not merely satisfied by
	// luck. With Claude blocked by a condition nothing can clear right now, the
	// same request must WAIT rather than fall back to the implementer's own
	// provider on high-risk work.
	blocked := map[domain.ProviderProfileID]domain.CapacityState{
		claude.ID: domain.CapacityUnavailable, codex.ID: domain.CapacityAvailable,
	}
	waiting := RouteExecution(RoutingRequest{
		Role:                       domain.WorkflowRoleReviewer,
		Complexity:                 ComplexityHighRisk,
		CurrentImplementerProvider: "openai",
		Policy:                     policy,
		EligibleProfiles:           profiles,
		Capacity:                   blocked,
	})
	if !waiting.Waiting || waiting.SelectedProfileID != "" {
		t.Fatalf("high-risk independence must never fall back to the implementer's provider: %+v", waiting)
	}
}

// The classifier's typed reset signal: only a typed error may supply a reset
// time, so a scraped or guessed timestamp can never reach the cooldown.
type resetAtErr struct {
	at time.Time
}

func (e resetAtErr) Error() string              { return "rate limit exceeded" }
func (e resetAtErr) ProviderResetAt() time.Time { return e.at }

func TestClassifyProviderFailure_TypedResetOnly(t *testing.T) {
	at := time.Date(2026, 8, 23, 19, 0, 0, 0, time.UTC)
	if got := classifyProviderFailure(resetAtErr{at: at}); got.ResetAt == nil || !got.ResetAt.Equal(at) {
		t.Fatalf("typed reset = %v, want %v", got.ResetAt, at)
	}
	if got := classifyProviderFailure(stdctx.Canceled); got.ResetAt != nil {
		t.Fatalf("an untyped error must never yield a reset time, got %v", got.ResetAt)
	}
}
