package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// task_integration_fresh_review_exception.go — one more fresh review, when a
// person says so.
//
// maxIntegrationFreshReviews is two, and the reasoning behind it is sound: a
// target that moves under a task twice while it is being reviewed is a
// scheduling problem, and a third automatic re-review does not fix it. When the
// bound is reached the task parks and asks a person to look.
//
// What was missing is what happens AFTER the person looks. The recommended
// action for integration_stale_review_after_rebase is "review the rebased work
// on the task's own branch, then continue this run" — but continuing re-derives
// the same staleness, finds the same spent budget, and parks again. The person
// did exactly what they were asked and the run could not act on it. The only
// ways forward were raising the global bound (which would weaken every task's
// guard to unblock one) or editing the ledger by hand.
//
// So the bound stays exactly where it is, and a person may grant ONE additional
// generation to ONE task, on the record. That is a different thing from raising
// a limit: it is bounded to a single task, a single workspace state, names who
// authorized it and why, and leaves every other task and every other run
// governed by the same two.
//
// What it never does: reuse a review. The exception authorizes a NEW fresh
// review generation, which goes through the identical dispatch path as the
// first two — a new review run, against the workspace as it actually stands.
// Nothing prior is re-read, and no previous checkpoint is removed or amended.
//
// One per workspace state. The exception records the fingerprint it was granted
// for, and a second request naming the same fingerprint returns the existing
// grant rather than adding another generation. A person who wants to authorize
// again after the tree has genuinely moved is describing a different state and
// gets a new grant; a poll, a retry or a double-click is describing the same one
// and gets nothing new. That is what makes it idempotent and restart-safe
// without a second ledger: the grants themselves are the record.

// integrationFreshReviewExceptionPhase is the durable grant. Append-only, on
// the CHILD run, beside the requests and answers it widens.
const integrationFreshReviewExceptionPhase = "integration_fresh_review_exception"

// IntegrationFreshReviewException is one human-authorized extra generation.
// Every field exists so the grant can be audited rather than taken on trust.
type IntegrationFreshReviewException struct {
	TaskID      string `json:"taskId"`
	MasterRunID string `json:"masterRunId,omitempty"`
	ChildRunID  string `json:"childRunId,omitempty"`
	// ApprovedBy names the person. Required: an exception attributed to
	// whoever held the session token is one nobody can be asked about later.
	ApprovedBy string `json:"approvedBy"`
	// Reason is why one more review is the right answer here. Required.
	Reason string `json:"reason"`
	// Fingerprint is the workspace state this grant was made for, and the key
	// that makes a repeat request idempotent rather than cumulative.
	Fingerprint string `json:"fingerprint"`
	// PriorAttempts is how many fresh reviews had already been spent, so the
	// record says what was exhausted rather than merely that something was.
	PriorAttempts int `json:"priorAttempts"`
	// Generation counts the exceptional grants for this task, from 1. It is
	// deliberately separate from the ordinary attempt count: the two are
	// different kinds of authority and conflating them would hide which was
	// which.
	Generation int       `json:"generation"`
	GrantedAt  time.Time `json:"grantedAt"`
}

// IntegrationFreshReviewExceptionRequest is what a caller must supply.
type IntegrationFreshReviewExceptionRequest struct {
	MasterRunID string
	TaskID      string
	ApprovedBy  string
	Reason      string
}

