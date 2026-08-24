package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Plan / Acceptance Criteria Amendment.
//
// A planned task's acceptance criteria are handed to the reviewer verbatim and
// judged strictly. That is correct for a criterion about the WORK. It is a trap
// for a criterion about the ENVIRONMENT the work happens in, because those
// expire: the plan is written at one moment and executed at another, and in
// between a person can legitimately change the very state a criterion asserts.
//
// When that happens the reviewer is right to block, the work is right to be
// blocked, and the thing that is actually wrong is the criterion. Before this,
// AO had no way to say so. The only exits were editing the database by hand,
// replanning the whole objective (destroying every completed task with it), or
// fabricating the state the criterion described — the first two are damage and
// the third is a lie.
//
// What stops this from becoming a way to argue a reviewer out of a real
// finding is not good intentions; it is four properties the mechanism enforces:
//
//  1. A named human approves it. An agent cannot amend its own criteria.
//  2. The original text survives forever, in an append-only ledger.
//  3. A reason and at least one piece of concrete evidence are required.
//  4. The work is reviewed again, from scratch, by an independent review. A
//     verdict reached under a criterion that no longer exists does not carry
//     over — and neither does an approval.
//
// (4) is the one that matters most, and it cuts both ways: amending a criterion
// never approves anything, it only re-opens the question under criteria that
// describe reality.

// TaskCriterionAmendmentRequest is one human's amendment of one criterion.
type TaskCriterionAmendmentRequest struct {
	RunID  string
	TaskID string
	// CriterionIndex is the position in the task's CURRENT acceptance criteria.
	CriterionIndex int
	// OriginalCriterion, when set, must match the text at that index. It is the
	// caller's proof that it is amending the criterion it thinks it is: an
	// index alone silently amends the wrong one if the criteria moved between
	// the read and the write.
	OriginalCriterion string
	// AmendedCriterion is the replacement. Empty declares the criterion
	// obsolete and removes it.
	AmendedCriterion string
	// Reason is why the criterion no longer describes reality.
	Reason string
	// Evidence is what proves the reason — commit ids, observations, anything a
	// later reader can check. At least one is required.
	Evidence []string
	// ApprovedBy names the human who authorized this. Required.
	ApprovedBy string
}

// taskCriterionAmendedPhase is the durable record on the CHILD run, where the
// reviewer's verdict lives, so the amendment appears in the same ledger a
// person reads when asking why a fresh review was opened.
const taskCriterionAmendedPhase = "task_criterion_amended"

