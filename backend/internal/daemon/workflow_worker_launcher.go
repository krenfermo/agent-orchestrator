package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentcred"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	agentauth "github.com/aoagents/agent-orchestrator/backend/internal/service/agentauth"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// workflow_worker_launcher.go — P5-A phase 2C: a worker gets its own identity.
//
// Until this existed the daemon wired no WorkerLauncher at all, so workers went
// out through spawnerWorkerLauncher inside the workflow package — which cannot
// reach agentauth, and therefore could not hand a worker anything. `ao work
// report` consequently worked only on a trusted-local desktop, where a
// cookie-less call resolves the bootstrap admin, and failed authentication on
// every installation that requires an identity.
//
// This is the reviewer launcher's pattern applied to the other agent AO starts,
// with one difference that drives the whole design.
//
// THE ORDERING PROBLEM. A reviewer's credential is bound to the WORKER's
// session, which already exists when the reviewer launches. A worker's session
// is created BY the spawn that must already carry the credential in its
// environment, so at issue time there is no session to name. The three facts do
// not reconcile:
//
//   - the CLI finds its credential only through AO_AGENT_CREDENTIAL_FILE, set
//     in the pane's environment at spawn;
//   - a credential must be bound to the session it may speak for;
//   - the session id does not exist until Spawn returns.
//
// So a worker credential is minted UNBOUND, handed over, and bound exactly once
// when the launch reports which session it produced. The window between the two
// grants nothing: AgentAuthority.MayReachSession refuses every session route
// while SessionID is empty, so an unbound credential is inert rather than
// permissive, and a launch that dies in that window leaves an authority that
// could never have been used.
//
// A binding that does not land is a failed launch. It is not proceeded past:
// the credential is taken back and the file removed, because an agent holding a
// token AO cannot account for is the state this whole mechanism exists to
// prevent.

// workerCredentialIssuer is service/agentauth as this launcher needs it: mint
// one credential for one launch, bind it to the session that launch produced,
// and take it back. Narrow on purpose — a launcher may hand an agent an
// identity and end it, and can do nothing else with authorization.
type workerCredentialIssuer interface {
	Issue(ctx context.Context, in agentauth.IssueInput) (agentauth.Issued, error)
	BindSession(ctx context.Context, credentialID string, sessionID domain.SessionID) (bool, error)
	Revoke(ctx context.Context, credentialID string) (int64, error)
	RevokeSupersededWorkers(ctx context.Context, stepID, keepAttemptID string) (int64, error)
}

// workerHandlePrefix namespaces the credential FILE a worker launch is handed.
//
// It is derived from the attempt id rather than the session id for the same
// reason the credential is: the attempt is known before the spawn and the
// session is not. It is also unique per launch, which is what keeps two
// generations of the same step from sharing a file.
const workerHandlePrefix = "workflow-worker-"

func workerCredentialHandle(attemptID string) string {
	return workerHandlePrefix + attemptID
}

// workflowWorkerLauncher is the daemon's concrete workflowcore.WorkerLauncher:
// the pre-existing Spawner, plus the identity a worker needs to report on its
// own work.
type workflowWorkerLauncher struct {
	spawner workflowcore.Spawner
	clock   func() time.Time
	dataDir string
	log     *slog.Logger
	// credentials mints the worker's OWN identity. Nil disables it entirely and
	// the launch behaves exactly as it did before this file existed.
	credentials workerCredentialIssuer
	// requireAgentIdentity is true on an installation where a cookie-less call
	// resolves nobody. There, a worker AO cannot give an identity to is a
	// worker that provably cannot report, and the launch is refused rather than
	// started — the same judgement the reviewer launcher makes, for the same
	// reason: starting it anyway moves the dead end later without removing it.
	requireAgentIdentity bool
}

var _ workflowcore.WorkerLauncher = (*workflowWorkerLauncher)(nil)

