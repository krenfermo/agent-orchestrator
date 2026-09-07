package daemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentcred"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	agentauth "github.com/aoagents/agent-orchestrator/backend/internal/service/agentauth"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type fakeCredentialIssuer struct {
	issued   []agentauth.IssueInput
	revoked  []string
	issueErr error
	token    string
}

func (f *fakeCredentialIssuer) Issue(_ context.Context, in agentauth.IssueInput) (agentauth.Issued, error) {
	f.issued = append(f.issued, in)
	if f.issueErr != nil {
		return agentauth.Issued{}, f.issueErr
	}
	token := f.token
	if token == "" {
		token = "agent-token-abc"
	}
	return agentauth.Issued{
		Token: token,
		Credential: domain.AgentCredential{
			ID:            "agc-1",
			Role:          in.Role,
			ProjectID:     in.ProjectID,
			SessionID:     in.SessionID,
			WorkflowRunID: in.WorkflowRunID,
			ReviewRunID:   in.ReviewRunID,
			Permissions:   domain.AgentRoleCeiling(in.Role),
		},
	}, nil
}

func (f *fakeCredentialIssuer) RevokeForReviewRun(_ context.Context, reviewRunID string) (int64, error) {
	f.revoked = append(f.revoked, reviewRunID)
	return 1, nil
}

func credentialLaunchRequest() workflowcore.ReviewerLaunchRequest {
	return workflowcore.ReviewerLaunchRequest{
		Harness:         domain.ReviewerClaudeCode,
		WorkerSessionID: "agent-orchestrator-59",
		ProjectID:       "proj-1",
		ReviewID:        "review-1",
		RunID:           "bf660d26",
		WorkspacePath:   "/ws/wf",
		Prompt:          "review this worktree",
		WorkflowRunID:   "wf-98ab416c",
		WorkflowStepID:  "wfs-04b67615",
		OwnerUserID:     "user-owner",
		Generation:      1,
	}
}

func newCredentialLauncher(t *testing.T, issuer agentCredentialIssuer, requireIdentity bool) (*workflowReviewerLauncher, *fakeWorkflowReviewerRuntime, string) {
	t.Helper()
	dataDir := t.TempDir()
	rt := &fakeWorkflowReviewerRuntime{}
	return &workflowReviewerLauncher{
		reviewers:            &fakeReviewerResolver{adapter: &fakeReviewerAdapter{cmd: ports.ReviewCommandSpec{Argv: []string{"claude"}}}},
		runtime:              rt,
		dataDir:              dataDir,
		credentials:          issuer,
		requireAgentIdentity: requireIdentity,
	}, rt, dataDir
}

