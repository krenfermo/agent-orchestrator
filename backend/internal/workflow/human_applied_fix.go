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

// resumeHumanAppliedFix reopens review for a run whose workspace changed after
// its fix budget ran out.
//
// It lives in ContinueRun and nowhere else, for the same reason every other
// resume rule here does: THIS call is a person saying "I have dealt with it",
// and a rule that re-opens review must never fire off a 2s read poll. It is a
// no-op for every run without the exact durable shape below, and every
// precondition is re-derived now rather than trusted from the recorded stop.
func (c *Coordinator) resumeHumanAppliedFix(ctx stdctx.Context, run domain.WorkflowRun) (domain.WorkflowRun, bool, error) {
	if run.State != domain.WorkflowRunNeedsAttention {
		return run, false, nil
	}
	if c.sessionFacts == nil || c.workspaceFacts == nil || c.reviewRuns == nil {
		return run, false, nil
	}
	reason, _, ok := c.stopReason(ctx, run)
	if !ok || reason != ReasonFixBudgetExhausted {
		return run, false, nil
	}

	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return run, false, err
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
		return run, false, nil
	}
	// The exact resting shape of a budget-exhausted stop. Anything in motion —
	// a running fix, a running review, a work step still going — means the run
	// is not waiting on a person and this rule has no business acting.
	if workStep.State != domain.WorkflowStepCompleted ||
		reviewStep.State != domain.WorkflowStepWaiting ||
		fixStep.State != domain.WorkflowStepWaiting {
		return run, false, nil
	}
	if verifyStep != nil && verifyStep.State == domain.WorkflowStepRunning {
		return run, false, nil
	}
	if open, qerr := c.hasOpenQuestion(ctx, run.ID, nil); qerr != nil || open {
		return run, false, qerr
	}

	// There must be a previous review that asked for changes: this recovery is
	// about a finding somebody addressed, not about a run that never got one.
	if reviewStep.ReviewRunID == nil {
		return run, false, nil
	}
	prevReview, found, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
	if err != nil {
		return run, false, err
	}
	if !found || prevReview.Verdict != domain.VerdictChangesRequested {
		return run, false, nil
	}

	// The workspace, read now, must belong to this run and must differ from
	// what the last review judged.
	workCP, hasCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID)
	if err != nil {
		return run, false, err
	}
	if !hasCP || workCP.SessionID == nil || *workCP.SessionID == "" {
		return run, false, nil
	}
	sessionID := *workCP.SessionID
	sess, sfound, err := c.sessionFacts.GetSession(ctx, domain.SessionID(sessionID))
	if err != nil {
		return run, false, err
	}
	if !sfound {
		return run, false, nil
	}
	obs, obsOK := c.observeFixWorkspace(ctx, sess)
	if !obsOK {
		// Cannot read the workspace, so cannot prove anything changed. Failing
		// to read is never evidence.
		return run, false, nil
	}
	newFingerprint := WorkspaceFingerprint(obs)
	oldFingerprint := c.fingerprintThatExhaustedTheBudget(ctx, run.ID, fixStep.ID, prevReview)
	if oldFingerprint == "" || newFingerprint == "" || newFingerprint == oldFingerprint {
		return run, false, nil
	}
	// The worktree and branch must be the ones this run worked in. A change in
	// some other tree is not this run's fix.
	if !sameWorkspaceIdentity(sess, workCP) {
		return run, false, nil
	}

	stoppedAt, hasStop := c.stopRecordedAt(ctx, run.ID, ReasonFixBudgetExhausted)
	if !hasStop {
		return run, false, nil
	}
	// "Not something of AO's that is still moving." AO cannot see the edit
	// itself, but it can see whether its own worker could still be delivering.
	// If it could, the change is ambiguous and this rule must not adopt it.
	if agentMayStillBeDelivering(sess, c.clock()) {
		return run, false, nil
	}

	// Idempotence: one fingerprint, at most one fresh review. A repeated
	// Continue, a poll or a restart re-derives the same new fingerprint and
	// finds it already recorded.
	generation, alreadyRecorded := c.humanAppliedFixState(ctx, run.ID, newFingerprint)
	if alreadyRecorded {
		return run, false, nil
	}
	if generation > maxHumanAppliedFixRecoveries {
		return run, false, nil
	}
	// And the reviewer must not already be looking at exactly this content.
	if prevReview.TargetSHA == newFingerprint {
		return run, false, nil
	}

	rec := humanAppliedFixRecord{
		OldFingerprint: oldFingerprint, NewFingerprint: newFingerprint,
		PreviousReviewRunID: prevReview.ID,
		Attribution:         "external intervention (AO observed the change; it cannot attribute it to a person)",
		ObservedAt:          c.clock(), Generation: generation, StoppedAt: stoppedAt,
		SilentFor: workerSilence(sess, c.clock()).Round(time.Second).String(),
		SessionID: sessionID, Branch: sess.Metadata.Branch, Worktree: sess.Metadata.WorkspacePath,
	}
	// Written on the FIX step, carrying the new fingerprint, because that is
	// precisely the durable fact the next review's gate reads. It is NOT a fix
	// attempt: no workflow_attempt row, no cycle consumed, no budget touched.
	// The ledger says an external change was observed, which is what happened.
	stepID := fixStep.ID
	rid := prevReview.ID
	sid := sessionID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		SessionID:         &sid,
		ReviewRunID:       &rid,
		FingerprintBefore: oldFingerprint,
		FingerprintAfter:  newFingerprint,
		NextAction: fmt.Sprintf(
			"human_applied_fix_observed: the workspace changed after the fix budget was exhausted (recovery %d of %d) — re-opening an independent review of what is there now; no fix cycle was consumed",
			generation, maxHumanAppliedFixRecoveries),
		DurablePhase:   humanAppliedFixPhase,
		PayloadVersion: "v1",
		RetryState:     rec.json(),
		CreatedAt:      c.clock(),
	}); err != nil {
		return run, false, err
	}

	run = c.unparkRun(ctx, run, ReasonFixBudgetExhausted,
		"the workspace changed after the budget was exhausted, so a fresh independent review is due")
	if c.log != nil {
		c.log.Info("workflow: observed an externally applied fix after the fix budget was exhausted",
			"run", run.ID, "generation", generation,
			"oldFingerprint", shortFingerprint(oldFingerprint), "newFingerprint", shortFingerprint(newFingerprint),
			"previousReview", prevReview.ID)
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