// AmendTaskAcceptanceCriterion is the supported way to change a planned task's
// acceptance criteria after the plan was accepted.
//
// It is deliberately narrow: one criterion, one reason, one approver, one
// amendment per call. A bulk "rewrite the criteria" API would be the same
// mechanism with the auditability removed.
func (c *Coordinator) AmendTaskAcceptanceCriterion(ctx stdctx.Context, req TaskCriterionAmendmentRequest) (domain.WorkflowTaskCriterionAmendment, error) {
	req.Reason = strings.TrimSpace(req.Reason)
	req.ApprovedBy = strings.TrimSpace(req.ApprovedBy)
	req.AmendedCriterion = strings.TrimSpace(req.AmendedCriterion)
	evidence := make([]string, 0, len(req.Evidence))
	for _, e := range req.Evidence {
		if e = strings.TrimSpace(e); e != "" {
			evidence = append(evidence, e)
		}
	}

	switch {
	case req.ApprovedBy == "":
		// The single most important refusal in this file. Without it the
		// mechanism is "an agent may rewrite the bar it is judged against".
		return domain.WorkflowTaskCriterionAmendment{},
			fmt.Errorf("%w: an acceptance criterion may only be amended with a named human approver", ErrInvalid)
	case req.Reason == "":
		return domain.WorkflowTaskCriterionAmendment{},
			fmt.Errorf("%w: an amendment must say why the criterion no longer describes reality", ErrInvalid)
	case len(evidence) == 0:
		return domain.WorkflowTaskCriterionAmendment{},
			fmt.Errorf("%w: an amendment must carry at least one piece of checkable evidence", ErrInvalid)
	}

	tasks, err := c.planStore.ListWorkflowTasks(ctx, req.RunID)
	if err != nil {
		return domain.WorkflowTaskCriterionAmendment{}, err
	}
	var task *domain.WorkflowTask
	for i := range tasks {
		if tasks[i].ID == req.TaskID {
			task = &tasks[i]
			break
		}
	}
	if task == nil {
		return domain.WorkflowTaskCriterionAmendment{},
			fmt.Errorf("%w: run %s has no task %s", ErrNotFound, req.RunID, req.TaskID)
	}
	if task.State.Terminal() {
		// A completed task's criteria are history. Amending them would rewrite
		// the standard something was already judged against, which is exactly
		// the abuse this mechanism must not enable.
		return domain.WorkflowTaskCriterionAmendment{},
			fmt.Errorf("%w: task %s is already %s; its acceptance criteria are history", ErrInvalid, req.TaskID, task.State)
	}

	var criteria []string
	if err := json.Unmarshal([]byte(task.AcceptanceCriteriaJSON), &criteria); err != nil {
		return domain.WorkflowTaskCriterionAmendment{},
			fmt.Errorf("%w: task %s has unreadable acceptance criteria", ErrInvalid, req.TaskID)
	}
	if req.CriterionIndex < 0 || req.CriterionIndex >= len(criteria) {
		return domain.WorkflowTaskCriterionAmendment{},
			fmt.Errorf("%w: task %s has %d acceptance criteria; there is no criterion %d",
				ErrInvalid, req.TaskID, len(criteria), req.CriterionIndex)
	}
	original := criteria[req.CriterionIndex]
	if want := strings.TrimSpace(req.OriginalCriterion); want != "" && want != strings.TrimSpace(original) {
		// Optimistic concurrency on the text itself. Two amendments racing on
		// one task must not silently amend each other's criterion.
		return domain.WorkflowTaskCriterionAmendment{},
			fmt.Errorf("%w: criterion %d is not the text this amendment expected", ErrInvalid, req.CriterionIndex)
	}

	disposition := domain.WorkflowTaskCriterionObsolete
	applied := append(append([]string{}, criteria[:req.CriterionIndex]...), criteria[req.CriterionIndex+1:]...)
	if req.AmendedCriterion != "" {
		disposition = domain.WorkflowTaskCriterionAmended
		applied = append([]string{}, criteria...)
		applied[req.CriterionIndex] = req.AmendedCriterion
	}

	child, hasChild := c.executionRunFor(ctx, *task)
	amendment := domain.WorkflowTaskCriterionAmendment{
		ID:                    "wfca-" + c.newID(),
		WorkflowRunID:         req.RunID,
		TaskID:                req.TaskID,
		CriterionIndex:        int64(req.CriterionIndex),
		OriginalCriterion:     original,
		AmendedCriterion:      req.AmendedCriterion,
		Disposition:           disposition,
		Reason:                req.Reason,
		Evidence:              evidence,
		ApprovedBy:            req.ApprovedBy,
		SupersededReviewRunID: latestReviewRunID(child),
		CreatedAt:             c.clock(),
	}
	// The ledger row and the new criteria land together or not at all.
	if err := c.planStore.AmendWorkflowTaskCriterion(ctx, amendment, applied, c.clock()); err != nil {
		return domain.WorkflowTaskCriterionAmendment{}, err
	}
	if c.log != nil {
		c.log.Info("workflow: a task's acceptance criterion was amended by a person",
			"run", req.RunID, "task", req.TaskID, "index", req.CriterionIndex,
			"disposition", disposition, "approvedBy", req.ApprovedBy)
	}

	if !hasChild {
		// Nothing has executed this task yet, so the amended criteria simply
		// reach the child when it is dispatched. There is no verdict to
		// invalidate and no review to re-open.
		return amendment, nil
	}
	if err := c.requireFreshReviewAfterAmendment(ctx, child, *task, applied, amendment); err != nil {
		return amendment, err
	}
	return amendment, nil
}