// AuthorizeIntegrationFreshReviewException grants one additional fresh-review
// generation to one parked task.
//
// It refuses everything it cannot justify: a task that is not parked (there is
// nothing to unblock), a task parked for some other reason (the remedy is that
// reason's, not this one's), a budget that is not actually spent (the ordinary
// path still works and should be used), and a request with no named approver or
// no reason (which is not an authorization at all).
func (c *Coordinator) AuthorizeIntegrationFreshReviewException(
	ctx stdctx.Context,
	req IntegrationFreshReviewExceptionRequest,
) (IntegrationFreshReviewException, error) {
	req.ApprovedBy = strings.TrimSpace(req.ApprovedBy)
	req.Reason = strings.TrimSpace(req.Reason)
	if req.ApprovedBy == "" {
		return IntegrationFreshReviewException{}, fmt.Errorf("%w: an exceptional fresh review requires a named approver", ErrInvalid)
	}
	if req.Reason == "" {
		return IntegrationFreshReviewException{}, fmt.Errorf("%w: an exceptional fresh review requires a reason", ErrInvalid)
	}
	if c.planStore == nil {
		return IntegrationFreshReviewException{}, fmt.Errorf("%w: no plan store", ErrInvalid)
	}
	tasks, err := c.planStore.ListWorkflowTasks(ctx, req.MasterRunID)
	if err != nil {
		return IntegrationFreshReviewException{}, err
	}
	var task *domain.WorkflowTask
	for i := range tasks {
		if tasks[i].ID == req.TaskID {
			task = &tasks[i]
			break
		}
	}
	if task == nil {
		return IntegrationFreshReviewException{}, fmt.Errorf("%w: run %s has no task %s", ErrNotFound, req.MasterRunID, req.TaskID)
	}
	if !task.State.Parked() {
		return IntegrationFreshReviewException{}, fmt.Errorf(
			"%w: task %s is %s, not parked — there is nothing for an exceptional review to unblock", ErrInvalid, req.TaskID, task.State)
	}
	// Only the stop this exists for. Another parked reason has its own remedy,
	// and granting a review generation would not address it.
	if task.AttentionReason != string(integration.ReasonStaleReviewAfterRebase) {
		return IntegrationFreshReviewException{}, fmt.Errorf(
			"%w: task %s is parked on %q, which one more review does not answer", ErrInvalid, req.TaskID, task.AttentionReason)
	}
	if task.ExecutionRunID == nil || *task.ExecutionRunID == "" {
		return IntegrationFreshReviewException{}, fmt.Errorf("%w: task %s has no execution run", ErrInvalid, req.TaskID)
	}
	childRunID := *task.ExecutionRunID

	prior, err := c.integrationFreshReviewAttempts(ctx, childRunID)
	if err != nil {
		return IntegrationFreshReviewException{}, err
	}
	granted, err := c.integrationFreshReviewExceptions(ctx, childRunID)
	if err != nil {
		return IntegrationFreshReviewException{}, err
	}
	fingerprint := c.currentTaskFingerprint(ctx, childRunID)
	if fingerprint == "" {
		return IntegrationFreshReviewException{}, fmt.Errorf(
			"%w: AO cannot read the workspace this exception would be granted for", ErrInvalid)
	}
	// Idempotence first, and deliberately BEFORE the budget check: a repeat
	// request for a workspace state already granted is the same decision
	// arriving twice, and it must return that decision rather than be refused
	// for a budget its own grant has already widened.
	for _, g := range granted {
		if g.Fingerprint == fingerprint {
			return g, nil
		}
	}
	// The ordinary budget must actually be spent. While it is not, the normal
	// path still works, and an exception granted early would be headroom nobody
	// needed and everybody later has to reason about.
	if prior < maxIntegrationFreshReviews+len(granted) {
		return IntegrationFreshReviewException{}, fmt.Errorf(
			"%w: task %s has %d of %d fresh reviews used; the ordinary budget is not exhausted",
			ErrInvalid, req.TaskID, prior, maxIntegrationFreshReviews+len(granted))
	}

	exception := IntegrationFreshReviewException{
		TaskID:        req.TaskID,
		MasterRunID:   req.MasterRunID,
		ChildRunID:    childRunID,
		ApprovedBy:    req.ApprovedBy,
		Reason:        req.Reason,
		Fingerprint:   fingerprint,
		PriorAttempts: prior,
		Generation:    len(granted) + 1,
		GrantedAt:     c.clock(),
	}
	payload, err := json.Marshal(exception)
	if err != nil {
		return IntegrationFreshReviewException{}, err
	}
	run, ok, err := c.store.GetWorkflowRun(ctx, childRunID)
	if err != nil {
		return IntegrationFreshReviewException{}, err
	}
	if !ok {
		return IntegrationFreshReviewException{}, fmt.Errorf("%w: run %s", ErrNotFound, childRunID)
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: childRunID,
		ProjectID:     run.ProjectID,
		RetryState:    string(payload),
		NextAction: fmt.Sprintf(
			"%s: %s authorized one more fresh review for task %s after %d were spent, for workspace %s — %s",
			integrationFreshReviewExceptionPhase, exception.ApprovedBy, exception.TaskID,
			exception.PriorAttempts, shortFingerprint(exception.Fingerprint), exception.Reason),
		DurablePhase:      integrationFreshReviewExceptionPhase,
		PayloadVersion:    "v1",
		FingerprintBefore: fingerprint,
		CreatedAt:         exception.GrantedAt,
	}); err != nil {
		return IntegrationFreshReviewException{}, err
	}
	if c.log != nil {
		c.log.Info("workflow: a person authorized one more integration fresh review",
			"run", req.MasterRunID, "task", req.TaskID, "approvedBy", exception.ApprovedBy,
			"generation", exception.Generation, "priorAttempts", exception.PriorAttempts)
	}
	return exception, nil
}

