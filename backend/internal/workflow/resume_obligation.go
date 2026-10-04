package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// resume_obligation.go — P1-B §B: resume becomes obligation-driven.
//
// "Continue" used to be a verb with no stated object. It did whatever the run
// happened to need, which was correct but unexplainable: a person pressing it
// could not say beforehand what it would do, and neither could a test.
//
// This file names the object. For every recoverable lifecycle phase it answers
// "what durable obligation remains?", and Resume then performs only that.
//
// It is deliberately a READ over the same durable facts the existing resume
// path already gates itself on -- it is not a second dispatcher. Every
// idempotency guarantee §B demands (no duplicate worker launch, no duplicate
// fix prompt, no duplicate reviewer, no duplicate child, no double-spent retry
// budget, no accidental second plan) is enforced inside those existing guards,
// each of which re-derives its own evidence at call time. Re-implementing them
// here would create exactly the second source of truth that produces the
// duplicates. What this adds is that the obligation is now stated, so a caller
// knows what was owed and what was discharged.

// ResumeObligationKind is the closed vocabulary of durable obligations.
type ResumeObligationKind string

const (
	// ResumeObligationNone means nothing is outstanding.
	ResumeObligationNone ResumeObligationKind = "none"
	// ResumeObligationPlanGeneration is an objective whose planner has not run.
	ResumeObligationPlanGeneration ResumeObligationKind = "plan_generation"
	// ResumeObligationPlanApproval is a validated plan awaiting a decision.
	ResumeObligationPlanApproval ResumeObligationKind = "plan_approval"
	// ResumeObligationPlanDispatch is an approved plan with tasks that can be
	// dispatched or advanced.
	ResumeObligationPlanDispatch ResumeObligationKind = "plan_dispatch"
	// ResumeObligationWorkDispatch is a work step that has never produced a
	// confirmed worker.
	ResumeObligationWorkDispatch ResumeObligationKind = "work_dispatch"
	// ResumeObligationWorkObservation is a running worker: the obligation is to
	// observe it, never to launch a second one.
	ResumeObligationWorkObservation ResumeObligationKind = "work_observation"
	// ResumeObligationReviewDispatch is completed work whose review has not
	// started.
	ResumeObligationReviewDispatch ResumeObligationKind = "review_dispatch"
	// ResumeObligationReviewObservation is a review in flight.
	ResumeObligationReviewObservation ResumeObligationKind = "review_observation"
	// ResumeObligationFixDelivery is a review that requested changes whose fix
	// cycle has not been delivered.
	ResumeObligationFixDelivery ResumeObligationKind = "fix_delivery"
	// ResumeObligationFixObservation is a fix cycle in flight.
	ResumeObligationFixObservation ResumeObligationKind = "fix_observation"
	// ResumeObligationVerify is an approved review whose verification has not
	// concluded.
	ResumeObligationVerify ResumeObligationKind = "verify"
	// ResumeObligationConvergence is a parent objective waiting for its
	// children to finish and integrate.
	ResumeObligationConvergence ResumeObligationKind = "convergence"
	// ResumeObligationTerminal means the run has ended; a resume is inert.
	ResumeObligationTerminal ResumeObligationKind = "terminal"
)

// ResumeObligation is one durable obligation, with the rows it concerns.
type ResumeObligation struct {
	Kind        ResumeObligationKind
	StepID      string
	TaskID      string
	ChildRunID  string
	Explanation string
	// Automatic reports whether re-entering the resume path discharges this
	// obligation with no person involved. A plan awaiting MANUAL approval is
	// the obvious false: the obligation is real and it is not AO's.
	Automatic bool
}

