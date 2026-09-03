package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// worker_signal_reconcile.go — the missing-first-signal reconciliation.
//
// The incident this file exists for (wf-00283521, worker medusa-4):
//
//	work step  failed      agent_start_failed
//	           "worker produced no first signal within 10m0s"
//
// while the Claude worker was, at that exact moment, sixteen minutes into a
// complete implementation which it went on to finish. The work was real, the
// worktree carried it, and it was eventually committed by hand as 74f053a6.
// AO never adopted it, because it had already declared the worker dead.
//
// The defect was not the timeout. It was the inference: `FirstSignalAt` is
// unset means the HOOK PIPELINE has said nothing, and AO treated that as proof
// the PROCESS never started. Those are different claims, and only the first one
// is supported. A provider whose hooks were never installed, whose hook
// transport is broken, or which simply has not reached a hook-firing boundary
// yet, produces exactly the same absence as one stuck at a trust prompt.
//
// So an absent first signal now opens a RECONCILIATION rather than closing the
// step. AO goes and looks at every independent fact it can actually obtain —
// the session row, activity since dispatch, a turn boundary, a forced (never
// throttled) live git observation of the worktree, and, when a liveness probe
// is wired, the runtime process/pane itself — and only declares a startup
// failure when every one of them is silent AND the session is provably not
// alive, or when the far longer reconcile bound has passed with nothing at all
// to show.
//
// Nothing here weakens a terminal failure that AO can prove. A session that is
// gone with no work still fails, immediately, exactly as before.

const (
	// workStepSignalReconcileTimeout is the outer bound on a work step whose
	// worker never produced a first signal AND for which AO could never obtain
	// any other evidence either — no activity, no turn boundary, no observable
	// worktree, no liveness answer.
	//
	// It is deliberately far longer than workStepFirstSignalTimeout, because it
	// bounds a different question. The ten-minute mark is "should AO stop
	// assuming this is a normal cold start?" — the answer to which is now
	// "yes, start reconciling", not "give up". This one is "AO has looked at
	// everything it can look at, repeatedly, for long enough that a working
	// agent would have left a mark on the worktree by now". A real
	// implementation turn that produces literally nothing observable for this
	// long is not a turn AO can distinguish from a dead process.
	workStepSignalReconcileTimeout = 45 * time.Minute

	// workerSignalDelayedPhase is the durable record of the reconciliation:
	// written every time AO declines to call a signal-less worker dead, with the
	// evidence it stood on. It is what makes "AO waited another 20 minutes"
	// auditable rather than invisible.
	workerSignalDelayedPhase = "worker_signal_delayed"
)

// WorkerLifecycleState is the durable vocabulary for where one work step's
// worker actually is. Before this existed AO had exactly two answers for a
// worker it could not hear from — "running" and "failed" — and the incident
// above is what happens when the only way out of the first is the second.
//
// The values are written as checkpoint durable phases (worker_observed_* and
// the two phases above), so they survive restart and are greppable, and they
// are surfaced on the work step's evidence so a person reads the same word AO
// decided on.
type WorkerLifecycleState string

const (
	// WorkerLifecycleStarting means dispatched, inside the ordinary startup grace,
	// no first signal yet. Nothing is wrong.
	WorkerLifecycleStarting WorkerLifecycleState = "starting"
	// WorkerLifecycleRunning means the worker has proven it is working — a first
	// signal, activity since dispatch, or observable work in the tree.
	WorkerLifecycleRunning WorkerLifecycleState = "running"
	// WorkerLifecycleSignalDelayed means past the startup grace with no first
	// signal, but AO holds positive evidence that the worker is alive or
	// working. Explicitly NOT a failure; explicitly not "healthy" either.
	WorkerLifecycleSignalDelayed WorkerLifecycleState = "signal_delayed"
	// WorkerLifecycleReconciling means past the startup grace, no first signal, and
	// AO could not obtain the evidence it needs either way. It keeps looking,
	// bounded by workStepSignalReconcileTimeout.
	WorkerLifecycleReconciling WorkerLifecycleState = "reconciling"
	// WorkerLifecycleCompleted means the worker produced verifiable work.
	WorkerLifecycleCompleted WorkerLifecycleState = "completed"
	// WorkerLifecycleFailed means AO can prove the worker did not start and left
	// nothing behind.
	WorkerLifecycleFailed WorkerLifecycleState = "failed"
)

