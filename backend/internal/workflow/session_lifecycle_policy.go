package workflow

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// SessionLifecycleRequest is SessionLifecyclePolicy's pure input (checkpoint
// brief §3): every fact a lifecycle decision may consult, gathered by the
// caller from already-durable state. DecideSessionLifecycle performs no IO,
// so the same request always produces the same decision.
type SessionLifecycleRequest struct {
	Role domain.WorkflowRole
	// CurrentSessionID empty means there is no existing session to reuse
	// (e.g. a brand-new task) — always NEW_SESSION in that case, trivially.
	CurrentSessionID string
	SessionHealth    domain.SessionHealth
	// TaskBoundary is true when the planned task this dispatch is for
	// differs from the task the current session was originally spawned for
	// (checkpoint brief §5/§13) — computed by the caller, never guessed
	// here from a clock or token count.
	TaskBoundary bool
	// ProviderSwitch is true when a permanent provider failover (8H/8L) has
	// already chosen a different harness than the current session's own.
	ProviderSwitch bool
	AttemptCount   int
	FixCycleCount  int
	// ContextPressure is true only when a real, typed provider signal says
	// so (checkpoint brief §5/§29: "No inventar thresholds de tokens si no
	// existen"). No production call site sets this true today — AO has no
	// per-session live token/context signal yet (confirmed by the 8L audit)
	// — but the field and its reason code exist so a future real signal has
	// somewhere to plug in without a second policy revision.
	ContextPressure bool
	// UsageKnown is false when token/context usage for the current session
	// is not observable — never treated as "no pressure", only as "unknown"
	// (checkpoint brief §15 "unknown != 0" analog).
	UsageKnown bool
	Policy     domain.WorkflowPolicy
}

// DecideSessionLifecycle is Checkpoint 8M's pure, deterministic V1 session
// lifecycle decision. It never calls an LLM, never performs IO, and never
// mutates state. Evaluation order (most conservative/certain facts first):
//
//  1. No current session -> NEW_SESSION (nothing to reuse).
//  2. Session unhealthy (terminated/stale/unknown) -> NEW_SESSION: a dead
//     or ambiguous session can never be safely reused (checkpoint brief §5:
//     "recovery no puede demostrar continuidad segura").
//  3. Task boundary -> NEW_SESSION (checkpoint brief §13, the largest
//     likely saving: a new planned task never inherits the old one's
//     session).
//  4. Provider switch -> NEW_SESSION (checkpoint brief §11): a durable
//     harness change is handled by 8H's own switching saga, never by
//     reusing a session that belongs to the wrong provider.
//  5. Fix cycles or attempts at/above the run's OWN already-configured
//     WorkflowPolicy budget -> COMPACT: still the same session (fix reuse
//     stays default per checkpoint brief §12), but with a fresh fact-only
//     context pack rather than relying purely on accumulated state. Never a
//     new invented number — reuses MaxFixCycles/MaxWorkProviderAttempts
//     verbatim (checkpoint brief §29).
//  6. Real context pressure -> COMPACT.
//  7. Otherwise -> REUSE, same task, healthy session, no real reason to
//     touch it (checkpoint brief §4).
func DecideSessionLifecycle(req SessionLifecycleRequest) domain.SessionLifecycleDecision {
	decision := domain.SessionLifecycleDecision{
		Role:          req.Role,
		PolicyVersion: domain.SessionLifecyclePolicyVersion,
		FromSessionID: req.CurrentSessionID,
	}

	if req.CurrentSessionID == "" {
		decision.Action = domain.LifecycleNewSession
		decision.Reasons = []domain.SessionLifecycleReason{domain.LifecycleReasonTaskBoundary}
		return decision
	}

	switch req.SessionHealth {
	case domain.SessionHealthTerminated, domain.SessionHealthStale, domain.SessionHealthUnknown:
		decision.Action = domain.LifecycleNewSession
		decision.Reasons = []domain.SessionLifecycleReason{domain.LifecycleReasonSessionUnhealthy}
		return decision
	}

	if req.TaskBoundary {
		decision.Action = domain.LifecycleNewSession
		decision.Reasons = []domain.SessionLifecycleReason{domain.LifecycleReasonTaskBoundary}
		return decision
	}
	if req.ProviderSwitch {
		decision.Action = domain.LifecycleNewSession
		decision.Reasons = []domain.SessionLifecycleReason{domain.LifecycleReasonProviderSwitch}
		return decision
	}

	policy := req.Policy
	var reasons []domain.SessionLifecycleReason
	if policy.MaxFixCycles > 0 && req.FixCycleCount >= policy.MaxFixCycles {
		reasons = append(reasons, domain.LifecycleReasonManyFixCycles)
	}
	if maxAttempts := effectiveMaxWorkProviderAttempts(policy); req.AttemptCount >= maxAttempts {
		reasons = append(reasons, domain.LifecycleReasonManyAttempts)
	}
	if req.ContextPressure {
		reasons = append(reasons, domain.LifecycleReasonContextPressureActual)
	}
	if len(reasons) > 0 {
		decision.Action = domain.LifecycleCompact
		decision.Reasons = reasons
		return decision
	}

	decision.Action = domain.LifecycleReuse
	decision.Reasons = []domain.SessionLifecycleReason{domain.LifecycleReasonSameTaskHealthy}
	if !req.UsageKnown {
		decision.Reasons = append(decision.Reasons, domain.LifecycleReasonUnknownUsage)
	}
	return decision
}

// sessionHealthFromFacts derives domain.SessionHealth from a session's own
// durable record (checkpoint brief §15) — never a guessed timeout. Stale
// means the session's own activity signal already looks terminal-shaped
// (exited) while the session row itself isn't marked terminated, an
// inconsistency worth treating conservatively rather than a numeric age.
func sessionHealthFromFacts(rec domain.SessionRecord, found bool) domain.SessionHealth {
	if !found {
		return domain.SessionHealthUnknown
	}
	if rec.IsTerminated {
		return domain.SessionHealthTerminated
	}
	switch rec.Activity.State {
	case domain.ActivityExited:
		return domain.SessionHealthStale
	case domain.ActivityWaitingInput, domain.ActivityBlocked:
		return domain.SessionHealthWaiting
	case domain.ActivityActive, domain.ActivityIdle:
		return domain.SessionHealthRunning
	default:
		return domain.SessionHealthUnknown
	}
}