// deriveResumeObligation is the pure classifier. Order follows the lifecycle,
// because an earlier unmet obligation always outranks a later one -- a run
// with an undispatched worker has nothing to say about its review.
func deriveResumeObligation(d RunDetail) ResumeObligation {
	if d.Run.State.Terminal() {
		return ResumeObligation{Kind: ResumeObligationTerminal,
			Explanation: "This run has ended; resuming it does nothing."}
	}

	// A planned objective's obligations live on its plan and tasks, never on
	// work/review steps it does not have.
	if d.Plan != nil {
		return planResumeObligation(d)
	}

	steps := map[domain.WorkflowStepKind]domain.WorkflowStep{}
	for _, s := range d.Steps {
		steps[s.Step.Kind] = s.Step
	}
	work, hasWork := steps[domain.WorkflowStepWork]
	review, hasReview := steps[domain.WorkflowStepReview]
	fix, hasFix := steps[domain.WorkflowStepFix]
	verify, hasVerify := steps[domain.WorkflowStepVerify]

	switch {
	case hasWork && (work.State == domain.WorkflowStepPending || work.State == domain.WorkflowStepReady ||
		work.State == domain.WorkflowStepWaiting || work.State == domain.WorkflowStepFailed):
		return ResumeObligation{Kind: ResumeObligationWorkDispatch, StepID: work.ID, Automatic: true,
			Explanation: "The work step has no confirmed worker. Resuming dispatches exactly one, and only if AO can prove none is already running."}
	case hasWork && work.State == domain.WorkflowStepRunning:
		return ResumeObligation{Kind: ResumeObligationWorkObservation, StepID: work.ID, Automatic: true,
			Explanation: "A worker is running. Resuming observes it; it never launches a second one."}

	// The fix cycle is checked BEFORE the review, and the order is the whole
	// correctness of this classifier for a run mid-cycle.
	//
	// While a fix is owed or in flight, the review step sits at `waiting` -- it
	// is waiting FOR that fix. Reading `review == waiting` as "no reviewer has
	// started" would tell an operator a reviewer is owed, at the exact moment
	// the thing actually owed is a fix cycle nobody must send twice. Caught by
	// TestResumeRunningFixNeverDuplicatesThePrompt.
	//
	// A cycle-1 review dispatch is unaffected: its fix step is still `pending`,
	// which is neither of the states below.
	case hasFix && (fix.State == domain.WorkflowStepReady || fix.State == domain.WorkflowStepWaiting):
		return ResumeObligation{Kind: ResumeObligationFixDelivery, StepID: fix.ID, Automatic: true,
			Explanation: "A fix cycle is owed to the worker. Resuming delivers it once, and only if AO can prove it has not already been delivered."}
	case hasFix && fix.State == domain.WorkflowStepRunning:
		return ResumeObligation{Kind: ResumeObligationFixObservation, StepID: fix.ID, Automatic: true,
			Explanation: "A fix cycle is in flight. Resuming observes it; it never re-sends the prompt."}

	case hasReview && work.State == domain.WorkflowStepCompleted &&
		(review.State == domain.WorkflowStepPending || review.State == domain.WorkflowStepReady ||
			review.State == domain.WorkflowStepWaiting):
		return ResumeObligation{Kind: ResumeObligationReviewDispatch, StepID: review.ID, Automatic: true,
			Explanation: "Work is complete and its review has not started. Resuming dispatches exactly one reviewer."}
	case hasReview && review.State == domain.WorkflowStepRunning:
		return ResumeObligation{Kind: ResumeObligationReviewObservation, StepID: review.ID, Automatic: true,
			Explanation: "A review is in flight. Resuming observes it; it never launches a second reviewer."}
	case hasVerify && !verify.State.Terminal() && review.State == domain.WorkflowStepCompleted:
		return ResumeObligation{Kind: ResumeObligationVerify, StepID: verify.ID, Automatic: true,
			Explanation: "Verification is outstanding. Resuming runs it against the approved review target, never against an unproven one."}
	}
	return ResumeObligation{Kind: ResumeObligationNone,
		Explanation: "Nothing durable is outstanding for this run."}
}

