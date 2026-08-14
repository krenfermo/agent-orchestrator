package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

	startRunID string
	startErr   error
	startCalls int

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

func (f *fakeWorkflowService) StartRun(_ context.Context, runID string) (workflowcore.RunDetail, error) {
	f.startRunID = runID
	f.startCalls++
	if f.startErr != nil {
		return workflowcore.RunDetail{}, f.startErr
	}
	if f.detail.Run.ID != "" {
		return f.detail, nil
	}
	return workflowcore.RunDetail{Run: domain.WorkflowRun{ID: runID, State: domain.WorkflowRunRunning}}, nil
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

func TestWorkflowStartRun(t *testing.T) {
	svc := &fakeWorkflowService{}
	srv := newWorkflowTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/start", "")
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if svc.startRunID != "wf-1" {
		t.Fatalf("forwarded run id = %q", svc.startRunID)
	}
}

// TestWorkflowStartRunIdempotentSecondCall asserts calling /start twice both
// forward to the service (which itself owns idempotency — see
// Coordinator.StartRun) and both return 200, never erroring on a repeat call.
func TestWorkflowStartRunIdempotentSecondCall(t *testing.T) {
	svc := &fakeWorkflowService{}
	srv := newWorkflowTestServer(t, svc)

	for i := 0; i < 2; i++ {
		body, status, headers := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/start", "")
		assertJSON(t, headers)
		if status != http.StatusOK {
			t.Fatalf("call %d: status=%d body=%s", i, status, body)
		}
	}
	if svc.startCalls != 2 {
		t.Fatalf("service StartRun calls = %d, want 2 (controller forwards every call; idempotency lives in the service)", svc.startCalls)
	}
}

func TestWorkflowStartRunAlreadyTerminal(t *testing.T) {
	svc := &fakeWorkflowService{startErr: workflowsvc.ErrAlreadyTerminal}
	srv := newWorkflowTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/start", "")
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusConflict, "WORKFLOW_ALREADY_TERMINAL")
}

func TestWorkflowStartRunNotFound(t *testing.T) {
	svc := &fakeWorkflowService{startErr: workflowsvc.ErrNotFound}
	srv := newWorkflowTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/workflows/missing/start", "")
	assertJSON(t, headers)
	assertErrorCode(t, body, status, http.StatusNotFound, "WORKFLOW_NOT_FOUND")
}

// TestWorkflowGetRunSurfacesCheckpointAndNextActionFields is a regression
// guard for Checkpoint 8B's response extension: branch/worktreePath/headSha/
// nextAction per step (from the latest checkpoint) and the run's own
// synthesized nextAction must round-trip through the wire view.
func TestWorkflowGetRunSurfacesCheckpointAndNextActionFields(t *testing.T) {
	now := time.Now().UTC()
	sessionID := "sess-1"
	stepID := "wfs-work"
	svc := &fakeWorkflowService{detail: workflowcore.RunDetail{
		Run:        domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1", Objective: "ship it", State: domain.WorkflowRunWaiting, CreatedAt: now, UpdatedAt: now},
		NextAction: "start_review",
		Steps: []workflowcore.StepDetail{
			{
				Step: domain.WorkflowStep{
					ID: stepID, WorkflowRunID: "wf-1", Kind: domain.WorkflowStepWork, Ordinal: 2,
					State: domain.WorkflowStepCompleted, SessionID: &sessionID, CreatedAt: now, UpdatedAt: now,
				},
				LatestCheckpoint: &domain.WorkflowCheckpoint{
					ID: "wfc-1", WorkflowRunID: "wf-1", WorkflowStepID: &stepID, ProjectID: "proj-1",
					SessionID: &sessionID, Branch: "ao/wf-1", WorktreePath: "/tmp/wt", HeadSHA: "deadbeef",
					NextAction: "start_review", CreatedAt: now,
				},
			},
		},
	}}
	srv := newWorkflowTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "GET", "/api/v1/workflows/wf-1", "")
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	bodyStr := string(body)
	for _, want := range []string{`"nextAction":"start_review"`, `"branch":"ao/wf-1"`, `"worktreePath":"/tmp/wt"`, `"headSha":"deadbeef"`, `"sessionId":"sess-1"`} {
		if !strings.Contains(bodyStr, want) {
			t.Fatalf("response body missing %q: %s", want, bodyStr)
		}
	}
}
