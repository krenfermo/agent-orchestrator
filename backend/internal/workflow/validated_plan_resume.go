package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// resumeValidatedPlan is CP11/CP12's deterministic reconciliation: a plan
// that finished validation but whose approval never landed.
//
// The window is real on two paths. finalizeGeneratedPlan writes `validated`
// (P12) and only then calls ApprovePlan (P13/P19); a crash in between leaves
// an objective that should have auto-approved sitting at "plan ready" with no
// resolver anywhere in the system -- boot recovery's plan switch had no
// `validated` case, getMasterRun reconciles only from `approved`, and
// ContinueRun delegated to GetRun, which does the same nothing. CP12 is the
// same stall with `approval_mode` already durably `auto`, i.e. with a record
// on disk saying it should have been approved automatically.
//
// The decision is taken from durable facts only, and it is the SAME decision
// finalizeGeneratedPlan takes: approve when the plan's own approval mode is
// `auto`, or when the run's frozen execution policy says the objective is
// autonomous. Neither is inferred and neither is invented -- and note that
// the policy read is only trustworthy because ensureFrozenExecutionPolicy has
// already refused to let an unprovable snapshot get this far (CP3).
//
// A manual objective is deliberately left exactly as it is: `validated` with
// no approval IS the approval prompt for a person, and auto-approving it here
// would answer a question that was asked of them.
//
// Idempotent by construction: ApprovePlan CASes `validated -> approved` and
// returns the current detail when the plan is already approved, so N restarts
// converge to one approval.
func (c *Coordinator) resumeValidatedPlan(ctx stdctx.Context, run domain.WorkflowRun, plan domain.WorkflowPlanRecord) error {
	if plan.Status != domain.WorkflowPlanValidated {
		return nil
	}
	if plan.ApprovalMode != domain.WorkflowPlanApprovalAuto && !policyForRun(run).Execution.AutonomousMode {
		return nil
	}
	// Keep the record honest the same way finalizeGeneratedPlan does: when it
	// is the frozen policy (not the client) that decided this, say so, so
	// approval_mode="auto" remains an inspectable approval source rather than
	// an approval that silently contradicts a "manual" record.
	if plan.ApprovalMode != domain.WorkflowPlanApprovalAuto {
		_, _ = c.planStore.SetWorkflowPlanApprovalMode(ctx, run.ID, domain.WorkflowPlanApprovalAuto, c.clock())
	}
	if _, err := c.ApprovePlan(ctx, run.ID); err != nil {
		return err
	}
	return nil
}
