package workflow

import (
	stdctx "context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

// usage_budget.go — where a token/cost ceiling actually stops something.
//
// SAFE BOUNDARIES ONLY. The gate is consulted before AO STARTS new work: a
// worker launch, a repair delivery, a child task. It is never consulted while a
// provider is mid-response, and there is no path here that terminates a running
// attempt. That is a deliberate refusal of P3-E §15's own warning: killing a
// provider mid-answer leaves an attempt whose mutation state nobody can prove,
// which is the ambiguous lifecycle the rest of this package exists to prevent.
// A budget is not worth reintroducing it for. So an over-budget run finishes
// what it started and starts nothing more.
//
// THE PARENT OWNS THE FAMILY'S SPEND. A budget measured per child would let ten
// tasks at 100k each run under a 200k parent — §16's exact example. The gate
// therefore measures the run's whole family (itself plus every child) whenever
// the frozen policy says children share the parent's ceiling, which is the
// default.
//
// A BUDGET AO CANNOT MEASURE DOES NOT BLOCK. No ledger store, no policy, or a
// cost ceiling whose models have no rate: the gate reports "not blocking" and
// says why. Refusing to dispatch on the strength of a number AO does not have
// would stop real work for a fiction.

// usageBudgetStore is the narrow read the gate needs, obtained by type
// assertion on the coordinator's Store exactly as usageAttributionStore is.
type usageBudgetStore interface {
	SumRunFamilyUsage(ctx stdctx.Context, runID string) ([]domain.ModelUsageLine, error)
}

// UsagePricer prices a model's tokens. *pricing.Table satisfies it; the
// coordinator takes the interface so internal/workflow does not depend on the
// rate card's implementation.
type UsagePricer interface {
	Cost(modelID string, tokens domain.UsageTokenTotals) domain.UsageCost
}

// usageBudgetStatus measures a run against its own frozen ceiling.
//
// It reads the budget from the run's POLICY SNAPSHOT, never from Settings: a
// ceiling raised or lowered after the run started must not change what this
// run was allowed to do, and a restart must reach the same verdict.
func (c *Coordinator) usageBudgetStatus(ctx stdctx.Context, run domain.WorkflowRun) domain.UsageBudgetStatus {
	policy := policyForRun(run).EffectiveUsageBudgetPolicy()
	status := domain.UsageBudgetStatus{Policy: policy, State: domain.BudgetUnset, Scope: "run"}
	if !policy.Configured() {
		return status
	}
	if c.usageBudgets == nil {
		status.Reason = "usage_budget_unmeasurable_no_ledger"
		return status
	}
	// A parent's family is the measured scope whenever children share its
	// ceiling. SumRunFamilyUsage folds parent and children in ONE query, so a
	// child that finished a moment ago cannot be missing from the parent's own
	// check.
	//
	// F1: children now inherit the parent's frozen ceiling, which is what lets
	// a long-running child be stopped by the family budget between the parent's
	// own task boundaries. That only measures the right thing if the family is
	// rooted at the PARENT: summing a child's own subtree against the parent's
	// ceiling would hand every child the whole budget again, which is the exact
	// failure §16 names and which ParentScoped exists to prevent.
	measured := run.ID
	scope := "run"
	if policy.ParentScoped() {
		scope = "family"
		if run.ParentWorkflowID != nil && *run.ParentWorkflowID != "" {
			measured = *run.ParentWorkflowID
		}
	}
	lines, err := c.usageBudgets.SumRunFamilyUsage(ctx, measured)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: usage budget not measured", "run", run.ID, "err", err)
		}
		status.Reason = "usage_budget_unmeasurable_read_failed"
		return status
	}
	var tokens domain.UsageTokenTotals
	var cost domain.UsageCost
	for _, line := range lines {
		tokens = tokens.Add(line.Tokens)
		cost = cost.Add(c.priceUsage(line.ModelID, line.Tokens))
	}
	return usagesvc.EvaluateWorkflowBudget(policy, tokens, cost, scope)
}

func (c *Coordinator) priceUsage(modelID string, tokens domain.UsageTokenTotals) domain.UsageCost {
	if c == nil || c.usagePricer == nil {
		return domain.UsageCost{Basis: domain.CostUnknown, UnpricedModels: []string{modelID}}
	}
	return c.usagePricer.Cost(modelID, tokens)
}

// usageBudgetBlocks reports whether a NEW dispatch must be refused, and parks
// the run for a human when it must.
//
// boundary names what was about to start ("worker", "repair", "child task"), so
// the stop the user reads says which decision the budget prevented rather than
// only that a number was crossed.
func (c *Coordinator) usageBudgetBlocks(ctx stdctx.Context, run domain.WorkflowRun, boundary string) bool {
	status := c.usageBudgetStatus(ctx, run)
	if !status.Blocking() {
		return false
	}
	c.parkForUsageBudget(ctx, run, boundary, status)
	return true
}

// parkForUsageBudget stops the run at a boundary and records why, in the same
// shape MarkCapacityRetryExhausted uses: a durable checkpoint carrying the
// next action, plus a state transition a human resolves.
//
// Nothing here cancels a live attempt. The run stops STARTING work; whatever is
// already running finishes and reports normally.
func (c *Coordinator) parkForUsageBudget(ctx stdctx.Context, run domain.WorkflowRun, boundary string, status domain.UsageBudgetStatus) {
	now := c.clock()
	next := fmt.Sprintf(
		"usage_budget_exhausted: %s — %s was not started because this %s has spent %d tokens against its budget. A human must raise the budget, or cancel the run. Work already running was left to finish.",
		status.Reason, boundary, status.Scope, status.TokensUsed,
	)
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		ProjectID:      run.ProjectID,
		NextAction:     next,
		DurablePhase:   usageBudgetExhaustedPhase,
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: usage budget stop not recorded", "run", run.ID, "err", err)
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil && c.log != nil {
			c.log.Warn("workflow: usage budget stop state transition failed", "run", run.ID, "err", err)
		}
	}
}

// usageBudgetExhaustedPhase is the durable phase of a budget stop.
const usageBudgetExhaustedPhase = "usage_budget_exhausted"