// integrationFreshReviewExceptions reads the grants from the append-only
// ledger, oldest first. Deriving the widened budget from the grants themselves
// is what makes this restart-safe with no counter to keep in step.
func (c *Coordinator) integrationFreshReviewExceptions(ctx stdctx.Context, childRunID string) ([]IntegrationFreshReviewException, error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, childRunID)
	if err != nil {
		return nil, err
	}
	var out []IntegrationFreshReviewException
	for _, cp := range cps {
		if cp.DurablePhase != integrationFreshReviewExceptionPhase {
			continue
		}
		var rec IntegrationFreshReviewException
		if json.Unmarshal([]byte(cp.RetryState), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

// integrationFreshReviewBudget is the number of fresh reviews this task may
// have: the global bound, plus one per human-authorized exception.
//
// maxIntegrationFreshReviews itself is never changed. Every task without an
// explicit grant is governed by exactly the same two it always was.
func (c *Coordinator) integrationFreshReviewBudget(ctx stdctx.Context, childRunID string) int {
	granted, err := c.integrationFreshReviewExceptions(ctx, childRunID)
	if err != nil {
		// An unreadable ledger must never widen a budget.
		return maxIntegrationFreshReviews
	}
	return maxIntegrationFreshReviews + len(granted)
}

// currentTaskFingerprint observes the workspace a grant would be made for, so
// the grant is tied to a state rather than to a moment.
func (c *Coordinator) currentTaskFingerprint(ctx stdctx.Context, childRunID string) string {
	if c.workspaceFacts == nil {
		return ""
	}
	steps, err := c.store.ListWorkflowSteps(ctx, childRunID)
	if err != nil {
		return ""
	}
	var workStep *domain.WorkflowStep
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepWork {
			workStep = &steps[i]
		}
	}
	if workStep == nil {
		return ""
	}
	workCP, ok, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID)
	if err != nil || !ok || workCP.WorktreePath == "" {
		return ""
	}
	// The session is used only to label the observation; a checkpoint without
	// one still names a worktree and a branch, which is what the fingerprint is
	// actually taken from.
	sessionID := ""
	if workCP.SessionID != nil {
		sessionID = *workCP.SessionID
	}
	run, ok, err := c.store.GetWorkflowRun(ctx, childRunID)
	if err != nil || !ok {
		return ""
	}
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      workCP.WorktreePath,
		Branch:    workCP.Branch,
		SessionID: domain.SessionID(sessionID),
		ProjectID: domain.ProjectID(run.ProjectID),
	})
	if err != nil {
		return ""
	}
	return WorkspaceFingerprint(obs)
}
