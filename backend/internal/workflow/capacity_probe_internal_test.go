package workflow

import (
	stdctx "context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// fakeProber records how many times it was asked, so the throttle can be
// proven rather than assumed.
type fakeProber struct {
	state  domain.CapacityState
	reason string
	ok     bool
	err    error
	calls  int
}

func (f *fakeProber) ProbeCapacity(stdctx.Context, domain.AgentHarness) (domain.CapacityState, string, bool, error) {
	f.calls++
	return f.state, f.reason, f.ok, f.err
}

// seedCodexProfile inserts the owned Codex profile whose foreign keys a scoped
// agent_health_events row needs. Mirrors ~/.ao/data's real second profile:
// enabled, authenticated, reviewer + capacity_telemetry capable, and never
// dispatched to.
func seedCodexProfile(t *testing.T, store *sqlite.Store, userID domain.UserID, profileID domain.ProviderProfileID, now time.Time) domain.ProviderProfile {
	t.Helper()
	p := domain.ProviderProfile{
		ID: profileID, UserID: userID, Provider: "openai", Harness: domain.HarnessCodex, DisplayName: "Codex",
		Enabled: true, AuthState: domain.ProviderAuthStateAuthenticated, AuthMethod: domain.AuthMethodCLIBootstrap,
		Capabilities: []domain.ProviderCapability{domain.CapabilityWorker, domain.CapabilityReviewer, domain.CapabilityCapacityTelemetry},
		CreatedAt:    now, UpdatedAt: now,
	}
	if _, err := store.InsertProviderProfile(t.Context(), p); err != nil {
		t.Fatalf("seed codex profile: %v", err)
	}
	return p
}

func probeFixture(t *testing.T, prober CapacityProber) (*Coordinator, *sqlite.Store, domain.UserID, domain.ProviderProfile) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	now := time.Now().UTC()
	userID := domain.UserID("user-probe")
	claudeID := domain.ProviderProfileID("profile-probe-claude")
	codexID := domain.ProviderProfileID("profile-probe-codex")
	seedUserAndProfile(t, store, userID, claudeID, now)
	codex := seedCodexProfile(t, store, userID, codexID, now)
	c := New(Deps{Store: store, Projects: store, CapacityProber: prober})
	return c, store, userID, codex
}

// Regression B: a Codex profile that has never been dispatched to starts
// unknown, an active probe concludes available, and the workflow can proceed —
// with no human ever opening or running Codex first (requirement §6).
func TestCapacitySnapshot_ProbesUnknownProfile(t *testing.T) {
	prober := &fakeProber{state: domain.CapacityAvailable, reason: "cli authenticated", ok: true}
	c, store, userID, codex := probeFixture(t, prober)
	ctx := t.Context()

	// Precondition: no recorded health fact of any kind for this profile.
	if _, ok, err := store.GetAgentHealthScoped(ctx, domain.HarnessCodex, userID, codex.ID); err != nil || ok {
		t.Fatalf("precondition: scoped health ok=%v err=%v, want no recorded fact", ok, err)
	}

	snapshot := c.capacitySnapshotForProfiles(ctx, userID, map[domain.ProviderProfileID]domain.ProviderProfile{codex.ID: codex})
	if snapshot[codex.ID] != domain.CapacityAvailable {
		t.Fatalf("capacity = %q, want available after a successful probe", snapshot[codex.ID])
	}
	if prober.calls != 1 {
		t.Fatalf("prober calls = %d, want exactly 1", prober.calls)
	}

	// Requirement §7: the conclusion is durable and self-describing, so the
	// capacity decision can be diagnosed after the fact.
	ev, ok, err := store.GetAgentHealthScoped(ctx, domain.HarnessCodex, userID, codex.ID)
	if err != nil || !ok {
		t.Fatalf("scoped health after probe: ok=%v err=%v, want a persisted fact", ok, err)
	}
	if ev.State != domain.AgentHealthAvailable {
		t.Fatalf("persisted state = %q, want available", ev.State)
	}
	if ev.Reason != "capacity probe: cli authenticated" {
		t.Fatalf("persisted reason = %q, want the probe's own evidence", ev.Reason)
	}

	// A second read is served from the durable fact — the probe is not re-run
	// on every routing evaluation.
	if got := c.capacitySnapshotForProfiles(ctx, userID, map[domain.ProviderProfileID]domain.ProviderProfile{codex.ID: codex}); got[codex.ID] != domain.CapacityAvailable {
		t.Fatalf("second capacity = %q, want available", got[codex.ID])
	}
	if prober.calls != 1 {
		t.Fatalf("prober calls = %d after a recorded fact, want no re-probe", prober.calls)
	}
}

// Regression C: a probe that concludes "unavailable" is recorded as such, so
// routing waits on real evidence rather than on absence of it.
func TestCapacitySnapshot_ProbeUnavailableIsRecorded(t *testing.T) {
	prober := &fakeProber{state: domain.CapacityUnavailable, reason: "cli reports not authenticated", ok: true}
	c, store, userID, codex := probeFixture(t, prober)
	ctx := t.Context()

	snapshot := c.capacitySnapshotForProfiles(ctx, userID, map[domain.ProviderProfileID]domain.ProviderProfile{codex.ID: codex})
	if snapshot[codex.ID] != domain.CapacityUnavailable {
		t.Fatalf("capacity = %q, want unavailable", snapshot[codex.ID])
	}
	ev, ok, err := store.GetAgentHealthScoped(ctx, domain.HarnessCodex, userID, codex.ID)
	if err != nil || !ok || ev.State != domain.AgentHealthUnavailable {
		t.Fatalf("persisted health = %+v ok=%v err=%v, want unavailable", ev, ok, err)
	}
}