// WorkerLivenessProbe is the OPTIONAL port that answers "is the runtime this
// session was launched into still alive right now?" — the tmux pane, the
// process, whatever the runtime adapter owns.
//
// Optional on purpose, and its absence is never read as death: a deployment
// without it simply has one fewer independent fact, and the reconciliation
// falls back on the session row, activity and the worktree. Returning an error
// means "AO could not tell", which is treated as unknown, never as dead.
type WorkerLivenessProbe interface {
	SessionAlive(ctx stdctx.Context, id domain.SessionID) (alive bool, known bool, err error)
}

// workerEvidence is every independent fact the reconciliation stands on. It is
// a plain value so evaluateWorkStepProgress stays a pure function of facts, and
// so the whole decision table is testable without a store, a git repository or
// a runtime.
type workerEvidence struct {
	// SessionAlive is the session row's own verdict: found, not terminated, not
	// exited. It is the weakest of the facts here — a row nobody updated looks
	// identical to a live worker — which is why it is never sufficient on its
	// own past the reconcile bound.
	SessionAlive bool
	// ActivitySinceDispatch is true when the session reported activity, or a
	// completed turn, at or after the dispatch. It is positive proof that
	// SOMETHING ran, and it is what the terminal activity observer supplies for
	// a provider whose hooks never fire.
	ActivitySinceDispatch bool
	// WorkspaceObserved records that a live git observation was actually
	// obtained. False means "AO did not get to look", which is unknown — never
	// "nothing is there".
	WorkspaceObserved bool
	// WorkEvidence is git-verified work: HEAD moved off the dispatch base, or
	// the tree is dirty/staged/untracked. This is the fact the medusa-4
	// incident turned on.
	WorkEvidence bool
	// CommitsSinceDispatch counts commits at or above the dispatch base that
	// the observation reports. Recorded for the ledger; the HEAD comparison in
	// WorkEvidence is what actually decides.
	CommitsSinceDispatch int
	// ProbeAlive / ProbeKnown are the optional runtime liveness answer.
	// ProbeKnown false means the probe is absent or could not tell.
	ProbeAlive bool
	ProbeKnown bool
}

// anyLiveness reports whether AO holds POSITIVE evidence that this worker is
// alive or has done something. It is deliberately not the negation of "proven
// dead": the two are different, and conflating them is the whole defect.
func (e workerEvidence) anyLiveness() bool {
	return e.WorkEvidence || e.ActivitySinceDispatch || (e.ProbeKnown && e.ProbeAlive)
}

// provablyDead reports whether AO can affirmatively say the worker is not
// there. Only two facts can say it: the session row is gone/terminated/exited,
// or a liveness probe that DID answer said no. Silence never says it.
func (e workerEvidence) provablyDead() bool {
	if e.ProbeKnown && !e.ProbeAlive {
		return true
	}
	return !e.SessionAlive
}

// firstSignalDecision is the reconciliation's verdict for a work step whose
// worker has produced no first signal past the startup grace.
type firstSignalDecision struct {
	Lifecycle WorkerLifecycleState
	Decision  WorkStepDecision
	// Detail is the one-line, evidence-bearing explanation recorded on the
	// durable checkpoint. Never a guess: it states what AO saw.
	Detail string
}

