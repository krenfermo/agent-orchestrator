package agentauth_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	agentauth "github.com/aoagents/agent-orchestrator/backend/internal/service/agentauth"
)

// worker_credential_test.go — P5-A phase 2C: a worker's own identity.
//
// The property under test throughout is that a worker credential is inert until
// it is bound, bound exactly once, and dead as soon as the launch it was minted
// for is over. Every case below is one of those three or a way they must fail
// closed.

func workerStore(t *testing.T) *fakeStore {
	t.Helper()
	f := newFakeStore()
	f.steps = map[string]domain.WorkflowStepState{}
	return f
}

func issueWorker(t *testing.T, svc *agentauth.Service, stepID, attemptID string) agentauth.Issued {
	t.Helper()
	issued, err := svc.Issue(context.Background(), agentauth.IssueInput{
		Role:              domain.AgentRoleWorker,
		UserID:            "user-owner",
		ProjectID:         "proj-1",
		WorkflowRunID:     "wf-1",
		WorkflowStepID:    stepID,
		RuntimeInstanceID: attemptID,
		RuntimeHandle:     "workflow-worker-" + attemptID,
	})
	if err != nil {
		t.Fatalf("Issue worker credential: %v", err)
	}
	return issued
}

// A worker credential is minted with NO session, because the session it will
// speak for does not exist yet — and in that state it reaches nothing.
func TestWorkerCredentialIsInertUntilBound(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	issued := issueWorker(t, svc, "wfs-1", "att-1")

	if issued.Credential.SessionID != "" {
		t.Fatalf("session = %q, want unbound at issue time", issued.Credential.SessionID)
	}
	authority := issued.Credential.Authority()
	// The whole safety of deferred binding: an unbound credential opens no
	// session route at all, so the window between minting and binding grants
	// nothing.
	if authority.MayReachSession("agent-orchestrator-59") {
		t.Fatal("an unbound worker credential reached a session")
	}
	if authority.MayReachSession("") {
		t.Fatal("an unbound worker credential reached the empty session")
	}
	// It does already know its run, which is what the report route resolves
	// against once the session is bound.
	if !authority.MayReachWorkflowRun("wf-1") {
		t.Fatal("the credential does not name its own run")
	}
	if authority.MayReachWorkflowRun("wf-2") {
		t.Fatal("the credential reached another run")
	}
}

// Every other role still requires a session at issue time: the exception is a
// worker's ordering problem, not a general relaxation.
func TestOnlyAWorkerMayBeIssuedUnbound(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	_, err := svc.Issue(context.Background(), agentauth.IssueInput{
		Role:      domain.AgentRoleReviewer,
		UserID:    "user-owner",
		ProjectID: "proj-1",
	})
	if err == nil {
		t.Fatal("a reviewer credential was issued with no session binding")
	}
}

// Binding happens exactly once. A second attempt, on any session, changes
// nothing — which is what makes "a binding is not a hint" true.
func TestBindSessionHappensExactlyOnce(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	issued := issueWorker(t, svc, "wfs-1", "att-1")
	ctx := context.Background()

	bound, err := svc.BindSession(ctx, issued.Credential.ID, "agent-orchestrator-59")
	if err != nil || !bound {
		t.Fatalf("first bind: bound=%v err=%v", bound, err)
	}
	// Now it reaches its own session and nothing else.
	principal, err := svc.ResolveAgentPrincipal(ctx, issued.Token)
	if err != nil {
		t.Fatalf("ResolveAgentPrincipal: %v", err)
	}
	if !principal.Agent.MayReachSession("agent-orchestrator-59") {
		t.Fatal("a bound worker cannot reach its own session")
	}
	if principal.Agent.MayReachSession("agent-orchestrator-58") {
		t.Fatal("a bound worker reached another session")
	}

	// A second bind, to a DIFFERENT session, must not re-point it.
	bound, err = svc.BindSession(ctx, issued.Credential.ID, "agent-orchestrator-58")
	if err != nil {
		t.Fatalf("second bind errored: %v", err)
	}
	if bound {
		t.Fatal("a credential was re-bound to a second session")
	}
	principal, err = svc.ResolveAgentPrincipal(ctx, issued.Token)
	if err != nil {
		t.Fatalf("ResolveAgentPrincipal after second bind: %v", err)
	}
	if principal.Agent.SessionID != "agent-orchestrator-59" {
		t.Fatalf("session = %q; the first binding must stand", principal.Agent.SessionID)
	}
}

