package agentauth_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	agentauth "github.com/aoagents/agent-orchestrator/backend/internal/service/agentauth"
)

// worker_authority_window_test.go — P5-A phase 2C audit: what a worker's
// credential can still do between the end of its attempt and the moment its
// authority is taken back.
//
// AgentRoleWorker's ceiling is session read, session write and workflow read.
// Session WRITE is not a narrow grant: AuthorizeSessionAccess gates
// POST /sessions/{id}/send (steer the agent in that session), /kill,
// /rollback, /restore, /resume-agent, /switch-agent, /pr/claim,
// PUT /reviewer and PUT /auto-review on exactly that permission. A credential
// that outlives its turn therefore keeps the ability to type into the session
// it was launched for — and Checkpoint 8D reuses one worker session across a
// step's whole loop, so that session is the one a LATER agent will be working
// in.
//
// These tests pin the boundary of that window from both ends.

// sessionWrites are the reachable actions that make the window worth closing.
// The list is here so a reader can see what "session write" actually buys
// rather than having to trust the phrase.
var sessionWrites = []string{
	"POST /sessions/{id}/send",
	"POST /sessions/{id}/kill",
	"POST /sessions/{id}/rollback",
	"POST /sessions/{id}/restore",
	"POST /sessions/{id}/resume-agent",
	"POST /sessions/{id}/switch-agent",
	"POST /sessions/{id}/pr/claim",
	"PUT  /sessions/{id}/reviewer",
	"PUT  /sessions/{id}/auto-review",
}

// A LIVE worker holds session write, which is the whole point: it has to be
// able to work. This is the control for the two tests below.
func TestALiveWorkerHoldsSessionWrite(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	issued := issueWorker(t, svc, "wfs-1", "att-1")
	store.steps["wfs-1"] = domain.WorkflowStepRunning
	ctx := context.Background()
	if _, err := svc.BindSession(ctx, issued.Credential.ID, "agent-orchestrator-59"); err != nil {
		t.Fatalf("BindSession: %v", err)
	}

	principal, err := svc.ResolveAgentPrincipal(ctx, issued.Token)
	if err != nil {
		t.Fatalf("a live worker cannot authenticate: %v", err)
	}
	own := domain.AuthzResource{Scope: domain.AuthzScopeProject, Project: "proj-1"}
	if !principal.Agent.Allows(domain.PermSessionWrite, own) ||
		!principal.Agent.MayReachSession("agent-orchestrator-59") {
		t.Fatalf("a live worker lacks session write over its own session; it could not do its job (%d routes)", len(sessionWrites))
	}
}

// THE WINDOW, CLOSED. Once the step stops running, the eager close takes the
// authority back immediately — before any sweep — so none of sessionWrites is
// reachable by the credential of a turn that is over.
//
// Before phase 2C's eager close this test failed: the credential stayed active
// until the reconciler's next pass, and for that whole interval it could still
// type into the session a later agent would be working in.
func TestAFinishedWorkerLosesSessionWriteImmediately(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	issued := issueWorker(t, svc, "wfs-1", "att-1")
	store.steps["wfs-1"] = domain.WorkflowStepRunning
	ctx := context.Background()
	if _, err := svc.BindSession(ctx, issued.Credential.ID, "agent-orchestrator-59"); err != nil {
		t.Fatalf("BindSession: %v", err)
	}

	// The turn ends.
	store.steps["wfs-1"] = domain.WorkflowStepCompleted

	// The eager close, at the transition — not the sweep.
	if _, err := svc.CloseFinishedWorkers(ctx); err != nil {
		t.Fatalf("CloseFinishedWorkers: %v", err)
	}

	if _, err := svc.ResolveAgentPrincipal(ctx, issued.Token); err == nil {
		t.Fatalf("a finished worker still authenticates, so it still reaches %d session writes including %q",
			len(sessionWrites), sessionWrites[0])
	}
}

// And the eager close is guarded by the same predicate the sweep uses, so
// calling it while the step is still running is a no-op. That is what makes it
// safe to call from the coordinator's ordinary observation pass rather than
// from every transition a step can make.
func TestTheEagerCloseNeverTouchesALiveWorker(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	issued := issueWorker(t, svc, "wfs-1", "att-1")
	ctx := context.Background()
	if _, err := svc.BindSession(ctx, issued.Credential.ID, "agent-orchestrator-59"); err != nil {
		t.Fatalf("BindSession: %v", err)
	}

	for _, state := range []domain.WorkflowStepState{
		domain.WorkflowStepReady, domain.WorkflowStepRunning, domain.WorkflowStepWaiting,
	} {
		store.steps["wfs-1"] = state
		closed, err := svc.CloseFinishedWorkers(ctx)
		if err != nil {
			t.Fatalf("state %q: %v", state, err)
		}
		if len(closed) != 0 {
			t.Fatalf("state %q: the eager close ended a live worker's credential", state)
		}
		if _, err := svc.ResolveAgentPrincipal(ctx, issued.Token); err != nil {
			t.Fatalf("state %q: a live worker lost its identity: %v", state, err)
		}
	}
}

// THE GENERATION FENCE, for every action rather than one route.
//
// `ao work report` is fenced twice: once by the credential, and once by the
// route resolving "the non-terminal run whose work step is dispatched into this
// session", which a superseded pane no longer satisfies. The other session
// writes have no such route-level check -- /send and /kill are gated on the
// bound session and nothing else.
//
// So the fence for them cannot be a per-route rule; it has to be the credential
// itself. It is: a replacement revokes its predecessors by ATTEMPT, and a
// revoked credential authenticates for NOTHING. That is strictly stronger than
// fencing one route, and this test states it in those terms so a future change
// that weakens supersession into a per-route check is caught.
func TestSupersessionFencesEveryAction(t *testing.T) {
	store := workerStore(t)
	svc := agentauth.New(store, nil, 0)
	ctx := context.Background()

	old := issueWorker(t, svc, "wfs-1", "att-1")
	if _, err := svc.BindSession(ctx, old.Credential.ID, "agent-orchestrator-59"); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	store.steps["wfs-1"] = domain.WorkflowStepRunning

	// The old pane can act, which is what makes the fence necessary.
	if _, err := svc.ResolveAgentPrincipal(ctx, old.Token); err != nil {
		t.Fatalf("the live worker cannot act: %v", err)
	}

	// A replacement launches. Checkpoint 8D reuses the SAME session, so the old
	// credential is still bound to a session that is very much in use -- which
	// is exactly why revoking it, rather than fencing one route, is the answer.
	fresh := issueWorker(t, svc, "wfs-1", "att-2")
	if _, err := svc.BindSession(ctx, fresh.Credential.ID, "agent-orchestrator-59"); err != nil {
		t.Fatalf("BindSession(replacement): %v", err)
	}
	if _, err := svc.RevokeSupersededWorkers(ctx, "wfs-1", "att-2"); err != nil {
		t.Fatalf("RevokeSupersededWorkers: %v", err)
	}

	// The superseded pane authenticates for nothing at all: not the report
	// route, and not any of the session writes either.
	if _, err := svc.ResolveAgentPrincipal(ctx, old.Token); err == nil {
		t.Fatalf("a superseded worker still authenticates, so it still reaches %d session writes", len(sessionWrites))
	}
	// And the replacement is untouched.
	principal, err := svc.ResolveAgentPrincipal(ctx, fresh.Token)
	if err != nil {
		t.Fatalf("the replacement lost its identity: %v", err)
	}
	if !principal.Agent.MayReachSession("agent-orchestrator-59") {
		t.Fatal("the replacement cannot reach the session it was launched for")
	}
}
