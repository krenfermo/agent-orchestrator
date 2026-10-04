package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// A rebase that changed what a task contributes, answered the way AO already
// answers a stale approval everywhere else: by going and getting a fresh one.
//
// The Integration Coordinator decides WHETHER the approval still describes the
// work (integration/effective.go). This file decides what to do about "no", and
// the answer is deliberately not the one every other integration attention gets.
// A merge conflict is a person's problem. A stale approval is not: the work is
// fine, the target is fine, and the only thing missing is a reviewer's opinion
// of the rebased change -- which AO can ask for.
//
// So this reuses the transition verify_fresh_review.go already built for the
// verification half of the same problem, rather than inventing a second one:
//
//	integration says the replay changed the task's contribution
//	  -> can AO ask a reviewer at all? (a reviewer, a review run, budget left)
//	     no  -> park the TASK for a person, with the two fingerprints
//	     yes -> integration_fresh_review_required, review step reopened to
//	            `waiting`, verify step reopened, the child run un-completed
//	            -> dispatchReviewStep dispatches ONE more cycle at the rebased
//	               workspace (its existing fresh-review branch, unchanged)
//	            -> approved          -> verify re-runs, the run completes, and
//	                                    the next reconcile integrates the task
//	            -> changes_requested -> the ordinary fix/review loop, unchanged
//
// What it does NOT do: it never reuses the stale approval, it never re-runs the
// worker (the work is not what went stale), and it never asks a reviewer more
// than maxIntegrationFreshReviews times for one task -- a target that keeps
// moving under a task faster than it can be reviewed is a person's problem, not
// a retry.

const (
	// integrationFreshReviewRequiredPhase is the durable decision: this task's
	// approval no longer describes what its replay would land, and one fresh
	// review has been authorized. Written BEFORE anything is reopened, so a
	// daemon that dies mid-transition resumes the decision rather than
	// re-deciding it against a target that may have moved again.
	integrationFreshReviewRequiredPhase = "integration_fresh_review_required"
	// integrationFreshReviewAnsweredPhase closes one request: the task
	// integrated after its fresh review, so the request is spent. Split from the
	// request for the same reason verify's approval checkpoint is -- so the
	// ledger can tell "asked" from "answered" without comparing timestamps.
	integrationFreshReviewAnsweredPhase = "integration_fresh_review_answered"
	// maxIntegrationFreshReviews bounds how many times ONE task may be sent back
	// to a reviewer because a replay changed its diff. Two: the first is the
	// ordinary case (a dependency landed underneath it), the second covers a
	// second dependency landing during the first fresh review. A third means
	// the target is moving faster than this task can be reviewed, which no
	// number of further reviews fixes.
	maxIntegrationFreshReviews = 2
)

// errIntegrationFreshReview means the task's integration stopped to obtain a
// fresh review, and that review has been requested.
//
// Like errIntegrationBusy it must NOT park anything. Nothing is wrong: the
// task's child run is running again, its siblings are untouched, and the next
// reconcile pass carries the review cycle forward.
var errIntegrationFreshReview = errors.New("workflow: task integration is waiting for a fresh review of its rebased work")

// errIntegrationWaitingOnDependency means a task this one requires has not been
// integrated yet. Also not a park, and also not a failure: it is simply not this
// task's turn, and the pass that discovers it must leave every sibling alone.
var errIntegrationWaitingOnDependency = errors.New("workflow: task integration is waiting for a dependency to integrate")

// IntegrationFreshReviewRecord is the durable payload of both checkpoints. It
// pins every fact the decision was made from, so a restart re-reads the decision
// instead of re-deriving it from a target that has since moved.
type IntegrationFreshReviewRecord struct {
	TaskID string `json:"taskId"`
	// MasterRunID is the objective whose integration asked for this review.
	MasterRunID string `json:"masterRunId,omitempty"`
	// ApprovedEffectiveChange and CurrentEffectiveChange are the Coordinator's
	// two answers: the identity of the change the reviewer approved, and the
	// identity of the change replaying it onto the current target produced.
	// They are what makes "the approval went stale" a checkable claim rather
	// than an assertion.
	ApprovedEffectiveChange string `json:"approvedEffectiveChange,omitempty"`
	CurrentEffectiveChange  string `json:"currentEffectiveChange,omitempty"`
	// TargetSHA and SourceSHA are where the target was and what the replay
	// produced, so the ledger still describes the situation after both move.
	TargetSHA string `json:"targetSha,omitempty"`
	SourceSHA string `json:"sourceSha,omitempty"`
	Strategy  string `json:"strategy,omitempty"`
	// ApprovedFingerprint is the workspace identity the stale review run was
	// dispatched against, and CurrentFingerprint the workspace as it stands
	// after the refresh. They are the review dispatcher's own vocabulary, which
	// is why they are recorded alongside the change identities rather than
	// instead of them.
	ApprovedFingerprint string `json:"approvedFingerprint,omitempty"`
	CurrentFingerprint  string `json:"currentFingerprint,omitempty"`
	Branch              string `json:"branch,omitempty"`
	WorktreePath        string `json:"worktreePath,omitempty"`
	HeadSHA             string `json:"headSha,omitempty"`
	ReviewStepID        string `json:"reviewStepId,omitempty"`
	// PriorReviewRunID is the review run whose approval went stale, kept so the
	// fresh review is provably a DIFFERENT run rather than an edit of the old.
	PriorReviewRunID string `json:"priorReviewRunId,omitempty"`
	// Attempt counts this task's fresh reviews, and is what
	// maxIntegrationFreshReviews is applied to.
	Attempt int `json:"attempt"`
}

