package workflow

import (
	stdctx "context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// decisionResolverPreferredHarness is Checkpoint 8K-B's cross-provider
// preference: claude-code asks -> codex resolves, codex asks -> claude-code
// resolves. Checkpoint 8L extracts this exact table into execution_router.go's
// oppositeHarness so reviewer routing and the decision resolver share one
// mapping instead of two independently-hardcoded ones (checkpoint brief
// §11: "No dupliques provider-selection logic").
func decisionResolverPreferredHarness(asking domain.AgentHarness) (domain.AgentHarness, bool) {
	return oppositeHarness(asking)
}

// DecisionProviderSelection is the outcome of selectDecisionResolverProvider:
// either a usable harness, or a reason no harness is currently usable
// (surfaced as the read-time-derived "waiting_for_capacity" NextAction,
// never converted to human_required).
type DecisionProviderSelection struct {
	Harness   domain.AgentHarness
	Available bool
	// SameProvider is true when the selected harness is the SAME provider
	// that asked the question (only possible when AllowSameProviderResolver
	// is true and the preferred opposite provider is unavailable).
	SameProvider bool
}

// selectDecisionResolverProvider is Checkpoint 8K-B's pure provider-selection
// decision: prefer the opposite provider from whoever asked; if it is
// unavailable, fall back to the SAME provider only when
// WorkflowPolicy.AllowSameProviderResolver is true; otherwise report
// Available=false so the caller leaves the question at state=resolving and
// retries on the next reconcile pass rather than escalating to
// human_required. Availability is read through the exact same
// domain.AgentHealth.Available(now) fact Checkpoint 8H's work-dispatch
// failover already uses (see failover.go's selectFallbackForWork) — no
// second capacity query path.
func (c *Coordinator) selectDecisionResolverProvider(ctx stdctx.Context, asking domain.AgentHarness, policy domain.WorkflowPolicy, now time.Time) (DecisionProviderSelection, error) {
	preferred, ok := decisionResolverPreferredHarness(asking)
	if !ok {
		return DecisionProviderSelection{}, nil
	}
	health, err := c.agentHealth(ctx, preferred)
	if err != nil {
		return DecisionProviderSelection{}, err
	}
	if health.Available(now) {
		return DecisionProviderSelection{Harness: preferred, Available: true}, nil
	}
	if !policy.AllowSameProviderResolver {
		return DecisionProviderSelection{Harness: preferred, Available: false}, nil
	}
	// Same-provider fallback: the asking provider itself may resolve its own
	// question, opt-in only (see WorkflowPolicy.AllowSameProviderResolver's
	// doc comment for the self-review ambiguity risk this accepts).
	sameHealth, err := c.agentHealth(ctx, asking)
	if err != nil {
		return DecisionProviderSelection{}, err
	}
	if sameHealth.Available(now) {
		return DecisionProviderSelection{Harness: asking, Available: true, SameProvider: true}, nil
	}
	return DecisionProviderSelection{Harness: preferred, Available: false}, nil
}
