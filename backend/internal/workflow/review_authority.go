package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// review_authority.go — which review speaks for a review step, and what to do
// when the one it points at has stopped speaking.
//
// The incident (wf-756988ae, review run 4ac56ac5). AO's reviewer-stall
// detection decided a still-working Codex reviewer had stalled, and closed its
// review run out as `cancelled`. The reviewer then finished, found no
// task-scoped defect, and submitted `approved` through the official command.
// AO answered REVIEW_INVALID — "review run … is not running" — and destroyed
// the verdict. The review step was left at `waiting`, pointing at that same
// cancelled run, and it never moved again.
//
// Three separate defects produced that, and this file is the third of the three
// fixes:
//
//  1. the verdict was UNRECORDABLE. Fixed at the source: submitOne now preserves
//     a late verdict instead of rejecting it (migration 0135).
//  2. the step could never re-dispatch. Fixed in review_dispatch.go: the
//     fix-cycle idempotency guard treated "a run exists for this fingerprint" as
//     "this fingerprint has been reviewed", even for a run with no verdict.
//  3. nothing ever LOOKED at the contradiction. observeReviewStep only runs for
//     a review step at `running`; a step resting at `waiting` over a terminal
//     run was seen by nobody, at boot or on wake. That is this file.
//
// The authority model, stated once:
//
//	workflow_steps.review_run_id IS the authority pointer. Exactly one review
//	run speaks for a review step at any instant: the one that column names.
//
// Everything else follows from it. A verdict recorded while a run was
// authoritative is a decision. A verdict that arrived after AO closed the run
// out is EVIDENCE — adoptable if and only if that run is still the one the step
// points at, and inert forever once a replacement has been bound (which is why
// recordReviewDispatchSuccess marks the outgoing run superseded before it
// rebinds the step). A late verdict therefore can never overwrite a newer
// authoritative review, and a newer review can never silently discard work a
// reviewer really did.

// reviewRunStillSpeaks reports whether a review run is still capable of
// concluding the step it is attached to: it is running, or it already recorded
// a verdict while it was authoritative.
//
// A run AO closed out without a verdict does not speak. That is the whole of
// defect (2): treating such a run as an answer is what makes a review step
// unable to ask the question again.
func reviewRunStillSpeaks(run domain.ReviewRun) bool {
	if run.HasDurableVerdict() {
		return true
	}
	return run.Status == domain.ReviewRunRunning
}

// liftRestingReviewStep brings a review step back to `running` before a verdict
// is applied to it.
//
// It is required, not tidy: recordReviewOutcome's transitions start from where
// the step actually is, and `waiting -> completed` is not a legal edge. A
// verdict applied to a resting step without this lift is silently dropped — the
// step stays exactly where it was, which is the unreachable state the caller was
// trying to resolve.
func (c *Coordinator) liftRestingReviewStep(
	ctx stdctx.Context, step domain.WorkflowStep, authorityRunID string,
) (domain.WorkflowStep, error) {
	if step.State != domain.WorkflowStepWaiting {
		return step, nil
	}
	if authorityRunID != "" {
		moved, err := c.store.UpdateWorkflowStepStateIfReviewRun(ctx, step.ID,
			domain.WorkflowStepWaiting, domain.WorkflowStepRunning, authorityRunID, c.clock())
		if err != nil {
			return step, err
		}
		if !moved {
			return step, errReviewAuthorityLost
		}
		step.State = domain.WorkflowStepRunning
		return step, nil
	}
	if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID,
		domain.WorkflowStepWaiting, domain.WorkflowStepRunning, c.clock()); err != nil {
		return step, err
	}
	step.State = domain.WorkflowStepRunning
	return step, nil
}

// ReviewAuthorityOutcome is what reconciliation concluded about a review step's
// authority pointer.
type ReviewAuthorityOutcome string

