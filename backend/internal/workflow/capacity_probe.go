package workflow

import (
	stdctx "context"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// CapacityProber actively determines whether a harness can accept work right
// now (Checkpoint 8P-E.13A.4), instead of waiting for a dispatch to happen to
// find out.
//
// Before this, capacity had exactly one source: the agent_health_events rows
// workflow dispatch writes AFTER a real launch succeeds or fails
// (health.go's recordAgentHealthSuccess/recordAgentHealthFailure). That is
// purely REACTIVE, so a freshly connected provider profile that has never been
// dispatched to reports domain.CapacityUnknown forever, and the only way to
// leave that state was for a human to make AO run that provider once. In
// ~/.ao/data the Codex profile had zero rows in agent_health_events,
// conversation_provider_events and telemetry_event for exactly that reason.
//
// A prober answers from cheap LOCAL evidence only — is the harness CLI
// installed, and does its own auth/status command say the credentials are
// good. It never sends a prompt, never spends quota, and never scrapes a
// vendor page. That deliberately bounds what it can conclude: it proves a
// provider is reachable and usable, not that a rate limit is clear. Quota
// exhaustion continues to arrive through the reactive failure path, which
// (being newer) always wins the health read.
type CapacityProber interface {
	// ProbeCapacity reports harness's currently observable capacity. ok=false
	// means "could not determine" — the caller must leave the state unknown
	// and must NOT record a durable fact, so a probe that cannot conclude can
	// never masquerade as evidence. reason is short, human-readable, and
	// persisted verbatim as the health event's reason.
	ProbeCapacity(ctx stdctx.Context, harness domain.AgentHarness) (state domain.CapacityState, reason string, ok bool, err error)
}

// capacityProbeThrottle is the minimum interval between two probe ATTEMPTS
// for the same (harness, user, profile) when the previous attempt could not
// conclude. A conclusive probe needs no throttle: it writes a durable health
// event, so the very next capacityStateForProfile read finds a recorded state
// and never probes again. This throttle exists only so an indeterminate probe
// (CLI missing an auth subcommand, a hung binary, a prober that isn't wired)
// cannot turn every routing evaluation into another subprocess spawn —
// requirement §4's "do not spin continuously," applied at the probe layer as
// well as at the wake layer.
const capacityProbeThrottle = 5 * time.Minute

// capacityProbeGate remembers the last INDETERMINATE probe attempt per scope.
// In-memory only and best-effort: losing it across a daemon restart just means
// one extra probe attempt, which is cheap and correct.
type capacityProbeGate struct {
	mu       sync.Mutex
	attempts map[capacityProbeKey]time.Time
}

type capacityProbeKey struct {
	harness domain.AgentHarness
	userID  domain.UserID
	profile domain.ProviderProfileID
}

// allow reports whether a probe attempt for key may run now, recording the
// attempt when it may.
func (g *capacityProbeGate) allow(key capacityProbeKey, now time.Time) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if last, ok := g.attempts[key]; ok && now.Sub(last) < capacityProbeThrottle {
		return false
	}
	if g.attempts == nil {
		g.attempts = make(map[capacityProbeKey]time.Time)
	}
	g.attempts[key] = now
	return true
}

// clear forgets key's throttle, called once a probe concludes so a later
// unknown (e.g. after the health row is superseded) can probe immediately.
func (g *capacityProbeGate) clear(key capacityProbeKey) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.attempts, key)
}

// probeCapacityForProfile actively resolves an unknown capacity state for one
// eligible profile and, when the probe concludes, persists the result as a
// scoped agent_health_events row so every later reader (routing, the 8J
// capacity view, the frontend) sees the same durable evidence for the same
// reason — requirement §7.
//
// Returns the resolved state and whether the probe concluded. A nil prober, a
// profile that does not advertise capacity_telemetry, a throttled scope, or a
// prober that returns ok=false all leave the state unknown, which
// capacityEligible still treats as routable: an indeterminate probe must never
// downgrade a provider to "unavailable" on no evidence.
func (c *Coordinator) probeCapacityForProfile(ctx stdctx.Context, scope healthScope, profile domain.ProviderProfile) (domain.CapacityState, bool) {
	if c.capacityProber == nil || !domain.HasCapability(profile.Capabilities, domain.CapabilityCapacityTelemetry) {
		return domain.CapacityUnknown, false
	}
	key := capacityProbeKey{harness: profile.Harness, userID: scope.userID, profile: scope.profileID}
	if !c.probeGate.allow(key, c.clock()) {
		return domain.CapacityUnknown, false
	}
	state, reason, ok, err := c.capacityProber.ProbeCapacity(ctx, profile.Harness)
	if err != nil || !ok {
		if c.log != nil {
			c.log.Debug("capacity probe indeterminate", "harness", profile.Harness, "profileID", scope.profileID, "reason", reason, "error", err)
		}
		return domain.CapacityUnknown, false
	}
	c.probeGate.clear(key)
	c.recordProbedCapacity(ctx, profile.Harness, scope, state, reason)
	return state, true
}

// recordProbedCapacity writes the probe's conclusion into the same durable
// stream dispatch outcomes already use, so a capacity decision is diagnosable
// from one place. The reason is prefixed so a probed fact is always
// distinguishable from a dispatch-observed one in the stored row.
func (c *Coordinator) recordProbedCapacity(ctx stdctx.Context, harness domain.AgentHarness, scope healthScope, state domain.CapacityState, reason string) {
	health := domain.AgentHealthUnavailable
	switch state {
	case domain.CapacityAvailable:
		health = domain.AgentHealthAvailable
	case domain.CapacityLimited, domain.CapacityCooldown:
		health = domain.AgentHealthCooldown
	}
	if reason == "" {
		reason = string(state)
	}
	_, _ = c.store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID:                "ahe-" + c.newID(),
		Harness:           harness,
		UserID:            scope.userID,
		ProviderProfileID: scope.profileID,
		State:             health,
		Reason:            "capacity probe: " + reason,
		CreatedAt:         c.clock(),
	})
}