// LaunchWorker mints the identity, starts the worker, and binds the credential
// to the session the launch produced.
//
// The order is the contract. Mint before spawn, because the environment is
// fixed at spawn. Bind after spawn, because the session does not exist before
// it. Revoke on any failure in between, because a credential nobody can account
// for must not survive the launch that failed to give it a subject.
func (l *workflowWorkerLauncher) LaunchWorker(ctx context.Context, req workflowcore.WorkerLaunchRequest) (workflowcore.WorkerLaunchResult, error) {
	env := map[string]string{}
	for k, v := range req.RuntimeEnv {
		env[k] = v
	}

	credentialID, credentialPath, err := l.issueWorkerCredential(ctx, req, env)
	if err != nil {
		return workflowcore.WorkerLaunchResult{}, err
	}

	rec, _, _, err := l.spawner.Spawn(ctx, ports.SpawnConfig{
		ProjectID:     req.ProjectID,
		Kind:          domain.KindWorker,
		Harness:       req.Harness,
		IssueID:       req.IssueID,
		Prompt:        req.Prompt,
		DisplayName:   req.DisplayName,
		BaseRef:       req.BaseRef,
		RuntimeEnv:    env,
		Owner:         req.Owner,
		WorkflowRunID: req.WorkflowRunID,
		Placement:     req.Placement,
		Branch:        req.PlacementBranch,
	})
	if err != nil {
		l.revokeWorkerCredential(ctx, credentialID, credentialPath)
		return workflowcore.WorkerLaunchResult{}, err
	}

	// A launch that named no session has not proven a launch — workflowcore
	// treats that as ambiguous rather than successful. The credential cannot be
	// bound to anything, so it is taken back here rather than left unbound and
	// live for its whole TTL.
	if strings.TrimSpace(string(rec.ID)) == "" {
		l.revokeWorkerCredential(ctx, credentialID, credentialPath)
		return workflowcore.WorkerLaunchResult{Session: rec, LaunchedAt: l.now()}, nil
	}

	if err := l.bindWorkerCredential(ctx, credentialID, credentialPath, rec.ID); err != nil {
		return workflowcore.WorkerLaunchResult{}, err
	}
	return workflowcore.WorkerLaunchResult{Session: rec, LaunchedAt: l.now()}, nil
}