// Binding is refused where it could not mean anything, and a revoked credential
// can never be resurrected into one.
func TestBindSessionRefusals(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	ctx := context.Background()

	if _, err := svc.BindSession(ctx, "", "agent-orchestrator-59"); err == nil {
		t.Error("binding with no credential id succeeded")
	}
	issued := issueWorker(t, svc, "wfs-1", "att-1")
	if _, err := svc.BindSession(ctx, issued.Credential.ID, ""); err == nil {
		t.Error("binding to an empty session succeeded; that would report success while leaving it unbound")
	}
	if _, err := svc.Revoke(ctx, issued.Credential.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	bound, err := svc.BindSession(ctx, issued.Credential.ID, "agent-orchestrator-59")
	if err != nil {
		t.Fatalf("bind after revoke errored: %v", err)
	}
	if bound {
		t.Fatal("a revoked credential was bound to a session")
	}
	// An unknown credential binds nothing and does not error: it is the same
	// answer as already-bound, and for the same reason.
	if bound, err := svc.BindSession(ctx, "no-such-credential", "agent-orchestrator-59"); err != nil || bound {
		t.Fatalf("binding an unknown credential: bound=%v err=%v", bound, err)
	}
}

// Revocation is idempotent: the state the caller wanted is the state that holds.
func TestRevokeIsIdempotent(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	issued := issueWorker(t, svc, "wfs-1", "att-1")
	ctx := context.Background()

	n, err := svc.Revoke(ctx, issued.Credential.ID)
	if err != nil || n != 1 {
		t.Fatalf("first revoke: n=%d err=%v", n, err)
	}
	n, err = svc.Revoke(ctx, issued.Credential.ID)
	if err != nil {
		t.Fatalf("second revoke errored: %v", err)
	}
	if n != 0 {
		t.Fatalf("second revoke reported %d rows, want 0", n)
	}
	if _, err := svc.ResolveAgentPrincipal(ctx, issued.Token); err == nil {
		t.Fatal("a revoked credential still authenticates")
	}
	// A blank id is a no-op rather than an error: there is nothing to end.
	if n, err := svc.Revoke(ctx, ""); err != nil || n != 0 {
		t.Fatalf("revoking nothing: n=%d err=%v", n, err)
	}
}

// A worker whose step is still running keeps its identity, however long the
// work takes. This is the property that stops the sweep recreating the failure
// the credential exists to prevent.
func TestAnActiveWorkerKeepsItsCredential(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	issued := issueWorker(t, svc, "wfs-1", "att-1")
	store.steps["wfs-1"] = domain.WorkflowStepRunning
	ctx := context.Background()

	for _, state := range []domain.WorkflowStepState{
		domain.WorkflowStepReady, domain.WorkflowStepRunning, domain.WorkflowStepWaiting,
	} {
		store.steps["wfs-1"] = state
		pending, err := svc.ListPendingWorkerRevocations(ctx)
		if err != nil {
			t.Fatalf("ListPendingWorkerRevocations: %v", err)
		}
		if len(pending) != 0 {
			t.Fatalf("state %q: the sweep would revoke a live worker's credential", state)
		}
		revoked, err := svc.ReconcileStaleWorkerCredentials(ctx)
		if err != nil {
			t.Fatalf("ReconcileStaleWorkerCredentials: %v", err)
		}
		if len(revoked) != 0 {
			t.Fatalf("state %q: the sweep revoked a live worker's credential", state)
		}
		if _, err := svc.ResolveAgentPrincipal(ctx, issued.Token); err != nil {
			t.Fatalf("state %q: a live worker lost its identity: %v", state, err)
		}
	}
}

// And loses it as soon as the step stops running — by any ending.
func TestAFinishedWorkerLosesItsCredential(t *testing.T) {
	for _, state := range []domain.WorkflowStepState{
		domain.WorkflowStepCompleted,
		domain.WorkflowStepFailed,
		domain.WorkflowStepCancelled,
		domain.WorkflowStepPending,
		// A step whose row is gone is finished too: the predicate is NOT
		// EXISTS, not a state comparison.
		"",
	} {
		t.Run(string(state)+"|gone", func(t *testing.T) {
			store := workerStore(t)
			svc := agentauth.New(store, nil, 0)
			issued := issueWorker(t, svc, "wfs-1", "att-1")
			if state != "" {
				store.steps["wfs-1"] = state
			}
			ctx := context.Background()

			revoked, err := svc.ReconcileStaleWorkerCredentials(ctx)
			if err != nil {
				t.Fatalf("ReconcileStaleWorkerCredentials: %v", err)
			}
			if len(revoked) != 1 || revoked[0].CredentialID != issued.Credential.ID {
				t.Fatalf("revoked = %+v, want the one worker credential", revoked)
			}
			// It says WHICH obligation it discharged, so a reader can check it.
			if revoked[0].WorkflowStepID != "wfs-1" {
				t.Errorf("the revocation does not name the step it was for: %+v", revoked[0])
			}
			if _, err := svc.ResolveAgentPrincipal(ctx, issued.Token); err == nil {
				t.Fatal("a finished worker still authenticates")
			}
			// Idempotent: a second pass finds nothing left to do.
			again, err := svc.ReconcileStaleWorkerCredentials(ctx)
			if err != nil {
				t.Fatalf("second pass: %v", err)
			}
			if len(again) != 0 {
				t.Fatalf("second pass revoked %d again", len(again))
			}
		})
	}
}

// A replacement ends its predecessor — the one ending the derived sweep cannot
// see, because a step being re-dispatched is still running.
func TestAReplacementEndsItsPredecessor(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	ctx := context.Background()

	first := issueWorker(t, svc, "wfs-1", "att-1")
	store.steps["wfs-1"] = domain.WorkflowStepRunning
	// The step is still running, so the sweep would keep the old credential.
	if revoked, err := svc.ReconcileStaleWorkerCredentials(ctx); err != nil || len(revoked) != 0 {
		t.Fatalf("the sweep can see this ending after all: revoked=%d err=%v", len(revoked), err)
	}

	second := issueWorker(t, svc, "wfs-1", "att-2")
	n, err := svc.RevokeSupersededWorkers(ctx, "wfs-1", "att-2")
	if err != nil {
		t.Fatalf("RevokeSupersededWorkers: %v", err)
	}
	if n != 1 {
		t.Fatalf("revoked %d superseded credentials, want 1", n)
	}
	if _, err := svc.ResolveAgentPrincipal(ctx, first.Token); err == nil {
		t.Fatal("the superseded worker still authenticates")
	}
	if _, err := svc.ResolveAgentPrincipal(ctx, second.Token); err != nil {
		t.Fatalf("the replacement lost its own identity: %v", err)
	}

	// Without both ids it cannot tell a predecessor from the launch it is
	// authorizing, so it does nothing rather than guess.
	if n, err := svc.RevokeSupersededWorkers(ctx, "wfs-1", ""); err != nil || n != 0 {
		t.Fatalf("revoking with no attempt: n=%d err=%v", n, err)
	}
	if n, err := svc.RevokeSupersededWorkers(ctx, "", "att-2"); err != nil || n != 0 {
		t.Fatalf("revoking with no step: n=%d err=%v", n, err)
	}
}

// A worker credential never reaches outside its project, whatever the account
// behind it may do elsewhere. This is what carries cross-project — and
// therefore cross-tenant — refusal, because a project belongs to exactly one
// tenant.
func TestWorkerCredentialIsConfinedToItsProject(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	issued := issueWorker(t, svc, "wfs-1", "att-1")
	if _, err := svc.BindSession(context.Background(), issued.Credential.ID, "agent-orchestrator-59"); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	authority := issued.Credential.Authority()
	authority.SessionID = "agent-orchestrator-59"

	own := domain.AuthzResource{Scope: domain.AuthzScopeProject, Project: "proj-1"}
	other := domain.AuthzResource{Scope: domain.AuthzScopeProject, Project: "proj-2"}
	if !authority.Allows(domain.PermSessionWrite, own) {
		t.Fatal("a worker cannot write in its own project")
	}
	if authority.Allows(domain.PermSessionWrite, other) {
		t.Fatal("a worker reached another project — and therefore another tenant")
	}
	// And it holds only the worker ceiling: no user, settings or org authority.
	for _, perm := range []domain.Permission{domain.PermSessionRead, domain.PermSessionWrite, domain.PermWorkflowRead} {
		if !authority.Allows(perm, own) {
			t.Errorf("a worker lacks %q, which ao work report needs", perm)
		}
	}
	if authority.Allows(domain.PermWorkflowCancel, own) {
		t.Error("a worker may cancel a workflow; that is outside its ceiling")
	}
}

// A pass that cannot read the store reports the failure rather than an empty
// sweep, so a caller cannot read "nothing to do" off a broken database.
func TestAFailedSweepNeverReportsSuccess(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	issueWorker(t, svc, "wfs-1", "att-1")
	store.listErr = errBoom
	ctx := context.Background()

	if _, err := svc.ReconcileStaleWorkerCredentials(ctx); err == nil {
		t.Fatal("a sweep that could not read reported success")
	}
	store.listErr = nil
	store.revokeErr = errBoom
	if _, err := svc.ReconcileStaleWorkerCredentials(ctx); err == nil {
		t.Fatal("a sweep that could not write reported success")
	}
	// The obligation survives: with the store healthy again, the next pass
	// discharges it. Nothing was remembered and nothing needed replaying.
	store.revokeErr = nil
	revoked, err := svc.ReconcileStaleWorkerCredentials(ctx)
	if err != nil {
		t.Fatalf("recovery pass: %v", err)
	}
	if len(revoked) != 1 {
		t.Fatalf("the recovery pass revoked %d, want the stranded credential", len(revoked))
	}
}

var errBoom = errBoomType{}

type errBoomType struct{}

func (errBoomType) Error() string { return "database is locked" }