const (
	// ReviewAuthorityIntact means the pointer still names a run that speaks:
	// running, or concluded with a verdict. Nothing to do.
	ReviewAuthorityIntact ReviewAuthorityOutcome = "intact"
	// ReviewAuthorityLateVerdictAdopted means the run AO gave up on had in fact
	// produced a verdict, it is still the authoritative pointer, and that
	// verdict has now been adopted as the step's real outcome.
	ReviewAuthorityLateVerdictAdopted ReviewAuthorityOutcome = "late_verdict_adopted"
	// ReviewAuthorityLateVerdictRefused means the run produced a verdict after
	// AO closed it out, and AO can PROVE that verdict can never become this
	// step's outcome — the step has ended, or was never dispatched. It is
	// recorded once as unadoptable and never retried. See
	// late_verdict_disposition.go.
	ReviewAuthorityLateVerdictRefused ReviewAuthorityOutcome = "late_verdict_refused"
	// ReviewAuthorityRebindPending means the pointer names a closed-out run with
	// no verdict of any kind, and the step has been released so the ordinary
	// bounded review dispatch can bind exactly one replacement.
	ReviewAuthorityRebindPending ReviewAuthorityOutcome = "rebind_pending"
	// ReviewAuthorityStopped means the bounded retry budget for rebinding this
	// step is spent, and AO stopped for a person with the evidence.
	ReviewAuthorityStopped ReviewAuthorityOutcome = "stopped"
	// ReviewAuthorityNotApplicable means there was nothing to reconcile.
	ReviewAuthorityNotApplicable ReviewAuthorityOutcome = "not_applicable"
)

// Resolved reports whether reconciliation changed durable state.
func (o ReviewAuthorityOutcome) Resolved() bool {
	switch o {
	case ReviewAuthorityLateVerdictAdopted, ReviewAuthorityLateVerdictRefused,
		ReviewAuthorityRebindPending, ReviewAuthorityStopped:
		return true
	default:
		return false
	}
}

// reviewAuthorityRebindPhase is the durable record of one rebind authorization:
// which run was abandoned, what it was reviewing, and which generation of the
// retry this is.
//
// It exists because review_capacity_retry is NOT sufficient to reconstruct the
// intended retry after a restart — that checkpoint records the sentence
// "retrying per execution policy" and a `{}` payload, naming neither the run it
// closed out nor the target it was reviewing. Anything reading it after a
// restart knows a retry was intended and nothing about what to retry.
const reviewAuthorityRebindPhase = "review_authority_rebind"

// lateVerdictAdoptedPhase records that a run's late verdict became this step's
// outcome. One per (step, review run), and the durable proof that adoption has
// already happened.
const lateVerdictAdoptedPhase = "review_late_verdict_adopted"

// maxReviewAuthorityRebinds bounds how many times reconciliation may release a
// review step for a replacement. It is what makes requirement "repeated
// reconciliation must not create unlimited reviewer sessions" structural rather
// than hopeful: each rebind is durably counted, the count survives restarts, and
// a spent budget stops for a person instead of looping.
const maxReviewAuthorityRebinds = 3

// reviewAuthorityRebindRecord is the decoded payload of that checkpoint.
type reviewAuthorityRebindRecord struct {
	// Generation counts rebinds for this step, 1-based.
	Generation int `json:"generation"`
	// AbandonedRunID is the review run whose authority was released.
	AbandonedRunID string `json:"abandonedRunId"`
	// AbandonedStatus is the terminal status AO had given it.
	AbandonedStatus string `json:"abandonedStatus"`
	// TargetSHA is what that run was reviewing, so the replacement reviews the
	// same thing rather than drifting.
	TargetSHA string `json:"targetSha"`
	// Harness is the provider that was reviewing.
	Harness string `json:"harness"`
	// Reason names why authority was released.
	Reason string `json:"reason"`
}

