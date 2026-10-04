package domain

import (
	"math"
	"time"
)

// AgentHealthState is the durable, derived-at-read-time availability of one
// harness for workflow dispatch (Checkpoint 8H). It is intentionally the
// minimal state set §5 of the checkpoint asks for — not a full
// ResourceManager: AO persists failure/success facts as an append-only
// event log (AgentHealthEvent) and derives this state from the latest event
// per harness, per the "persist facts, derive state" rule the rest of AO
// already follows.
type AgentHealthState string

const (
	// AgentHealthAvailable means the harness has no recorded cooldown/outage
	// in effect and may be dispatched to.
	AgentHealthAvailable AgentHealthState = "available"
	// AgentHealthCooldown means the harness recently failed with an eligible
	// provider failure and is in a durable cooldown window. The window has no
	// invented duration when the provider gave no reliable reset time — see
	// AgentHealthEvent.CooldownUntil being nil despite State being cooldown.
	AgentHealthCooldown AgentHealthState = "cooldown"
	// AgentHealthUnavailable means the harness failed with a non-time-boxed
	// condition (e.g. binary missing, auth failure) that a cooldown timer
	// cannot heal on its own.
	AgentHealthUnavailable AgentHealthState = "unavailable"
	// AgentHealthUnknown means no health event has ever been recorded for
	// this harness. Not the same as available: it is simply unobserved.
	AgentHealthUnknown AgentHealthState = "unknown"
)

// Valid reports whether a health state is persistable.
func (s AgentHealthState) Valid() bool {
	switch s {
	case AgentHealthAvailable, AgentHealthCooldown, AgentHealthUnavailable, AgentHealthUnknown:
		return true
	default:
		return false
	}
}

// AgentHealthEvent is one append-only durable fact about a harness's
// dispatch outcome (Checkpoint 8H). Never updated; the current AgentHealth
// for a harness is derived by reading the latest event for it.
//
// UserID/ProviderProfileID (Checkpoint 8P-C) scope an event to the exact
// owner+connection that produced it. Both are empty for events recorded
// before 8P-C, or for an unowned run/trusted-local dispatch with no matched
// profile -- these "legacy/global" rows remain readable as a compatibility
// fallback (see service/capacity's precedence rule) but a scoped row for the
// same user+profile+harness always takes precedence over them.
type AgentHealthEvent struct {
	ID                  string
	Harness             AgentHarness
	UserID              UserID
	ProviderProfileID   ProviderProfileID
	State               AgentHealthState
	Reason              string
	FailureClass        WorkflowErrorClass
	CooldownUntil       *time.Time
	ConsecutiveFailures int64
	CreatedAt           time.Time
}

// AgentHealthRecovery names HOW a non-available harness may become available
// again. It is the missing half of AgentHealthState, and the reason a single
// transient launch failure used to remove a healthy provider from routing
// permanently.
//
// Before this existed, every failure class that was not rate_limited/
// capacity_exhausted/transient was persisted as AgentHealthUnavailable with no
// cooldown_until, no expiry, and no re-evaluation rule. Available() then
// answered false for it forever, routing refused to select the provider, and
// because health is only ever refreshed as a side effect of a real dispatch,
// no dispatch could ever happen to refresh it. That is a self-locking state:
// the evidence that would clear it can only be produced by an action the
// evidence itself forbids. wf-57f90ff2 sat in waiting_for_capacity for exactly
// that reason after one already-fixed tmux launch race classified as
// agent_start_failed.
//
// Naming the recovery mode makes the question answerable rather than
// permanent: every state below either expires on its own, can be re-proven by
// a probe, or genuinely requires a human to change something.
type AgentHealthRecovery string

