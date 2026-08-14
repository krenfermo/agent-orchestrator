package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type fakeWorkflowService struct {
	createdProjectID string
	createdObjective string
	createErr        error

	getRunID string
	getErr   error

	listFilter workflowsvc.ListFilter
	listErr    error

	cancelRunID string
	cancelErr   error

	detail workflowcore.RunDetail
	runs   []domain.WorkflowRun
}

func (f *fakeWorkflowService) CreateRun(_ context.Context, projectID, objective string) (workflowcore.RunDetail, error) {
	f.createdProjectID = projectID
	f.createdObjective = objective
	if f.createErr != nil {
		return workflowcore.RunDetail{}, f.createErr
	}
	if f.detail.Run.ID != "" {
		return f.detail, nil
	}
	return workflowcore.RunDetail{Run: domain.WorkflowRun{
		ID: "wf-1", ProjectID: projectID, Objective: objective, State: domain.WorkflowRunPending,
	}}, nil
}

func (f *fakeWorkflowService) GetRun(_ context.Context, runID string) (workflowcore.RunDetail, error) {
	f.getRunID = runID
	if f.getErr != nil {
		return workflowcore.RunDetail{}, f.getErr
	}
	if f.detail.Run.ID != "" {
		return f.detail, nil
	}
	return workflowcore.RunDetail{Run: domain.WorkflowRun{ID: runID, State: domain.WorkflowRunPending}}, nil
}

func (f *fakeWorkflowService) ListRuns(_ context.Context, filter workflowsvc.ListFilter) ([]domain.WorkflowRun, error) {
	f.listFilter = filter
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.runs, nil
}

func (f *fakeWorkflowService) CancelRun(_ context.Context, runID string) (workflowcore.RunDetail, error) {
	f.cancelRunID = runID
	if f.cancelErr != nil {
		return workflowcore.RunDetail{}, f.cancelErr
	}
	return workflowcore.RunDetail{Run: domain.WorkflowRun{ID: runID, State: domain.WorkflowRunCancelled}}, nil
}

func newWorkflowTestServer(t *testing.T, svc workflowsvc.Manager) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{Workflows: svc}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWorkflowCreateRun(t *testing.T) {
	svc := &fakeWorkflowService{}
	srv := newWorkflowTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows", `{"objective":"ship the thing"}`)
	assertJSON(t, headers)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if svc.createdProjectID != "proj-1" || svc.createdObjective != "ship the thing" {
		t.Fatalf("forwarded projectID=%q objective=%q", svc.createdProjectID, svc.createdObjective)
	}
}

func TestWorkflowGetRun(t *testing.T) {
	svc := &fakeWorkflowService{}
	srv := newWorkflowTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "GET", "/api/v1/workflows/wf-1", "")
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if svc.getRunID != "wf-1" {
		t.Fatalf("forwarded run id = %q", svc.getRunID)
	}
}

func TestWorkflowGetRunNotFound(t *testing.T) {
	svc := &fakeWorkflowService{getErr: workflowsvc.ErrNotFound}
	srv := newWorkflowTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "GET", "/api/v1/workflows/missing", "")
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusNotFound, "WORKFLOW_NOT_FOUND")
}

func TestWorkflowListRuns(t *testing.T) {
	now := time.Now().UTC()
	svc := &fakeWorkflowService{runs: []domain.WorkflowRun{
		{ID: "wf-1", ProjectID: "proj-1", Objective: "a", State: domain.WorkflowRunPending, CreatedAt: now, UpdatedAt: now},
	}}
	srv := newWorkflowTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "GET", "/api/v1/workflows?projectId=proj-1", "")
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if svc.listFilter.ProjectID != "proj-1" {
		t.Fatalf("forwarded filter = %+v", svc.listFilter)
	}
}

func TestWorkflowCancelRun(t *testing.T) {
	svc := &fakeWorkflowService{}
	srv := newWorkflowTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/cancel", "")
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if svc.cancelRunID != "wf-1" {
		t.Fatalf("forwarded run id = %q", svc.cancelRunID)
	}
}

func TestWorkflowCancelRunAlreadyTerminal(t *testing.T) {
	svc := &fakeWorkflowService{cancelErr: workflowsvc.ErrAlreadyTerminal}
	srv := newWorkflowTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/cancel", "")
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusConflict, "WORKFLOW_ALREADY_TERMINAL")
}
