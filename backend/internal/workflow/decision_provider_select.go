package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// DecisionProviderSelection is the outcome of selectDecisionResolverProvider:
// either a usable harness, or a reason no harness is currently usable
// (surfaced as the read-time-derived "waiting_for_capacity" NextAction,
// never converted to human_required).
type DecisionProviderSelection struct {
	Harness   domain.AgentHarness
	Available bool
	// SameProvider is true when the selected harness is the SAME provider
	// that asked the question (only possible when the owner's
	// ReviewIndependence policy allows it and no independent provider is
	// eligible).
	SameProvider bool
}

// selectDecisionResolverProvider is Checkpoint 8P-C's policy-driven
// provider-selection decision for the 8K-B decision resolver: it now shares
// the exact same RouteExecution/eligibility path reviewer routing uses
// (routeReviewerDispatch) rather than keeping a second, independently
// hardcoded opposite-provider table -- no parallel router. Available=false
// (never an error) means the caller leaves the question at state=resolving
// and retries on the next reconcile pass rather than escalating to
// human_required.
func (c *Coordinator) selectDecisionResolverProvider(ctx stdctx.Context, run domain.WorkflowRun, asking domain.AgentHarness) DecisionProviderSelection {
	owner := c.runOwner(ctx, run.ID)
	snapshot := policyForRun(run).EffectiveExecutionPolicy()
	policy, eligible, ineligible, capacity := c.routingInputsForRole(ctx, owner, domain.WorkflowRoleDecisionResolver, snapshot)

	decision := RouteExecution(RoutingRequest{
		Role:                       domain.WorkflowRoleDecisionResolver,
		CurrentImplementerProvider: domain.ProviderForHarness(asking),
		Policy:                     policy,
		EligibleProfiles:           eligible,
		IneligibleReasons:          ineligible,
		Capacity:                   capacity,
	})
	stepID := (*string)(nil)
	_ = c.persistRoutingDecision(ctx, run, stepID, decision)
	if decision.Waiting || decision.SelectedHarness == "" {
		return DecisionProviderSelection{}
	}
	sameProvider := domain.ProviderForHarness(decision.SelectedHarness) == domain.ProviderForHarness(asking)
	return DecisionProviderSelection{Harness: decision.SelectedHarness, Available: true, SameProvider: sameProvider}
}