const (
	// AgentHealthRecoveryNone is the zero value: the harness is available or
	// simply unobserved, so there is nothing to recover from.
	AgentHealthRecoveryNone AgentHealthRecovery = ""
	// AgentHealthRecoveryCooldown means the condition is time-boxed. Once
	// CooldownUntil passes the observation carries no current evidence at all,
	// and the harness must be re-evaluated (probed, or simply retried) rather
	// than kept blocked.
	AgentHealthRecoveryCooldown AgentHealthRecovery = "cooldown"
	// AgentHealthRecoveryProbe means the condition is real right now and time
	// alone cannot clear it, but a cheap local capacity probe CAN prove whether
	// it still holds (a missing CLI is the canonical case: it stays blocking
	// until a probe finds the binary). The harness stays blocked until such a
	// probe concludes otherwise -- never merely because time passed.
	AgentHealthRecoveryProbe AgentHealthRecovery = "probe"
	// AgentHealthRecoveryManual means only a human changing credentials or
	// configuration can clear it. AO must not probe its way out of an auth
	// rejection: a local CLI reporting "authorized" does not disprove a
	// provider rejecting the account.
	AgentHealthRecoveryManual AgentHealthRecovery = "manual"
)

// AgentHealthReprobeInterval is how stale an AgentHealthRecoveryProbe
// observation must be before another probe is worth attempting. It bounds how
// often a provably-broken provider is re-tested without ever letting the
// observation become permanent.
const AgentHealthReprobeInterval = 10 * time.Minute

// AgentHealthPolicy is the explicit, per-failure-class health semantics AO
// applies to every recorded failure -- the "authoritative health policy" that
// replaces the previous rule of "rate/capacity/transient cool down, everything
// else is unavailable forever".
type AgentHealthPolicy struct {
	State    AgentHealthState
	Recovery AgentHealthRecovery
	// InitialCooldown/MaxCooldown bound the exponential backoff used when the
	// state is AgentHealthCooldown and the provider reported no reset time of
	// its own. Zero for non-cooldown policies.
	InitialCooldown time.Duration
	MaxCooldown     time.Duration
}

// CooldownFor returns the bounded exponential cooldown for the nth consecutive
// failure (1-based). Doubling, capped at MaxCooldown, never negative and never
// unbounded. A non-cooldown policy always returns 0.
func (p AgentHealthPolicy) CooldownFor(consecutiveFailures int64) time.Duration {
	if p.State != AgentHealthCooldown || p.InitialCooldown <= 0 {
		return 0
	}
	if consecutiveFailures < 1 {
		consecutiveFailures = 1
	}
	// Cap the exponent before computing it so a pathological failure count
	// cannot overflow the multiplication.
	exp := consecutiveFailures - 1
	if exp > 32 {
		exp = 32
	}
	d := time.Duration(math.Pow(2, float64(exp)) * float64(p.InitialCooldown))
	if p.MaxCooldown > 0 && d > p.MaxCooldown {
		return p.MaxCooldown
	}
	return d
}

// AgentHealthPolicyForFailure is the single authoritative mapping from a
// failure class to its health semantics. Every recorded failure goes through
// it, both when written (workflow/health.go) and when a stored row is read back
// (workflow/health.go's agentHealthFromEvent), so rows written by the older,
// permanence-by-default code are re-interpreted under this policy on the next
// read instead of needing a data migration.
//
// The classes are deliberately grouped by what actually clears them:
//
//  1. auth -> unavailable, manual. AO never touches authentication state.
//  2. binary_missing -> unavailable, probe. A local probe can prove the CLI is
//     back; time cannot.
//  3. rate_limited / capacity_exhausted -> cooldown. The caller supplies the
//     provider's own reset when one is known; otherwise bounded exponential
//     backoff, never a permanent unavailable.
//  4. everything else, including agent_start_failed, transient, runtime and
//     transport failures, and any class this function does not recognise ->
//     cooldown with a short bounded backoff. An unclassified failure is the
//     case AO knows least about, which is precisely why it must be the one it
//     is least willing to make permanent.
func AgentHealthPolicyForFailure(class WorkflowErrorClass) AgentHealthPolicy {
	switch class {
	case WorkflowErrorAuth:
		return AgentHealthPolicy{State: AgentHealthUnavailable, Recovery: AgentHealthRecoveryManual}
	case WorkflowErrorBinaryMissing:
		return AgentHealthPolicy{State: AgentHealthUnavailable, Recovery: AgentHealthRecoveryProbe}
	case WorkflowErrorRateLimited, WorkflowErrorCapacityExhausted:
		return AgentHealthPolicy{
			State:           AgentHealthCooldown,
			Recovery:        AgentHealthRecoveryCooldown,
			InitialCooldown: 5 * time.Minute,
			MaxCooldown:     time.Hour,
		}
	default:
		return AgentHealthPolicy{
			State:           AgentHealthCooldown,
			Recovery:        AgentHealthRecoveryCooldown,
			InitialCooldown: time.Minute,
			MaxCooldown:     15 * time.Minute,
		}
	}
}