// requestIntegrationFreshReview turns a stale approval into an authorized,
// bounded re-review of the rebased work.
//
// Every write is idempotent against re-entry from any poll or restart: the
// request checkpoint is bounded by its own count, and all three state changes
// are compare-and-swaps on the exact state they expect.
func (c *Coordinator) requestIntegrationFreshReview(
	ctx stdctx.Context,
	parent domain.WorkflowRun,
	task domain.WorkflowTask,
	child RunDetail,
	workCP domain.WorkflowCheckpoint,
	rec integration.Record,
) error {
	if rec.Attention == nil {
		return fmt.Errorf("workflow: task %s asked for a fresh review with no attention to explain it", task.ID)
	}
	att := *rec.Attention
	var reviewStep, verifyStep *domain.WorkflowStep
	for i := range child.Steps {
		switch child.Steps[i].Step.Kind {
		case domain.WorkflowStepReview:
			reviewStep = &child.Steps[i].Step
		case domain.WorkflowStepVerify:
			verifyStep = &child.Steps[i].Step
		}
	}
	// Every reason below is "AO cannot ask a reviewer", and each one falls back
	// to the ordinary task attention. Parking is the honest answer there: the
	// approval really is stale, and if nobody can be asked for a new one then a
	// person has to decide, exactly as they would for a conflict.
	switch {
	case reviewStep == nil || verifyStep == nil:
		return c.parkStaleReview(ctx, parent, task, att, "this task's execution run has no review and verify steps to re-open")
	case reviewStep.ReviewRunID == nil || c.reviewRuns == nil || c.reviewerLauncher == nil:
		return c.parkStaleReview(ctx, parent, task, att, "no reviewer is available to re-review the rebased work")
	case reviewStep.State != domain.WorkflowStepCompleted || verifyStep.State != domain.WorkflowStepCompleted:
		// Only a finished cycle can be re-opened. Anything else means the child
		// is already moving, and a second transition on top of it would be a
		// race with whatever is moving it.
		return c.parkStaleReview(ctx, parent, task, att, "this task's review and verification are not both completed, so there is no finished cycle to re-open")
	}

	attempt, err := c.integrationFreshReviewAttempts(ctx, child.Run.ID)
	if err != nil {
		return err
	}
	// The bound, widened only by explicit human grants for THIS task (see
	// task_integration_fresh_review_exception.go). With no grant this is
	// exactly maxIntegrationFreshReviews, as it always was.
	budget := c.integrationFreshReviewBudget(ctx, child.Run.ID)
	if attempt >= budget {
		return c.parkStaleReview(ctx, parent, task, att, fmt.Sprintf(
			"the target has moved under this task %d times since it was reviewed; a further re-review would not converge", attempt))
	}

	prior, found, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
	if err != nil {
		return err
	}
	if !found {
		return c.parkStaleReview(ctx, parent, task, att, "the review run that approved this work could not be read, so AO cannot tell a fresh review from the stale one")
	}

	record := IntegrationFreshReviewRecord{
		TaskID:      task.ID,
		MasterRunID: parent.ID,
		// The Coordinator's own two answers, taken from the record rather than
		// from the attention: the attention carries commits, and what went stale
		// is the CHANGE, which only these two identities describe.
		ApprovedEffectiveChange: rec.EffectiveFingerprintBefore,
		CurrentEffectiveChange:  rec.EffectiveFingerprintAfter,
		TargetSHA:               att.TargetSHA,
		SourceSHA:               att.SourceSHA,
		Strategy:                string(att.Strategy),
		// The dispatcher recognises a fresh review by the stale run's own
		// target, which is why this is read from the review run rather than
		// from anything remembered: a value that does not match it would make
		// dispatchReviewStep treat the fresh review as already served.
		ApprovedFingerprint: prior.TargetSHA,
		Branch:              workCP.Branch,
		WorktreePath:        workCP.WorktreePath,
		HeadSHA:             workCP.HeadSHA,
		ReviewStepID:        reviewStep.ID,
		PriorReviewRunID:    prior.ID,
		Attempt:             attempt + 1,
	}
	// The workspace as it stands after the refresh. Observed rather than
	// assumed: the rebase moved the worktree, and the reviewer must be pointed
	// at what is actually there. An unobservable workspace falls back to the
	// stale identity, which dispatchReviewStep re-observes anyway.
	if c.workspaceFacts != nil && workCP.WorktreePath != "" {
		if obs, oerr := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
			Path: workCP.WorktreePath, Branch: workCP.Branch,
			ProjectID: domain.ProjectID(parent.ProjectID),
		}); oerr == nil {
			record.CurrentFingerprint = WorkspaceFingerprint(obs)
			if obs.HeadSHA != "" {
				record.HeadSHA = obs.HeadSHA
			}
		}
	}
	if record.CurrentFingerprint == "" {
		record.CurrentFingerprint = record.ApprovedFingerprint
	}

	// Durable before mutation. A review reopened without this row would be an
	// unbounded, unauditable re-review -- the count above is read from these
	// rows and nothing else.
	if err := c.recordIntegrationFreshReview(ctx, child.Run, integrationFreshReviewRequiredPhase, record, fmt.Sprintf(
		"integration_fresh_review_required: replaying task %d (%s) onto the current target changed what it contributes (%s -> %s); re-reviewing the rebased work once (attempt %d of %d)",
		task.Ordinal, task.Title, shortFingerprint(record.ApprovedEffectiveChange), shortFingerprint(record.CurrentEffectiveChange),
		record.Attempt, budget)); err != nil {
		return err
	}

	now := c.clock()
	if _, err := c.store.ReopenCompletedWorkflowStep(ctx, reviewStep.ID, now); err != nil {
		return err
	}
	// The verify step is reopened too, and it has to be: reusing the previous
	// verification would credit a verdict about the pre-rebase content to work
	// that has been replayed since. It rests at `waiting`, never `failed` --
	// maybeVerify runs from there once the review is `completed` again.
	if _, err := c.store.ReopenCompletedWorkflowStep(ctx, verifyStep.ID, now); err != nil {
		return err
	}
	// The child run leaves its terminal state last, so a crash before this
	// point leaves a completed run whose steps are reopened -- which the next
	// pass finishes -- rather than a running run with a finished cycle.
	moved, err := c.store.UpdateWorkflowRunState(ctx, child.Run.ID,
		domain.WorkflowRunCompleted, domain.WorkflowRunRunning, now)
	if err != nil {
		return err
	}
	if !moved {
		// The child was not `completed` after all, so this pass has re-opened
		// the steps of a run somebody else is already moving. Reporting it is
		// the only safe answer: silently continuing would leave a terminal run
		// with re-opened steps, and the next reconcile would read it as
		// completed and integrate the very approval this call refused.
		if current, ok, gerr := c.store.GetWorkflowRun(ctx, child.Run.ID); gerr == nil && ok && current.State == domain.WorkflowRunRunning {
			// Already running: another pass got there first, which is the
			// idempotent case rather than a race lost.
			return fmt.Errorf("%w: task %s", errIntegrationFreshReview, task.ID)
		}
		return fmt.Errorf("workflow: task %s could not re-open its execution run for a fresh review", task.ID)
	}
	if c.log != nil {
		c.log.Info("workflow: a task's approval went stale when it was rebased; re-reviewing the rebased work",
			"run", parent.ID, "task", task.ID, "child", child.Run.ID, "attempt", record.Attempt)
	}
	return fmt.Errorf("%w: task %s", errIntegrationFreshReview, task.ID)
}

