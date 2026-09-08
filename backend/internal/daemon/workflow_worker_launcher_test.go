package daemon

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentcred"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	agentauth "github.com/aoagents/agent-orchestrator/backend/internal/service/agentauth"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// workflow_worker_launcher_test.go — P5-A phase 2C: the launch boundary.
//
// The ordering is the contract, so most of these are about what happens when
// one of its three steps fails. Mint before spawn, because the environment is
// fixed at spawn. Bind after spawn, because the session does not exist before
// it. And on any failure in between, take the credential back: a token in a
// pane that AO cannot account for is the state this whole mechanism exists to
// prevent.

type fakeWorkerIssuer struct {
	issued       []agentauth.IssueInput
	issueErr     error
	bindErr      error
	bindReturns  bool
	bound        []string
	boundTo      []domain.SessionID
	revoked      []string
	superseded   [][2]string
	supersedeErr error
	seq          int
}

func newFakeWorkerIssuer() *fakeWorkerIssuer { return &fakeWorkerIssuer{bindReturns: true} }

func (f *fakeWorkerIssuer) Issue(_ context.Context, in agentauth.IssueInput) (agentauth.Issued, error) {
	if f.issueErr != nil {
		return agentauth.Issued{}, f.issueErr
	}
	f.issued = append(f.issued, in)
	f.seq++
	id := "cred-" + string(rune('0'+f.seq))
	return agentauth.Issued{
		Token: "tok-" + id,
		Credential: domain.AgentCredential{
			ID: id, Role: in.Role, ProjectID: in.ProjectID,
			WorkflowRunID: in.WorkflowRunID, WorkflowStepID: in.WorkflowStepID,
			SessionID: in.SessionID, RuntimeInstanceID: in.RuntimeInstanceID,
		},
	}, nil
}

func (f *fakeWorkerIssuer) BindSession(_ context.Context, credentialID string, sessionID domain.SessionID) (bool, error) {
	if f.bindErr != nil {
		return false, f.bindErr
	}
	f.bound = append(f.bound, credentialID)
	f.boundTo = append(f.boundTo, sessionID)
	return f.bindReturns, nil
}

func (f *fakeWorkerIssuer) Revoke(_ context.Context, credentialID string) (int64, error) {
	f.revoked = append(f.revoked, credentialID)
	return 1, nil
}

func (f *fakeWorkerIssuer) RevokeSupersededWorkers(_ context.Context, stepID, keepAttemptID string) (int64, error) {
	if f.supersedeErr != nil {
		return 0, f.supersedeErr
	}
	f.superseded = append(f.superseded, [2]string{stepID, keepAttemptID})
	return 0, nil
}

type fakeWorkerSpawner struct {
	gotEnv  map[string]string
	session domain.SessionID
	err     error
	calls   int
}

func (f *fakeWorkerSpawner) Spawn(_ context.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	f.calls++
	f.gotEnv = cfg.RuntimeEnv
	if f.err != nil {
		return domain.SessionRecord{}, 0, 0, f.err
	}
	return domain.SessionRecord{ID: f.session}, 0, 0, nil
}

func workerRequest() workflowcore.WorkerLaunchRequest {
	return workflowcore.WorkerLaunchRequest{
		RunID: "wf-1", StepID: "wfs-1", AttemptID: "att-1",
		ProjectID: "proj-1", Owner: "user-owner", WorkflowRunID: "wf-1",
		RuntimeEnv: map[string]string{"CODEX_HOME": "/x"},
	}
}

func newTestWorkerLauncher(t *testing.T, issuer workerCredentialIssuer, spawner workflowcore.Spawner, requireIdentity bool) *workflowWorkerLauncher {
	t.Helper()
	return &workflowWorkerLauncher{
		spawner: spawner, dataDir: t.TempDir(),
		credentials: issuer, requireAgentIdentity: requireIdentity,
	}
}

