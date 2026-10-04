package usage

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// budget.go — where a token/cost ceiling turns into a decision.
//
// TWO THRESHOLDS, TWO MEANINGS. A warning is advice: it changes what the run
// detail and the advisor say and nothing else. A hard limit is a gate, and it
// is only ever consulted at a SAFE BOUNDARY — before AO starts a new dispatch.
// Nothing in P3-E kills a provider mid-response for crossing a line: a run
// interrupted between "the model is generating" and "AO recorded an attempt"
// is exactly the ambiguous lifecycle the workflow package spends thousands of
// lines avoiding, and a budget is not worth reintroducing it for.
//
// AN UNSET BUDGET IS NOT A BUDGET OF ZERO. Every ceiling defaults to 0 meaning
// "no limit", and BudgetUnset is a distinct state from BudgetOK, so a UI can
// tell "nobody set one" apart from "set, and comfortably under".
//
// A COST BUDGET IS ONLY ENFORCEABLE WHILE COST IS KNOWN. If a model in play is
// not covered by the rate card, the calculated cost is a lower bound, and a
// lower bound must not be allowed to trip a hard stop it might not really have
// reached. So an unpriced model leaves the COST ceiling unenforced (and says
// so in Reason); the TOKEN ceiling, which needs no rate card, still applies.

// EvaluateWorkflowBudget measures a run (or a parent's whole family, per
// scope) against its frozen ceiling.
func EvaluateWorkflowBudget(policy domain.UsageBudgetPolicy, tokens domain.UsageTokenTotals, cost domain.UsageCost, scope string) domain.UsageBudgetStatus {
	status := domain.UsageBudgetStatus{
		Policy: policy, State: domain.BudgetUnset, Scope: scope,
		TokensUsed: tokens.Total(), CostUsed: cost,
	}
	if scope == "" {
		status.Scope = "run"
	}
	if !policy.Configured() || (policy.WorkflowTokenBudget <= 0 && policy.WorkflowCostBudgetUSD <= 0) {
		return status
	}
	return applyThresholds(status, policy,
		policy.WorkflowTokenBudget, policy.WorkflowCostBudgetUSD,
		"workflow_token_budget", "workflow_cost_budget")
}

// EvaluateProjectDailyBudget measures a project period against the daily
// ceiling. It only means anything for the "today" period: a 30-day rollup
// compared against a per-day limit would be arithmetic nonsense, so any other
// period reports BudgetUnset rather than a misleading percentage.
func EvaluateProjectDailyBudget(policy domain.UsageBudgetPolicy, tokens domain.UsageTokenTotals, cost domain.UsageCost, period domain.UsagePeriod) domain.UsageBudgetStatus {
	status := domain.UsageBudgetStatus{
		Policy: policy, State: domain.BudgetUnset, Scope: "project_day",
		TokensUsed: tokens.Total(), CostUsed: cost,
	}
	if period != domain.UsagePeriodToday {
		return status
	}
	if policy.ProjectDailyTokenBudget <= 0 && policy.ProjectDailyCostBudgetUSD <= 0 {
		return status
	}
	return applyThresholds(status, policy,
		policy.ProjectDailyTokenBudget, policy.ProjectDailyCostBudgetUSD,
		"project_daily_token_budget", "project_daily_cost_budget")
}

func applyThresholds(
	status domain.UsageBudgetStatus,
	policy domain.UsageBudgetPolicy,
	tokenLimit int64,
	costLimit float64,
	tokenReason, costReason string,
) domain.UsageBudgetStatus {
	warn := float64(policy.EffectiveWarnPercent())
	status.State = domain.BudgetOK

	if tokenLimit > 0 {
		pct := float64(status.TokensUsed) / float64(tokenLimit) * 100
		status.TokenPercent = &pct
		switch {
		case pct >= 100:
			status.State, status.Reason = domain.BudgetExhausted, tokenReason+"_exhausted"
		case pct >= warn:
			status.State, status.Reason = domain.BudgetWarning, tokenReason+"_warning"
		}
	}

	if costLimit > 0 {
		switch {
		case !status.CostUsed.Known:
			// No cost is known at all: the ceiling cannot be judged, so it is
			// not judged. Saying "0% of budget" here would be a claim that the
			// run has spent nothing.
			if status.Reason == "" {
				status.Reason = costReason + "_unenforceable_unknown_cost"
			}
		case len(status.CostUsed.UnpricedModels) > 0:
			// A partial cost is a LOWER bound. It may warn — a lower bound
			// already past the threshold is certainly past it — but it may
			// never exhaust, because the true figure is unknown and a hard
			// stop on a guess is the thing P3-E forbids.
			pct := status.CostUsed.Amount / costLimit * 100
			status.CostPercent = &pct
			if pct >= warn && status.State == domain.BudgetOK {
				status.State, status.Reason = domain.BudgetWarning, costReason+"_warning_partial"
			}
		default:
			pct := status.CostUsed.Amount / costLimit * 100
			status.CostPercent = &pct
			switch {
			case pct >= 100:
				status.State, status.Reason = domain.BudgetExhausted, costReason+"_exhausted"
			case pct >= warn && status.State == domain.BudgetOK:
				status.State, status.Reason = domain.BudgetWarning, costReason+"_warning"
			}
		}
	}
	return status
}