// parkStaleReview is the fallback: the approval is stale and AO cannot obtain a
// new one, so the task stops for a person with both change identities in front
// of them.
func (c *Coordinator) parkStaleReview(
	ctx stdctx.Context,
	parent domain.WorkflowRun,
	task domain.WorkflowTask,
	att integration.Attention,
	why string,
) error {
	att.Detail = att.Detail + " — " + why
	return c.recordTaskIntegrationConflict(ctx, parent, task, att)
}

// closeIntegrationFreshReview answers an outstanding request once the task it
// was asked for has actually integrated. It is best-effort: the integration has
// already happened, and a failure to close the ledger row must not turn a
// successful promotion into an error.
func (c *Coordinator) closeIntegrationFreshReview(ctx stdctx.Context, child RunDetail) {
	record, outstanding, err := c.outstandingIntegrationFreshReview(ctx, child.Run.ID)
	if err != nil || !outstanding {
		return
	}
	_ = c.recordIntegrationFreshReview(ctx, child.Run, integrationFreshReviewAnsweredPhase, record, fmt.Sprintf(
		"integration_fresh_review_answered: task %s integrated after its fresh review (attempt %d)", record.TaskID, record.Attempt))
}

// integrationFreshReviewAttempts counts how many fresh reviews this task's
// integration has already asked for. It is read from the append-only ledger
// rather than from a counter, which is what makes the bound survive restarts.
func (c *Coordinator) integrationFreshReviewAttempts(ctx stdctx.Context, childRunID string) (int, error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, childRunID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == integrationFreshReviewRequiredPhase {
			n++
		}
	}
	return n, nil
}

