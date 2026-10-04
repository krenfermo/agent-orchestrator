package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// human_applied_fix.go — Checkpoint 8P-E.22.
//
// The dead end this removes, found on wf-c23a4b0c:
//
//	run=needs_attention  reason=fix_budget_exhausted
//	review=waiting  fix=waiting  verify=pending
//
// The reviewer kept requesting changes, the fix budget ran out, and AO stopped —
// correctly. Its own advice for that stop is "raise the fix budget, apply the
// changes yourself, or cancel". So a person applied the change themselves, in
// the run's own worktree, and then had nowhere to put it: POST /continue
// answered 200 and did nothing, because a cycle-N+1 review is gated on the FIX
// STEP recording a new fingerprint, and only a fix worker writes one. With the
// budget spent no fix worker can run, so the gate could never open again.
//
// The missing transition is not "give the run more budget". It is the honest
// observation that the workspace changed after the stop, by something other
// than a fix cycle:
//
//	fix_budget_exhausted
//	  -> human_applied_fix_observed   (a new fingerprint, recorded)
//	  -> a fresh INDEPENDENT review of what is actually there now
//	  -> ordinary verification
//	  -> completed, if it passes
//
// What it deliberately does not do is as important as what it does. It does not
// raise maxFixCycles, so the budget still means what it said. It does not create
// a fix attempt, so the ledger never claims an agent did work a person did. It
// does not skip the reviewer — the whole point is to get the change in front of
// one, because nobody has reviewed it yet. And it never asserts WHO made the
// change: AO cannot prove that, so the record says "external intervention".

// humanAppliedFixPhase is the durable record of that observation.
const humanAppliedFixPhase = "human_applied_fix_observed"

// maxHumanAppliedFixRecoveries bounds how many times one run may be rescued
// this way. A person who has now corrected the same run three times without it
// ever passing review is not being helped by a fourth silent re-review.
const maxHumanAppliedFixRecoveries = 3

// humanFixSettleWindow is how long this run's own worker must have been silent
// before a workspace change may be treated as an outside intervention.
//
// The hazard this guards is a delivery still landing: an agent mid-turn whose
// output is arriving while AO looks. It is emphatically NOT "the agent did
// anything at all since the stop" — a worker that finished a turn shortly after
// the budget ran out and has said nothing for hours is not in flight, and
// refusing on that basis strands exactly the run this recovery exists for
// (wf-c23a4b0c: last turn 21:26, stop 21:14, silent ever since).
//
// It mirrors fixCyclePickupTimeout deliberately: the two answer the same
// question — "could this agent still be about to act?" — and they should not
// disagree about how long that takes.
const humanFixSettleWindow = 10 * time.Minute