// planResumeObligation is deriveResumeObligation for a planned objective.
func planResumeObligation(d RunDetail) ResumeObligation {
	switch d.Plan.Status {
	case domain.WorkflowPlanPending, domain.WorkflowPlanRunning:
		return ResumeObligation{Kind: ResumeObligationPlanGeneration, Automatic: true,
			Explanation: "This objective has no plan yet. Resuming runs the planner once; a plan already in flight is observed, never started again."}
	case domain.WorkflowPlanValidated:
		automatic := d.Plan.ApprovalMode == domain.WorkflowPlanApprovalAuto
		explanation := "A validated plan is waiting for a person to approve it."
		if automatic {
			explanation = "A validated plan is waiting to be approved under this objective's automatic approval policy."
		}
		return ResumeObligation{Kind: ResumeObligationPlanApproval, Automatic: automatic, Explanation: explanation}
	case domain.WorkflowPlanApproved:
		for _, task := range d.Tasks {
			if task.State.Terminal() || task.State.Parked() {
				continue
			}
			child := ""
			if task.ExecutionRunID != nil {
				child = *task.ExecutionRunID
			}
			return ResumeObligation{Kind: ResumeObligationPlanDispatch, TaskID: task.ID, ChildRunID: child, Automatic: true,
				Explanation: "An approved plan has work still to dispatch or advance. Resuming advances it; a task already bound to a child run never gets a second one."}
		}
		return ResumeObligation{Kind: ResumeObligationConvergence, Automatic: true,
			Explanation: "Every task has finished or is parked. Resuming converges the objective on its children's results."}
	}
	// invalid / rejected: there is no execution obligation, only a plan one.
	return ResumeObligation{Kind: ResumeObligationNone,
		Explanation: "This objective's plan ended without becoming work; there is no execution obligation to resume."}
}

// ResumeReport is what a Resume call did, in the vocabulary above.
type ResumeReport struct {
	RunID string
	// Obligation is what was outstanding when the call started.
	Obligation ResumeObligation
	// Assessment is the recovery assessment AS OF the same moment, so a caller
	// that resumed something inert is told what to do instead.
	Assessment RecoveryAssessment
	// Performed reports whether AO actually re-entered the resume path. False
	// for a terminal run and for an obligation only a person can discharge.
	Performed bool
}

// ResumeRun is P1-B's obligation-driven resume.
//
// It states the obligation, then discharges it through the SAME
// evidence-gated continue path P0 built and hardened -- deliberately, because
// that path's guards are what make a repeated call idempotent, and a second
// dispatcher beside them is how duplicates are born. What is new is that the
// caller is told what was owed, what was done, and (when nothing could be)
// what to do instead.
//
// A run whose outstanding obligation is a person's is not driven at all: AO
// reports it rather than pretending a no-op was progress.
func (c *Coordinator) ResumeRun(ctx stdctx.Context, runID string) (RunDetail, ResumeReport, error) {
	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		return RunDetail{}, ResumeReport{}, err
	}
	strategy := c.strategyForRun(ctx, detail.Run)
	reuse := c.assessPlanReuse(ctx, detail, strategy)
	report := ResumeReport{
		RunID:      runID,
		Obligation: deriveResumeObligation(detail),
		Assessment: c.assessRecoveryFromDetail(detail, strategy.Effective, reuse.Reusability, c.repairsSpentFor(ctx, runID)),
	}

	if report.Obligation.Kind == ResumeObligationTerminal {
		// Inert on purpose. ContinueRun refuses a terminal run with
		// ErrAlreadyTerminal; reporting it as "nothing to do" is the same fact
		// without turning a page load into an error.
		return detail, report, nil
	}
	if !report.Obligation.Automatic {
		return detail, report, nil
	}

	resumed, err := c.ContinueRun(ctx, runID)
	if err != nil {
		return detail, report, err
	}
	report.Performed = true
	// Re-assess after the fact: the point of a resume is that the answer has
	// changed, and a caller acting on the pre-resume assessment would be acting
	// on a state that no longer exists.
	strategy = c.strategyForRun(ctx, resumed.Run)
	reuse = c.assessPlanReuse(ctx, resumed, strategy)
	report.Assessment = c.assessRecoveryFromDetail(resumed, strategy.Effective, reuse.Reusability, c.repairsSpentFor(ctx, runID))
	return resumed, report, nil
}
