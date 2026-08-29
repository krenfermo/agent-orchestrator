package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// SessionLifecyclePolicyVersion is the fixed Checkpoint 8M policy version.
// Persisted alongside every SessionLifecycleDecision so a past decision
// remains explainable against the rules that actually produced it, the same
// convention RoutingPolicyVersion/ReviewPolicyVersion already follow.
const SessionLifecyclePolicyVersion = "v1"

// SessionLifecycleAction is the closed set of outcomes SessionLifecyclePolicy
// may choose (checkpoint brief §3). REUSE keeps the current session exactly
// as-is; COMPACT reuses the current session but supplies a fresh, fact-only
// SessionContextPack instead of relying purely on accumulated conversation
// state; NEW_SESSION starts a fresh session and hands it a SessionContextPack
// instead of history; UNKNOWN means the decision could not be made safely
// (e.g. session facts unreadable) and the caller must not guess.
type SessionLifecycleAction string

// The four decisions the policy can reach. Unknown is not a default: it is the
// explicit refusal to decide when session facts could not be read, and callers
// must park rather than guess.
const (
	LifecycleReuse      SessionLifecycleAction = "reuse"
	LifecycleCompact    SessionLifecycleAction = "compact"
	LifecycleNewSession SessionLifecycleAction = "new_session"
	LifecycleUnknown    SessionLifecycleAction = "unknown"
)

// SessionLifecycleReason is a closed, stable, machine-checkable code
// explaining why SessionLifecyclePolicy reached a decision (checkpoint
// brief §14) — never free text, mirroring domain.RoutingReason's own
// closed-enum contract.
type SessionLifecycleReason string

// The closed V1 reason set. Keep in sync with Valid below -- a reason that is
// not listed there is not part of the enum, however plausible it looks.
const (
	LifecycleReasonSameTaskHealthy       SessionLifecycleReason = "same_task_healthy"
	LifecycleReasonTaskBoundary          SessionLifecycleReason = "task_boundary"
	LifecycleReasonProviderSwitch        SessionLifecycleReason = "provider_switch"
	LifecycleReasonManyAttempts          SessionLifecycleReason = "many_attempts"
	LifecycleReasonManyFixCycles         SessionLifecycleReason = "many_fix_cycles"
	LifecycleReasonContextPressureActual SessionLifecycleReason = "context_pressure_actual"
	LifecycleReasonSessionUnhealthy      SessionLifecycleReason = "session_unhealthy"
	LifecycleReasonManualPolicy          SessionLifecycleReason = "manual_policy"
	LifecycleReasonUnknownUsage          SessionLifecycleReason = "unknown_usage"
)

// Valid reports whether a reason code is part of the closed V1 enum.
func (r SessionLifecycleReason) Valid() bool {
	switch r {
	case LifecycleReasonSameTaskHealthy, LifecycleReasonTaskBoundary, LifecycleReasonProviderSwitch,
		LifecycleReasonManyAttempts, LifecycleReasonManyFixCycles, LifecycleReasonContextPressureActual,
		LifecycleReasonSessionUnhealthy, LifecycleReasonManualPolicy, LifecycleReasonUnknownUsage:
		return true
	default:
		return false
	}
}

// SessionHealth is the derived-at-read-time liveness of a session consulted
// by SessionLifecyclePolicy (checkpoint brief §15). No "stale" numeric
// timeout is invented here: Stale is reserved for a session whose own
// activity signal is a terminal-shaped state (exited) without the session
// itself being marked terminated — an inconsistency, not a guessed timeout.
type SessionHealth string

// The four health states. Stale is the inconsistency described above (a
// terminal-shaped activity signal on a session nothing marked terminated),
// never an elapsed-time guess.
const (
	SessionHealthRunning    SessionHealth = "running"
	SessionHealthWaiting    SessionHealth = "waiting"
	SessionHealthStale      SessionHealth = "stale"
	SessionHealthTerminated SessionHealth = "terminated"
	SessionHealthUnknown    SessionHealth = "unknown"
)

// SessionLifecyclePolicy is Checkpoint 8M's small, explicit policy knob set.
// Deliberately does not add any numeric token/time/attempt threshold of its
// own (checkpoint brief §29): "many attempts"/"many fix cycles" signals
// reuse WorkflowPolicy's own existing MaxWorkProviderAttempts/MaxFixCycles
// values rather than inventing new ones.
type SessionLifecyclePolicy struct {
	Version string `json:"version"`
}

// DefaultSessionLifecyclePolicy is the fixed V1 default.
func DefaultSessionLifecyclePolicy() SessionLifecyclePolicy {
	return SessionLifecyclePolicy{Version: SessionLifecyclePolicyVersion}
}

// SessionLifecycleDecision is the durable, explainable result of one
// SessionLifecyclePolicy evaluation (checkpoint brief §3/§18). Persisted
// verbatim — policy version and reason codes — never reduced to a bare
// action, so an attempt can always answer "why this session action" after
// the fact.
type SessionLifecycleDecision struct {
	Action          SessionLifecycleAction   `json:"action"`
	Reasons         []SessionLifecycleReason `json:"reasons"`
	PolicyVersion   string                   `json:"policyVersion"`
	Role            WorkflowRole             `json:"role,omitempty"`
	FromSessionID   string                   `json:"fromSessionId,omitempty"`
	ToSessionID     string                   `json:"toSessionId,omitempty"`
	ContextPackHash string                   `json:"contextPackHash,omitempty"`
}

// SessionContextPackVersion is the fixed V1 format version (checkpoint
// brief §7: "SessionContextPack v1").
const SessionContextPackVersion = "v1"

// SessionContextPack is Checkpoint 8M's structured, durable handoff format:
// facts only, never chain-of-thought or a raw transcript (checkpoint brief
// §6/§17). It wraps TaskCheckpointSummary (8J) — the exact same durable
// facts object 8J's own doc comment already reserved for "a future
// checkpoint... when opening a clean session" — rather than inventing a
// second, parallel fact schema.
type SessionContextPack struct {
	Version string                `json:"version"`
	Role    WorkflowRole          `json:"role,omitempty"`
	Facts   TaskCheckpointSummary `json:"facts"`
}

// ContentHash is the pack's idempotency key (checkpoint brief §7/§27): a
// sha256 of the pack's canonical JSON encoding. TaskCheckpointSummary itself
// carries no timestamp field, so this hash is naturally free of any
// "meter timestamps dentro de hashes" violation — two packs built from the
// identical facts always hash identically, and any real fact change (a new
// file touched, a new decision, a new fingerprint) always changes it.
func (p SessionContextPack) ContentHash() string {
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
