package workflow

import (
	stdctx "context"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// reviewPolicyDurablePhase and reviewPolicySkippedPhase are the two
// checkpoint DurablePhase strings Checkpoint 8I adds. Both are append-only
// rows on the review step's checkpoint stream, following the exact same
// pattern as every other durable phase in this package (review_dispatched,
// review_observed, verify_started, ...).
const (
	reviewPolicyDurablePhase = "review_policy_decision"
	reviewPolicySkippedPhase = "review_policy_skipped"
)

// computeReviewRiskFacts gathers every fact ReviewPolicy is allowed to
// consult (checkpoint brief §4), all derived from data already durable in
// the engine plus one ObserveWorkspace call (the same mechanism Verify
// already uses) for the real changed-file list — never a new LLM call.
func (c *Coordinator) computeReviewRiskFacts(ctx stdctx.Context, run domain.WorkflowRun, workStep domain.WorkflowStep, artifact PlanArtifact, workCP domain.WorkflowCheckpoint) (ReviewRiskFacts, error) {
	facts := ReviewRiskFacts{
		ObjectiveText:           run.Objective,
		AcceptanceCriteria:      artifact.AcceptanceCriteria,
		AcceptanceCriteriaEmpty: len(artifact.AcceptanceCriteria) == 0,
		VerifyCommandCount:      len(artifact.Verification.Commands),
		VerifyFileCheckCount:    len(artifact.Verification.Files),
	}

	attempts, err := c.store.ListWorkflowAttempts(ctx, workStep.ID)
	if err != nil {
		return facts, err
	}
	facts.PriorWorkProviderAttempts = len(attempts)

	if c.workspaceFacts != nil && workCP.WorktreePath != "" && workCP.SessionID != nil {
		obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
			Path:      workCP.WorktreePath,
			Branch:    workCP.Branch,
			SessionID: domain.SessionID(*workCP.SessionID),
			ProjectID: domain.ProjectID(run.ProjectID),
		})
		if err == nil {
			paths := workspaceChangedPaths(obs)
			facts.ChangedFilePaths = paths
			facts.ChangedFileCount = len(paths)
		}
		// A failed observation leaves ChangedFilePaths empty: the policy's
		// own "no changed files" default resolves to REQUIRED (see
		// EvaluateReviewPolicy), never to a silent SKIPPED — an
		// unobservable workspace must never look like a trivial one.
	}

	if facts.ChangedFileCount == 1 {
		sole := facts.ChangedFilePaths[0]
		for _, fc := range artifact.Verification.Files {
			if fc.Path == sole && fc.Exists && (fc.ExactContent != nil || fc.SHA256 != "") {
				facts.HasExactContentCheckForSoleChangedFile = true
				break
			}
		}
	}

	return facts, nil
}

// decodeReviewPolicyDecision unmarshals a review_policy_decision checkpoint's
// RetryState back into a ReviewPolicyDecision. Returns ok=false on any
// unmarshal error rather than panicking or guessing — an unreadable
// checkpoint must never be silently treated as a particular decision.
func decodeReviewPolicyDecision(retryState string) (ReviewPolicyDecision, bool) {
	var decision ReviewPolicyDecision
	if retryState == "" {
		return decision, false
	}
	if err := json.Unmarshal([]byte(retryState), &decision); err != nil {
		return decision, false
	}
	return decision, decision.Decision != ""
}

// DecodeReviewPolicyDecisionForTest exposes decodeReviewPolicyDecision to
// the external workflow_test package, so integration tests can assert on a
// persisted review_policy_decision checkpoint's actual reasons/version
// without duplicating the decode logic.
func DecodeReviewPolicyDecisionForTest(retryState string) (ReviewPolicyDecision, bool) {
	return decodeReviewPolicyDecision(retryState)
}

// persistReviewPolicyDecision durably records a ReviewPolicyDecision as its
// own checkpoint row, independent of whatever happens next (REQUIRED
// dispatch or SKIPPED short-circuit) — a workflow created today must be able
// to explain later exactly which facts and policy version produced this
// decision (checkpoint brief §7), and that explanation must survive even if
// the run later fails for an unrelated reason.
func (c *Coordinator) persistReviewPolicyDecision(ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep, decision ReviewPolicyDecision) error {
	payload, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	stepID := reviewStep.ID
	nextAction := "review_required: " + string(decision.Decision)
	if decision.Decision == ReviewSkipped {
		nextAction = "review_skipped_by_policy: proceeding to verify"
	}
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		RetryState:     string(payload),
		NextAction:     nextAction,
		DurablePhase:   reviewPolicyDurablePhase,
		PayloadVersion: ReviewPolicyVersion,
		CreatedAt:      c.clock(),
	})
	return err
}

// applyReviewPolicySkip advances a review step straight to completed WITHOUT
// ever creating a review_run or launching a reviewer process — the state
// machine only permits pending->ready->running->completed, so this walks
// those exact same durable transitions synthetically, mirroring what a real
// dispatch+approval would have produced, so nothing downstream (GetRun,
// frontend, Verify) needs to special-case a policy-skipped step differently
// from an approved one except by reading the review_policy_decision
// checkpoint. ReviewRunID is never set: "Reviewed: No — policy skipped" must
// remain distinguishable from "Claude approved" (checkpoint brief §8/§18).
func (c *Coordinator) applyReviewPolicySkip(ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep) (domain.WorkflowStep, error) {
	now := c.clock()
	for _, transition := range []domain.WorkflowStepState{domain.WorkflowStepReady, domain.WorkflowStepRunning, domain.WorkflowStepCompleted} {
		if reviewStep.State == transition {
			continue
		}
		if _, err := c.store.UpdateWorkflowStepState(ctx, reviewStep.ID, reviewStep.State, transition, now); err != nil {
			return reviewStep, err
		}
		reviewStep.State = transition
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		next := domain.WorkflowRunWaiting
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, next, now); err != nil {
			return reviewStep, err
		}
	}
	stepID := reviewStep.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction:     "verify",
		DurablePhase:   reviewPolicySkippedPhase,
		PayloadVersion: ReviewPolicyVersion,
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		return reviewStep, err
	}
	return reviewStep, nil
}