// ReconcileReviewAuthority is the boot- and wake-time answer to "does this
// review step still have a review that can conclude it".
//
// It is deliberately narrow. It never launches a reviewer, never cancels a
// running one, and never touches a step whose pointer is intact — dispatch and
// observation keep every responsibility they already had. All it does is
// resolve the one contradiction neither of them can see: a step waiting on a
// review that has stopped speaking.
func (c *Coordinator) ReconcileReviewAuthority(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
) (ReviewAuthorityOutcome, domain.WorkflowStep, domain.WorkflowRun, error) {
	if reviewStep.Kind != domain.WorkflowStepReview ||
		run.State.Terminal() {
		return ReviewAuthorityNotApplicable, reviewStep, run, nil
	}
	if c.reviewRuns == nil || reviewStep.ReviewRunID == nil || *reviewStep.ReviewRunID == "" {
		return ReviewAuthorityNotApplicable, reviewStep, run, nil
	}
	current, found, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
	if err != nil {
		return ReviewAuthorityNotApplicable, reviewStep, run, err
	}
	if !found {
		// A pointer that does not resolve is ambiguity, not absence, and
		// observeReviewStep already owns that reading for a running step. Leave
		// it: inventing a rebind from an unreadable pointer would be guessing.
		return ReviewAuthorityNotApplicable, reviewStep, run, nil
	}

	// A late verdict is the one case reconciliation owns even when the step is
	// running or terminal. Adoption spans several existing store calls, so a
	// crash may leave either side durable first. The completion marker is valid
	// only together with a matching review_observed row and the step state that
	// verdict requires. Old marker-first rows and new state-first interruptions
	// are both repaired here.
	if current.TerminalWithoutVerdict() && current.LateVerdict.Valid() && current.SupersededBy == "" {
		marker, reflected, serr := c.lateVerdictAdoptionState(ctx, run.ID, reviewStep, current)
		if serr != nil {
			return ReviewAuthorityNotApplicable, reviewStep, run, serr
		}
		if reflected {
			if marker {
				return ReviewAuthorityIntact, reviewStep, run, nil
			}
			if err := c.recordLateVerdictAdopted(ctx, run, reviewStep, current); err != nil {
				return ReviewAuthorityNotApplicable, reviewStep, run, err
			}
			return ReviewAuthorityLateVerdictAdopted, reviewStep, run, nil
		}
		// A verdict AO has already refused is DECIDED. Re-entering adoption for
		// it is what turned wf-c4c84f52's unapplicable approval into a
		// three-hour retry loop, so the refusal is checked before anything is
		// attempted rather than rediscovered by attempting it.
		if c.lateVerdictAlreadyDisposed(ctx, run.ID, reviewStep.ID, current.ID) {
			return ReviewAuthorityNotApplicable, reviewStep, run, nil
		}
		// And a verdict that cannot legally become this step's outcome is
		// refused ONCE, with the fact that proves it, instead of being tried
		// again on the next pass. See late_verdict_disposition.go.
		if reason, ok := lateVerdictAdoptable(reviewStep); !ok {
			if rerr := c.refuseLateVerdict(ctx, run, reviewStep, current, reason); rerr != nil {
				return ReviewAuthorityNotApplicable, reviewStep, run, rerr
			}
			return ReviewAuthorityLateVerdictRefused, reviewStep, run, nil
		}
		return c.adoptLateReviewVerdict(ctx, run, reviewStep, current)
	}

	if reviewStep.State.Terminal() {
		return ReviewAuthorityNotApplicable, reviewStep, run, nil
	}
	// A RUNNING review step is observation's, not this function's.
	// observeReviewStep reads the same review run and already has a complete
	// answer for every terminal status it can be in — including failing the step
	// and naming the stop. Reconciliation exists for the state observation
	// cannot see: a step that has come to REST (waiting, or never started) over a
	// review that can no longer conclude it, which observeReviewStep returns from
	// on its first line.
	if reviewStep.State == domain.WorkflowStepRunning {
		return ReviewAuthorityNotApplicable, reviewStep, run, nil
	}
	if reviewRunStillSpeaks(current) {
		// Intact: this run is still capable of concluding the step, so nothing
		// here has anything to resolve.
		//
		// Deliberately NOT extended to "…but has the step acted on it yet?". A
		// review step resting at `waiting` over an APPROVED run is a real,
		// intended state elsewhere in this package: verify_fresh_review.go parks
		// exactly that shape when an approval is still valid but older than the
		// code, and completing it from here silently destroys the fresh-review
		// recovery it was parked for. The unapplied-verdict case that motivated
		// looking at this is prevented at its source instead — review_progress.go
		// no longer parks a step over a verdict that won its cancellation race.
		return ReviewAuthorityIntact, reviewStep, run, nil
	}

	if !current.TerminalWithoutVerdict() {
		// Some other non-speaking shape (an unknown status). Never folded into a
		// neighbouring case.
		return ReviewAuthorityNotApplicable, reviewStep, run, nil
	}

	// The pointer names a run AO closed out with no verdict. Before anything is
	// retried: did the reviewer actually answer after AO stopped listening?
	//
	// This is requirement 4, and the ordering is the requirement. The late
	// verdict is checked FIRST, because a real review that already exists must
	// never be thrown away in favour of running a second reviewer over the same
	// diff. And it is adoptable only because this run is still the authoritative
	// pointer — a run some replacement had superseded would have been rebound
	// away from, and would not be read here at all.
	return c.releaseReviewAuthorityForRebind(ctx, run, reviewStep, current)
}