// The happy path, and the whole ordering in one assertion: minted unbound,
// handed over in the environment, spawned, then bound to the session that
// spawn produced.
func TestWorkerLaunchMintsHandsOverAndBinds(t *testing.T) {
	issuer := newFakeWorkerIssuer()
	spawner := &fakeWorkerSpawner{session: "agent-orchestrator-59"}
	l := newTestWorkerLauncher(t, issuer, spawner, true)

	res, err := l.LaunchWorker(context.Background(), workerRequest())
	if err != nil {
		t.Fatalf("LaunchWorker: %v", err)
	}
	if res.Session.ID != "agent-orchestrator-59" {
		t.Fatalf("session = %q", res.Session.ID)
	}

	// Minted UNBOUND, and bound to this launch's run, step and attempt.
	if len(issuer.issued) != 1 {
		t.Fatalf("issued %d credentials", len(issuer.issued))
	}
	in := issuer.issued[0]
	if in.SessionID != "" {
		t.Errorf("issued with session %q; it cannot be known yet", in.SessionID)
	}
	if in.Role != domain.AgentRoleWorker || in.WorkflowRunID != "wf-1" ||
		in.WorkflowStepID != "wfs-1" || in.RuntimeInstanceID != "att-1" || in.ProjectID != "proj-1" ||
		in.UserID != "user-owner" {
		t.Fatalf("issued with the wrong bindings: %+v", in)
	}

	// Handed over through the environment, without disturbing what was there.
	path, ok := spawner.gotEnv[agentcred.EnvCredentialFile]
	if !ok || path == "" {
		t.Fatalf("the credential was not handed to the pane: %+v", spawner.gotEnv)
	}
	if spawner.gotEnv["CODEX_HOME"] != "/x" {
		t.Errorf("the launcher dropped the runtime env: %+v", spawner.gotEnv)
	}
	// The request's own map must not have been mutated: the caller owns it.
	if _, leaked := workerRequest().RuntimeEnv[agentcred.EnvCredentialFile]; leaked {
		t.Error("the launcher wrote into the caller's env map")
	}
	// The file exists and carries no session (it is bound in the row, not the file).
	file, found, err := agentcred.Read(path)
	if err != nil || !found {
		t.Fatalf("read credential file: found=%v err=%v", found, err)
	}
	if file.Token == "" || file.CredentialID != "cred-1" {
		t.Fatalf("credential file = %+v", file)
	}

	// Bound exactly once, to the session the spawn produced.
	if len(issuer.bound) != 1 || issuer.bound[0] != "cred-1" || issuer.boundTo[0] != "agent-orchestrator-59" {
		t.Fatalf("bound = %v to %v", issuer.bound, issuer.boundTo)
	}
	if len(issuer.revoked) != 0 {
		t.Fatalf("a successful launch revoked %v", issuer.revoked)
	}
	// And its predecessors were ended before it was minted.
	if len(issuer.superseded) != 1 || issuer.superseded[0] != [2]string{"wfs-1", "att-1"} {
		t.Fatalf("superseded = %v", issuer.superseded)
	}
}

// Every failure between minting and binding takes the credential back.
func TestWorkerLaunchRevokesOnEveryFailurePath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(*fakeWorkerIssuer, *fakeWorkerSpawner)
		wantErr   bool
		wantSpawn int
	}{
		{
			name:      "the spawn failed",
			setup:     func(_ *fakeWorkerIssuer, s *fakeWorkerSpawner) { s.err = errors.New("no runtime") },
			wantErr:   true,
			wantSpawn: 1,
		},
		{
			name: "the launch named no session",
			// workflowcore treats this as ambiguous rather than successful, so
			// there is nothing to bind to and the credential must not survive.
			setup:     func(_ *fakeWorkerIssuer, s *fakeWorkerSpawner) { s.session = "" },
			wantErr:   false,
			wantSpawn: 1,
		},
		{
			name:      "the binding errored",
			setup:     func(i *fakeWorkerIssuer, _ *fakeWorkerSpawner) { i.bindErr = errors.New("database is locked") },
			wantErr:   true,
			wantSpawn: 1,
		},
		{
			name: "the binding matched no row",
			// Already bound, already revoked, or gone. All three mean AO cannot
			// account for the token this pane is holding.
			setup:     func(i *fakeWorkerIssuer, _ *fakeWorkerSpawner) { i.bindReturns = false },
			wantErr:   true,
			wantSpawn: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer := newFakeWorkerIssuer()
			spawner := &fakeWorkerSpawner{session: "agent-orchestrator-59"}
			tc.setup(issuer, spawner)
			l := newTestWorkerLauncher(t, issuer, spawner, true)

			_, err := l.LaunchWorker(context.Background(), workerRequest())
			if tc.wantErr && err == nil {
				t.Fatal("the launch reported success")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if spawner.calls != tc.wantSpawn {
				t.Errorf("spawned %d times, want %d", spawner.calls, tc.wantSpawn)
			}
			if len(issuer.revoked) != 1 || issuer.revoked[0] != "cred-1" {
				t.Fatalf("revoked = %v, want the credential taken back", issuer.revoked)
			}
			// And the file it was handed over in is gone.
			if path, ok := spawner.gotEnv[agentcred.EnvCredentialFile]; ok && path != "" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("the credential file survived a failed launch: %v", err)
				}
			}
		})
	}
}

