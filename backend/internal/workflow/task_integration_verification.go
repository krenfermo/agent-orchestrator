package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// This file answers one question for the audit ledger: WHAT verification
// authorized this integration.
//
// The reviewer's third finding was that nothing did. A fast-forward and a
// direct-branch proof both recorded `verificationRan: false`, which is a true
// statement about the Integration Coordinator's own activity and a false
// impression about the ref update: in both cases a real verify step had passed,
// durably, against exactly the content being integrated, and the ledger said
// nothing about it. An audit row that cannot name what authorized a ref update
// is not an audit row.
//
// So the evidence is read from the child's own durable records — the verify
// step, the result checkpoint under it, and the fingerprint that result was
// computed against — and handed to the Coordinator, which reuses it only while
// it still describes what is landing. Nothing here invents a verification: a
// child with no passing verify record produces no claim at all, and a plan that
// declared no verification says exactly that.

// taskVerificationEvidence reads the durable verification that authorized this
// task's work.
//
// fingerprint is the identity the evidence should be stated in, when the caller
// knows one that its own freshness check will re-observe (direct-branch's
// commit). Empty means "use what the verify result itself recorded", which is
// the workspace fingerprint an AO worktree is compared by.
func (c *Coordinator) taskVerificationEvidence(ctx stdctx.Context, child RunDetail, plan VerificationPlan, fingerprint string) integration.Verification {
	if len(plan.Commands) == 0 && len(plan.Files) == 0 {
		// Nothing was ever asked of this task, so nothing passed. Saying so is
		// materially different from "no record could be found", and the ledger
		// has to be able to tell a reader which of the two it is looking at.
		return integration.Verification{Source: integration.SourceNotPlanned}
	}
	cp, result, ok := latestPassingVerifyResult(ctx, c, child.Run.ID)
	if !ok {
		// No durable proof. The zero value is the honest answer, and it is what
		// makes the Coordinator record Ran=false for a run that genuinely has
		// no verification behind it rather than for every run.
		return integration.Verification{}
	}
	identity := fingerprint
	if identity == "" {
		identity = result.PostFingerprint
		if identity == "" {
			identity = result.PreFingerprint
		}
	}
	stepID := ""
	if cp.WorkflowStepID != nil {
		stepID = *cp.WorkflowStepID
	}
	return integration.Verification{
		Ran:         true,
		Passed:      true,
		Source:      integration.SourceTaskVerification,
		StepID:      stepID,
		EvidenceID:  cp.ID,
		Fingerprint: identity,
		Summary: fmt.Sprintf("the task's own verification passed (%d checks, checkpoint %s)",
			len(result.Checks), cp.ID),
	}
}

// latestPassingVerifyResult returns the newest verify_result checkpoint on a
// run whose recorded result actually passed.
//
// Newest-passing rather than newest: a run that failed verification, was fixed
// and verified again has both rows, and the one that authorized the work is the
// one that passed. A run whose LAST verification failed cannot reach here at
// all — the readiness gate refuses it — so this never launders a failure into
// an authorization.
func latestPassingVerifyResult(ctx stdctx.Context, c *Coordinator, runID string) (domain.WorkflowCheckpoint, VerifyResult, bool) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return domain.WorkflowCheckpoint{}, VerifyResult{}, false
	}
	var found domain.WorkflowCheckpoint
	var result VerifyResult
	ok := false
	for i := range checkpoints {
		cp := checkpoints[i]
		if cp.DurablePhase != verifyResultPhase {
			continue
		}
		var parsed VerifyResult
		if json.Unmarshal([]byte(cp.RetryState), &parsed) != nil || !parsed.Passed {
			continue
		}
		found, result, ok = cp, parsed, true
	}
	return found, result, ok
}

// observedWorktreeFingerprint is the identity of an AO-owned task worktree as
// it is right now, in the same scheme a verify result records.
//
// It returns "" when it cannot observe, and that empty string is load-bearing:
// an unknown identity can never equal a verification's, so the Coordinator
// treats the verdict as stale and re-verifies rather than crediting it to
// content nobody looked at. Failing closed here costs one verification run;
// failing open would put an unearned authorization in the ledger.
func (c *Coordinator) observedWorktreeFingerprint(ctx stdctx.Context, parent domain.WorkflowRun, workCP domain.WorkflowCheckpoint) string {
	if c.workspaceFacts == nil || workCP.WorktreePath == "" {
		return ""
	}
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path: workCP.WorktreePath, Branch: workCP.Branch,
		ProjectID: domain.ProjectID(parent.ProjectID),
	})
	if err != nil {
		return ""
	}
	return WorkspaceFingerprint(obs)
}
