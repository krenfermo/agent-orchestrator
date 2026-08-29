package workflow

import (
	stdctx "context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// recovery_assessment.go — P1-B §A/§G: one deterministic answer to "what
// should I do about this run".
//
// Everything it reads is already durable and already assembled by GetRun: the
// run state, its steps and attempts, its plan record and tasks, its canonical
// attention reason, its frozen execution strategy and its frozen repair
// policy. Nothing here calls a model, and nothing here writes. That is the
// point: an operator's recovery choice must be reproducible, and an LLM must
// never be the thing that decides AO has authority to act.
//
// It deliberately does not introduce a second classification of stops. The
// canonical reason and its AttentionDisposition (attention.go) remain the only
// vocabulary; this file turns one into a recommendation and states what is
// blocking, what is reusable and what is repairable.

// RecoveryAssessmentVersion versions the rules below, so an assessment shown
// to a person stays explainable after they change.
const RecoveryAssessmentVersion = "v1"

// RecoveryAssessment is the complete answer for one run.
type RecoveryAssessment struct {
	RunID string
	// RecommendedAction is the ONE thing AO advises.
	RecommendedAction domain.RecoveryAction
	// ReasonCode is the canonical attention reason behind the recommendation,
	// or a recovery-specific code when the run is not stopped on one.
	ReasonCode string
	// Explanation is AO's own sentence. For a human-owned stop it is the
	// disposition's HumanAction verbatim, so the recovery panel and the Board
	// can never tell a person two different things.
	Explanation string
	// AutomaticAllowed reports whether AO may take RecommendedAction itself,
	// without asking. It is false for every action a person owns, and false
	// for a repair unless the run's frozen policy says automatic.
	AutomaticAllowed bool
	// PlanReusable is the durable plan's reuse classification.
	PlanReusable domain.PlanReusability
	// RepairAvailable reports whether a Repair Agent could be created right
	// now: the condition is repairable, the budget is not spent, and the policy
	// permits at least suggesting it.
	RepairAvailable bool
	// RepairEligibility is the full deterministic answer behind
	// RepairAvailable, including WHY not.
	RepairEligibility domain.RepairEligibility
	// BlockingCondition names, in AO's words, what actually stands between
	// this run and progress. Empty when nothing does.
	BlockingCondition string
	// Obligation is the durable obligation a resume would discharge. Empty
	// when there is none.
	Obligation ResumeObligation
	// Strategy is the run's frozen execution strategy, so a caller never has to
	// resolve it a second way.
	Strategy domain.ExecutionStrategy
	// TargetRunID is the run an operator should actually act on. It differs
	// from RunID exactly when this run's stop is a mirror of a child's.
	TargetRunID string
	// StepID / TaskID / AttemptID point at the durable rows the recommendation
	// is about, when there is exactly one.
	StepID    string
	TaskID    string
	AttemptID string
	// Version is RecoveryAssessmentVersion.
	Version string
}

// AssessRecovery answers "what should I do about this run" for any run, at any
// time. It writes nothing, so a poll, a page load and an operator's terminal
// can all ask freely.
func (c *Coordinator) AssessRecovery(ctx stdctx.Context, runID string) (RecoveryAssessment, error) {
	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		return RecoveryAssessment{}, err
	}
	strategy := c.strategyForRun(ctx, detail.Run)
	reuse := c.assessPlanReuse(ctx, detail, strategy)
	return c.assessRecoveryFromDetail(detail, strategy.Effective, reuse.Reusability, c.repairsSpentFor(ctx, runID)), nil
}

