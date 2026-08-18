package workflow

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestClassifyProviderFailure_TypedSignals covers Checkpoint 8H test
// requirement #1 (typed rate limit / typed signals take priority) and #5/#6
// (auth, binary missing behavior).
func TestClassifyProviderFailure_TypedSignals(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantClass    domain.WorkflowErrorClass
		wantCert     ClassificationCertainty
		wantEligible bool
	}{
		{
			name:         "typed binary missing is eligible",
			err:          fmt.Errorf("agent binary %q: %w", "codex", ports.ErrAgentBinaryNotFound),
			wantClass:    domain.WorkflowErrorBinaryMissing,
			wantCert:     CertaintyActual,
			wantEligible: true,
		},
		{
			name:         "typed chat auth required is not eligible",
			err:          fmt.Errorf("wrap: %w", ports.ErrChatAuthRequired),
			wantClass:    domain.WorkflowErrorAuth,
			wantCert:     CertaintyActual,
			wantEligible: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyProviderFailure(tc.err)
			if got.Class != tc.wantClass || got.Certainty != tc.wantCert || got.Eligible != tc.wantEligible {
				t.Fatalf("classifyProviderFailure(%v) = %+v, want class=%s cert=%s eligible=%v",
					tc.err, got, tc.wantClass, tc.wantCert, tc.wantEligible)
			}
		})
	}
}

// TestClassifyProviderFailure_InferredRateLimit covers conservative textual
// parsing: a known rate-limit phrase classifies as rate_limited/inferred and
// IS failover-eligible.
func TestClassifyProviderFailure_InferredRateLimit(t *testing.T) {
	got := classifyProviderFailure(errors.New("request failed: 429 Too Many Requests"))
	if got.Class != domain.WorkflowErrorRateLimited || got.Certainty != CertaintyInferred || !got.Eligible {
		t.Fatalf("got %+v, want rate_limited/inferred/eligible", got)
	}
}

// TestClassifyProviderFailure_UsageLimitIsCapacity is Checkpoint 8P-D.3's
// regression for the real phrasing a Codex usage-limit hit actually used
// (Checkpoint 8P-D.2's evidence): "You've hit your usage limit..." and the
// CLI's own `codex_error_info: "usage_limit_exceeded"` field, neither of
// which the pre-existing "quota exceeded" phrase matched.
func TestClassifyProviderFailure_UsageLimitIsCapacity(t *testing.T) {
	cases := []string{
		"You've hit your usage limit. Upgrade to Pro or try again at Aug 20th, 2026 1:30 PM.",
		`codex_error_info: "usage_limit_exceeded"`,
	}
	for _, msg := range cases {
		got := classifyProviderFailure(errors.New(msg))
		if got.Class != domain.WorkflowErrorCapacityExhausted || got.Certainty != CertaintyInferred || !got.Eligible {
			t.Fatalf("classifyProviderFailure(%q) = %+v, want capacity_exhausted/inferred/eligible", msg, got)
		}
	}
}

// TestClassifyProviderFailure_UnknownStaysUnknown covers test requirement #2:
// an untyped, unrecognized error must classify with CertaintyUnknown and
// Eligible=false — it must never be silently treated as a provider failure
// worth an automatic failover.
func TestClassifyProviderFailure_UnknownStaysUnknown(t *testing.T) {
	got := classifyProviderFailure(errors.New("some unrelated internal error"))
	if got.Certainty != CertaintyUnknown || got.Eligible {
		t.Fatalf("got %+v, want unknown certainty and not eligible", got)
	}
	if !got.Class.Valid() {
		t.Fatalf("classification produced an invalid error class: %q", got.Class)
	}
}

// TestClassifyProviderFailure_NilError covers the defensive nil path: the
// classifier must still return a valid, non-eligible classification rather
// than panicking.
func TestClassifyProviderFailure_NilError(t *testing.T) {
	got := classifyProviderFailure(nil)
	if got.Eligible || !got.Class.Valid() {
		t.Fatalf("got %+v, want a valid, non-eligible classification for nil", got)
	}
}

// Checkpoint 8P-C: (*Coordinator).workFallbackHarness replaced the pure,
// context-free workFallbackHarness/oppositeHarness pair with a policy- and
// store-backed walk of the workflow owner's WorkerPriority list, so it can
// no longer be unit-tested without a Coordinator+store fixture. See
// TestReportWorkStepProviderFailure_ScopedToUserProfile (execution_policy
// integration tests) for equivalent reactive-failover coverage.

func TestEffectiveMaxWorkProviderAttempts_DefaultsWhenUnset(t *testing.T) {
	if got := effectiveMaxWorkProviderAttempts(domain.WorkflowPolicy{Version: "v1"}); got != 3 {
		t.Fatalf("got %d, want default of 3 for a pre-8H policy snapshot", got)
	}
	if got := effectiveMaxWorkProviderAttempts(domain.WorkflowPolicy{MaxWorkProviderAttempts: 5}); got != 5 {
		t.Fatalf("got %d, want explicit policy value of 5", got)
	}
}