// Where an identity is REQUIRED, a worker AO cannot give one to is not started:
// it provably could not report on its own work.
func TestWorkerLaunchIsRefusedWhenAnIdentityIsRequiredAndCannotBeMinted(t *testing.T) {
	issuer := newFakeWorkerIssuer()
	issuer.issueErr = errors.New("owning account is not active")
	spawner := &fakeWorkerSpawner{session: "agent-orchestrator-59"}
	l := newTestWorkerLauncher(t, issuer, spawner, true)

	if _, err := l.LaunchWorker(context.Background(), workerRequest()); err == nil {
		t.Fatal("a worker with no identity was launched on an installation that requires one")
	}
	if spawner.calls != 0 {
		t.Fatalf("the spawn happened anyway (%d calls)", spawner.calls)
	}
}

// On trusted-local the same failure costs nothing: a cookie-less call resolves
// the bootstrap admin, so a missing credential must not cost a launch.
func TestWorkerLaunchProceedsWithoutAnIdentityOnTrustedLocal(t *testing.T) {
	issuer := newFakeWorkerIssuer()
	issuer.issueErr = errors.New("no accounts yet")
	spawner := &fakeWorkerSpawner{session: "agent-orchestrator-59"}
	l := newTestWorkerLauncher(t, issuer, spawner, false)

	res, err := l.LaunchWorker(context.Background(), workerRequest())
	if err != nil {
		t.Fatalf("LaunchWorker: %v", err)
	}
	if res.Session.ID != "agent-orchestrator-59" {
		t.Fatalf("session = %q", res.Session.ID)
	}
	if _, handed := spawner.gotEnv[agentcred.EnvCredentialFile]; handed {
		t.Error("a credential file was handed over despite issuance failing")
	}
	if len(issuer.bound) != 0 {
		t.Errorf("bound %v with no credential", issuer.bound)
	}
}

// A launcher with no issuer wired is the pre-2C launch, byte for byte.
func TestWorkerLaunchWithoutAnIssuerIsUnchanged(t *testing.T) {
	spawner := &fakeWorkerSpawner{session: "agent-orchestrator-59"}
	l := &workflowWorkerLauncher{spawner: spawner, dataDir: t.TempDir()}

	res, err := l.LaunchWorker(context.Background(), workerRequest())
	if err != nil {
		t.Fatalf("LaunchWorker: %v", err)
	}
	if res.Session.ID != "agent-orchestrator-59" {
		t.Fatalf("session = %q", res.Session.ID)
	}
	if _, handed := spawner.gotEnv[agentcred.EnvCredentialFile]; handed {
		t.Error("a credential was handed over with no issuer wired")
	}
}

// A supersession that fails must not stop the replacement starting: the sweep
// still discharges the predecessor once the step stops running.
func TestASupersessionFailureDoesNotBlockTheReplacement(t *testing.T) {
	issuer := newFakeWorkerIssuer()
	issuer.supersedeErr = errors.New("database is locked")
	spawner := &fakeWorkerSpawner{session: "agent-orchestrator-59"}
	l := newTestWorkerLauncher(t, issuer, spawner, true)

	if _, err := l.LaunchWorker(context.Background(), workerRequest()); err != nil {
		t.Fatalf("LaunchWorker: %v", err)
	}
	if len(issuer.bound) != 1 {
		t.Fatalf("the replacement did not get its own identity: bound=%v", issuer.bound)
	}
}

// Two launches of the same step get different credential files, so one
// generation cannot read or clobber the other's.
func TestEachAttemptGetsItsOwnCredentialFile(t *testing.T) {
	issuer := newFakeWorkerIssuer()
	spawner := &fakeWorkerSpawner{session: "agent-orchestrator-59"}
	l := newTestWorkerLauncher(t, issuer, spawner, true)
	ctx := context.Background()

	first := workerRequest()
	if _, err := l.LaunchWorker(ctx, first); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	firstPath := spawner.gotEnv[agentcred.EnvCredentialFile]

	second := workerRequest()
	second.AttemptID = "att-2"
	if _, err := l.LaunchWorker(ctx, second); err != nil {
		t.Fatalf("second launch: %v", err)
	}
	secondPath := spawner.gotEnv[agentcred.EnvCredentialFile]

	if firstPath == secondPath {
		t.Fatalf("both attempts share the credential file %q", firstPath)
	}
	// And the second launch ended the first's credential explicitly.
	if len(issuer.superseded) != 2 || issuer.superseded[1] != [2]string{"wfs-1", "att-2"} {
		t.Fatalf("superseded = %v", issuer.superseded)
	}
}
