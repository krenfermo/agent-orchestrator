package domain

import "time"

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
type AgentHealthEvent struct {
	ID                  string
	Harness             AgentHarness
	State               AgentHealthState
	Reason              string
	FailureClass        WorkflowErrorClass
	CooldownUntil       *time.Time
	ConsecutiveFailures int64
	CreatedAt           time.Time
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
}

// Available reports whether dispatch may target this harness right now. A
// cooldown with a still-future CooldownUntil blocks dispatch; a cooldown
// whose CooldownUntil is nil (unknown reset) or already past does not block
// 8H's synchronous same-reconcile failover decision — 8H does not implement a
// scheduled retry, so an unknown-duration cooldown must not permanently wedge
// the only two-harness fallback order it supports.
func (h AgentHealth) Available(now time.Time) bool {
	switch h.State {
	case AgentHealthUnavailable:
		return false
	case AgentHealthCooldown:
		return h.CooldownUntil == nil || !h.CooldownUntil.After(now)
	default:
		return true
	}
}
