package domain

// WakePolicyVersion is the fixed Checkpoint 8N policy version.
const WakePolicyVersion = "v1"

// WakePolicy is Checkpoint 8N's small, explicit set of tunable knobs for the
// durable wake-up scheduler: how long to wait past a known provider capacity
// reset before retrying, and how bounded exponential backoff with jitter
// behaves when no reset time is known. Embedded in WorkflowPolicy exactly
// like RoutingPolicy (see EffectiveWakePolicy/EffectiveRoutingPolicy) so a
// run's full decision-making configuration stays in the one
// policy_snapshot column.
type WakePolicy struct {
	Version string `json:"version"`
	// KnownResetSafetyDelaySeconds is added on top of a real
	// AgentHealthEvent.CooldownUntil before scheduling a wake, so the wake
	// fires comfortably after the provider's own reported reset rather than
	// racing it.
	KnownResetSafetyDelaySeconds int `json:"knownResetSafetyDelaySeconds"`
	// InitialBackoffSeconds/MaxBackoffSeconds/BackoffMultiplier/JitterSeconds
	// govern the unknown-reset branch: bounded exponential backoff with
	// additive jitter, never an unbounded or unconditional fixed-interval
	// poll (the max backoff is the ceiling this checkpoint allows, not the
	// primary mechanism).
	InitialBackoffSeconds int     `json:"initialBackoffSeconds"`
	MaxBackoffSeconds     int     `json:"maxBackoffSeconds"`
	BackoffMultiplier     float64 `json:"backoffMultiplier"`
	JitterSeconds         int     `json:"jitterSeconds"`
	// MaxAttempts bounds how many times a single wake scope may be retried
	// before the scheduler gives up and cancels it (Checkpoint 8N's own
	// "wake budget exhausted" ceiling) — beyond this, a human must notice
	// via the run staying in WorkflowRunWaiting with no further wake
	// scheduled.
	MaxAttempts int `json:"maxAttempts"`
}

// DefaultWakePolicy is Checkpoint 8N's fixed V1 default.
func DefaultWakePolicy() WakePolicy {
	return WakePolicy{
		Version:                      WakePolicyVersion,
		KnownResetSafetyDelaySeconds: 30,
		InitialBackoffSeconds:        60,
		MaxBackoffSeconds:            1800,
		BackoffMultiplier:            2.0,
		JitterSeconds:                15,
		MaxAttempts:                  8,
	}
}