// reconcileMissingFirstSignal is the whole rule, and its default is to KEEP
// WAITING rather than to fail.
//
//	work evidence                  -> completed (adopt it; never re-dispatch)
//	any positive liveness          -> signal_delayed, no state change
//	provably dead, nothing to show -> failed, agent_start_failed
//	past the reconcile bound,
//	  still nothing at all         -> failed, agent_start_failed
//	otherwise                      -> reconciling, no state change
func reconcileMissingFirstSignal(ev workerEvidence, now, dispatchedAt time.Time) firstSignalDecision {
	if ev.WorkEvidence {
		return firstSignalDecision{
			Lifecycle: WorkerLifecycleCompleted,
			Decision: WorkStepDecision{
				Progress:   WorkerResultAvailable,
				NextStep:   domain.WorkflowStepCompleted,
				NextRun:    domain.WorkflowRunWaiting,
				NextAction: "start_review",
			},
			Detail: "no first signal ever arrived, but the worktree carries git-verified work from this dispatch — adopting it instead of declaring a startup failure",
		}
	}

	// Proven absent, with nothing to show for it. This is the only shape of
	// "the worker never started" AO can actually assert, and it is exactly the
	// shape the truly-dead case has.
	if ev.provablyDead() {
		return firstSignalDecision{
			Lifecycle: WorkerLifecycleFailed,
			Decision: WorkStepDecision{
				Progress: WorkerFailed,
				NextStep: domain.WorkflowStepFailed,
				NextRun:  domain.WorkflowRunNeedsAttention,
				NextAction: "worker produced no first signal within " + workStepFirstSignalTimeout.String() +
					" of dispatch and is no longer running, with no commit, dirty, staged or untracked change to show for it — startup failed",
				ErrorClass: domain.WorkflowErrorAgentStartFailed,
			},
			Detail: "no first signal, no activity, no work, and the worker is provably not running",
		}
	}

	if ev.anyLiveness() {
		return firstSignalDecision{
			Lifecycle: WorkerLifecycleSignalDelayed,
			Decision:  WorkStepDecision{Progress: WorkerActive, NoChange: true},
			Detail: fmt.Sprintf(
				"no first signal past the %s startup grace, but the worker is alive (activitySinceDispatch=%v runtimeAlive=%v workspaceObserved=%v) — AO is not treating a missing signal as a dead worker",
				workStepFirstSignalTimeout, ev.ActivitySinceDispatch, ev.ProbeKnown && ev.ProbeAlive, ev.WorkspaceObserved),
		}
	}

	// Nothing either way, for long enough that a working agent would have left
	// a mark. Bounded rather than polled forever — the same reason
	// workStepFirstSignalTimeout existed at all, applied to the right question.
	if !dispatchedAt.IsZero() && now.Sub(dispatchedAt) > workStepSignalReconcileTimeout {
		return firstSignalDecision{
			Lifecycle: WorkerLifecycleFailed,
			Decision: WorkStepDecision{
				Progress: WorkerFailed,
				NextStep: domain.WorkflowStepFailed,
				NextRun:  domain.WorkflowRunNeedsAttention,
				NextAction: "worker produced no first signal, no session activity and no workspace change within " +
					workStepSignalReconcileTimeout.String() + " of dispatch — startup likely failed (e.g. blocked on an interactive prompt, auth, or a launch error)",
				ErrorClass: domain.WorkflowErrorAgentStartFailed,
			},
			Detail: "reconciliation ran to its bound with no evidence of any kind",
		}
	}

	return firstSignalDecision{
		Lifecycle: WorkerLifecycleReconciling,
		Decision:  WorkStepDecision{Progress: WorkerCreated, NoChange: true},
		Detail: fmt.Sprintf(
			"no first signal past the %s startup grace and no independent evidence either way yet (workspaceObserved=%v sessionAlive=%v) — reconciling until %s",
			workStepFirstSignalTimeout, ev.WorkspaceObserved, ev.SessionAlive, workStepSignalReconcileTimeout),
	}
}

