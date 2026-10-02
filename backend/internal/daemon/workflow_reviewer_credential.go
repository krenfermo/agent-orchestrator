package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentcred"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	agentauth "github.com/aoagents/agent-orchestrator/backend/internal/service/agentauth"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// reviewerHandlePrefix is ReviewerIdentity's own prefix, kept here so a handle
// can be turned back into the review run it belongs to. The two must agree;
// reviewerRunIDFromHandle is the only place that assumes they do.
const reviewerHandlePrefix = "workflow-review-"

func reviewerRunIDFromHandle(handleID string) string {
	return strings.TrimPrefix(handleID, reviewerHandlePrefix)
}

// issueAgentCredential mints the reviewer's own scoped identity and puts its
// file path into the pane's environment. It returns the path so a failed launch
// can take the credential back.
//
// Three outcomes, and the middle one is the point:
//
//   - no issuer wired: nothing happens, and the pane launches exactly as it did
//     before P4-I. Every headless wiring and every test lands here.
//   - issuable: the pane gets a credential bound to this project, this session,
//     this run, this step and this launch generation, capped by the reviewer
//     role -- session read/write and workflow read, and nothing else, whatever
//     the account it acts for may do elsewhere.
//   - not issuable (mint or hand-over fails): the launch is refused on an
//     installation that requires an identity, and -- since AR-1a (D-SEC-2) --
//     for every run created expecting its reviewer's identity, trusted-local
//     included. Only an unowned legacy run on trusted-local still continues
//     header-less. Starting a reviewer that provably cannot report its verdict is
//     the exact failure this mechanism exists to end, and doing it anyway would
//     merely move the dead end thirty minutes later, to the staleness threshold.
//
// IssuesReviewerIdentity reports whether this launcher hands reviewers AO's own
// credential. When it does, an owned run's review run is created with
// ReviewerIdentityExpected, and a credential that cannot be minted or handed
// over fails that launch on every installation (AR-1a D-SEC-2): a reviewer that
// fell back to speaking header-less would be indistinguishable from the
// worker's own shell, which is exactly the verdict forgery this closes.
var _ workflowcore.ReviewerIdentityIssuer = (*workflowReviewerLauncher)(nil)

func (l *workflowReviewerLauncher) IssuesReviewerIdentity() bool {
	return l.credentials != nil
}

func (l *workflowReviewerLauncher) issueAgentCredential(
	ctx context.Context, req workflowcore.ReviewerLaunchRequest, handleID string, env map[string]string,
) (string, error) {
	if l.credentials == nil {
		return "", nil
	}
	issued, err := l.credentials.Issue(ctx, agentauth.IssueInput{
		Role:      domain.AgentRoleReviewer,
		UserID:    req.OwnerUserID,
		ProjectID: req.ProjectID,
		// The WORKER's session: that is the session the review_run belongs to,
		// and therefore the one `ao review submit <session>` addresses. A
		// reviewer bound to its own pane name could not record anything.
		SessionID:      req.WorkerSessionID,
		WorkflowRunID:  req.WorkflowRunID,
		WorkflowStepID: req.WorkflowStepID,
		ReviewRunID:    req.RunID,
		RuntimeHandle:  handleID,
		Generation:     req.Generation,
	})
	if err != nil {
		if l.requireAgentIdentity {
			return "", fmt.Errorf(
				"reviewer identity: this installation requires an authenticated identity and AO could not mint one for review run %s, so the reviewer could not record a verdict: %w",
				req.RunID, err)
		}
		if req.ReviewerIdentityExpected {
			// AR-1a (D-SEC-2): on trusted-local too. The run was created
			// expecting its reviewer to hold an identity, so a header-less
			// reviewer could not record its verdict; failing the launch lets
			// the ordinary reviewer-launch retry try again instead of
			// starting a dead end.
			return "", fmt.Errorf("reviewer identity: AO could not mint a credential for review run %s: %w", req.RunID, err)
		}
		// Trusted-local, and a run that does not expect an identity (an
		// unowned legacy run): the reviewer's header-less call still resolves
		// the bootstrap owner, exactly as before.
		return "", nil
	}
	path := agentcred.Path(l.dataDir, handleID)
	if werr := agentcred.Write(path, agentcred.File{
		Token:          issued.Token,
		CredentialID:   issued.Credential.ID,
		Role:           string(issued.Credential.Role),
		ProjectID:      string(issued.Credential.ProjectID),
		SessionID:      string(issued.Credential.SessionID),
		WorkflowRunID:  issued.Credential.WorkflowRunID,
		WorkflowStepID: issued.Credential.WorkflowStepID,
		ReviewRunID:    issued.Credential.ReviewRunID,
		ExpiresAt:      issued.Credential.ExpiresAt,
	}); werr != nil {
		// A credential the agent cannot read is the same as none at all, so it
		// is taken back immediately rather than left live and unreachable.
		l.revokeAgentCredential(ctx, req.RunID, "")
		if l.requireAgentIdentity || req.ReviewerIdentityExpected {
			// AR-1a (D-SEC-2): see the mint failure above.
			return "", fmt.Errorf("reviewer identity: hand credential to review run %s: %w", req.RunID, werr)
		}
		return "", nil
	}
	env[agentcred.EnvCredentialFile] = path
	return path, nil
}

// revokeAgentCredential takes back every credential minted for one review run
// and removes the file the agent would read it from. Best effort in both
// directions: this runs on failure paths, and a revocation that itself fails
// must not mask the failure that led here. The row's own expiry is the backstop.
func (l *workflowReviewerLauncher) revokeAgentCredential(ctx context.Context, reviewRunID, path string) {
	if l.credentials != nil && strings.TrimSpace(reviewRunID) != "" {
		_, _ = l.credentials.RevokeForReviewRun(ctx, reviewRunID)
	}
	if strings.TrimSpace(path) != "" {
		_ = agentcred.Remove(path)
	}
}

// revokeAgentCredentialForHandle is the same, addressed by the runtime handle a
// termination path has in hand.
func (l *workflowReviewerLauncher) revokeAgentCredentialForHandle(ctx context.Context, handleID string) {
	if strings.TrimSpace(handleID) == "" {
		return
	}
	l.revokeAgentCredential(ctx, reviewerRunIDFromHandle(handleID), agentcred.Path(l.dataDir, handleID))
}
