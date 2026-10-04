package workflow

import (
	stdctx "context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// incident_progress.go — Checkpoint 8P-E.21.
//
// One derived value the modal renders directly, plus the facts that make it
// legible. It exists so the UI never has to know the state machine: a person
// looking at a stopped run should read "revisión independiente en curso", not
// infer it from an incident state, a repair run id and a step table.
//
// Every value is derived from durable state on each read. Nothing here is
// stored, nothing is timed, and there is no frontend timer to simulate motion —
// a progress that advanced on a clock rather than on a fact would eventually
// claim a repair was verified while it was still building.

// IncidentProgress is the whole vocabulary the modal shows.
type IncidentProgress string

const (
	// IncidentProgressAnalyzing means AO has an incident and no diagnosis yet.
	IncidentProgressAnalyzing IncidentProgress = "analyzing"
	// IncidentProgressWaitingCapacity means AO wants to run a diagnostic agent and
	// no provider currently has capacity. Self-remediable; a wake is pending.
	IncidentProgressWaitingCapacity IncidentProgress = "waiting_capacity"
	// IncidentProgressDiagnosing means a Diagnostic Agent is running.
	IncidentProgressDiagnosing IncidentProgress = "diagnosing"
	// IncidentProgressDiagnosisBlocked means a Diagnostic Agent was launched and is
	// waiting on a person inside its own session — a trust dialog, a permission
	// prompt, a login. It is deliberately NOT "diagnosing": that is where this
	// state used to hide, and reporting an investigation as making progress
	// while it waits for input nobody knows to give is the whole of incident F.
	IncidentProgressDiagnosisBlocked IncidentProgress = "diagnosis_blocked"
	// IncidentProgressDiagnosed means a diagnosis exists and proposes nothing to run.
	IncidentProgressDiagnosed IncidentProgress = "diagnosed"
	// IncidentProgressAwaitingApproval means a diagnosis proposes an action that
	// needs a person to say yes.
	IncidentProgressAwaitingApproval IncidentProgress = "awaiting_approval"
	// IncidentProgressRepairing means the repair run's work step is running.
	IncidentProgressRepairing IncidentProgress = "repairing"
	// IncidentProgressReviewing means the repair is under review.
	IncidentProgressReviewing IncidentProgress = "reviewing"
	// IncidentProgressVerifying means the repair is being verified. Repairing,
	// reviewing and verifying track the repair run's own steps, so the modal
	// shows where the repair actually is.
	IncidentProgressVerifying IncidentProgress = "verifying"
	// IncidentProgressResolved means AO did something attributable and it verified.
	IncidentProgressResolved IncidentProgress = "resolved"
	// IncidentProgressClosed means the condition went away by another route.
	IncidentProgressClosed IncidentProgress = "closed"
	// IncidentProgressRefused means AO declined to act and said why.
	IncidentProgressRefused IncidentProgress = "refused"
	// IncidentProgressNeedsDecision means the diagnosis is a human decision with
	// options, and none of them is AO's to take.
	IncidentProgressNeedsDecision IncidentProgress = "needs_decision"
)

// IncidentStatus is the derived, render-ready view of an incident's motion.
type IncidentStatus struct {
	Progress IncidentProgress

	// DiagnosticHarness is the provider actually chosen for the investigation.
	DiagnosticHarness string
	// CapacityReasons explains a waiting_capacity state in routing's own words.
	CapacityReasons []string
	// NextEvaluationAt is the pending wake, when one exists — "AO will look
	// again at …" rather than "AO might look again".
	NextEvaluationAt *time.Time

	// Diagnosis is the investigation as an observable background job: its
	// state, the agent and provider running it, when it started, how long it
	// has been going, its last activity and last signal, and — when AO can
	// tell — what it is blocked on. Derived on every read from durable state
	// and the agent session, never stored and never timed by the UI. See
	// incident_diagnosis_job.go.
	Diagnosis IncidentDiagnosisJob

	// Repair linkage, for the states that are about a repair run.
	RepairRunID     string
	ReviewerHarness string
	VerifyResult    string
	FinalSHA        string
}

// DeriveIncidentStatus projects an incident (and, when it has one, its repair
// run) onto the modal's vocabulary.
//
// The order below is the order the states actually occur in, and it must stay
// that way: terminal outcomes are decided first, so a resolved incident never
// renders as "verifying" because its repair run's verify step happens to still
// read `completed`.
func (c *Coordinator) DeriveIncidentStatus(ctx stdctx.Context, inc Incident) IncidentStatus {
	st := IncidentStatus{
		RepairRunID: inc.RepairRunID,
		Progress:    IncidentProgressAnalyzing,
		Diagnosis:   c.DeriveIncidentDiagnosisJob(ctx, inc),
	}

	switch inc.State {
	case IncidentResolved:
		st.Progress = IncidentProgressResolved
		c.attachRepairFacts(ctx, &st)
		return st
	case IncidentClosed:
		st.Progress = IncidentProgressClosed
		return st
	case IncidentRefused:
		st.Progress = IncidentProgressRefused
		return st
	}

	// A dispatched repair is the most specific thing that can be true, so it
	// wins over the diagnosis states below: once a repair is running, "awaiting
	// approval" is history.
	if inc.RepairRunID != "" {
		st.Progress = c.repairProgress(ctx, inc.RepairRunID)
		c.attachRepairFacts(ctx, &st)
		return st
	}

	if inc.Diagnosis != nil {
		st.DiagnosticHarness = inc.Diagnosis.Harness
		switch {
		case inc.Diagnosis.Class == IncidentHumanDecision:
			st.Progress = IncidentProgressNeedsDecision
		case inc.Diagnosis.Action != nil && incidentActionNeedsApproval(inc.Diagnosis.Class, inc.Diagnosis.Action.Kind):
			st.Progress = IncidentProgressAwaitingApproval
		default:
			st.Progress = IncidentProgressDiagnosed
		}
		return st
	}

	if inc.State == IncidentDiagnosing {
		st.Progress = IncidentProgressDiagnosing
		st.DiagnosticHarness = inc.DiagnosticHarness
		// Checkpoint 8P-E.24: "diagnosing" is where an investigation blocked on
		// an interactive prompt used to hide. The job's own state is the honest
		// answer, and a job waiting on a person is not AO making progress.
		if st.Diagnosis.State == DiagnosisWaitingForUser {
			st.Progress = IncidentProgressDiagnosisBlocked
		}
		return st
	}
	if inc.WaitingForCapacity {
		st.Progress = IncidentProgressWaitingCapacity
		st.CapacityReasons = inc.CapacityReasons
		st.NextEvaluationAt = c.nextIncidentEvaluation(ctx, inc.RunID)
		return st
	}
	return st
}

// repairProgress reads where the repair run actually is, from its own steps.
//
// It reports the step that is genuinely in motion rather than a phase name,
// because a repair run is an ordinary workflow run and its steps are the only
// honest answer to "what is happening right now".
func (c *Coordinator) repairProgress(ctx stdctx.Context, repairRunID string) IncidentProgress {
	steps, err := c.store.ListWorkflowSteps(ctx, repairRunID)
	if err != nil {
		return IncidentProgressRepairing
	}
	inMotion := func(s domain.WorkflowStep) bool {
		return s.State == domain.WorkflowStepRunning || s.State == domain.WorkflowStepReady
	}
	// Checked latest-first: a run whose verify is moving is verifying, whatever
	// its earlier steps now say.
	for _, kind := range []struct {
		kind     domain.WorkflowStepKind
		progress IncidentProgress
	}{
		{domain.WorkflowStepVerify, IncidentProgressVerifying},
		{domain.WorkflowStepReview, IncidentProgressReviewing},
		{domain.WorkflowStepFix, IncidentProgressRepairing},
		{domain.WorkflowStepWork, IncidentProgressRepairing},
	} {
		for _, s := range steps {
			if s.Kind == kind.kind && inMotion(s) {
				return kind.progress
			}
		}
	}
	return IncidentProgressRepairing
}

// attachRepairFacts fills in the reviewer, the verification result and the
// landed commit once the repair run has produced them.
func (c *Coordinator) attachRepairFacts(ctx stdctx.Context, st *IncidentStatus) {
	if st.RepairRunID == "" {
		return
	}
	st.ReviewerHarness = c.repairReviewerHarness(ctx, st.RepairRunID)
	st.VerifyResult = c.repairVerifyResult(ctx, st.RepairRunID)
	st.FinalSHA = c.repairFinalSHA(ctx, st.RepairRunID)
}

// nextIncidentEvaluation reports when AO will look at a capacity wait again.
// nil means no wake is pending, which the modal must render as "not scheduled"
// rather than inventing an estimate.
func (c *Coordinator) nextIncidentEvaluation(ctx stdctx.Context, runID string) *time.Time {
	if c.wakeScheduler == nil || runID == "" {
		return nil
	}
	next, err := c.wakeScheduler.NextForRun(ctx, domain.WorkflowRunID(runID))
	if err != nil || next == nil {
		return nil
	}
	at := next.ScheduledAt
	return &at
}
