package workflow

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// recovery_execution.go — the technical half of "why is this stuck".
//
// The recovery assessment already answers the human question: what should be
// done, and may AO do it. P3-D §12 adds the question an engineer asks next and
// that AO could previously only answer by opening ao.db: WHICH execution are we
// talking about — which attempt, whose, of what generation, on which session,
// and what was the last thing that actually happened to it.
//
// It is a projection, not a second source of truth (§12: "no duplicar truth").
// Every field is read off rows the run detail has already loaded — the step,
// its attempts, its newest checkpoint — so asking for it costs nothing beyond
// the read the caller already paid for, and no field can disagree with the
// ledger it came from.
//
// Two things it deliberately does not do:
//
//   - it never infers. A fact AO does not hold is zero, and zero renders as
//     "unknown" rather than as a plausible-looking default;
//   - it exposes identities and bounded classifications only (§35). No prompt
//     text, no provider credentials, no pane contents.

// RecoveryExecution is the current execution one run's recovery is about.
type RecoveryExecution struct {
	// StepID/StepKind name the step this execution belongs to.
	StepID   string
	StepKind string
	// AttemptID is the durable attempt row. Empty when the step has none.
	AttemptID     string
	AttemptNumber int64
	// Provider is the attempt's harness — which agent is (or was) doing this.
	Provider string
	// SessionID is the agent session the step durably owns, when it owns one.
	SessionID string
	// LifecycleState is the step's own state, so a reader never has to guess
	// which phase the attempt sits in.
	LifecycleState string
	// Authority is what AO is entitled to conclude about the attempt row: is it
	// the one that currently holds authority, is it history, has its cycle been
	// superseded, or is it a legacy row whose cycle cannot be proven. For a fix
	// step this is ClassifyFixAttempt's answer verbatim; for every other kind it
	// is the same vocabulary applied to the simpler question the row can answer.
	Authority FixAttemptAuthority
	// StartedAt is when this attempt was dispatched.
	StartedAt time.Time
	// FinishedAt is when it concluded, if it has.
	FinishedAt *time.Time
	// Outcome/ErrorClass are the attempt's own conclusion, empty while open.
	Outcome    string
	ErrorClass string
	// LastEventPhase/LastEventAt are the newest durable checkpoint recorded
	// against this step: the last thing AO can prove happened here.
	LastEventPhase string
	LastEventAt    time.Time
}

// Empty reports an execution AO could not identify at all.
func (e RecoveryExecution) Empty() bool { return e.StepID == "" }

// deriveRecoveryExecution picks the execution a recovery answer is about and
// projects it.
//
// The choice of step is deliberate and ordered: the step the resume obligation
// already named wins, because that is the row the recommendation is about and
// the two must never describe different executions. Failing that, the newest
// step that is actually in flight; failing that, the newest step with an
// attempt at all, so a stopped run still names what it last tried.
func deriveRecoveryExecution(d RunDetail, preferredStepID string, authority FixAuthority) RecoveryExecution {
	step, ok := pickRecoveryStep(d, preferredStepID)
	if !ok {
		return RecoveryExecution{}
	}
	e := RecoveryExecution{
		StepID:         step.Step.ID,
		StepKind:       string(step.Step.Kind),
		LifecycleState: string(step.Step.State),
	}
	if step.Step.SessionID != nil {
		e.SessionID = *step.Step.SessionID
	}
	if step.LatestCheckpoint != nil {
		e.LastEventPhase = step.LatestCheckpoint.DurablePhase
		e.LastEventAt = step.LatestCheckpoint.CreatedAt
	}
	if len(step.Attempts) == 0 {
		return e
	}
	// The newest OPEN attempt is the execution in flight; with none open, the
	// newest attempt is what this step last tried. Position is used only to
	// order rows here — never as identity, which is what fix_attempt_identity.go
	// exists to keep true.
	attempt := step.Attempts[len(step.Attempts)-1]
	for i := len(step.Attempts) - 1; i >= 0; i-- {
		if step.Attempts[i].Outcome == "" {
			attempt = step.Attempts[i]
			break
		}
	}
	e.AttemptID = attempt.ID
	e.AttemptNumber = attempt.AttemptNumber
	e.Provider = attempt.Harness
	e.StartedAt = attempt.StartedAt
	e.FinishedAt = attempt.FinishedAt
	e.Outcome = string(attempt.Outcome)
	e.ErrorClass = string(attempt.ErrorClass)
	e.Authority = ClassifyFixAttempt(attempt, authority)
	if step.Step.Kind != domain.WorkflowStepFix && e.Authority == FixAttemptLegacyUnproven {
		// Only a fix attempt carries a cycle key, so only a fix attempt can be
		// legacy-unproven in the sense that classification means. Every other
		// kind's open row is simply the step's current attempt, and saying
		// "unproven" about it would be a claim about identity that this row was
		// never asked to carry.
		e.Authority = FixAttemptActive
	}
	return e
}

// pickRecoveryStep implements the ordering described above.
func pickRecoveryStep(d RunDetail, preferredStepID string) (StepDetail, bool) {
	if preferredStepID != "" {
		for _, s := range d.Steps {
			if s.Step.ID == preferredStepID {
				return s, true
			}
		}
	}
	var inFlight, withAttempt StepDetail
	var haveInFlight, haveAttempt bool
	for _, s := range d.Steps {
		if s.Step.State == domain.WorkflowStepRunning || s.Step.State == domain.WorkflowStepReady {
			inFlight, haveInFlight = s, true
		}
		if len(s.Attempts) > 0 {
			withAttempt, haveAttempt = s, true
		}
	}
	if haveInFlight {
		return inFlight, true
	}
	if haveAttempt {
		return withAttempt, true
	}
	return StepDetail{}, false
}
