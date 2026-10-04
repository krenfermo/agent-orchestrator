package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/authz"
)

// allowAllAuthorizer says yes to everything, so these tests can prove the
// BINDING alone denies -- not the permission cap, which authz's own tests cover.
// If the binding were not enforced here, every case below would pass.
type allowAllAuthorizer struct{}

func (allowAllAuthorizer) Authorize(context.Context, domain.Principal, domain.Permission, domain.AuthzResource) error {
	return nil
}
func (allowAllAuthorizer) Resolve(context.Context, domain.Principal) (authz.Subject, error) {
	return authz.Subject{}, nil
}
func (allowAllAuthorizer) InstallationUnclaimed(context.Context) (bool, error) { return false, nil }

type staticScope struct {
	sessionProject domain.ProjectID
	runProject     domain.ProjectID
}

func (s staticScope) GetSessionProjectID(context.Context, domain.SessionID) (domain.ProjectID, bool, error) {
	return s.sessionProject, s.sessionProject != "", nil
}
func (s staticScope) GetWorkflowRunProjectID(context.Context, string) (domain.ProjectID, bool, error) {
	return s.runProject, s.runProject != "", nil
}

func agentRequest(method, target string, authority *domain.AgentAuthority) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	p := domain.Principal{
		User:       domain.User{ID: "user-owner", Role: domain.UserRoleOwner, Status: domain.UserStatusActive},
		AuthMethod: domain.AuthMethodOIDC,
	}
	if authority != nil {
		p.AuthMethod = domain.AuthMethodAgent
		p.Agent = authority
	}
	return r.WithContext(identity.WithPrincipal(r.Context(), p))
}

func reviewerAuthority() *domain.AgentAuthority {
	return &domain.AgentAuthority{
		Role:          domain.AgentRoleReviewer,
		ProjectID:     "proj-1",
		SessionID:     "agent-orchestrator-59",
		WorkflowRunID: "wf-98ab416c",
		Permissions:   domain.AgentRoleCeiling(domain.AgentRoleReviewer),
	}
}

// A reviewer records its verdict on the session it was launched for. It must not
// be able to steer a colleague's session merely because both live in the project
// its account can reach -- project-scoped RBAC is the right model for a person
// and the wrong one for a credential minted for a single launch.
func TestAgentReachesOnlyTheSessionItWasLaunchedFor(t *testing.T) {
	scoping := SessionScoping{Guard: Guard{Authz: allowAllAuthorizer{}, Scope: staticScope{sessionProject: "proj-1"}}}

	w := httptest.NewRecorder()
	own := agentRequest(http.MethodPost, "/api/v1/sessions/agent-orchestrator-59/reviews/submit", reviewerAuthority())
	if !AuthorizeSessionAccess(w, own, scoping, "agent-orchestrator-59") {
		t.Fatalf("a reviewer was denied its own session (%d): %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	foreign := agentRequest(http.MethodPost, "/api/v1/sessions/agent-orchestrator-58/send", reviewerAuthority())
	if AuthorizeSessionAccess(w, foreign, scoping, "agent-orchestrator-58") {
		t.Fatalf("a reviewer reached a session it was not launched for")
	}
	// 404, never 403: a session id a caller may not reach must be
	// indistinguishable from one that does not exist.
	if w.Code != http.StatusNotFound {
		t.Fatalf("denial status = %d, want 404", w.Code)
	}
}

// The binding is a property of the CREDENTIAL, so it holds even where
// authorization is not wired at all -- a trusted-local desktop must still not
// let a reviewer steer a session it has no business in.
func TestAgentSessionBindingHoldsWithAuthorizationDisabled(t *testing.T) {
	for name, scoping := range map[string]SessionScoping{
		"no guard, no ownership": {},
		"trusted local":          {TrustedLocal: true},
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := agentRequest(http.MethodPost, "/api/v1/sessions/agent-orchestrator-58/send", reviewerAuthority())
			if AuthorizeSessionAccess(w, r, scoping, "agent-orchestrator-58") {
				t.Fatalf("a reviewer reached a foreign session with authorization disabled")
			}
			// Its own session still works.
			w = httptest.NewRecorder()
			own := agentRequest(http.MethodPost, "/api/v1/sessions/agent-orchestrator-59/reviews/submit", reviewerAuthority())
			if !AuthorizeSessionAccess(w, own, scoping, "agent-orchestrator-59") {
				t.Fatalf("a reviewer was denied its own session with authorization disabled")
			}
		})
	}
}

// Same rule for the run it belongs to.
func TestAgentReachesOnlyTheWorkflowRunItBelongsTo(t *testing.T) {
	g := Guard{Authz: allowAllAuthorizer{}, Scope: staticScope{runProject: "proj-1"}}

	w := httptest.NewRecorder()
	own := agentRequest(http.MethodGet, "/api/v1/workflows/wf-98ab416c", reviewerAuthority())
	if !g.AllowWorkflowRun(w, own, domain.PermWorkflowRead, "wf-98ab416c") {
		t.Fatalf("an agent was denied its own run (%d): %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	foreign := agentRequest(http.MethodPost, "/api/v1/workflows/wf-other/cancel", reviewerAuthority())
	if g.AllowWorkflowRun(w, foreign, domain.PermWorkflowCancel, "wf-other") {
		t.Fatalf("an agent reached a run it does not belong to")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("denial status = %d, want 404", w.Code)
	}
}

// A human request carries no agent authority, so both gates behave exactly as
// they did before P4-I.
func TestHumanRequestsPassTheBindingGatesUntouched(t *testing.T) {
	scoping := SessionScoping{Guard: Guard{Authz: allowAllAuthorizer{}, Scope: staticScope{sessionProject: "proj-1"}}}
	w := httptest.NewRecorder()
	r := agentRequest(http.MethodPost, "/api/v1/sessions/agent-orchestrator-58/send", nil)
	if !AuthorizeSessionAccess(w, r, scoping, "agent-orchestrator-58") {
		t.Fatalf("a human request was denied by the agent binding gate (%d)", w.Code)
	}

	g := Guard{Authz: allowAllAuthorizer{}, Scope: staticScope{runProject: "proj-1"}}
	w = httptest.NewRecorder()
	if !g.AllowWorkflowRun(w, agentRequest(http.MethodGet, "/api/v1/workflows/wf-other", nil), domain.PermWorkflowRead, "wf-other") {
		t.Fatalf("a human request was denied by the run binding gate (%d)", w.Code)
	}
}