// outstandingIntegrationFreshReview returns the newest request that has not been
// answered yet.
func (c *Coordinator) outstandingIntegrationFreshReview(ctx stdctx.Context, childRunID string) (IntegrationFreshReviewRecord, bool, error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, childRunID)
	if err != nil {
		return IntegrationFreshReviewRecord{}, false, err
	}
	var requested, answered IntegrationFreshReviewRecord
	var haveRequest, haveAnswer bool
	for _, cp := range cps {
		var rec IntegrationFreshReviewRecord
		switch cp.DurablePhase {
		case integrationFreshReviewRequiredPhase:
			if json.Unmarshal([]byte(cp.RetryState), &rec) == nil {
				requested, haveRequest = rec, true
			}
		case integrationFreshReviewAnsweredPhase:
			if json.Unmarshal([]byte(cp.RetryState), &rec) == nil {
				answered, haveAnswer = rec, true
			}
		}
	}
	if !haveRequest {
		return IntegrationFreshReviewRecord{}, false, nil
	}
	if haveAnswer && answered.Attempt >= requested.Attempt {
		return IntegrationFreshReviewRecord{}, false, nil
	}
	return requested, true, nil
}

// pendingIntegrationFreshReview is the read dispatchReviewStep makes for a
// review step reopened by an integration rather than by a verify recovery. It
// speaks the dispatcher's vocabulary -- the two workspace fingerprints -- so the
// existing fresh-review branch serves both without knowing which asked.
func (c *Coordinator) pendingIntegrationFreshReview(ctx stdctx.Context, runID, reviewStepID string) (VerifyFreshReviewRecord, bool) {
	record, outstanding, err := c.outstandingIntegrationFreshReview(ctx, runID)
	if err != nil || !outstanding {
		return VerifyFreshReviewRecord{}, false
	}
	if record.ReviewStepID != "" && record.ReviewStepID != reviewStepID {
		return VerifyFreshReviewRecord{}, false
	}
	return VerifyFreshReviewRecord{
		// The integration replay's own attempt count IS its generation: attempt
		// 3 is a different authorized question from attempt 2, and must get a
		// different dispatch identity.
		Purpose:             freshReviewPurposeIntegration,
		Generation:          record.Attempt,
		TargetKey:           record.TaskID,
		ApprovedFingerprint: record.ApprovedFingerprint,
		CurrentFingerprint:  record.CurrentFingerprint,
		HeadSHA:             record.HeadSHA,
		Branch:              record.Branch,
		WorktreePath:        record.WorktreePath,
		ReviewStepID:        record.ReviewStepID,
		PriorReviewRunID:    record.PriorReviewRunID,
	}, true
}

// recordIntegrationFreshReview writes one checkpoint of this lifecycle. Like
// recordFreshReview, and unlike recordAttentionStop, it is NOT best-effort where
// it gates a mutation: these rows are the entire durable account of a re-review
// AO asked for on its own initiative.
func (c *Coordinator) recordIntegrationFreshReview(ctx stdctx.Context, run domain.WorkflowRun, phase string, record IntegrationFreshReviewRecord, detail string) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	stepID := record.ReviewStepID
	var step *string
	if stepID != "" {
		step = &stepID
	}
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    step,
		ProjectID:         run.ProjectID,
		Branch:            record.Branch,
		WorktreePath:      record.WorktreePath,
		HeadSHA:           record.HeadSHA,
		RetryState:        string(payload),
		FingerprintBefore: record.ApprovedFingerprint,
		FingerprintAfter:  record.CurrentFingerprint,
		NextAction:        detail,
		DurablePhase:      phase,
		PayloadVersion:    "v1",
		CreatedAt:         c.clock(),
	})
	return err
}
