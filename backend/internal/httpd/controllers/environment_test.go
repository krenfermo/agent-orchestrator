package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	environmentsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/environment"
)

type fakeEnvironmentStatusProvider struct {
	status     environmentsvc.Status
	statusErr  error
	ghStatus   environmentsvc.GitHubStatus
	ghTestErr  error
	statusHits int
	ghTestHits int
}

func (f *fakeEnvironmentStatusProvider) Status(context.Context) (environmentsvc.Status, error) {
	f.statusHits++
	return f.status, f.statusErr
}

func (f *fakeEnvironmentStatusProvider) TestGitHub(context.Context) (environmentsvc.GitHubStatus, error) {
	f.ghTestHits++
	return f.ghStatus, f.ghTestErr
}

func TestEnvironmentRoutes_DefaultToStubWithoutService(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	body, status, headers := doRequest(t, srv, "GET", "/api/v1/environment/status", "")
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusNotImplemented, "NOT_IMPLEMENTED")
}

func TestEnvironmentAPI_Status(t *testing.T) {
	fake := &fakeEnvironmentStatusProvider{status: environmentsvc.Status{
		Codex:     environmentsvc.AgentCapability{ID: "codex", Installed: true, AuthState: "authorized", Source: "unknown"},
		Claude:    environmentsvc.AgentCapability{ID: "claude-code", Source: "unknown"},
		GitHub:    environmentsvc.GitHubStatus{Installed: true, AuthState: environmentsvc.GitHubAuthStateAuthenticated},
		Projects:  environmentsvc.ProjectsSummary{Count: 2},
		Readiness: environmentsvc.Readiness{Overall: environmentsvc.ReadinessReady},
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{
		Environment: fake,
	}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	body, status, headers := doRequest(t, srv, "GET", "/api/v1/environment/status", "")
	if status != http.StatusOK {
		t.Fatalf("GET /environment/status = %d, want 200; body=%s", status, body)
	}
	assertJSON(t, headers)
	var got environmentsvc.Status
	mustJSON(t, body, &got)
	if got.Projects.Count != 2 || got.Readiness.Overall != environmentsvc.ReadinessReady {
		t.Errorf("got = %+v", got)
	}
	if fake.statusHits != 1 {
		t.Errorf("statusHits = %d, want 1", fake.statusHits)
	}
}

func TestEnvironmentAPI_TestGitHub(t *testing.T) {
	fake := &fakeEnvironmentStatusProvider{ghStatus: environmentsvc.GitHubStatus{
		Installed: true, AuthState: environmentsvc.GitHubAuthStateAuthenticated, Login: "octocat", Host: "github.com",
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{
		Environment: fake,
	}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/environment/github/test", "")
	if status != http.StatusOK {
		t.Fatalf("POST /environment/github/test = %d, want 200; body=%s", status, body)
	}
	assertJSON(t, headers)
	var got environmentsvc.GitHubStatus
	mustJSON(t, body, &got)
	if got.Login != "octocat" {
		t.Errorf("Login = %q, want octocat", got.Login)
	}
	if fake.ghTestHits != 1 {
		t.Errorf("ghTestHits = %d, want 1", fake.ghTestHits)
	}
}
