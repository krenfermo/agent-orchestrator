package httpd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/executionpolicy"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/providerprofile"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type staticPlannerHTTP struct {
	plan workflowcore.MasterPlan
}

func (p *staticPlannerHTTP) Generate(context.Context, workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	return workflowcore.PlannerResponse{Plan: p.plan, Provider: "fake", Model: "fake-v1"}, nil
}
func (p *staticPlannerHTTP) Descriptor() (string, string) { return "fake", "fake-v1" }

type staticContextHTTP struct{}

func (staticContextHTTP) Build(_ context.Context, proj domain.ProjectRecord) (workflowcore.PlannerContext, error) {
	return workflowcore.PlannerContext{Version: "v1", ProjectID: string(proj.ID), ProjectPath: proj.Path, Documents: []workflowcore.PlannerDocument{}}, nil
}

func twoStepMasterPlanHTTP() workflowcore.MasterPlan {
	verify := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", Args: []string{"test", "./..."}, TimeoutSeconds: 60, RequiredExitCode: 0, RetrySafe: true}}, Files: []workflowcore.VerificationFileCheck{}}
	return workflowcore.MasterPlan{Version: "v1", Objective: "Build users", Summary: "two steps", Steps: []workflowcore.PlannedStep{
		{ID: "model", Title: "Model", Description: "Add the model.", Dependencies: []string{}, AcceptanceCriteria: []string{"ok"}, Verify: verify},
		{ID: "tests", Title: "Tests", Description: "Add tests.", Dependencies: []string{"model"}, AcceptanceCriteria: []string{"ok"}, Verify: verify},
	}}
}

// TestChildWorkflowOwnershipIDOR is Checkpoint 8P-C.1's E2E A/security
// proof: User A creates a Master Workflow with planned child tasks; the
// master and every child task are durably owned by A; User B cannot GET
// any child run or its questions by id (404, not 403 -- existence must not
// leak), matching provider_profiles.go's IDOR precedent.
func TestChildWorkflowOwnershipIDOR(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	authMgr := authsvc.New(store, func() time.Time { return time.Now().UTC() })
	coord := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store,
		Planner: &staticPlannerHTTP{plan: twoStepMasterPlanHTTP()}, PlannerContextBuilder: staticContextHTTP{},
		ProviderProfiles: store, ExecutionPolicies: store,
		QuestionsStore: store,
		Clock:          func() time.Time { return time.Now().UTC() },
	})
	answerSvc := &questions.AnswerService{Store: store, Runs: store}
	profileSvc := &providerprofile.Service{Store: store, DataDir: fixedDataDir(t.TempDir())}
	policySvc := &executionpolicy.Service{Store: store}

	cfg := config.Config{TrustedLocalMode: false}
	deps := APIDeps{
		Auth: authMgr, Workflows: workflowsvc.New(coord), Questions: answerSvc,
		ProjectOwnership: store, WorkflowOwnership: store,
		ProviderProfiles: profileSvc, ExecutionPolicy: policySvc,
	}
	router := NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	if _, err := authMgr.CreateUser(t.Context(), authsvc.CreateUserInput{Email: "alice-child@example.com", Username: "alice-child", Password: "correct-horse-a"}); err != nil {
		t.Fatalf("create user A: %v", err)
	}
	if _, err := authMgr.CreateUser(t.Context(), authsvc.CreateUserInput{Email: "bob-child@example.com", Username: "bob-child", Password: "correct-horse-b"}); err != nil {
		t.Fatalf("create user B: %v", err)
	}
	client := &http.Client{}
	_, cookieA := loginOK(t, srv.URL, "alice-child@example.com", "correct-horse-a")
	_, cookieB := loginOK(t, srv.URL, "bob-child@example.com", "correct-horse-b")

	// A creates a Master Workflow (manual approval), then generates and
	// approves the plan through the normal manual-plan API path -- no
	// autonomous execution.
	createBody, _ := json.Marshal(map[string]any{
		"objective": "Build users", "masterPlan": true, "planApprovalMode": "manual",
	})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/projects/p/workflows", strings.NewReader(string(createBody)))
	if err != nil {
		t.Fatalf("build create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookieA)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create master workflow: %v", err)
	}
	var created struct {
		Workflow struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
		} `json:"workflow"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.Workflow.Run.ID == "" {
		t.Fatalf("create master workflow: status=%d body=%+v", resp.StatusCode, created)
	}
	masterID := created.Workflow.Run.ID

	assertStatusMethod(t, client, http.MethodPost, srv.URL+"/api/v1/workflows/"+masterID+"/plan/generate", cookieA, nil, http.StatusOK)
	assertStatusMethod(t, client, http.MethodPost, srv.URL+"/api/v1/workflows/"+masterID+"/plan/approve", cookieA, nil, http.StatusOK)

	// --- parent owner=A, both children owner=A (checkpoint brief E2E A) ---
	masterDetail := getWorkflowDetail(t, client, srv.URL, masterID, cookieA)
	if len(masterDetail.Tasks) != 2 || masterDetail.Tasks[0].ExecutionRunID == "" {
		t.Fatalf("expected 2 tasks with the first dispatched, got %+v", masterDetail.Tasks)
	}
	child1 := masterDetail.Tasks[0].ExecutionRunID

	child1Owner, err := store.GetWorkflowRunOwner(ctx, child1)
	if err != nil {
		t.Fatalf("GetWorkflowRunOwner(child1): %v", err)
	}
	if child1Owner == nil {
		t.Fatalf("child1 has no owner")
	}
	if masterOwner, _ := store.GetWorkflowRunOwner(ctx, masterID); masterOwner == nil || *masterOwner != *child1Owner {
		t.Fatalf("child1 owner %v does not match master owner %v", child1Owner, masterOwner)
	}

	// --- B cannot see the master run, the child run, or the child's
	// questions by id -- 404, never a distinguishable 403. ---
	assertStatus(t, client, srv.URL+"/api/v1/workflows/"+masterID, cookieB, http.StatusNotFound)
	assertStatus(t, client, srv.URL+"/api/v1/workflows/"+child1, cookieB, http.StatusNotFound)
	assertStatus(t, client, srv.URL+"/api/v1/workflows/"+child1+"/questions", cookieB, http.StatusNotFound)

	// --- A can see its own child run and questions list (200, even if empty). ---
	assertStatus(t, client, srv.URL+"/api/v1/workflows/"+child1, cookieA, http.StatusOK)
	assertStatus(t, client, srv.URL+"/api/v1/workflows/"+child1+"/questions", cookieA, http.StatusOK)
}

type workflowDetailTasksView struct {
	Tasks []struct {
		ExecutionRunID string `json:"executionWorkflowId"`
	} `json:"tasks"`
}

func getWorkflowDetail(t *testing.T, client *http.Client, baseURL, id string, cookie *http.Cookie) workflowDetailTasksView {
	t.Helper()
	body := getBody(t, client, baseURL+"/api/v1/workflows/"+id, cookie)
	var out struct {
		Workflow workflowDetailTasksView `json:"workflow"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode workflow detail: %v\nbody=%s", err, body)
	}
	return out.Workflow
}