// humanAppliedFixRecord is what the ledger keeps. Every field is something AO
// observed rather than something it was told.
type humanAppliedFixRecord struct {
	OldFingerprint      string `json:"oldFingerprint"`
	NewFingerprint      string `json:"newFingerprint"`
	PreviousReviewRunID string `json:"previousReviewRunId"`
	// Attribution is deliberately coarse. AO can prove the workspace changed
	// and that no agent of this run was running when it did; it cannot prove a
	// person did it, so it does not say so.
	Attribution string    `json:"attribution"`
	ObservedAt  time.Time `json:"observedAt"`
	Generation  int       `json:"recoveryGeneration"`
	// StoppedAt is when the budget-exhausted stop was recorded, which is what
	// makes "after the stop" a checkable claim rather than an assumption.
	StoppedAt time.Time `json:"stoppedAt"`
	// SilentFor is how long this run's worker had been quiet when the change
	// was adopted, so "nothing of AO's was in flight" is a checkable claim
	// rather than an assertion.
	SilentFor string `json:"workerSilentFor,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Worktree  string `json:"worktree,omitempty"`
}

func (r humanAppliedFixRecord) json() string {
	b, err := json.Marshal(r)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// externalFixAdoption is what evaluateExternalAppliedFix concluded, WITHOUT
// having changed anything. Splitting the decision from the write is what lets
// the same rule answer two different questions with one implementation: "should
// this Continue re-open review?" (resumeHumanAppliedFix, which then writes) and
// "is there an unreviewed authoritative state this run is being held away from?"
// (head_convergence.go's read-only probe, which then routes the run into the one
// mutating path rather than opening a second one).
type externalFixAdoption struct {
	// Adoptable is true only when every precondition below holds right now.
	Adoptable bool
	// Reason names, in AO's own words, what was concluded — including why not.
	Reason string

	OldFingerprint string
	NewFingerprint string
	PreviousReview domain.ReviewRun
	Generation     int
	StoppedAt      time.Time

	session domain.SessionRecord
	fixStep domain.WorkflowStep
	silent  time.Duration
}

// resumeHumanAppliedFix reopens review for a run whose workspace changed after
// its fix budget ran out.
//
// It is the ONLY writer for this transition, and every entry point — the
// person's Continue, the parent objective's reconcile, boot recovery — reaches
// it through ContinueRun rather than duplicating it, so one new authoritative
// state can never produce two fresh reviews. Every precondition is re-derived
// now rather than trusted from the recorded stop.
func (c *Coordinator) resumeHumanAppliedFix(ctx stdctx.Context, run domain.WorkflowRun) (domain.WorkflowRun, bool, error) {
	decision, err := c.evaluateExternalAppliedFix(ctx, run)
	if err != nil || !decision.Adoptable {
		return run, false, err
	}
	return c.adoptExternalAppliedFix(ctx, run, decision)
}

// evaluateExternalAppliedFix decides, and writes nothing.
func (c *Coordinator) evaluateExternalAppliedFix(ctx stdctx.Context, run domain.WorkflowRun) (externalFixAdoption, error) {
	no := func(reason string) (externalFixAdoption, error) {
		return externalFixAdoption{Reason: reason}, nil
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		return no("the run is not parked, so there is no stop to recover from")
	}
	if c.sessionFacts == nil || c.workspaceFacts == nil || c.reviewRuns == nil {
		return no("this configuration has no session/workspace/review facts to decide on")
	}
	reason, _, ok := c.stopReason(ctx, run)
	if !ok || reason != ReasonFixBudgetExhausted {
		return no("the run is not stopped on fix_budget_exhausted")
	}

	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return externalFixAdoption{}, err
	}
	var workStep, reviewStep, fixStep, verifyStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		case domain.WorkflowStepReview:
			reviewStep = &steps[i]
		case domain.WorkflowStepFix:
			fixStep = &steps[i]
		case domain.WorkflowStepVerify:
			verifyStep = &steps[i]
		}
	}
	if workStep == nil || reviewStep == nil || fixStep == nil {
		return no("this run has no work/review/fix step triple to recover")
	}
	// The exact resting shape of a budget-exhausted stop. Anything in motion —
	// a running fix, a running review, a work step still going — means the run
	// is not waiting on a person and this rule has no business acting.
	if workStep.State != domain.WorkflowStepCompleted ||
		reviewStep.State != domain.WorkflowStepWaiting ||
		fixStep.State != domain.WorkflowStepWaiting {
		return no("the run's steps are not at the resting shape of a budget-exhausted stop")
	}
	if verifyStep != nil && verifyStep.State == domain.WorkflowStepRunning {
		return no("verification is running, so the run is not waiting on anybody")
	}
	if open, qerr := c.hasOpenQuestion(ctx, run.ID, nil); qerr != nil {
		return externalFixAdoption{}, qerr
	} else if open {
		return no("this run has an open question, which is a person's to answer first")
	}

	// There must be a previous review that asked for changes: this recovery is
	// about a finding somebody addressed, not about a run that never got one.
	if reviewStep.ReviewRunID == nil {
		return no("the review step names no review run, so there is no verdict to supersede")
	}
	prevReview, found, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
	if err != nil {
		return externalFixAdoption{}, err
	}
	if !found || prevReview.EffectiveVerdict() != domain.VerdictChangesRequested {
		return no("the authoritative review did not request changes")
	}

	// The workspace, read now, must belong to this run and must differ from
	// what the last review judged.
	workCP, hasCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID)
	if err != nil {
		return externalFixAdoption{}, err
	}
	if !hasCP || workCP.SessionID == nil || *workCP.SessionID == "" {
		return no("AO has no durable record of which session owns this run's workspace")
	}
	sessionID := *workCP.SessionID
	sess, sfound, err := c.sessionFacts.GetSession(ctx, domain.SessionID(sessionID))
	if err != nil {
		return externalFixAdoption{}, err
	}
	if !sfound {
		return no("the session that owns this run's workspace no longer exists")
	}
	obs, obsOK := c.observeFixWorkspace(ctx, sess)
	if !obsOK {
		// Cannot read the workspace, so cannot prove anything changed. Failing
		// to read is never evidence.
		return no("AO could not read the workspace, and failing to read is never evidence that it changed")
	}
	newFingerprint := WorkspaceFingerprint(obs)
	oldFingerprint := c.fingerprintThatExhaustedTheBudget(ctx, run.ID, fixStep.ID, prevReview)
	if oldFingerprint == "" || newFingerprint == "" || newFingerprint == oldFingerprint {
		return no("the workspace is exactly the state the last review judged")
	}
	// The worktree and branch must be the ones this run worked in. A change in
	// some other tree is not this run's fix.
	if !sameWorkspaceIdentity(sess, workCP) {
		return no("the observed workspace is not the one this run owns")
	}

	stoppedAt, hasStop := c.stopRecordedAt(ctx, run.ID, ReasonFixBudgetExhausted)
	if !hasStop {
		return no("AO has no durable record of when this run stopped")
	}
	// "Not something of AO's that is still moving." AO cannot see the edit
	// itself, but it can see whether its own worker could still be delivering.
	// If it could, the change is ambiguous and this rule must not adopt it.
	if agentMayStillBeDelivering(sess, c.clock()) {
		return no("this run's own worker may still be delivering, so the change's provenance is ambiguous")
	}

	// Idempotence: one fingerprint, at most one fresh review. A repeated
	// Continue, a poll or a restart re-derives the same new fingerprint and
	// finds it already recorded.
	generation, alreadyRecorded := c.humanAppliedFixState(ctx, run.ID, newFingerprint)
	if alreadyRecorded {
		return no("this exact workspace state has already been adopted and reviewed")
	}
	if generation > maxHumanAppliedFixRecoveries {
		return no(fmt.Sprintf("this run has already used its %d external-fix recoveries", maxHumanAppliedFixRecoveries))
	}
	// And the reviewer must not already be looking at exactly this content.
	if prevReview.TargetSHA == newFingerprint {
		return no("the authoritative review already targets exactly this workspace state")
	}

	return externalFixAdoption{
		Adoptable:      true,
		Reason:         "the workspace changed after the budget was exhausted, so a fresh independent review is due",
		OldFingerprint: oldFingerprint,
		NewFingerprint: newFingerprint,
		PreviousReview: prevReview,
		Generation:     generation,
		StoppedAt:      stoppedAt,
		session:        sess,
		fixStep:        *fixStep,
		silent:         workerSilence(sess, c.clock()),
	}, nil
}

// adoptExternalAppliedFix writes the adoption and un-parks the run. It is
// reached only from resumeHumanAppliedFix, so the ledger has exactly one writer
// for this transition.
func (c *Coordinator) adoptExternalAppliedFix(
	ctx stdctx.Context, run domain.WorkflowRun, d externalFixAdoption,
) (domain.WorkflowRun, bool, error) {
	sess := d.session
	rec := humanAppliedFixRecord{
		OldFingerprint: d.OldFingerprint, NewFingerprint: d.NewFingerprint,
		PreviousReviewRunID: d.PreviousReview.ID,
		Attribution:         "external intervention (AO observed the change; it cannot attribute it to a person)",
		ObservedAt:          c.clock(), Generation: d.Generation, StoppedAt: d.StoppedAt,
		SilentFor: d.silent.Round(time.Second).String(),
		SessionID: string(sess.ID), Branch: sess.Metadata.Branch, Worktree: sess.Metadata.WorkspacePath,
	}
	// Written on the FIX step, carrying the new fingerprint, because that is
	// precisely the durable fact the next review's gate reads. It is NOT a fix
	// attempt: no workflow_attempt row, no cycle consumed, no budget touched.
	// The ledger says an external change was observed, which is what happened.
	stepID := d.fixStep.ID
	rid := d.PreviousReview.ID
	sid := string(sess.ID)
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		SessionID:         &sid,
		ReviewRunID:       &rid,
		FingerprintBefore: d.OldFingerprint,
		FingerprintAfter:  d.NewFingerprint,
		NextAction: fmt.Sprintf(
			"human_applied_fix_observed: the workspace changed after the fix budget was exhausted (recovery %d of %d) — re-opening an independent review of what is there now; no fix cycle was consumed",
			d.Generation, maxHumanAppliedFixRecoveries),
		DurablePhase:   humanAppliedFixPhase,
		PayloadVersion: "v1",
		RetryState:     rec.json(),
		CreatedAt:      c.clock(),
	}); err != nil {
		return run, false, err
	}

	run = c.unparkRun(ctx, run, ReasonFixBudgetExhausted, d.Reason)
	if c.log != nil {
		c.log.Info("workflow: observed an externally applied fix after the fix budget was exhausted",
			"run", run.ID, "generation", d.Generation,
			"oldFingerprint", shortFingerprint(d.OldFingerprint), "newFingerprint", shortFingerprint(d.NewFingerprint),
			"previousReview", d.PreviousReview.ID)
	}
	if refreshed, ok2, rerr := c.store.GetWorkflowRun(ctx, run.ID); rerr == nil && ok2 {
		run = refreshed
	}
	return run, true, nil
}

// fingerprintThatExhaustedTheBudget is the content the last review actually
// judged — the thing a new fingerprint has to differ from.
//
// The review run's own TargetSHA is preferred because it is what the reviewer
// was pointed at; the fix step's last recorded FingerprintAfter is the fallback
// for a run whose review predates that field being populated.
func (c *Coordinator) fingerprintThatExhaustedTheBudget(ctx stdctx.Context, runID, fixStepID string, prevReview domain.ReviewRun) string {
	if fp := strings.TrimSpace(prevReview.TargetSHA); fp != "" {
		return fp
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return ""
	}
	fp := ""
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != fixStepID {
			continue
		}
		if cp.FingerprintAfter != "" {
			fp = cp.FingerprintAfter
		}
	}
	return fp
}

// stopRecordedAt returns when this run's current stop was written.
func (c *Coordinator) stopRecordedAt(ctx stdctx.Context, runID, reason string) (time.Time, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return time.Time{}, false
	}
	var at time.Time
	for _, cp := range cps {
		if cp.DurablePhase == reason && cp.CreatedAt.After(at) {
			at = cp.CreatedAt
		}
	}
	return at, !at.IsZero()
}

// agentMayStillBeDelivering reports whether this run's own worker could still be
// putting something into the workspace.
//
// Two things make that true: it is active right now, or it has spoken recently
// enough that a turn could still be completing. Anything older is a worker that
// has stopped, and a change appearing after that is not its delivery.
//
// Ambiguity is always a refusal here — but "it did something an hour ago" is not
// ambiguity, it is history.
func agentMayStillBeDelivering(sess domain.SessionRecord, now time.Time) bool {
	if sess.Activity.State == domain.ActivityActive {
		return true
	}
	for _, at := range []time.Time{sess.Activity.LastActivityAt, sess.TurnCompletedAt} {
		if !at.IsZero() && now.Sub(at) < humanFixSettleWindow {
			return true
		}
	}
	return false
}

// sameWorkspaceIdentity checks the observed workspace is the one this run owns.
func sameWorkspaceIdentity(sess domain.SessionRecord, workCP domain.WorkflowCheckpoint) bool {
	if strings.TrimSpace(sess.Metadata.WorkspacePath) == "" {
		return false
	}
	if wt := strings.TrimSpace(workCP.WorktreePath); wt != "" && wt != strings.TrimSpace(sess.Metadata.WorkspacePath) {
		return false
	}
	if br := strings.TrimSpace(workCP.Branch); br != "" && strings.TrimSpace(sess.Metadata.Branch) != "" && br != strings.TrimSpace(sess.Metadata.Branch) {
		return false
	}
	return true
}

// humanAppliedFixState folds the ledger into "which recovery would this be, and
// has this exact fingerprint already been recorded".
func (c *Coordinator) humanAppliedFixState(ctx stdctx.Context, runID, newFingerprint string) (generation int, alreadyRecorded bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// A read failure must never look like "not yet recorded", which would
		// let a repeated Continue open a second review for one fingerprint.
		return 0, true
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase != humanAppliedFixPhase {
			continue
		}
		n++
		var rec humanAppliedFixRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.NewFingerprint == newFingerprint {
			return n, true
		}
	}
	return n + 1, false
}

// workerSilence is how long the run's worker has said nothing.
func workerSilence(sess domain.SessionRecord, now time.Time) time.Duration {
	latest := sess.Activity.LastActivityAt
	if sess.TurnCompletedAt.After(latest) {
		latest = sess.TurnCompletedAt
	}
	if latest.IsZero() {
		return 0
	}
	return now.Sub(latest)
}
