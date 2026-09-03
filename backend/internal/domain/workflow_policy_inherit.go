package domain

import "fmt"

// InheritWorkflowPolicy produces the policy a child (master-task) run must
// execute under, given its parent objective's already-frozen policy and the
// child's own creation-time default policy.
//
// A child run is where ALL of an autonomous objective's real work happens: the
// worker, the review, the fix and the verify steps live on the child, not on
// the parent. So every knob that expresses "what this objective is allowed to
// do by itself" has to reach the child, or the parent's contract is decorative.
// Before this existed only Execution travelled, and a parent created with
// autonomy=auto_decide_low_risk / repair=automatic produced children frozen at
// the creation defaults ask_always / suggest — every worker question escalated
// to a person and no repair ever ran unattended, while every durable row looked
// healthy.
//
// The classification, field by field:
//
//	Version                          CHILD-LOCAL    snapshot shape, not semantics
//	MaxFixCycles                     INHERIT*       review<->fix budget
//	MaxWorkProviderAttempts          INHERIT*       dispatch/failover budget
//	MaxReviewProviderAttempts        INHERIT*       review dispatch budget
//	MaxAutoAnsweredQuestionsPerStep  INHERIT*       auto-answer loop safety
//	AllowSameProviderResolver        INHERIT        resolver independence
//	Routing                          INHERIT*       per-role harness preference
//	Wake                             INHERIT*       unattended retry schedule
//	Execution                        INHERIT        owner priorities + autonomous mode
//	Strategy                         RECOMPUTE      ChildExecutionStrategy decides it
//	Repair                           INHERIT*       may AO repair this unattended
//	Autonomy                         INHERIT*       may AO answer questions itself
//	Usage                            INHERIT*       the family's frozen ceiling
//
// (*) LEGACY FALLBACK: a field the parent never recorded is NOT copied. A
// pre-P1-B/P3-C/P3-E parent snapshot carries the zero value for Repair,
// Autonomy and Usage, and copying that zero over the child's own default would
// be inventing a decision nobody made. Keeping the child's default is both the
// safe answer and a no-op, because the Effective*Policy readers map the zero
// value to exactly the same conservative default (ask_always, suggest, no
// ceiling) on either side. Inheritance therefore never escalates autonomy: it
// can only carry forward a choice a person actually made on the parent.
//
// Strategy is the one field that is deliberately recomputed rather than
// inherited: a child is never `master` and never deeper than
// ExecutionStrategyMaxChildDepth, so copying the parent's selection would
// re-open the fan-out ChildExecutionStrategy exists to close.
//
// The result is a pure value. Callers persist it; nothing here reads live
// settings, so a Settings edit after the parent started can never reach a child
// through this path.
func InheritWorkflowPolicy(parent, child WorkflowPolicy) WorkflowPolicy {
	out := child

	// Execution: the whole frozen snapshot, including AutonomousMode and the
	// owner's priority lists. The caller stamps Provenance afterwards.
	out.Execution = parent.Execution

	// Bounded budgets. Only a recorded (>0) parent value travels; 0 means the
	// parent snapshot predates the field, and the child keeps its own default.
	if parent.MaxFixCycles > 0 {
		out.MaxFixCycles = parent.MaxFixCycles
	}
	if parent.MaxWorkProviderAttempts > 0 {
		out.MaxWorkProviderAttempts = parent.MaxWorkProviderAttempts
	}
	if parent.MaxReviewProviderAttempts > 0 {
		out.MaxReviewProviderAttempts = parent.MaxReviewProviderAttempts
	}
	if parent.MaxAutoAnsweredQuestionsPerStep > 0 {
		out.MaxAutoAnsweredQuestionsPerStep = parent.MaxAutoAnsweredQuestionsPerStep
	}

	// A bool has no "unrecorded" state distinct from false, and false is also
	// the default on both sides, so this is safe to carry unconditionally.
	out.AllowSameProviderResolver = parent.AllowSameProviderResolver

	if parent.Routing.Version != "" {
		out.Routing = parent.Routing
	}
	if parent.Wake.Version != "" {
		out.Wake = parent.Wake
	}
	if parent.Repair.Mode.Valid() {
		out.Repair = parent.Repair
	}
	if parent.Autonomy.Mode.Valid() {
		out.Autonomy = parent.Autonomy
	}
	if parent.Usage.Version != "" || parent.Usage.Configured() {
		out.Usage = parent.Usage
	}

	return out
}

// RequireInheritedWorkflowPolicy proves a child run is actually executing under
// its parent objective's frozen contract, and says precisely which field it
// disagrees on when it is not.
//
// It compares EFFECTIVE values rather than raw ones, for two reasons. A legacy
// parent that recorded nothing and a child that recorded nothing agree — both
// read as the same conservative default — so this stays a no-op for every run
// that predates these fields. And a child whose stored value differs only in a
// way no reader can observe is not a real disagreement worth refusing a
// dispatch over.
//
// Execution provenance is checked by the caller, which knows whether the parent
// can prove its own freeze. This function checks the semantics.
func RequireInheritedWorkflowPolicy(parent, child WorkflowPolicy) error {
	if parent.Execution.AutonomousMode != child.Execution.AutonomousMode {
		return fmt.Errorf("autonomousMode: parent=%t child=%t",
			parent.Execution.AutonomousMode, child.Execution.AutonomousMode)
	}
	if got, want := child.EffectiveAutonomyPolicy().Mode, parent.EffectiveAutonomyPolicy().Mode; got != want {
		return fmt.Errorf("autonomy.mode: parent=%s child=%s", want, got)
	}
	parentRepair, childRepair := parent.EffectiveRepairPolicy(), child.EffectiveRepairPolicy()
	if parentRepair.Mode != childRepair.Mode {
		return fmt.Errorf("repair.mode: parent=%s child=%s", parentRepair.Mode, childRepair.Mode)
	}
	if parentRepair.MaxRepairCycles != childRepair.MaxRepairCycles {
		return fmt.Errorf("repair.maxRepairCycles: parent=%d child=%d",
			parentRepair.MaxRepairCycles, childRepair.MaxRepairCycles)
	}
	parentUsage, childUsage := parent.EffectiveUsageBudgetPolicy(), child.EffectiveUsageBudgetPolicy()
	if parentUsage.WorkflowTokenBudget != childUsage.WorkflowTokenBudget ||
		parentUsage.WorkflowCostBudgetUSD != childUsage.WorkflowCostBudgetUSD {
		return fmt.Errorf("usage ceiling: parent=(%d tokens, %.4f usd) child=(%d tokens, %.4f usd)",
			parentUsage.WorkflowTokenBudget, parentUsage.WorkflowCostBudgetUSD,
			childUsage.WorkflowTokenBudget, childUsage.WorkflowCostBudgetUSD)
	}
	if parent.MaxFixCycles > 0 && child.MaxFixCycles != parent.MaxFixCycles {
		return fmt.Errorf("maxFixCycles: parent=%d child=%d", parent.MaxFixCycles, child.MaxFixCycles)
	}
	if parent.MaxAutoAnsweredQuestionsPerStep > 0 &&
		child.MaxAutoAnsweredQuestionsPerStep != parent.MaxAutoAnsweredQuestionsPerStep {
		return fmt.Errorf("maxAutoAnsweredQuestionsPerStep: parent=%d child=%d",
			parent.MaxAutoAnsweredQuestionsPerStep, child.MaxAutoAnsweredQuestionsPerStep)
	}
	return nil
}