// adoptLateReviewVerdict makes a late verdict the step's real outcome.
//
// It routes through recordReviewOutcome — the same function an on-time verdict
// goes through — so an adopted approval and an on-time approval leave the same
// durable trail and reach verify by the same path. The only thing that differs
// is the checkpoint recorded first, which says plainly that this verdict arrived
// after AO had given up on the run, so nobody reading the ledger later has to
// reconstruct why a cancelled run concluded a step.
func (c *Coordinator) adoptLateReviewVerdict(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	current domain.ReviewRun,
) (ReviewAuthorityOutcome, domain.WorkflowStep, domain.WorkflowRun, error) {
	// REVALIDATE, immediately before anything is applied.
	//
	// `current` reached this function through a read that may be several store
	// calls old, and in that window another caller can have created a
	// replacement, superseded this run and rebound the step. Acting on the cached
	// value would complete or park a step that now belongs to a different review.
	// The re-read is the cheap half of the defence; the guarded writes below are
	// the half that actually binds.
	fresh, found, ferr := c.getWorkflowStep(ctx, run.ID, reviewStep.ID)
	if ferr != nil {
		return ReviewAuthorityNotApplicable, reviewStep, run, ferr
	}
	if !found {
		return ReviewAuthorityNotApplicable, reviewStep, run, nil
	}
	reviewStep = fresh
	if reviewStep.ReviewRunID == nil || *reviewStep.ReviewRunID != current.ID {
		// A replacement already took the step.
		return ReviewAuthorityNotApplicable, reviewStep, run, nil
	}
	latest, ok, lrerr := c.reviewRuns.GetReviewRun(ctx, current.ID)
	if lrerr != nil {
		return ReviewAuthorityNotApplicable, reviewStep, run, lrerr
	}
	if !ok || latest.SupersededBy != "" || !latest.LateVerdict.Valid() {
		return ReviewAuthorityNotApplicable, reviewStep, run, nil
	}
	current = latest

	// A review step resting at `waiting` has to come back before it can be
	// concluded — under the same authority guard, so the lift itself cannot land
	// after a replacement has taken over. See liftRestingReviewStep.
	lifted, lerr := c.liftRestingReviewStep(ctx, reviewStep, current.ID)
	if lerr != nil {
		if errors.Is(lerr, errReviewAuthorityLost) {
			return ReviewAuthorityNotApplicable, reviewStep, run, nil
		}
		return ReviewAuthorityNotApplicable, reviewStep, run, lerr
	}
	reviewStep = lifted

	// Applied by exactly the route an on-time verdict takes. This function
	// decides WHOSE verdict counts; what a verdict MEANS — approve to verify,
	// changes_requested to a fix cycle, the fix budget that bounds it — belongs
	// to applyTerminalReviewRun and must have one implementation.
	//
	// It had two, briefly, and that is precisely how an adopted late
	// changes_requested came to rest at `waiting` while dispatching no fix: this
	// copy knew about the verdict and the cascade behind it did not.
	updated, handled, err := c.applyTerminalReviewRun(ctx, run, reviewStep, current, current.ID)
	if errors.Is(err, errReviewAuthorityLost) {
		// A replacement took the step mid-apply. The guarded write refused, so
		// nothing landed on its behalf; the owner decides now.
		return ReviewAuthorityNotApplicable, reviewStep, run, nil
	}
	if err != nil {
		return ReviewAuthorityNotApplicable, reviewStep, run, err
	}
	if !handled {
		return ReviewAuthorityNotApplicable, reviewStep, run, nil
	}
	reviewStep = updated

	// Re-read and prove the invariant before writing the completion marker. The
	// transition and review_observed checkpoint are separate existing store
	// calls; if either side failed, leaving the marker absent makes the next boot
	// retry. Conversely, if they both landed and only the marker write failed,
	// the next boot finalizes without applying the outcome twice.
	if fresh, ok, ferr := c.getWorkflowStep(ctx, run.ID, reviewStep.ID); ferr != nil {
		return ReviewAuthorityNotApplicable, reviewStep, run, ferr
	} else if ok {
		reviewStep = fresh
	}
	marker, reflected, serr := c.lateVerdictAdoptionState(ctx, run.ID, reviewStep, current)
	if serr != nil {
		return ReviewAuthorityNotApplicable, reviewStep, run, serr
	}
	if !reflected {
		return ReviewAuthorityNotApplicable, reviewStep, run, fmt.Errorf(
			"late review verdict %s was applied but its durable workflow outcome is incomplete", current.ID)
	}
	if !marker {
		if err := c.recordLateVerdictAdopted(ctx, run, reviewStep, current); err != nil {
			return ReviewAuthorityNotApplicable, reviewStep, run, err
		}
	}
	if refreshed, ok, rerr := c.store.GetWorkflowRun(ctx, run.ID); rerr == nil && ok {
		run = refreshed
	}
	if c.log != nil {
		c.log.Info("workflow: adopted a review verdict that arrived after AO closed its run out",
			"run", run.ID, "step", reviewStep.ID, "reviewRun", current.ID, "verdict", current.LateVerdict)
	}
	return ReviewAuthorityLateVerdictAdopted, reviewStep, run, nil
}

