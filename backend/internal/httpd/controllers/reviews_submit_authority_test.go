package controllers_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	reviewsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/review"
)

// reviews_submit_authority_test.go -- AR-1a / D-SEC-2 at the transport: the
// review service must be told WHO the router authenticated, and its refusal
// must reach the caller as a 403 rather than a generic failure.

func reviewSubmitServer(t *testing.T, svc reviewsvc.Manager, resolver identity.AgentResolver, trustedLocal bool) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(
		config.Config{TrustedLocalMode: trustedLocal},
		log, nil,
		httpd.APIDeps{Reviews: svc, AgentAuth: resolver},
		httpd.ControlDeps{},
	))
	t.Cleanup(srv.Close)
	return srv
}

func postReviewSubmit(t *testing.T, srv *httptest.Server, session, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/sessions/"+session+"/reviews/submit",
		bytes.NewBufferString(`{"runId":"run-1","verdict":"approved"}`))
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
	return resp.StatusCode
}

func reviewerAuthority(sessionID domain.SessionID, reviewRunID string) *domain.AgentAuthority {
	return &domain.AgentAuthority{
		CredentialID: "cred-r", Role: domain.AgentRoleReviewer, ProjectID: "proj-1",
		SessionID: sessionID, WorkflowRunID: "wf-1", ReviewRunID: reviewRunID,
		Permissions: domain.AgentRoleCeiling(domain.AgentRoleReviewer),
	}
}

func TestReviewSubmitHandsTheAgentAuthorityToTheService(t *testing.T) {
	svc := &fakeReviewService{}
	srv := reviewSubmitServer(t, svc, staticAgentResolver{token: "tok-r", authority: reviewerAuthority("mer-1", "run-1")}, true)

	if status := postReviewSubmit(t, srv, "mer-1", "tok-r"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if svc.submitter.Agent == nil || svc.submitter.Agent.Role != domain.AgentRoleReviewer || svc.submitter.Agent.ReviewRunID != "run-1" {
		t.Fatalf("service saw submitter %+v, want the reviewer authority", svc.submitter)
	}
}

func TestReviewSubmitByAPersonCarriesNoAgentAuthority(t *testing.T) {
	svc := &fakeReviewService{}
	srv := reviewSubmitServer(t, svc, staticAgentResolver{token: "tok-r", authority: reviewerAuthority("mer-1", "run-1")}, true)

	if status := postReviewSubmit(t, srv, "mer-1", ""); status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (trusted-local person)", status)
	}
	if svc.submitter.Agent != nil {
		t.Fatalf("a header-less request reached the service as an agent: %+v", svc.submitter.Agent)
	}
}

func TestReviewSubmitRefusalIsA403(t *testing.T) {
	svc := &fakeReviewService{submitErr: reviewsvc.ErrForbidden}
	worker := boundWorkerAuthority("mer-1")
	srv := reviewSubmitServer(t, svc, staticAgentResolver{token: "tok-w", authority: worker}, true)

	if status := postReviewSubmit(t, srv, "mer-1", "tok-w"); status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if svc.submitter.Agent == nil || svc.submitter.Agent.Role != domain.AgentRoleWorker {
		t.Fatalf("service saw submitter %+v, want the worker authority", svc.submitter)
	}
}
