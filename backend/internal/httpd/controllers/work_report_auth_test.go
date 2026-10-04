package controllers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// work_report_auth_test.go — the identity binding on P5-A phase 2B's route.
//
// The work-report route reuses AuthorizeSessionAccess rather than inventing a
// gate, so what has to be true is that a WORKER credential is bound the same
// way a reviewer's is: to the one session it was launched for, and to nothing
// else in the project its account can reach.
//
// That matters more here than it looks. A report is a declaration that travels
// into another run's review pack, so a credential that could post one into a
// session it does not own would be able to put words in a colleague's mouth —
// and the reviewer reading them would have no way to tell.

func workerAuthority(sessionID string) *domain.AgentAuthority {
	return &domain.AgentAuthority{
		Role:          domain.AgentRoleWorker,
		ProjectID:     "proj-1",
		SessionID:     domain.SessionID(sessionID),
		WorkflowRunID: "wf-98ab416c",
		Permissions:   domain.AgentRoleCeiling(domain.AgentRoleWorker),
	}
}

// A worker may report on its own session and on no other.
func TestWorkerReportsOnlyOnTheSessionItWasLaunchedFor(t *testing.T) {
	scoping := SessionScoping{Guard: Guard{Authz: allowAllAuthorizer{}, Scope: staticScope{sessionProject: "proj-1"}}}

	w := httptest.NewRecorder()
	own := agentRequest(http.MethodPost, "/api/v1/sessions/agent-orchestrator-59/work-report", workerAuthority("agent-orchestrator-59"))
	if !AuthorizeSessionAccess(w, own, scoping, "agent-orchestrator-59") {
		t.Fatalf("a worker was denied its own session (%d): %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	foreign := agentRequest(http.MethodPost, "/api/v1/sessions/agent-orchestrator-58/work-report", workerAuthority("agent-orchestrator-59"))
	if AuthorizeSessionAccess(w, foreign, scoping, "agent-orchestrator-58") {
		t.Fatal("a worker reported into a session it was not launched for")
	}
	// 404, never 403: a session id a caller may not reach must be
	// indistinguishable from one that does not exist.
	if w.Code != http.StatusNotFound {
		t.Fatalf("denial status = %d, want 404", w.Code)
	}
}

// The binding is a property of the CREDENTIAL, so it holds on a trusted-local
// desktop with no authorization wired at all.
func TestWorkerReportBindingHoldsWithAuthorizationDisabled(t *testing.T) {
	for name, scoping := range map[string]SessionScoping{
		"no guard, no ownership": {},
		"trusted local":          {TrustedLocal: true},
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := agentRequest(http.MethodPost, "/api/v1/sessions/agent-orchestrator-58/work-report", workerAuthority("agent-orchestrator-59"))
			if AuthorizeSessionAccess(w, r, scoping, "agent-orchestrator-58") {
				t.Fatal("a worker reached a foreign session with authorization disabled")
			}
		})
	}
}

// A REVIEWER credential must not be able to file a work report either. The
// route's permission is session write, which a reviewer holds — so what stops
// it is the session binding, and this pins that the binding is what carries the
// weight rather than the permission set.
func TestAReviewerCannotFileAReportIntoAnotherSession(t *testing.T) {
	scoping := SessionScoping{Guard: Guard{Authz: allowAllAuthorizer{}, Scope: staticScope{sessionProject: "proj-1"}}}
	w := httptest.NewRecorder()
	r := agentRequest(http.MethodPost, "/api/v1/sessions/agent-orchestrator-58/work-report", reviewerAuthority())
	if AuthorizeSessionAccess(w, r, scoping, "agent-orchestrator-58") {
		t.Fatal("a reviewer filed a work report into a session it was not launched for")
	}
}
