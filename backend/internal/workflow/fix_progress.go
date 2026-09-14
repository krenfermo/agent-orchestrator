package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// fixCyclePickupTimeout bounds how long AO waits for a worker session to BEGIN
// the turn a freshly delivered fix cycle asked for. It mirrors
// workStepFirstSignalTimeout deliberately, at the same value and for the same
// reason: both answer "the agent was handed something and has not visibly
// reacted to it", and the two windows should not disagree about how long that
// is allowed to take.
const fixCyclePickupTimeout = 10 * time.Minute

// fixActiveSilenceWindow bounds how long an ACTIVE fix cycle may go unheard
// before AO re-checks its runtime (P9 §18). It is not a timeout: silence alone
// is never a conclusion. A laptop that slept for three hours wakes with every
// clock "silent" and a worker that is perfectly alive, so past this window AO
// asks the runtime whether the execution is provably gone -- and only a proven
// ending moves the step. Quiet-but-alive and unprovable both leave it alone.
// Same value as workerNeedsInputCorroborationWindow: both answer "how long may a
// live agent say nothing before AO looks harder".
const fixActiveSilenceWindow = 15 * time.Minute

// fixCycleStarted reports whether the worker session produced ANY signal that
// postdates this cycle's dispatch.
//
// This is the fact observeFixStep was missing, and the whole of incident
// wf-57f90ff2 (2026-08-23). A fix cycle is delivered into a session that has
// already run earlier cycles, so every activity field on that session row is
// populated — with the PREVIOUS cycle's values. Cycle 2 was dispatched at
// 18:43:59Z into a session whose activity_state was `idle`, whose
// activity_last_at was 18:35:33Z and whose turn_completed_at was 18:35:34Z:
// all of it the record of cycle 1, which had ended eight minutes earlier.
// Seventy-six seconds later AO read those stale facts as this cycle's outcome
// and stopped the run saying "fix worker idle with no verifiable new change".
// The worker had not gone idle without changing anything; it had never started.
//
// The one guard that existed against this, `sess.FirstSignalAt.IsZero()`, only
// protects a session that has never signalled at all — which a reused worker
// session, by construction, never is. So from cycle 2 onwards there was no
// grace period whatsoever, and observationThrottle being three seconds meant
// the very first poll after a dispatch could end the run.
//
// The clocks are compared against the dispatch instant for exactly the reason
// classifyFixDelivery compares its own against the intent instant: activity
// that predates the dispatch can never be evidence about it.
func fixCycleStarted(sess domain.SessionRecord, dispatchedAt time.Time) bool {
	if dispatchedAt.IsZero() {
		// No instant to compare against, so this rule has nothing to say.
		// Missing evidence must never become evidence: answering "started"
		// leaves every caller with exactly its pre-existing behaviour.
		return true
	}
	return afterInstant(sess.Activity.LastActivityAt, dispatchedAt) ||
		afterInstant(sess.TurnCompletedAt, dispatchedAt) ||
		afterInstant(sess.FirstSignalAt, dispatchedAt) ||
		// P9 §18: an agent that was already ACTIVE keeps emitting same-state
		// signals, which move only the coalesced liveness clock (last_signal_at),
		// never the transition clock. Such a signal after the dispatch, from a
		// session that reads active, is the agent working -- counting it keeps a
		// busy fix cycle from being stopped as "never started". It is deliberately
		// NOT counted for an idle session: an idle heartbeat after the dispatch is
		// exactly the stale-looking fact incident wf-57f90ff2 was stopped on.
		(sess.Activity.State == domain.ActivityActive && afterInstant(sess.Activity.LastSignalAt, dispatchedAt))
}

// fixCycleNumberOf reads the cycle number off a dispatch checkpoint's durable
// delivery record, returning 0 when the checkpoint carries none.
func fixCycleNumberOf(cp domain.WorkflowCheckpoint) int {
	var rec promptDeliveryRecord
	if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
		return 0
	}
	return rec.CycleNumber
}

// fixCycleNotStartedDetail renders the stop in terms of what AO can actually
// prove: the prompt was delivered, and the worker has said nothing since. It
// deliberately does not claim the worker produced nothing — AO has no evidence
// about the worker's output, only about its silence.
func fixCycleNotStartedDetail(cp domain.WorkflowCheckpoint, sessionID string, now time.Time) string {
	cycle := fixCycleNumberOf(cp)
	silence := now.Sub(cp.CreatedAt).Round(time.Second)
	return fmt.Sprintf(
		"fix cycle %d was delivered to worker session %s %s ago and that session has produced no activity, "+
			"no turn boundary and no signal since — the worker never started this cycle, so AO has no evidence "+
			"about what it would have changed",
		cycle, sessionID, silence)
}