// workerEvidenceFor gathers the facts. It is the only I/O in this file.
//
// The forced observation is the load-bearing part: observeWorkStep's ordinary
// git observation is throttled off the newest checkpoint's age, and a step that
// is quietly polling writes no checkpoints, so the throttle can keep the ONE
// fact that would have saved medusa-4 permanently out of reach. When there is
// no first signal past the grace, AO pays for the observation every time.
func (c *Coordinator) workerEvidenceFor(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	sessionFound bool,
	sess domain.SessionRecord,
	obs ports.WorkspaceObservation,
	workspaceAvailable bool,
	baseSHA string,
	dispatchedAt time.Time,
) (workerEvidence, ports.WorkspaceObservation, bool) {
	ev := workerEvidence{
		SessionAlive: sessionFound && !sess.IsTerminated && sess.Activity.State != domain.ActivityExited,
	}
	if !dispatchedAt.IsZero() {
		if !sess.Activity.LastActivityAt.IsZero() && !sess.Activity.LastActivityAt.Before(dispatchedAt) {
			ev.ActivitySinceDispatch = true
		}
		if !sess.TurnCompletedAt.IsZero() && !sess.TurnCompletedAt.Before(dispatchedAt) {
			ev.ActivitySinceDispatch = true
		}
	}

	if sessionFound {
		obs, workspaceAvailable = c.forceWorkspaceObservation(ctx, sess, obs, workspaceAvailable)
	}
	ev.WorkspaceObserved = workspaceAvailable
	if workspaceAvailable {
		if obs.HeadSHA != "" && obs.HeadSHA != baseSHA {
			ev.WorkEvidence = true
		}
		if obs.Dirty || obs.Staged || obs.Untracked || len(obs.Changes) > 0 {
			ev.WorkEvidence = true
		}
		for _, commit := range obs.Commits {
			if commit.SHA != "" && commit.SHA != baseSHA {
				ev.CommitsSinceDispatch++
				continue
			}
			break
		}
	}

	if c.workerLiveness != nil && sessionFound {
		if alive, known, err := c.workerLiveness.SessionAlive(ctx, sess.ID); err == nil && known {
			ev.ProbeAlive, ev.ProbeKnown = alive, true
		}
	}
	_ = run
	return ev, obs, workspaceAvailable
}

// recordFirstSignalReconciliation writes the durable record of one
// reconciliation pass, at most once per distinct verdict.
//
// Once per verdict, not once per poll: this runs from the read path, so a step
// that sits in signal_delayed for twenty minutes would otherwise write a row
// per poll describing one unchanged condition — the same problem
// recordAttentionStopOnce exists for, and solved the same way.
func (c *Coordinator) recordFirstSignalReconciliation(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	verdict firstSignalDecision, ev workerEvidence, carry domain.WorkflowCheckpoint,
) {
	// Only the two WAITING verdicts write a row of their own. The completing and
	// failing verdicts are already recorded by observeWorkStep's own checkpoint
	// for this same decision, and a second row at the same instant would tie on
	// created_at with it — which matters, because "the latest checkpoint for this
	// step" is how review dispatch resolves the worktree and the fingerprint to
	// review.
	if verdict.Lifecycle != WorkerLifecycleSignalDelayed && verdict.Lifecycle != WorkerLifecycleReconciling {
		return
	}
	if c.checkpointPhaseIsCurrentForStep(ctx, run.ID, step.ID, workerSignalDelayedPhase, verdict.Detail) {
		return
	}
	stepID := step.ID
	payload, _ := json.Marshal(struct {
		Lifecycle WorkerLifecycleState `json:"lifecycle"`
		Evidence  workerEvidence       `json:"evidence"`
	}{Lifecycle: verdict.Lifecycle, Evidence: ev})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		SessionID:      carry.SessionID,
		// Branch/WorktreePath/BaseSHA carry forward for the same reason
		// observeWorkStep's own checkpoint carries them: this row becomes "the
		// latest checkpoint for this step", and dropping them here would
		// silently lose the worktree the reviewer is later launched against.
		Branch:         carry.Branch,
		WorktreePath:   carry.WorktreePath,
		BaseSHA:        carry.BaseSHA,
		HeadSHA:        carry.HeadSHA,
		NextAction:     verdict.Detail,
		DurablePhase:   workerSignalDelayedPhase,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: recording a first-signal reconciliation failed", "run", run.ID, "step", step.ID, "err", err)
	}
}

// checkpointPhaseIsCurrentForStep reports whether this step's newest checkpoint
// already says exactly this phase and detail.
func (c *Coordinator) checkpointPhaseIsCurrentForStep(ctx stdctx.Context, runID, stepID, phase, detail string) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false
	}
	var newest domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		if !found || !cp.CreatedAt.Before(newest.CreatedAt) {
			newest, found = cp, true
		}
	}
	return found && newest.DurablePhase == phase && newest.NextAction == detail
}