// The reviewer is handed an identity of its own, bound to its launch, and it
// arrives through a file the pane can read rather than through an env var that
// every child process and every `ps eww` would also see.
func TestReviewerLaunchMintsABoundCredential(t *testing.T) {
	issuer := &fakeCredentialIssuer{}
	l, rt, dataDir := newCredentialLauncher(t, issuer, true)

	req := credentialLaunchRequest()
	if _, err := l.Launch(context.Background(), req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(issuer.issued) != 1 {
		t.Fatalf("credentials issued = %d, want 1", len(issuer.issued))
	}
	in := issuer.issued[0]
	if in.Role != domain.AgentRoleReviewer || in.UserID != "user-owner" || in.ProjectID != "proj-1" {
		t.Fatalf("credential is not scoped to the run's owner and project: %+v", in)
	}
	// The WORKER's session, because that is the session the review run belongs
	// to and therefore the one `ao review submit <session>` addresses. Binding
	// to the reviewer's own pane name would record nothing.
	if in.SessionID != "agent-orchestrator-59" {
		t.Fatalf("credential session = %q, want the worker session the verdict is recorded against", in.SessionID)
	}
	if in.WorkflowRunID != "wf-98ab416c" || in.WorkflowStepID != "wfs-04b67615" || in.ReviewRunID != "bf660d26" {
		t.Fatalf("credential is not bound to this launch: %+v", in)
	}

	path := rt.lastCfg.Env[agentcred.EnvCredentialFile]
	if path == "" {
		t.Fatalf("the pane was not told where its credential is: %+v", rt.lastCfg.Env)
	}
	if want := agentcred.Path(dataDir, l.ReviewerIdentity(req)); path != want {
		t.Fatalf("credential path = %q, want %q", path, want)
	}
	// The token is in the file, never in the environment.
	for k, v := range rt.lastCfg.Env {
		if v == "agent-token-abc" {
			t.Fatalf("the raw token was exported in env var %q", k)
		}
	}
	file, ok, err := agentcred.Read(path)
	if err != nil || !ok {
		t.Fatalf("credential file unreadable: %v (ok=%v)", err, ok)
	}
	if file.Token != "agent-token-abc" {
		t.Fatalf("credential file token = %q", file.Token)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credential: %v", err)
	}
	if perm := info.Mode().Perm(); perm != fs.FileMode(0o600) {
		t.Fatalf("credential file mode = %v, want 0600", perm)
	}
}

// On an installation where a cookie-less request resolves nobody, a reviewer
// with no credential is a reviewer whose verdict can never be recorded. Starting
// it anyway would only move the dead end thirty minutes later, to the staleness
// threshold -- which is exactly how wf-98ab416c was parked.
func TestReviewerLaunchIsRefusedWhenItsIdentityCannotBeMinted(t *testing.T) {
	issuer := &fakeCredentialIssuer{issueErr: errors.New("owning account is not active")}
	l, rt, _ := newCredentialLauncher(t, issuer, true)

	if _, err := l.Launch(context.Background(), credentialLaunchRequest()); err == nil {
		t.Fatal("a reviewer was launched with no way to record a verdict")
	}
	if rt.calls != 0 {
		t.Fatalf("the runtime was created despite the refusal (%d calls)", rt.calls)
	}
}

// On a trusted-local desktop the reviewer's cookie-less call resolves the
// bootstrap admin anyway, so a missing credential costs nothing and must not
// cost a launch.
func TestTrustedLocalReviewerLaunchSurvivesAMissingIdentity(t *testing.T) {
	issuer := &fakeCredentialIssuer{issueErr: errors.New("no owner recorded for this run")}
	l, rt, _ := newCredentialLauncher(t, issuer, false)

	if _, err := l.Launch(context.Background(), credentialLaunchRequest()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if rt.calls != 1 {
		t.Fatalf("runtime creations = %d, want 1", rt.calls)
	}
	if _, ok := rt.lastCfg.Env[agentcred.EnvCredentialFile]; ok {
		t.Fatalf("a credential path was exported for a credential that was never minted")
	}
}

// With no issuer wired -- every pre-P4-I configuration -- the pane launches
// exactly as it did before, and nothing is written anywhere.
func TestReviewerLaunchWithoutAnIssuerIsUnchanged(t *testing.T) {
	l, rt, dataDir := newCredentialLauncher(t, nil, false)

	if _, err := l.Launch(context.Background(), credentialLaunchRequest()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, ok := rt.lastCfg.Env[agentcred.EnvCredentialFile]; ok {
		t.Fatalf("a credential path was exported with no issuer wired")
	}
	if _, err := os.Stat(agentcred.Dir(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("a credential directory was created with no issuer wired (%v)", err)
	}
}

// A launch that failed leaves no live grant behind: the pane never existed, so
// nobody can ever legitimately present the credential minted for it.
func TestAFailedLaunchTakesItsCredentialBack(t *testing.T) {
	issuer := &fakeCredentialIssuer{}
	l, rt, dataDir := newCredentialLauncher(t, issuer, true)
	rt.createErr = errors.New("tmux: no server running")

	if _, err := l.Launch(context.Background(), credentialLaunchRequest()); err == nil {
		t.Fatal("expected the runtime failure")
	}
	if len(issuer.revoked) != 1 || issuer.revoked[0] != "bf660d26" {
		t.Fatalf("revocations = %v, want the failed launch's review run", issuer.revoked)
	}
	if _, err := os.Stat(agentcred.Path(dataDir, "workflow-review-bf660d26")); !os.IsNotExist(err) {
		t.Fatalf("the credential file outlived the launch that failed (%v)", err)
	}
}

// reviewerRunIDFromHandle is what lets a termination path revoke by review run
// when all it holds is the runtime handle. It must agree with ReviewerIdentity.
func TestReviewerHandleRoundTripsToItsReviewRun(t *testing.T) {
	l := &workflowReviewerLauncher{}
	req := credentialLaunchRequest()
	if got := reviewerRunIDFromHandle(l.ReviewerIdentity(req)); got != req.RunID {
		t.Fatalf("handle round trip = %q, want %q", got, req.RunID)
	}
}