// assessRecoveryFromDetail is AssessRecovery's pure core, taking the durable
// facts as arguments so it can be exercised directly and so a caller that has
// already loaded them does not load them twice.
func (c *Coordinator) assessRecoveryFromDetail(
	d RunDetail, strategy domain.ExecutionStrategy, reuse domain.PlanReusability, repairsSpent int,
) RecoveryAssessment {
	life := DeriveLifecycle(LifecycleInput{Detail: d, Questions: d.Questions})
	policy := policyForRun(d.Run).EffectiveRepairPolicy()

	a := RecoveryAssessment{
		RunID:        d.Run.ID,
		ReasonCode:   life.AttentionReason,
		PlanReusable: reuse,
		Strategy:     strategy,
		TargetRunID:  d.Run.ID,
		Version:      RecoveryAssessmentVersion,
	}
	if life.AttentionWorkflowID != "" {
		// The objective's stop is its child's stop. An operator acting here
		// must be sent to the run that actually owns the problem.
		a.TargetRunID = life.AttentionWorkflowID
	}

	// 1. Terminal first. A finished run is not a recovery question, and saying
	//    anything else about it would invite a second execution of work that
	//    already ended.
	if d.Run.State.Terminal() {
		a.RecommendedAction = domain.RecoveryTerminal
		a.Explanation = fmt.Sprintf("This run is already %s. There is nothing to recover.", d.Run.State)
		if a.ReasonCode == "" {
			a.ReasonCode = string(d.Run.State)
		}
		a.RepairEligibility = domain.RepairIneligible
		return a
	}

	obligation := deriveResumeObligation(d)
	a.Obligation = obligation
	a.StepID, a.TaskID = obligation.StepID, obligation.TaskID

	reason, disp, classified := resolveAttentionReason(d)
	stopped := d.Run.State == domain.WorkflowRunNeedsAttention

	// 2. A stop AO cannot name is the fail-closed case, and it is the ONLY
	//    place `unrecoverable` comes from. It is not a verdict about the run:
	//    it is a statement that AO has no durable fact to reason from, which is
	//    exactly what must stop it recommending anything.
	if stopped && !classified {
		a.RecommendedAction = domain.RecoveryUnrecoverable
		a.ReasonCode = unclassifiedStop
		a.Explanation = "This run is stopped and AO has no durable record of why, so it will not recommend an action it cannot justify. Inspect the run's checkpoints and its session, then continue or cancel it."
		a.BlockingCondition = "no canonical stop reason is recorded for this run"
		a.RepairEligibility = domain.RepairUnknownCondition
		return a
	}

	if classified {
		a.ReasonCode = reason
		if disp.HumanAction != "" {
			a.Explanation = disp.HumanAction
		}
		a.RepairEligibility = repairEligibility(disp, policy, repairsSpent)
		a.RepairAvailable = a.RepairEligibility.Allowed() && policy.Mode != domain.RepairModeDisabled
	} else {
		a.RepairEligibility = domain.RepairIneligible
	}

	// 3. A stop AO is still working on is not the operator's problem. Saying
	//    "resume" here is truthful (a resume is a no-op that re-derives the
	//    same evidence) and it never asks anybody for anything.
	if disp.SelfRemediable {
		a.RecommendedAction = domain.RecoveryResume
		a.AutomaticAllowed = true
		if a.Explanation == "" {
			a.Explanation = "AO is still working on this by itself; a scheduled retry will pick it up."
		}
		return a
	}

	// 4. A repairable stop under an automatic policy is the one case AO may
	//    write code about without being asked. Under `suggest` the action is
	//    offered and nothing starts.
	if a.RepairAvailable {
		a.RecommendedAction = domain.RecoveryRepair
		a.AutomaticAllowed = policy.Mode == domain.RepairModeAutomatic
		a.BlockingCondition = disp.HumanAction
		return a
	}

	// 5. A planned run whose plan cannot be trusted has a plan answer before it
	//    has an execution answer: executing a stale plan is the silent-wrong
	//    outcome §C exists to prevent.
	if strategy.Planned() {
		switch reuse {
		case domain.PlanReuseStaleRevalidatable:
			a.RecommendedAction = domain.RecoveryRegeneratePlan
			a.BlockingCondition = "the plan was generated against a project context that has since changed"
			if a.Explanation == "" {
				a.Explanation = "This objective has a plan, but the project context it was generated from has changed. Revalidate it, or regenerate the plan, before any of it executes."
			}
			return a
		case domain.PlanReuseExact:
			if stopped || obligation.Kind == ResumeObligationPlanApproval || obligation.Kind == ResumeObligationPlanDispatch {
				if !disp.Nonrecoverable && (obligation.Kind == ResumeObligationPlanApproval || obligation.Kind == ResumeObligationPlanDispatch) {
					a.RecommendedAction = domain.RecoveryReusePlan
					a.AutomaticAllowed = obligation.Kind == ResumeObligationPlanDispatch
					if a.Explanation == "" {
						a.Explanation = "A validated plan for this objective already exists and still matches the project. Reuse it instead of planning again."
					}
					return a
				}
			}
		}
	}

	// 6. A stop whose remedy is not a continue is not a continue. Offering one
	//    would be offering a button that provably does nothing.
	if disp.Nonrecoverable {
		a.RecommendedAction = domain.RecoveryRestartRequired
		if disp.Recovery.Valid() {
			a.RecommendedAction = disp.Recovery
		}
		a.BlockingCondition = disp.HumanAction
		return a
	}

	// 7. Anything else AO has named and cannot act on itself is the operator's,
	//    at whatever specificity the disposition recorded.
	if stopped || (classified && disp.HumanAction != "") {
		a.RecommendedAction = domain.RecoveryOperatorAction
		if disp.Recovery.Valid() {
			a.RecommendedAction = disp.Recovery
		}
		a.BlockingCondition = disp.HumanAction
		return a
	}

	// 8. Not stopped. Either there is a durable obligation to discharge, or the
	//    run is simply running and there is nothing to recover.
	if obligation.Kind != ResumeObligationNone {
		a.RecommendedAction = domain.RecoveryResume
		a.AutomaticAllowed = true
		a.Explanation = obligation.Explanation
		return a
	}
	a.RecommendedAction = domain.RecoveryResume
	a.AutomaticAllowed = life.CanContinue
	a.Explanation = "This run is progressing on its own. Resuming re-checks its durable evidence and does nothing unless something is genuinely outstanding."
	return a
}

// repairEligibility is the deterministic repair decision, in one place.
//
// Order matters and is the safety model: an unrepairable CONDITION is refused
// before any policy or budget is consulted, so no policy setting anywhere can
// make an unprovable-provenance stop repairable.
func repairEligibility(disp AttentionDisposition, policy domain.RepairPolicySnapshot, spent int) domain.RepairEligibility {
	if !disp.Repairable {
		return domain.RepairIneligible
	}
	if policy.Mode == domain.RepairModeDisabled {
		return domain.RepairPolicyDisabled
	}
	if spent >= policy.MaxRepairCycles {
		return domain.RepairBudgetExhausted
	}
	return domain.RepairEligible
}

// recoveryActionFor is the disposition-only view of the recommendation, used
// where a full assessment is not available (the Board's own projection). It
// mirrors steps 3/6/7 above and nothing else.
func recoveryActionFor(disp AttentionDisposition) domain.RecoveryAction {
	switch {
	case disp.Recovery.Valid():
		return disp.Recovery
	case disp.SelfRemediable:
		return domain.RecoveryResume
	case disp.Nonrecoverable:
		return domain.RecoveryRestartRequired
	case disp.HumanAction != "":
		return domain.RecoveryOperatorAction
	default:
		return domain.RecoveryUnrecoverable
	}
}
