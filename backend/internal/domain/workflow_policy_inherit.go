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
//	SessionCompactionEnabled         INHERIT*       may a fix cycle compact first
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
// SessionCompactionEnabled obeys that same rule, and it is the reason
// CompactionProvenance exists at all. The field is a bare bool, so unlike
// Repair/Autonomy/Usage it has no "unrecorded" value distinct from its
// default -- the test AllowSameProviderResolver gets away with (copy it
// unconditionally, false is the default on both sides) would here mean a
// parent that merely predates compaction could hand a child a decision nobody
// took, and a `true` that reached a snapshot by any route other than an opt-in
// would propagate to every child of that objective. So the gate is the
// PROVENANCE, not the value: the parent's flag travels only when the parent
// carries a recorded choice, and the child's record says `inherited` naming
// where it came from. An explicit false inherits exactly as an explicit true
// does -- a run somebody told not to compact must pass that refusal down
// rather than letting a child fall back to a default that only happens to
// agree today.
//
// The child's own value never wins, for the same reason no other field's
// does: a child run is where the parent objective's work executes, and
// RequireInheritedWorkflowPolicy refuses a dispatch whose child disagrees with
// the contract it is executing under. Nothing in AO creates a child through
// the create-run API, so there is no path by which a child could hold a
// competing explicit request in the first place.
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

	// Session compaction is also a bool, and is NOT safe to carry
	// unconditionally for exactly that reason -- see the note on the legacy
	// fallback above. The recorded choice is what travels; an unrecorded
	// parent leaves the child at its own default, whatever the parent's raw
	// flag happens to say. The caller stamps ParentRunID, the same way it
	// stamps Execution.Provenance.
	if parent.CompactionProvenance.Recorded() {
		out.SessionCompactionEnabled = parent.SessionCompactionEnabled
		out.CompactionProvenance = parent.CompactionProvenance
		out.CompactionProvenance.Source = SessionCompactionInherited
		out.CompactionProvenance.ParentRunID = ""
	}

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
	// Compaction is compared only when the parent recorded a choice, mirroring
	// what InheritWorkflowPolicy is willing to copy. A legacy parent whose raw
	// flag is false and a child at the same default agree trivially and are
	// not compared at all, so no pre-existing run is refused by this -- and a
	// parent that DID opt in must be able to prove its children are running
	// under that opt-in, because the child is where every fix cycle happens.
	if enabled, recorded := parent.SessionCompactionRequested(); recorded {
		childEnabled, childRecorded := child.SessionCompactionRequested()
		if !childRecorded || childEnabled != enabled {
			return fmt.Errorf("sessionCompactionEnabled: parent=%t child=%t (child recorded=%t)",
				enabled, child.SessionCompactionEnabled, childRecorded)
		}
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