// observeFixStep is Checkpoint 8D's fact-based fix-step evaluation function,
// mirroring observeWorkStep's shape and conservatism exactly: it never
// trusts agent transcript text, only session facts (IsTerminated/
// ActivityState) and a live workspace fingerprint comparison against the
// fingerprint recorded when this fix cycle was dispatched
// (workflow.WorkspaceFingerprint over ports.WorkspaceObservation). A fix
// step never reaches "completed" in this checkpoint (verify, the natural
// judge of "truly done," is out of scope) — it only ever resolves to
// "waiting" (a genuinely new fingerprint was observed: the cycle is
// considered delivered, ready for the next re-review) or "failed" (the
// worker session ended with no verifiable change at all).
func (c *Coordinator) observeFixStep(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) (domain.WorkflowStep, error) {
	if step.Kind != domain.WorkflowStepFix || step.State != domain.WorkflowStepRunning {
		return step, nil
	}
	if c.sessionFacts == nil || c.workspaceFacts == nil {
		return step, nil
	}
	latestCP, hasCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID)
	if err != nil {
		return step, err
	}
	if !hasCP || latestCP.SessionID == nil || *latestCP.SessionID == "" {
		// Defensive: a running fix step should always have a dispatch
		// checkpoint with a session id (recordFixDispatchSuccess always
		// writes one). If it somehow doesn't, there is nothing to observe.
		return step, nil
	}
	now := c.clock()
	if now.Sub(latestCP.CreatedAt) <= observationThrottle {
		// Too fresh to justify another live ObserveWorkspace shell-out; wait
		// for a later call. Not an error.
		return step, nil
	}
	fingerprintBefore := latestCP.FingerprintBefore

	sessionID := domain.SessionID(*latestCP.SessionID)
	sess, found, err := c.sessionFacts.GetSession(ctx, sessionID)
	if err != nil {
		return step, err
	}

	terminatedOrExited := !found || sess.IsTerminated || sess.Activity.State == domain.ActivityExited
	if terminatedOrExited {
		return c.concludeEndedFixSession(ctx, run, step, sess, fingerprintBefore)
	}

	// Checkpoint 8P-E.16: nothing below may draw a conclusion about what this
	// fix cycle produced until AO has evidence that the worker actually began
	// it. See fixCycleStarted — this gate is the whole of incident wf-57f90ff2.
	if !fixCycleStarted(sess, latestCP.CreatedAt) {
		// A genuinely new fingerprint always outranks the gate: it is direct
		// evidence of work, and no activity clock can contradict it.
		// Hoisted out of the `if` it used to live in so the stop below can
		// persist the very observation this branch already paid for, rather
		// than reaching a conclusion on evidence nothing wrote down.
		obs, observedOK := c.observeFixWorkspace(ctx, sess)
		if observedOK {
			if fp := WorkspaceFingerprint(obs); fp != fingerprintBefore {
				return c.recordFixOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunWaiting,
					fp, true, "fix delivered — awaiting next review cycle", "", &obs)
			}
		}
		if now.Sub(latestCP.CreatedAt) <= fixCyclePickupTimeout {
			// Still inside the window a worker is allowed to take to pick the
			// cycle up. Absence of a signal here is not yet a fact about
			// anything, so record nothing at all.
			return step, nil
		}
		return c.stopFixAmbiguous(ctx, run, step, sessionID, ReasonFixCycleNotStarted,
			fixCycleNotStartedDetail(latestCP, *latestCP.SessionID, now), observationOrNil(obs, observedOK))
	}

	switch sess.Activity.State {
	case domain.ActivityActive:
		return c.observeActiveFixSilence(ctx, run, step, sess, latestCP.CreatedAt, fingerprintBefore, now)
	case domain.ActivityWaitingInput, domain.ActivityBlocked:
		return c.stopFix(ctx, run, step, domain.WorkflowStepWaiting, ReasonFixWorkerBlocked,
			"fix worker awaiting input/blocked — needs human attention", "")
	case domain.ActivityIdle:
		obs, ok := c.observeFixWorkspace(ctx, sess)
		if !ok {
			return step, nil
		}
		fp := WorkspaceFingerprint(obs)
		if fp != fingerprintBefore {
			return c.recordFixOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunWaiting,
				fp, true, "fix delivered — awaiting next review cycle", "", &obs)
		}
		// As with the initial work step, idle is the persisted default before
		// the first TUI hook signal. A restart during that initialization window
		// must not turn missing evidence into a failed fix attempt.
		if sess.FirstSignalAt.IsZero() {
			return step, nil
		}
		// Conservative, mirrors evaluateWorkStepProgress's idle+no-evidence
		// rule exactly: "Codex went idle but did not actually change
		// anything new" must not silently trigger a new review.
		return c.stopFixAmbiguous(ctx, run, step, sessionID, ReasonFixNoVerifiableChange,
			"fix worker idle with no verifiable new change — needs human review", &obs)
	default:
		return step, nil
	}
}