// issueWorkerCredential mints the credential and puts its file path into the
// launch environment. It returns the credential id and the path so a failed
// launch can take both back.
//
// Three outcomes, and the middle one is the point:
//
//   - no issuer wired: nothing happens and the launch is byte-for-byte what it
//     was before phase 2C. Every headless wiring and every existing test lands
//     here;
//   - issuable: the pane gets a credential bound to this project, this run,
//     this step and this launch's attempt, capped by the worker role — session
//     read/write and workflow read, and nothing else, whatever the account it
//     acts for may do elsewhere;
//   - not issuable where an identity is REQUIRED: the launch is refused.
func (l *workflowWorkerLauncher) issueWorkerCredential(
	ctx context.Context, req workflowcore.WorkerLaunchRequest, env map[string]string,
) (string, string, error) {
	if l.credentials == nil {
		return "", "", nil
	}
	// A replacement ends its predecessors first. The derived sweep cannot do
	// this one: a step being re-dispatched is still running, so the credential
	// of the launch being replaced still satisfies the sweep's "may live" rule
	// even though that launch is over. Best-effort — a failure here must not
	// stop the new worker starting, and the sweep still discharges it once the
	// step stops.
	if _, err := l.credentials.RevokeSupersededWorkers(ctx, req.StepID, req.AttemptID); err != nil && l.log != nil {
		l.log.Warn("worker identity: could not revoke a superseded worker credential",
			"step", req.StepID, "attempt", req.AttemptID, "err", err)
	}
	issued, err := l.credentials.Issue(ctx, agentauth.IssueInput{
		Role:      domain.AgentRoleWorker,
		UserID:    req.Owner,
		ProjectID: req.ProjectID,
		// Deliberately empty: the session this will speak for does not exist
		// yet. See the file comment; BindSession fills it in below, once.
		SessionID:      "",
		WorkflowRunID:  req.RunID,
		WorkflowStepID: req.StepID,
		RuntimeHandle:  workerCredentialHandle(req.AttemptID),
		// The attempt is this launch's fence. It is what makes a replacement
		// worker's credential distinguishable from its predecessor's, and it is
		// why the handle above cannot be reused across generations.
		RuntimeInstanceID: req.AttemptID,
	})
	if err != nil {
		if l.requireAgentIdentity {
			return "", "", fmt.Errorf(
				"worker identity: this installation requires an authenticated identity and AO could not mint one for work step %s, so the worker could not report on its own work: %w",
				req.StepID, err)
		}
		// Trusted-local: the worker's cookie-less call resolves the bootstrap
		// admin anyway, so a missing credential costs nothing and must not cost
		// a launch.
		return "", "", nil
	}

	path := agentcred.Path(l.dataDir, workerCredentialHandle(req.AttemptID))
	if werr := agentcred.Write(path, agentcred.File{
		Token:          issued.Token,
		CredentialID:   issued.Credential.ID,
		Role:           string(issued.Credential.Role),
		ProjectID:      string(issued.Credential.ProjectID),
		WorkflowRunID:  issued.Credential.WorkflowRunID,
		WorkflowStepID: issued.Credential.WorkflowStepID,
		ExpiresAt:      issued.Credential.ExpiresAt,
	}); werr != nil {
		// A credential the agent cannot read is the same as none at all, so it
		// is taken back immediately rather than left live and unreachable.
		l.revokeWorkerCredential(ctx, issued.Credential.ID, "")
		if l.requireAgentIdentity {
			return "", "", fmt.Errorf("worker identity: hand credential to work step %s: %w", req.StepID, werr)
		}
		return "", "", nil
	}
	env[agentcred.EnvCredentialFile] = path
	return issued.Credential.ID, path, nil
}

// bindWorkerCredential completes the deferred binding, and treats a binding
// that did not land as a failed launch.
//
// The refusal is the important half. BindSession reports false when the row was
// already bound, already revoked, or gone — and in every one of those the
// credential in this pane's environment is one AO cannot account for. Letting
// the worker run with it would leave a live token whose subject AO never
// decided, which is precisely what the binding exists to make impossible.
func (l *workflowWorkerLauncher) bindWorkerCredential(
	ctx context.Context, credentialID, credentialPath string, sessionID domain.SessionID,
) error {
	if l.credentials == nil || credentialID == "" {
		return nil
	}
	bound, err := l.credentials.BindSession(ctx, credentialID, sessionID)
	if err != nil {
		l.revokeWorkerCredential(ctx, credentialID, credentialPath)
		return fmt.Errorf("worker identity: bind credential to session %s: %w", sessionID, err)
	}
	if !bound {
		l.revokeWorkerCredential(ctx, credentialID, credentialPath)
		return fmt.Errorf(
			"worker identity: credential %s could not be bound to session %s (already bound, revoked, or gone), so this launch has no identity AO can account for",
			credentialID, sessionID)
	}
	return nil
}

// revokeWorkerCredential ends a credential and removes the file it was handed
// over in. Best-effort and idempotent: the row is what authorises, so a file
// left behind is a dead token, and a revocation that did not land is re-derived
// by the reconciler's next pass.
func (l *workflowWorkerLauncher) revokeWorkerCredential(ctx context.Context, credentialID, path string) {
	if l.credentials != nil && strings.TrimSpace(credentialID) != "" {
		_, _ = l.credentials.Revoke(ctx, credentialID)
	}
	if strings.TrimSpace(path) != "" {
		_ = agentcred.Remove(path)
	}
}

func (l *workflowWorkerLauncher) now() time.Time {
	if l.clock != nil {
		return l.clock()
	}
	return time.Now().UTC()
}
