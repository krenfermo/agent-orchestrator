package controllers_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
)

// work_report_oidc_test.go — P5-A phase 2C, end to end over HTTP with
// trusted-local OFF.
//
// This is the gap 2C exists to close. Before it, `ao work report` worked only on
// a desktop, where a cookie-less call resolves the bootstrap admin; on an
// installation that requires an identity there was nothing for a worker to
// present, so the call was simply unauthenticated.
//
// These drive the REAL router — the real identity middleware, the real session
// scoping — with a real worker credential in the real header, and assert the
// three answers that matter: its own session works, another session does not,
// and no credential at all does not.

// staticAgentResolver resolves one token to one worker authority, the way
// agentauth's own resolver does after a successful bind.
type staticAgentResolver struct {
	token     string
	authority *domain.AgentAuthority
}

func (r staticAgentResolver) ResolveAgentPrincipal(_ context.Context, raw string) (domain.Principal, error) {
	if raw != r.token || r.authority == nil {
		return domain.Principal{}, errNoSuchCredential
	}
	return domain.Principal{
		User:       domain.User{ID: "user-owner", Role: domain.UserRoleOwner, Status: domain.UserStatusActive},
		AuthMethod: domain.AuthMethodAgent,
		Agent:      r.authority,
	}, nil
}

type noSuchCredential struct{}

func (noSuchCredential) Error() string { return "agent credential not found, revoked, or expired" }

var errNoSuchCredential = noSuchCredential{}

// boundWorkerAuthority is what a worker credential looks like AFTER the
// deferred binding has landed: it names one session, one run and one step.
func boundWorkerAuthority(sessionID domain.SessionID) *domain.AgentAuthority {
	return &domain.AgentAuthority{
		CredentialID:   "cred-1",
		Role:           domain.AgentRoleWorker,
		ProjectID:      "proj-1",
		SessionID:      sessionID,
		WorkflowRunID:  "wf-1",
		WorkflowStepID: "wfs-1",
		Permissions:    domain.AgentRoleCeiling(domain.AgentRoleWorker),
	}
}

// ownedSessions is the session-ownership store an installation always wires.
//
// It has to be here for these tests to mean anything. SessionScoping.enforced()
// is false when Ownership is nil, and an unenforced scoping lets every session
// route through — which is true of `ao review submit` and every other session
// route too, not something this one introduced. A test that omitted it would
// therefore "pass" while proving nothing about OIDC.
type ownedSessions map[domain.SessionID]domain.UserID

func (o ownedSessions) GetSessionOwner(_ context.Context, id domain.SessionID) (*domain.UserID, error) {
	owner, ok := o[id]
	if !ok {
		return nil, nil
	}
	return &owner, nil
}

// oidcServer builds the router the way a real identity-requiring installation
// does: trusted-local OFF, session ownership wired, and one agent credential
// live — so nothing resolves an identity except the header.
func oidcServer(t *testing.T, svc workflowsvc.Manager, resolver identity.AgentResolver) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(
		config.Config{TrustedLocalMode: false},
		log, nil,
		httpd.APIDeps{
			Workflows: svc,
			AgentAuth: resolver,
			SessionOwnership: ownedSessions{
				"agent-orchestrator-59": "user-owner",
				"agent-orchestrator-58": "user-owner",
			},
		},
		httpd.ControlDeps{},
	))
	t.Cleanup(srv.Close)
	return srv
}

func postWorkReport(t *testing.T, srv *httptest.Server, session, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/api/v1/sessions/"+session+"/work-report", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(identity.AgentTokenHeader, token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// THE HEADLINE. Under OIDC, a worker presenting its own credential records its
// report — no cookie, no bootstrap admin, no trusted-local.
func TestUnderOIDCAWorkerReportsWithItsOwnCredential(t *testing.T) {
	svc := &workReportService{receipt: workReportReceiptFor("wf-1")}
	resolver := staticAgentResolver{token: "tok-1", authority: boundWorkerAuthority("agent-orchestrator-59")}
	srv := oidcServer(t, svc, resolver)

	status, body := postWorkReport(t, srv, "agent-orchestrator-59", "tok-1", `{"summary":"renamed the helper"}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(svc.got) != 1 || svc.got[0].Summary != "renamed the helper" {
		t.Fatalf("the service did not receive the report: %+v", svc.got)
	}
}

// And reaches nothing else. A worker bound to one session cannot file into
// another, whatever its account may do elsewhere.
func TestUnderOIDCAWorkerCannotReportIntoAnotherSession(t *testing.T) {
	svc := &workReportService{receipt: workReportReceiptFor("wf-1")}
	resolver := staticAgentResolver{token: "tok-1", authority: boundWorkerAuthority("agent-orchestrator-59")}
	srv := oidcServer(t, svc, resolver)

	status, body := postWorkReport(t, srv, "agent-orchestrator-58", "tok-1", `{"summary":"not mine"}`)
	// 404, never 403: a session a caller may not reach is indistinguishable
	// from one that does not exist.
	if status != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", status, body)
	}
	if len(svc.got) != 0 {
		t.Fatalf("a foreign report reached the service: %+v", svc.got)
	}
}

// An UNBOUND credential — one whose launch died between minting and binding —
// reaches no session at all. This is what makes the deferred binding safe.
func TestUnderOIDCAnUnboundCredentialReachesNothing(t *testing.T) {
	svc := &workReportService{receipt: workReportReceiptFor("wf-1")}
	resolver := staticAgentResolver{token: "tok-1", authority: boundWorkerAuthority("")}
	srv := oidcServer(t, svc, resolver)

	status, body := postWorkReport(t, srv, "agent-orchestrator-59", "tok-1", `{"summary":"x"}`)
	if status != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404 for an unbound credential", status, body)
	}
	if len(svc.got) != 0 {
		t.Fatalf("an unbound credential filed a report: %+v", svc.got)
	}
}

// A revoked or unknown token authenticates nobody, and — critically — does not
// fall through to any other identity. A stale agent token must not borrow
// whatever happens to be lying around.
func TestUnderOIDCARevokedCredentialIsRefused(t *testing.T) {
	svc := &workReportService{receipt: workReportReceiptFor("wf-1")}
	// The resolver knows "tok-1"; the worker presents the token it used to have.
	resolver := staticAgentResolver{token: "tok-1", authority: boundWorkerAuthority("agent-orchestrator-59")}
	srv := oidcServer(t, svc, resolver)

	status, body := postWorkReport(t, srv, "agent-orchestrator-59", "tok-revoked", `{"summary":"x"}`)
	if status == http.StatusOK {
		t.Fatalf("a revoked credential filed a report: %s", body)
	}
	if len(svc.got) != 0 {
		t.Fatalf("a revoked credential reached the service: %+v", svc.got)
	}
}

// And with no credential at all, under OIDC, there is no identity to fall back
// on — which is precisely the state 2C exists to fix, asserted so a regression
// that silently re-enables anonymous reporting is caught.
func TestUnderOIDCNoCredentialMeansNoReport(t *testing.T) {
	svc := &workReportService{receipt: workReportReceiptFor("wf-1")}
	resolver := staticAgentResolver{token: "tok-1", authority: boundWorkerAuthority("agent-orchestrator-59")}
	srv := oidcServer(t, svc, resolver)

	status, body := postWorkReport(t, srv, "agent-orchestrator-59", "", `{"summary":"x"}`)
	if status == http.StatusOK {
		t.Fatalf("an unauthenticated report was accepted: %s", body)
	}
	if len(svc.got) != 0 {
		t.Fatalf("an unauthenticated report reached the service: %+v", svc.got)
	}
}
