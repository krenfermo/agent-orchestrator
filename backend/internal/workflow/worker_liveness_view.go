package workflow

import (
	stdctx "context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// worker_liveness_view.go -- the third clock the run detail was missing.
//
// A run card carries two timestamps today and neither one answers "is the
// worker alive right now":
//
//   - Lifecycle.LastActivityAt / Presentation.LastMeaningfulActivityAt is the
//     newest entry of the run's bounded timeline. It is a WORKFLOW clock, and
//     its invariant is explicit and correct: an idle worker during a review is
//     not an idle workflow, so it must never be moved by a worker's heartbeat.
//   - sessions.activity_last_at is a TRANSITION clock: when the session entered
//     the state it is in now. Migration 0168 exists because AO had been reading
//     it as a liveness clock, and a worker that stays `active` for twenty
//     minutes never moves it.
//
// So the operator report -- "last activity 12 minutes ago" printed over a
// worker making six model calls a minute -- was not a bug in either field. It
// was the absence of a third one. This file adds it WITHOUT redefining either:
// worker liveness only ever moves its own number forward, and a run with no
// running worker contributes nothing to it.
//
// COST. This is read in GetRun only -- the run DETAIL path -- and never in the
// Board projection, which polls every two seconds across every card. It is one
// session read, skipped entirely unless a step is actually running.

// WorkerLiveness is the running worker's two clocks, projected onto its run.
//
// Observed=false is the honest answer for a run with no running worker, a step
// with no session, or a session AO could not read. Every other field is then
// meaningless and must be rendered as absent, never as "0s ago" -- which would
// be the same class of misreport this file exists to end, only inverted.
type WorkerLiveness struct {
	Observed  bool
	SessionID string
	StepID    string
	StepKind  domain.WorkflowStepKind
	// LastSignalAt is Activity.Liveness(): the newest moment AO was heard from
	// by this session at all, including the signals that repeat the state it
	// already had. This is the one a UI must render as "last activity".
	LastSignalAt time.Time
	// LastTransitionAt is Activity.LastActivityAt: when the session entered
	// its current state. Kept beside the signal clock rather than replacing
	// it, because "working on the same thing since 17:10" is a real and
	// useful fact -- it is only a misreport when it is labelled as liveness.
	LastTransitionAt time.Time
	State            domain.ActivityState
}

// SilentFor reports how long the worker has been unheard from, and whether
// that is knowable.
func (w WorkerLiveness) SilentFor(now time.Time) (time.Duration, bool) {
	if !w.Observed || w.LastSignalAt.IsZero() {
		return 0, false
	}
	d := now.Sub(w.LastSignalAt)
	if d < 0 {
		return 0, true
	}
	return d, true
}

// observeWorkerLiveness annotates the detail with the running worker's clocks.
//
// Best-effort by construction: it writes nothing, and every failure to obtain
// an answer leaves Observed false rather than guessing -- the same discipline
// annotateFixDeliveryReceipt already applies on this path.
func (c *Coordinator) observeWorkerLiveness(ctx stdctx.Context, detail *RunDetail) {
	if detail == nil || c == nil || c.sessionFacts == nil {
		return
	}
	if detail.Run.State.Terminal() {
		// A finished run has no live worker. Reporting the last session it
		// used would produce a "heard from 3 days ago" line under a completed
		// run, which reads as a fault where there is none.
		return
	}
	step, sessionID, ok := runningWorkerSession(detail.Steps)
	if !ok {
		return
	}
	sess, found, err := c.sessionFacts.GetSession(ctx, domain.SessionID(sessionID))
	if err != nil || !found {
		return
	}
	detail.WorkerLiveness = WorkerLiveness{
		Observed:         true,
		SessionID:        sessionID,
		StepID:           step.ID,
		StepKind:         step.Kind,
		LastSignalAt:     sess.Activity.Liveness(),
		LastTransitionAt: sess.Activity.LastActivityAt,
		State:            sess.Activity.State,
	}
}

// runningWorkerSession picks the step whose agent is the one a person means by
// "the worker": the newest RUNNING step that owns a session.
//
// Newest rather than first because a fix cycle's step is the live one once it
// starts, and the work step it followed keeps its session id long after its own
// turn ended. Review steps are included deliberately -- a reviewer is an agent
// AO is waiting on too, and a run whose reviewer has gone silent is exactly as
// interesting as one whose worker has.
func runningWorkerSession(steps []StepDetail) (domain.WorkflowStep, string, bool) {
	var best domain.WorkflowStep
	var sessionID string
	found := false
	for _, sd := range steps {
		if sd.Step.State != domain.WorkflowStepRunning {
			continue
		}
		if sd.Step.SessionID == nil || *sd.Step.SessionID == "" {
			continue
		}
		if !found || sd.Step.Ordinal >= best.Ordinal {
			best, sessionID, found = sd.Step, *sd.Step.SessionID, true
		}
	}
	return best, sessionID, found
}