// requireFreshReviewAfterAmendment makes the amended criteria the ones the
// reviewer will actually be judged against, and re-opens the review.
//
// Two writes, in this order, because the order is the guarantee: the child's
// plan artifact is what BuildReviewPrompt reads, so updating it BEFORE
// re-opening the review is what stops a fresh reviewer being handed the old
// criteria. Re-opening first would race a dispatch against the update.
func (c *Coordinator) requireFreshReviewAfterAmendment(
	ctx stdctx.Context,
	child RunDetail,
	task domain.WorkflowTask,
	applied []string,
	amendment domain.WorkflowTaskCriterionAmendment,
) error {
	planStepID := ""
	for _, s := range child.Steps {
		if s.Step.Kind == domain.WorkflowStepPlan {
			planStepID = s.Step.ID
			break
		}
	}
	if planStepID != "" {
		artifact, err := c.planArtifactForRun(ctx, child.Run)
		if err != nil {
			return err
		}
		artifact.AcceptanceCriteria = applied
		raw, err := MarshalPlanArtifact(artifact)
		if err != nil {
			return err
		}
		if _, err := c.store.UpdateWorkflowStepArtifact(ctx, planStepID, raw, c.clock()); err != nil {
			return err
		}
	}

	// The durable statement that the previous verdict no longer applies. It is
	// written on the child, next to the review it supersedes, because that is
	// where a person looks when asking why the reviewer was asked again.
	body, _ := json.Marshal(struct {
		AmendmentID  string   `json:"amendmentId"`
		TaskID       string   `json:"taskId"`
		Disposition  string   `json:"disposition"`
		Original     string   `json:"originalCriterion"`
		Amended      string   `json:"amendedCriterion,omitempty"`
		Reason       string   `json:"reason"`
		Evidence     []string `json:"evidence"`
		ApprovedBy   string   `json:"approvedBy"`
		Superseded   string   `json:"supersededReviewRunId,omitempty"`
		NowRequiring []string `json:"criteriaNowInForce"`
	}{
		AmendmentID: amendment.ID, TaskID: task.ID, Disposition: string(amendment.Disposition),
		Original: amendment.OriginalCriterion, Amended: amendment.AmendedCriterion,
		Reason: amendment.Reason, Evidence: amendment.Evidence, ApprovedBy: amendment.ApprovedBy,
		Superseded: amendment.SupersededReviewRunID, NowRequiring: applied,
	})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: child.Run.ID,
		ProjectID:     child.Run.ProjectID,
		NextAction: fmt.Sprintf(
			"task_criterion_amended: %s approved %s of criterion %d on task %d (%s) — the previous verdict no longer applies and an independent review of the current work is due",
			amendment.ApprovedBy, amendment.Disposition, amendment.CriterionIndex, task.Ordinal, task.Title),
		DurablePhase:   taskCriterionAmendedPhase,
		PayloadVersion: "v1",
		RetryState:     string(body),
		CreatedAt:      c.clock(),
	}); err != nil {
		return err
	}

	// Re-open the review itself. Nothing here approves anything: the reviewer
	// is asked again, from scratch, against criteria that now describe reality.
	return c.reopenReviewAfterAmendment(ctx, child, amendment)
}

// supersededDispatchPhase is the durable record that a review dispatch was
// retired because the criteria it was issued under no longer exist.
const supersededDispatchPhase = "review_dispatch_superseded"