// stopFixAmbiguous is the ONLY way fix observation reaches
// ambiguous_worker_state. It goes through the evidence gate first, so the stop
// it writes carries the bounded snapshot AO stood on rather than a conclusion
// on its own. See ambiguous_worker_state.go.
func (c *Coordinator) stopFixAmbiguous(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	sessionID domain.SessionID,
	reason, detail string,
	obs *ports.WorkspaceObservation,
) (domain.WorkflowStep, error) {
	// The fix path resolves its session off the step's own dispatch checkpoint
	// and passes it in; the fallback covers the case where that row carried
	// none, and resolves it the same way — from this step's checkpoints, never
	// from a neighbouring step's.
	if sessionID == "" {
		sessionID = c.DurableSessionForStep(ctx, run.ID, step)
	}
	raised, err := c.raiseAmbiguousWorkerState(
		ctx, run, step, reason, detail, c.observedWorkerFactsFor(ctx, sessionID, obs))
	if err != nil {
		return step, err
	}
	if err := assertAmbiguousEvidence(raised.ErrorClass(), raised); err != nil {
		return step, err
	}
	return c.stopFix(ctx, run, step, domain.WorkflowStepWaiting, reason, detail, raised.ErrorClass())
}

// stopFix is fix observation's counterpart to stopReview: the same
// recordFixOutcome write it always did, plus Checkpoint 8P-E.13's canonical
// attention record so the resulting needs_attention can name itself.
func (c *Coordinator) stopFix(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	nextStep domain.WorkflowStepState,
	reason, detail string,
	errClass domain.WorkflowErrorClass,
) (domain.WorkflowStep, error) {
	updated, err := c.recordFixOutcome(ctx, run, step, nextStep, domain.WorkflowRunNeedsAttention,
		"", false, detail, errClass, nil)
	if err != nil {
		return updated, err
	}
	c.recordAttentionStop(ctx, run, &updated.ID, reason, detail)
	return updated, nil
}

func (c *Coordinator) observeFixWorkspace(ctx stdctx.Context, sess domain.SessionRecord) (ports.WorkspaceObservation, bool) {
	if sess.Metadata.WorkspacePath == "" {
		return ports.WorkspaceObservation{}, false
	}
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      sess.Metadata.WorkspacePath,
		Branch:    sess.Metadata.Branch,
		SessionID: sess.ID,
		ProjectID: sess.ProjectID,
	})
	if err != nil {
		return ports.WorkspaceObservation{}, false
	}
	return obs, true
}

// recordFixOutcome persists one fix-step observation's outcome: the step/run
// transitions (through the same ValidWorkflowStepTransition/
// ValidWorkflowRunTransition guards every other workflow write goes
// through), an append-only checkpoint carrying the newly observed
// fingerprint (when resolved), and, for a resolved or failed outcome, the
// current cycle's workflow_attempt outcome/error_class.
func (c *Coordinator) recordFixOutcome(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	nextStep domain.WorkflowStepState,
	nextRun domain.WorkflowRunState,
	fingerprintAfter string,
	resolved bool,
	nextAction string,
	errClass domain.WorkflowErrorClass,
	observed *ports.WorkspaceObservation,
) (domain.WorkflowStep, error) {
	now := c.clock()

	if domain.ValidWorkflowStepTransition(step.State, nextStep) {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, nextStep, now); err != nil {
			return step, err
		}
		step.State = nextStep
	} else if c.log != nil {
		c.log.Info("workflow: skipping invalid fix-step observation transition (benign race)",
			"step", step.ID, "from", step.State, "to", nextStep)
	}

	if domain.ValidWorkflowRunTransition(run.State, nextRun) {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, nextRun, now); err != nil {
			return step, err
		}
	} else if c.log != nil && run.State != nextRun {
		c.log.Info("workflow: skipping invalid run transition from fix-step observation (benign race)",
			"run", run.ID, "from", run.State, "to", nextRun)
	}

	stepID := step.ID
	// The workspace identity the fingerprint was computed FROM, not just the
	// fingerprint. A WorkspaceFingerprint hashes head_sha among its inputs, so
	// a fingerprint names exactly one commit — but only this row can say which
	// one. Without it, the only durable (fingerprint -> HEAD) binding AO held
	// was the work step's own completion checkpoint, and every fix cycle that
	// committed left that binding silently stale. That is the whole of the
	// wf-a21d98aa incident: verification resolved the approved commit of a
	// third-cycle approval to the FIRST cycle's HEAD, concluded the branch had
	// advanced when it had not, and parked a run whose drift was its own
	// authorized fix worker's. See approvedHeadSHA.
	headSHA, branch, worktreePath := "", "", ""
	if observed != nil {
		headSHA, branch, worktreePath = observed.HeadSHA, observed.Branch, observed.Path
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:               "wfc-" + c.newID(),
		WorkflowRunID:    run.ID,
		WorkflowStepID:   &stepID,
		ProjectID:        run.ProjectID,
		Branch:           branch,
		WorktreePath:     worktreePath,
		HeadSHA:          headSHA,
		FingerprintAfter: fingerprintAfter,
		NextAction:       nextAction,
		DurablePhase:     "fix_observed_" + string(nextStep),
		PayloadVersion:   "v1",
		RetryState:       "{}",
		CreatedAt:        now,
	}); err != nil {
		return step, err
	}

	if resolved || errClass != "" || nextStep.Terminal() {
		latestAttempt, hasAttempt, aerr := c.store.GetLatestWorkflowAttempt(ctx, step.ID)
		if aerr == nil && hasAttempt {
			finishedAt := time.Time{}
			if latestAttempt.FinishedAt != nil {
				finishedAt = *latestAttempt.FinishedAt
			}
			outcome := latestAttempt.Outcome
			ec := latestAttempt.ErrorClass
			switch {
			case resolved:
				outcome = domain.WorkflowAttemptSucceeded
				finishedAt = now
			case nextStep == domain.WorkflowStepFailed:
				outcome = domain.WorkflowAttemptFailed
				finishedAt = now
			}
			if errClass != "" {
				ec = errClass
			}
			_ = c.store.UpdateWorkflowAttemptOutcome(ctx, latestAttempt.ID, finishedAt, outcome, ec)
		}
	}

	return step, nil
}