// lateVerdictAdoptionState validates the completion invariant:
//
//	authoritative late verdict + matching review_observed outcome + required
//	durable step state + review_late_verdict_adopted marker.
//
// marker and reflected are returned separately so either crash direction can
// self-heal: marker without reflected state reapplies the outcome; reflected
// state without marker only finalizes the marker.
func (c *Coordinator) lateVerdictAdoptionState(
	ctx stdctx.Context,
	runID string,
	step domain.WorkflowStep,
	reviewRun domain.ReviewRun,
) (marker, reflected bool, err error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false, false, err
	}
	outcome := false
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != step.ID ||
			cp.ReviewRunID == nil || *cp.ReviewRunID != reviewRun.ID {
			continue
		}
		switch cp.DurablePhase {
		case lateVerdictAdoptedPhase:
			marker = true
		case "review_observed":
			if cp.ReviewVerdict == string(reviewRun.EffectiveVerdict()) {
				outcome = true
			}
		}
	}
	wantState := domain.WorkflowStepWaiting
	if reviewRun.EffectiveVerdict() == domain.VerdictApproved {
		wantState = domain.WorkflowStepCompleted
	}
	stateReflected := step.State == wantState
	if !stateReflected && marker && outcome &&
		reviewRun.EffectiveVerdict() == domain.VerdictApproved &&
		step.State == domain.WorkflowStepWaiting {
		// Verification can legitimately reopen a completed approval for one
		// explicitly authorized fresh review. That later transition does not make
		// the earlier adoption incomplete. Require its durable authorization so a
		// historical marker-first waiting step cannot masquerade as this case.
		_, stateReflected = c.pendingFreshReview(ctx, runID, step.ID)
	}
	return marker, outcome && stateReflected, nil
}

// recordLateVerdictAdopted writes the completion marker last. Callers must have
// already proved lateVerdictAdoptionState.reflected; the marker therefore means
// completed adoption, never attempted adoption.
func (c *Coordinator) recordLateVerdictAdopted(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	current domain.ReviewRun,
) error {
	stepID := reviewStep.ID
	runID := current.ID
	detail := fmt.Sprintf(
		"review_authority: review run %s was closed out as %s and the reviewer's %s verdict arrived afterwards; it is still this step's authoritative review, so the verdict is adopted rather than re-reviewed",
		current.ID, current.Status, current.LateVerdict)
	_, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		ReviewRunID:    &runID,
		ReviewVerdict:  string(current.LateVerdict),
		HeadSHA:        current.TargetSHA,
		NextAction:     detail,
		DurablePhase:   lateVerdictAdoptedPhase,
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      c.clock(),
	})
	if errors.Is(err, domain.ErrDuplicateWorkflowCheckpoint) {
		// Another reconciler finalized after this caller proved the same durable
		// outcome. The receipt's partial unique index makes that success.
		return nil
	}
	return err
}

