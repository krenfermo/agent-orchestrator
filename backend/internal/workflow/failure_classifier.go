package workflow

import (
	"errors"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ClassificationCertainty records how confident a ProviderFailureClassification
// is, per Checkpoint 8H §3's requirement that every classification be able to
// record actual/inferred/unknown rather than silently treating a guess as a
// fact.
type ClassificationCertainty string

const (
	// CertaintyActual means the class came from a typed signal (a sentinel
	// error, an adapter capability, a known exit class) — not text parsing.
	CertaintyActual ClassificationCertainty = "actual"
	// CertaintyInferred means the class came from conservative, last-resort
	// parsing of an untyped error's text. Eligibility still applies, but
	// callers that need to distinguish provenance (telemetry, UI) can.
	CertaintyInferred ClassificationCertainty = "inferred"
	// CertaintyUnknown means no signal, typed or textual, distinguished this
	// failure from a generic one. The class still gets a value (never empty)
	// so downstream bookkeeping always has one, but eligibility is false.
	CertaintyUnknown ClassificationCertainty = "unknown"
)

// ProviderFailureClassification is the provider-neutral result of classifying
// one dispatch failure (Checkpoint 8H §3). Class is always a valid
// domain.WorkflowErrorClass so it can be recorded on a workflow_attempts row
// unconditionally.
type ProviderFailureClassification struct {
	Class     domain.WorkflowErrorClass
	Certainty ClassificationCertainty
	// Eligible reports whether this class is one automatic failover may act
	// on (Checkpoint 8H §8): rate_limited, capacity_exhausted, or an
	// agent_start_failed/transient signal after the one short same-provider
	// retry §15 allows. Implementation/test/review failures are never
	// eligible — those are normal work, not provider failure.
	Eligible bool
	// ResetAt is the provider's OWN reported reset time, when it reported one.
	// It is populated only from a typed signal (an error implementing
	// ProviderResetAtError), never from parsed prose, and it is what makes a
	// cooldown honour a real reset instead of AO's generic backoff. nil means
	// "no reset time is known", which is a fact in its own right and must not
	// be replaced by an invented timestamp.
	ResetAt *time.Time
}

// ProviderResetAtError is the typed signal an adapter uses to report the exact
// moment a rate limit or quota window clears. AO deliberately accepts a reset
// time from nowhere else: a timestamp scraped out of an error string is a guess,
// and scheduling a workflow's next attempt against a guess is worse than
// scheduling it against an honest bounded backoff.
type ProviderResetAtError interface {
	error
	// ProviderResetAt returns the reported reset instant. A zero time means the
	// provider named the condition but not when it clears.
	ProviderResetAt() time.Time
}

// providerResetAt extracts a typed provider reset time from err, if any error
// in its chain reports one.
func providerResetAt(err error) *time.Time {
	var typed ProviderResetAtError
	if !errors.As(err, &typed) {
		return nil
	}
	at := typed.ProviderResetAt()
	if at.IsZero() {
		return nil
	}
	return &at
}

// classifyProviderFailure is the single provider-neutral failure classifier
// Checkpoint 8H's dispatch call sites use. Priority order, per §3 of the
// checkpoint: (1) typed sentinel errors; (2) provider-reported capability
// signals (passed by the caller as capacityExhausted/rateLimited, since only
// the caller — e.g. an adapter-level capability check — can know these
// reliably); (3) known conservative substring parsing, marked inferred; (4)
// unknown, not eligible. It deliberately does NOT pattern-match arbitrary
// strings against every class name — only a short, explicit, reviewed list of
// phrases known to be provider rate-limit/capacity language.
func classifyProviderFailure(err error) ProviderFailureClassification {
	cls := classifyProviderFailureClass(err)
	cls.ResetAt = providerResetAt(err)
	return cls
}

func classifyProviderFailureClass(err error) ProviderFailureClassification {
	if err == nil {
		return ProviderFailureClassification{Class: domain.WorkflowErrorAmbiguousWorkerState, Certainty: CertaintyUnknown}
	}

	switch {
	case errors.Is(err, ports.ErrAgentBinaryNotFound):
		// §14: "Si Codex no existe pero Claude sí: puede permitirse fallback
		// si la policy lo permite" — binary_missing IS a failover candidate.
		// Whether an alternate harness is actually installed/healthy is the
		// budget/health layer's job, not the classifier's; if both harnesses
		// are missing, that layer surfaces needs_attention instead.
		return ProviderFailureClassification{Class: domain.WorkflowErrorBinaryMissing, Certainty: CertaintyActual, Eligible: true}
	case errors.Is(err, ports.ErrChatAuthRequired):
		// §14: auth failures must not auto-retry and AO must never touch
		// authentication state — conservatively not failover-eligible either
		// (a human needs to resolve the credential), always needs_attention.
		return ProviderFailureClassification{Class: domain.WorkflowErrorAuth, Certainty: CertaintyActual, Eligible: false}
	case errors.Is(err, ports.ErrProviderProfileRequired):
		// Checkpoint 8P-B.1: the workflow owner has no connected provider
		// profile for this harness. Same treatment as ErrChatAuthRequired --
		// a configuration/security gap a human must resolve (connect the
		// provider), never auto-retried, never failover-eligible (the
		// fallback harness is no more likely to have a profile).
		return ProviderFailureClassification{Class: domain.WorkflowErrorAuth, Certainty: CertaintyActual, Eligible: false}
	}

	text := strings.ToLower(err.Error())
	switch {
	case containsAny(text, "rate limit", "rate-limit", "ratelimited", "429", "too many requests"):
		return ProviderFailureClassification{Class: domain.WorkflowErrorRateLimited, Certainty: CertaintyInferred, Eligible: true}
	case containsAny(text, "capacity", "overloaded", "no capacity", "quota exceeded", "usage limit", "usage_limit_exceeded"):
		return ProviderFailureClassification{Class: domain.WorkflowErrorCapacityExhausted, Certainty: CertaintyInferred, Eligible: true}
	case containsAny(text, "unauthorized", "not logged in", "logged out", "authentication", "auth failed", "401", "403"):
		return ProviderFailureClassification{Class: domain.WorkflowErrorAuth, Certainty: CertaintyInferred, Eligible: false}
	case containsAny(text, "binary not found", "executable file not found", "no such file or directory"):
		return ProviderFailureClassification{Class: domain.WorkflowErrorBinaryMissing, Certainty: CertaintyInferred, Eligible: false}
	}

	// No typed or textual signal distinguished this failure. Conservative
	// default: agent_start_failed is still literally true here (dispatch
	// failed to start the agent) and is NOT auto-failover-eligible — an
	// unclassified failure must not silently trigger a provider switch.
	return ProviderFailureClassification{Class: domain.WorkflowErrorAgentStartFailed, Certainty: CertaintyUnknown, Eligible: false}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