// AgentHealth is the derived-at-read-time health of one harness: the latest
// AgentHealthEvent recorded for it, or the zero-value AgentHealthUnknown
// state if none exists.
type AgentHealth struct {
	Harness             AgentHarness
	State               AgentHealthState
	Reason              string
	FailureClass        WorkflowErrorClass
	CooldownUntil       *time.Time
	LastFailureAt       *time.Time
	LastSuccessAt       *time.Time
	ConsecutiveFailures int64
	// Recovery is how this state may clear (see AgentHealthRecovery). Derived
	// from the recorded failure class, never stored: the policy is allowed to
	// change, and a past row must be readable under the current one.
	Recovery AgentHealthRecovery
	// ObservedAt is when the underlying event was recorded. It is what makes
	// freshness expressible at all -- "this is what we last saw, and how long
	// ago" rather than "this is how things are".
	ObservedAt time.Time
}

// EffectiveState is the harness's state AS OF now, applying the freshness
// semantics AgentHealthState alone cannot express.
//
// The load-bearing case is an expired cooldown. A cooldown that has run out is
// not evidence of anything current: it is a past failure whose agreed waiting
// period is over. Reporting it as AgentHealthUnknown (rather than continuing to
// report cooldown) is what lets routing treat the harness as conservatively
// routable again and lets the capacity layer decide to probe it -- without ever
// fabricating an "available" that nothing observed.
//
// An AgentHealthRecoveryProbe unavailable deliberately does NOT decay with
// time: a missing binary is still missing an hour later. It clears only when a
// probe or a real dispatch says otherwise.
func (h AgentHealth) EffectiveState(now time.Time) AgentHealthState {
	switch h.State {
	case AgentHealthCooldown:
		if h.CooldownUntil != nil && h.CooldownUntil.After(now) {
			return AgentHealthCooldown
		}
		return AgentHealthUnknown
	case AgentHealthUnavailable:
		return AgentHealthUnavailable
	case AgentHealthAvailable:
		return AgentHealthAvailable
	default:
		return AgentHealthUnknown
	}
}

// ProbeEligible reports whether a capacity probe is worth attempting for this
// harness right now.
//
// This is the freshness rule the incident report asks for, stated positively:
// an observation with no valid permanent reason and no unexpired cooldown must
// be re-evaluated by evidence, not treated as authoritative forever.
func (h AgentHealth) ProbeEligible(now time.Time) bool {
	switch h.Recovery {
	case AgentHealthRecoveryManual:
		// Credentials are not AO's to re-test.
		return false
	case AgentHealthRecoveryCooldown:
		return h.CooldownUntil == nil || !h.CooldownUntil.After(now)
	case AgentHealthRecoveryProbe:
		return h.ObservedAt.IsZero() || !now.Before(h.ObservedAt.Add(AgentHealthReprobeInterval))
	default:
		return h.State == AgentHealthUnknown
	}
}

// Available reports whether dispatch may target this harness right now. A
// cooldown with a still-future CooldownUntil blocks dispatch; an expired or
// unknown-duration cooldown does not, so an unknown-duration cooldown can never
// permanently wedge the synchronous same-reconcile failover decision.
func (h AgentHealth) Available(now time.Time) bool {
	switch h.EffectiveState(now) {
	case AgentHealthUnavailable, AgentHealthCooldown:
		return false
	default:
		return true
	}
}