// releaseReviewAuthorityForRebind hands the step back to ordinary review
// dispatch so it can bind exactly one replacement — or stops, with the evidence,
// when the bounded budget for doing so is spent.
//
// It does NOT launch anything. Releasing means: record the authorization
// durably (so a restart can reconstruct what was being retried, which
// review_capacity_retry alone cannot), leave the step where dispatch can see it,
// and let dispatchReviewStep's existing routing/capacity machinery decide when
// and to which provider. That is what keeps "repeated reconciliation must not
// create unlimited reviewers" true: this function creates none, and the outbox
// key dispatch already uses admits one.
func (c *Coordinator) releaseReviewAuthorityForRebind(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	current domain.ReviewRun,
) (ReviewAuthorityOutcome, domain.WorkflowStep, domain.WorkflowRun, error) {
	prior, generation := c.latestReviewAuthorityRebind(ctx, run.ID, reviewStep.ID)
	alreadyClaimed := prior.AbandonedRunID == current.ID
	if !alreadyClaimed && generation >= maxReviewAuthorityRebinds {
		return c.stopReviewAuthority(ctx, run, reviewStep, current, generation)
	}

	stepID := reviewStep.ID
	runID := current.ID
	if !alreadyClaimed {
		// THE CLAIM, and it is the database that decides who wins.
		//
		// This function runs from boot, from every wake and from every poll, and
		// two of those landing together would otherwise both read "no
		// authorization yet", both append one, and between them consume two retry
		// generations and authorize two replacement reviewers for a single
		// abandoned review. An in-process lock cannot fix that: the callers need
		// not share a process, and the answer has to survive a restart.
		//
		// Migration 0135 puts a UNIQUE index on (workflow_step_id,
		// review_run_id) for this phase alone, so exactly one INSERT can succeed
		// per abandoned run. The loser gets ErrDuplicateWorkflowCheckpoint and
		// becomes a no-op — the winner's release is already in flight, and a
		// second one would be a second reviewer.
		record := reviewAuthorityRebindRecord{
			Generation:      generation + 1,
			AbandonedRunID:  current.ID,
			AbandonedStatus: string(current.Status),
			TargetSHA:       current.TargetSHA,
			Harness:         string(current.Harness),
			Reason: fmt.Sprintf(
				"review run %s was closed out as %s with no verdict and none arrived afterwards",
				current.ID, current.Status),
		}
		payload, _ := json.Marshal(record)
		detail := fmt.Sprintf(
			"review_authority_rebind: %s; releasing this step for replacement review %d/%d over target %s",
			record.Reason, record.Generation, maxReviewAuthorityRebinds, orValue(current.TargetSHA, "(unrecorded)"))
		if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID:             "wfc-" + c.newID(),
			WorkflowRunID:  run.ID,
			WorkflowStepID: &stepID,
			ProjectID:      run.ProjectID,
			ReviewRunID:    &runID,
			HeadSHA:        current.TargetSHA,
			NextAction:     detail,
			DurablePhase:   reviewAuthorityRebindPhase,
			PayloadVersion: "v1",
			RetryState:     string(payload),
			CreatedAt:      c.clock(),
		}); err != nil {
			if errors.Is(err, domain.ErrDuplicateWorkflowCheckpoint) {
				// Another reconciler holds this claim. Nothing further to do:
				// the release below is the claim-holder's to perform, and it is
				// idempotent if this pass is simply behind.
				return ReviewAuthorityNotApplicable, reviewStep, run, nil
			}
			return ReviewAuthorityNotApplicable, reviewStep, run, err
		}
	}

	// Release the pointer itself, AFTER the authorization is durable.
	//
	// Recording the intent is not enough, and the live wf-756988ae state is what
	// proved it: authorizing a rebind while leaving review_run_id aimed at the
	// cancelled run means the step is STILL "waiting on a review run that is not
	// running" for as long as dispatch cannot proceed — an unwired launcher, a
	// provider still cooling down, a routing decision that has to wait for
	// capacity. That is precisely the state that must not be able to persist.
	//
	// And the release is CONDITIONAL, in one statement, on the run still having
	// no late verdict. The late-verdict check at the top of this decision is a
	// READ, and a read cannot bind: the reviewer can durably write its verdict in
	// the instant between that read and this write. Clearing the pointer anyway
	// would orphan a valid authoritative verdict and put a replacement reviewer
	// over work that was already finished. Making the condition part of the
	// UPDATE removes the window entirely — either the release wins and no late
	// verdict can ever be authoritative for this step again, or the verdict wins
	// and the pointer is still there for it to be adopted through.
	released, err := c.store.ReleaseWorkflowStepReviewRunIfNoLateVerdict(
		ctx, reviewStep.ID, current.ID, c.clock())
	if err != nil {
		return ReviewAuthorityNotApplicable, reviewStep, run, err
	}
	if !released {
		// The decision was stale. Re-read and let the next line of reasoning run
		// against what is actually there — which, when a verdict landed, is the
		// adoption this release was about to make impossible. The claim above
		// stays; it is keyed to this abandoned run, so no generation is consumed
		// twice and a later pass can complete the release if the pointer simply
		// moved.
		latest, found, rerr := c.reviewRuns.GetReviewRun(ctx, current.ID)
		if rerr != nil {
			return ReviewAuthorityNotApplicable, reviewStep, run, rerr
		}
		if found && latest.LateVerdict.Valid() && latest.SupersededBy == "" {
			if fresh, ok, ferr := c.getWorkflowStep(ctx, run.ID, reviewStep.ID); ferr == nil && ok {
				reviewStep = fresh
			}
			if reviewStep.ReviewRunID != nil && *reviewStep.ReviewRunID == current.ID {
				return c.adoptLateReviewVerdict(ctx, run, reviewStep, latest)
			}
		}
		return ReviewAuthorityNotApplicable, reviewStep, run, nil
	}
	reviewStep.ReviewRunID = nil
	if c.log != nil {
		c.log.Warn("workflow: releasing a review step whose review can no longer conclude it",
			"run", run.ID, "step", reviewStep.ID, "reviewRun", current.ID,
			"status", current.Status, "generation", generation+1)
	}
	return ReviewAuthorityRebindPending, reviewStep, run, nil
}