// Regression D: an indeterminate probe leaves the state unknown, writes NO
// durable fact (absence of evidence is not evidence), and is throttled so a
// re-entrant dispatch path cannot spawn a subprocess per evaluation.
func TestCapacitySnapshot_IndeterminateProbeIsThrottledAndNotRecorded(t *testing.T) {
	prober := &fakeProber{state: domain.CapacityUnknown, reason: "auth probe inconclusive", ok: false}
	c, store, userID, codex := probeFixture(t, prober)
	ctx := t.Context()
	profiles := map[domain.ProviderProfileID]domain.ProviderProfile{codex.ID: codex}

	for i := 0; i < 5; i++ {
		if got := c.capacitySnapshotForProfiles(ctx, userID, profiles); got[codex.ID] != domain.CapacityUnknown {
			t.Fatalf("capacity = %q, want unknown after an inconclusive probe", got[codex.ID])
		}
	}
	if prober.calls != 1 {
		t.Fatalf("prober calls = %d across 5 evaluations, want 1 (throttled)", prober.calls)
	}
	if _, ok, _ := store.GetAgentHealthScoped(ctx, domain.HarnessCodex, userID, codex.ID); ok {
		t.Fatalf("an inconclusive probe must not persist a health fact")
	}

	// Once the throttle window elapses, exactly one more attempt is allowed —
	// bounded retry, never a busy loop.
	c.probeGate.attempts[capacityProbeKey{harness: domain.HarnessCodex, userID: userID, profile: codex.ID}] =
		time.Now().UTC().Add(-capacityProbeThrottle - time.Second)
	if got := c.capacitySnapshotForProfiles(ctx, userID, profiles); got[codex.ID] != domain.CapacityUnknown {
		t.Fatalf("capacity = %q, want unknown", got[codex.ID])
	}
	if prober.calls != 2 {
		t.Fatalf("prober calls = %d after the throttle window, want 2", prober.calls)
	}
}

// A probe error is treated exactly like an inconclusive probe: unknown, no
// durable fact, and never a downgrade to unavailable.
func TestCapacitySnapshot_ProbeErrorLeavesUnknown(t *testing.T) {
	prober := &fakeProber{err: errors.New("binary exploded")}
	c, store, userID, codex := probeFixture(t, prober)
	ctx := t.Context()

	got := c.capacitySnapshotForProfiles(ctx, userID, map[domain.ProviderProfileID]domain.ProviderProfile{codex.ID: codex})
	if got[codex.ID] != domain.CapacityUnknown {
		t.Fatalf("capacity = %q, want unknown on probe error", got[codex.ID])
	}
	if _, ok, _ := store.GetAgentHealthScoped(ctx, domain.HarnessCodex, userID, codex.ID); ok {
		t.Fatalf("a failed probe must not persist a health fact")
	}
}

// A LIVE recorded cooldown is authoritative: the probe is never consulted, so
// an optimistic local probe can never clear a real, observed rate limit while
// its window is still open. (An EXPIRED one is a different fact entirely — see
// TestCapacitySnapshot_ExpiredCooldownIsReprobed.)
func TestCapacitySnapshot_RecordedFailureIsNeverOverriddenByProbe(t *testing.T) {
	prober := &fakeProber{state: domain.CapacityAvailable, reason: "cli authenticated", ok: true}
	c, store, userID, codex := probeFixture(t, prober)
	ctx := t.Context()

	cooldownUntil := time.Now().UTC().Add(30 * time.Minute)
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-cooldown", Harness: domain.HarnessCodex, UserID: userID, ProviderProfileID: codex.ID,
		State: domain.AgentHealthCooldown, Reason: "rate_limited",
		FailureClass: domain.WorkflowErrorRateLimited, CooldownUntil: &cooldownUntil,
		ConsecutiveFailures: 1, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record cooldown: %v", err)
	}

	got := c.capacitySnapshotForProfiles(ctx, userID, map[domain.ProviderProfileID]domain.ProviderProfile{codex.ID: codex})
	if got[codex.ID] != domain.CapacityCooldown {
		t.Fatalf("capacity = %q, want the recorded cooldown to stand", got[codex.ID])
	}
	if prober.calls != 0 {
		t.Fatalf("prober calls = %d, want 0: a recorded fact is authoritative", prober.calls)
	}
}

// A profile that does not advertise capacity_telemetry is never probed.
func TestCapacitySnapshot_ProfileWithoutTelemetryCapabilityIsNotProbed(t *testing.T) {
	prober := &fakeProber{state: domain.CapacityAvailable, ok: true}
	c, _, userID, codex := probeFixture(t, prober)
	codex.Capabilities = []domain.ProviderCapability{domain.CapabilityReviewer}

	got := c.capacitySnapshotForProfiles(t.Context(), userID, map[domain.ProviderProfileID]domain.ProviderProfile{codex.ID: codex})
	if got[codex.ID] != domain.CapacityUnknown {
		t.Fatalf("capacity = %q, want unknown", got[codex.ID])
	}
	if prober.calls != 0 {
		t.Fatalf("prober calls = %d, want 0 without capacity_telemetry", prober.calls)
	}
}

// A nil prober keeps the exact pre-Checkpoint-8P-E.13A.4 behavior.
func TestCapacitySnapshot_NilProberKeepsUnknown(t *testing.T) {
	c, _, userID, codex := probeFixture(t, nil)
	got := c.capacitySnapshotForProfiles(t.Context(), userID, map[domain.ProviderProfileID]domain.ProviderProfile{codex.ID: codex})
	if got[codex.ID] != domain.CapacityUnknown {
		t.Fatalf("capacity = %q, want unknown with no prober wired", got[codex.ID])
	}
}