// supersedeReviewDispatch retires whatever is left of the review the amendment
// invalidated, so the next dispatch is a genuinely new one.
//
// The problem it solves is specific. A review step's trigger-review command
// lives in the outbox under a per-cycle idempotency key, and dispatch is
// single-flight against it: an entry that is already dispatched or acknowledged
// means "this cycle was issued", and the dispatcher will try to ADOPT its
// review run rather than start another. That is exactly right for a crash
// recovery and exactly wrong after an amendment, where the point is that the
// old cycle's verdict no longer counts. Left alone, the stale entry either gets
// re-adopted (reviving a verdict reached under a criterion that is gone) or,
// when the workspace has since moved and no review run matches, parks the step
// as review_dispatch_ambiguous — which is what wf-04e8309d actually did.
//
// So every non-terminal trigger-review entry on the step is moved to failed,
// carrying an error class that says why. Nothing is deleted: the entries and
// their review runs stay exactly where they are, and the checkpoint below names
// which verdict the amendment superseded.
//
// Idempotent and restart-safe by construction: an entry already terminal is
// skipped, so a second call, a retry or a resume after a crash finds nothing
// left to retire and writes nothing.
func (c *Coordinator) supersedeReviewDispatch(
	ctx stdctx.Context,
	child RunDetail,
	amendment domain.WorkflowTaskCriterionAmendment,
) error {
	reviewStepID := ""
	for _, s := range child.Steps {
		if s.Step.Kind == domain.WorkflowStepReview {
			reviewStepID = s.Step.ID
			break
		}
	}
	if reviewStepID == "" {
		return nil
	}
	entries, err := c.store.ListWorkflowOutboxByRun(ctx, child.Run.ID)
	if err != nil {
		return err
	}
	retired := make([]string, 0, 1)
	for _, e := range entries {
		if e.CommandType != domain.WorkflowOutboxTriggerReview ||
			e.WorkflowStepID == nil || *e.WorkflowStepID != reviewStepID {
			continue
		}
		if e.Status == domain.WorkflowOutboxFailed {
			continue // already retired; nothing to do and nothing to record
		}
		moved, err := c.store.UpdateWorkflowOutboxStatus(ctx, e.ID, e.Status,
			domain.WorkflowOutboxFailed, c.clock(), supersededDispatchPhase)
		if err != nil {
			return err
		}
		if moved {
			retired = append(retired, e.IdempotencyKey)
		}
	}
	if len(retired) == 0 {
		return nil
	}
	body, _ := json.Marshal(struct {
		AmendmentID           string   `json:"amendmentId"`
		TaskID                string   `json:"taskId"`
		RetiredDispatches     []string `json:"retiredDispatches"`
		SupersededReviewRunID string   `json:"supersededReviewRunId,omitempty"`
	}{amendment.ID, amendment.TaskID, retired, amendment.SupersededReviewRunID})
	stepID := reviewStepID
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  child.Run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      child.Run.ProjectID,
		NextAction: fmt.Sprintf(
			"review_dispatch_superseded: %d review dispatch(es) were retired because amendment %s changed the acceptance criteria they were issued under — the next review is a new, independent cycle",
			len(retired), amendment.ID),
		DurablePhase:   supersededDispatchPhase,
		PayloadVersion: "v1",
		RetryState:     string(body),
		CreatedAt:      c.clock(),
	})
	return err
}

// ResumeAmendedTaskReview finishes an amendment whose fresh review never got
// opened — a daemon that died between the two writes, or (as happened on
// wf-04e8309d) an amendment applied by a build whose re-open was wrong.
//
// It creates NO new amendment. It re-applies the consequences of the amendment
// already on record: retire any superseded dispatch, put the review step back
// where a new cycle is dispatched from, and unpark the run. Every one of those
// is idempotent, so calling it on a task that is already moving does nothing.
func (c *Coordinator) ResumeAmendedTaskReview(ctx stdctx.Context, runID, taskID string) error {
	amendments, err := c.planStore.ListWorkflowTaskCriterionAmendments(ctx, runID)
	if err != nil {
		return err
	}
	var latest domain.WorkflowTaskCriterionAmendment
	for _, a := range amendments {
		if a.TaskID == taskID {
			latest = a
		}
	}
	if latest.ID == "" {
		return fmt.Errorf("%w: task %s has no recorded acceptance-criterion amendment to resume", ErrInvalid, taskID)
	}
	tasks, err := c.planStore.ListWorkflowTasks(ctx, runID)
	if err != nil {
		return err
	}
	for i := range tasks {
		if tasks[i].ID != taskID {
			continue
		}
		child, ok := c.executionRunFor(ctx, tasks[i])
		if !ok {
			return fmt.Errorf("%w: task %s has no execution run", ErrInvalid, taskID)
		}
		return c.reopenReviewAfterAmendment(ctx, child, latest)
	}
	return fmt.Errorf("%w: run %s has no task %s", ErrNotFound, runID, taskID)
}