// stopReviewAuthority is the bounded end of the retry ladder: the step's review
// cannot conclude it, replacements have been authorized as often as policy
// allows, and AO stops for a person rather than opening reviewer number four.
func (c *Coordinator) stopReviewAuthority(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	current domain.ReviewRun,
	generation int,
) (ReviewAuthorityOutcome, domain.WorkflowStep, domain.WorkflowRun, error) {
	detail := fmt.Sprintf(
		"review_state_ambiguous: this step's review run %s ended as %s with no verdict, and %d replacement reviews have already been authorized (max %d)",
		current.ID, current.Status, generation, maxReviewAuthorityRebinds)
	updated, err := c.stopReview(ctx, run, reviewStep, ReasonReviewStateAmbiguous, detail,
		string(current.Verdict), domain.WorkflowErrorReviewerLaunchFailed, "")
	if err != nil {
		return ReviewAuthorityNotApplicable, reviewStep, run, err
	}
	reviewStep = updated
	if refreshed, ok, rerr := c.store.GetWorkflowRun(ctx, run.ID); rerr == nil && ok {
		run = refreshed
	}
	return ReviewAuthorityStopped, reviewStep, run, nil
}

// pendingReviewAuthorityRebind reports whether this step is resting on an
// authorized-but-unserved rebind: reconciliation released its review, and no
// replacement has been bound since.
//
// "No replacement since" is read off the step itself rather than off a second
// marker — recordReviewDispatchSuccess sets review_run_id the moment one binds,
// so a non-nil pointer IS the proof the authorization was served. That keeps the
// two facts impossible to disagree with each other.
func (c *Coordinator) pendingReviewAuthorityRebind(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
) (reviewAuthorityRebindRecord, bool) {
	if reviewStep.ReviewRunID != nil && *reviewStep.ReviewRunID != "" {
		return reviewAuthorityRebindRecord{}, false
	}
	rec, count := c.latestReviewAuthorityRebind(ctx, run.ID, reviewStep.ID)
	if count == 0 || rec.AbandonedRunID == "" {
		return reviewAuthorityRebindRecord{}, false
	}
	return rec, true
}

// latestReviewAuthorityRebind returns the newest rebind authorization for a step
// and how many have been recorded.
//
// The count is read from the ledger rather than held in memory for the obvious
// reason: the budget has to survive the restart it exists to bound.
func (c *Coordinator) latestReviewAuthorityRebind(
	ctx stdctx.Context, runID, stepID string,
) (reviewAuthorityRebindRecord, int) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return reviewAuthorityRebindRecord{}, 0
	}
	var newest reviewAuthorityRebindRecord
	count := 0
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		if cp.DurablePhase != reviewAuthorityRebindPhase {
			continue
		}
		count++
		var rec reviewAuthorityRebindRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) == nil {
			newest = rec
		}
	}
	return newest, count
}