// observationOrNil hands on an observation only when one was actually obtained.
// A zero WorkspaceObservation would be recorded as a clean tree, which is the
// one thing an unobserved worktree must never be read as.
func observationOrNil(obs ports.WorkspaceObservation, ok bool) *ports.WorkspaceObservation {
	if !ok {
		return nil
	}
	return &obs
}

// concludeEndedFixSession is the one ending a fix cycle's worker session can
// have once it provably stopped running: a new fingerprint delivers the cycle,
// an unchanged one fails it. A workspace AO cannot read concludes nothing.
func (c *Coordinator) concludeEndedFixSession(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	sess domain.SessionRecord, fingerprintBefore string,
) (domain.WorkflowStep, error) {
	obs, ok := c.observeFixWorkspace(ctx, sess)
	if !ok {
		return step, nil
	}
	fp := WorkspaceFingerprint(obs)
	if fp != fingerprintBefore {
		return c.recordFixOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunWaiting,
			fp, true, "fix delivered (worker session ended) — awaiting next review cycle", "", &obs)
	}
	return c.stopFix(ctx, run, step, domain.WorkflowStepFailed, ReasonFixNoVerifiableChange,
		"fix worker session terminated with no verifiable change (no dirty, staged, or untracked change, and fingerprint unchanged)",
		domain.WorkflowErrorWorkerTerminatedUnexpectedly)
}

// observeActiveFixSilence is P9's liveness for a fix cycle delivered into the
// worker's EXISTING session (§18).
//
// The fix step owns no session row of its own, so nothing else watches it: a fix
// agent whose process died while its row still read `active` stayed "running"
// forever. The liveness clock is the session's coalesced signal clock
// (Activity.Liveness), floored at this cycle's own dispatch so the previous
// cycle's signals can never make this one look heard-from (or silent).
//
// Silence is never the conclusion. Past fixActiveSilenceWindow AO asks the
// runtime, and:
//
//   - owned and running  -> quiet but alive (a long tool call, a slept laptop):
//     nothing is written;
//   - provably gone      -> the session ended; concluded exactly like a
//     terminated row, on workspace evidence;
//   - anything unprovable (no port, a mismatch, an unread probe) -> nothing is
//     written. An unknown probe is never proof a session is dead.
func (c *Coordinator) observeActiveFixSilence(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	sess domain.SessionRecord, dispatchedAt time.Time, fingerprintBefore string, now time.Time,
) (domain.WorkflowStep, error) {
	heard := sess.Activity.Liveness()
	if heard.Before(dispatchedAt) {
		heard = dispatchedAt
	}
	if now.Sub(heard) <= fixActiveSilenceWindow {
		return step, nil
	}
	obs := c.observeWorkerRuntime(ctx, sess.ID)
	if obs == nil || !obs.Proof.ExecutionProvenGone() {
		return step, nil
	}
	if c.log != nil {
		c.log.Warn("workflow: an active fix cycle went silent and its runtime is provably gone",
			"run", run.ID, "step", step.ID, "session", sess.ID, "proof", obs.Proof, "silent", now.Sub(heard).Round(time.Second))
	}
	return c.concludeEndedFixSession(ctx, run, step, sess, fingerprintBefore)
}