// reopenReviewAfterAmendment puts the child back in the state the ordinary
// cascade dispatches a NEW review cycle from.
//
// That state is `waiting`, not `pending`, and the difference is load-bearing.
// `pending` is what a review step holds before the work step has finished, so
// the cascade advances it to `ready` first — and dispatchReviewStep reads
// `ready` as "this is a crash-recovery resume of the cycle already in flight",
// recomputes the SAME cycle number, and collides with that cycle's already
// acknowledged outbox entry. The dispatch then adopts nothing (the workspace
// has moved, so the natural-key lookup finds no review run for the new target)
// and the step parks as review_dispatch_ambiguous. `waiting` is exactly the
// shape a delivered fix leaves behind, which is the situation this really is:
// the previous verdict is spent and the next cycle is due.
//
// A step already `waiting` is therefore left alone rather than "reset" — moving
// it would be the bug.
func (c *Coordinator) reopenReviewAfterAmendment(ctx stdctx.Context, child RunDetail, amendment domain.WorkflowTaskCriterionAmendment) error {
	// First retire whatever is left of the dispatch the amendment invalidated.
	// Before the step is moved, so a poll landing in between finds a step that
	// is not yet dispatchable rather than one whose stale outbox entry is still
	// adoptable.
	if err := c.supersedeReviewDispatch(ctx, child, amendment); err != nil {
		return err
	}
	for _, s := range child.Steps {
		switch s.Step.Kind {
		case domain.WorkflowStepReview, domain.WorkflowStepFix:
			if s.Step.State == domain.WorkflowStepWaiting || s.Step.State.Terminal() {
				continue
			}
			if _, err := c.store.UpdateWorkflowStepState(ctx, s.Step.ID, s.Step.State,
				domain.WorkflowStepWaiting, c.clock()); err != nil {
				return err
			}
		}
	}
	run, ok, err := c.store.GetWorkflowRun(ctx, child.Run.ID)
	if err != nil || !ok {
		return err
	}
	if run.State == domain.WorkflowRunNeedsAttention {
		// The stop being cleared is whatever the run happened to be parked on —
		// the exhausted fix budget, an ambiguous dispatch, a blocked worker. All
		// of them are answered by the same fact, so the record says that fact
		// rather than guessing at a reason code.
		c.unparkRun(ctx, run, taskCriterionAmendedPhase,
			"an acceptance criterion was amended by a person, so an independent review of the current work is due")
	}
	return nil
}

// executionRunFor loads the child run executing a task, if it has one.
func (c *Coordinator) executionRunFor(ctx stdctx.Context, task domain.WorkflowTask) (RunDetail, bool) {
	id := ""
	if task.ExecutionRunID != nil {
		id = *task.ExecutionRunID
	}
	if id == "" {
		found, ok, err := c.planStore.FindWorkflowRunByPlannedTask(ctx, task.ID)
		if err != nil || !ok {
			return RunDetail{}, false
		}
		id = found
	}
	child, err := c.GetRun(ctx, id)
	if err != nil {
		return RunDetail{}, false
	}
	return child, true
}

// latestReviewRunID names the verdict an amendment supersedes, when there is
// one to name.
func latestReviewRunID(child RunDetail) string {
	for _, s := range child.Steps {
		if s.Step.Kind == domain.WorkflowStepReview && s.Step.ReviewRunID != nil {
			return *s.Step.ReviewRunID
		}
	}
	return ""
}

// ListTaskCriterionAmendments returns every amendment recorded for a master
// run, oldest first, so a reader can see the standard a task was judged against
// and how it changed.
func (c *Coordinator) ListTaskCriterionAmendments(ctx stdctx.Context, runID string) ([]domain.WorkflowTaskCriterionAmendment, error) {
	return c.planStore.ListWorkflowTaskCriterionAmendments(ctx, runID)
}
