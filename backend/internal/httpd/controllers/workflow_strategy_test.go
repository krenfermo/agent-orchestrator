package controllers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// strategyWorkflowService is a fake that implements the planner and strategy
// capabilities the create route type-asserts for, and records exactly which
// creation path the controller chose. The point of these tests is the routing
// decision and the wire contract, not the coordinator's behaviour -- that is
// covered in internal/workflow.
type strategyWorkflowService struct {
	fakeWorkflowService

	taskReq        *workflowcore.TaskRunRequest
	objectiveMode  domain.WorkflowPlanApprovalMode
	objectiveSel   *domain.ExecutionStrategySelection
	legacyPlanCall bool
}

var (
	_ workflowsvc.PlannerManager  = (*strategyWorkflowService)(nil)
	_ workflowsvc.StrategyManager = (*strategyWorkflowService)(nil)
)

func (f *strategyWorkflowService) created(sel domain.ExecutionStrategySelection) workflowcore.RunDetail {
	policy := domain.DefaultWorkflowPolicy()
	policy.Strategy = sel
	snapshot, _ := json.Marshal(policy)
	f.detail = workflowcore.RunDetail{Run: domain.WorkflowRun{
		ID: "wf-1", ProjectID: "proj-1", State: domain.WorkflowRunPending, PolicySnapshot: string(snapshot),
	}}
	return f.detail
}

func (f *strategyWorkflowService) CreateTaskRun(_ context.Context, req workflowcore.TaskRunRequest) (workflowcore.RunDetail, error) {
	f.taskReq = &req
	return f.created(req.Strategy), nil
}

func (f *strategyWorkflowService) CreateObjectiveRunWithStrategy(_ context.Context, _, _ string, mode domain.WorkflowPlanApprovalMode, sel domain.ExecutionStrategySelection) (workflowcore.RunDetail, error) {
	f.objectiveMode, f.objectiveSel = mode, &sel
	return f.created(sel), nil
}

func (f *strategyWorkflowService) EffectiveStrategy(context.Context, string) (domain.ExecutionStrategySelection, error) {
	return domain.ExecutionStrategySelection{}, nil
}

func (f *strategyWorkflowService) CreateObjectiveRun(_ context.Context, _, _ string, mode domain.WorkflowPlanApprovalMode) (workflowcore.RunDetail, error) {
	f.legacyPlanCall, f.objectiveMode = true, mode
	return f.created(domain.ExecutionStrategySelection{}), nil
}

func (f *strategyWorkflowService) GeneratePlan(_ context.Context, runID string) (workflowcore.RunDetail, error) {
	return workflowcore.RunDetail{Run: domain.WorkflowRun{ID: runID}}, nil
}
func (f *strategyWorkflowService) ApprovePlan(_ context.Context, runID string) (workflowcore.RunDetail, error) {
	return workflowcore.RunDetail{Run: domain.WorkflowRun{ID: runID}}, nil
}
func (f *strategyWorkflowService) RejectPlan(_ context.Context, runID string) (workflowcore.RunDetail, error) {
	return workflowcore.RunDetail{Run: domain.WorkflowRun{ID: runID}}, nil
}

func createdStrategyView(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp struct {
		Workflow struct {
			Run struct {
				ExecutionStrategy map[string]any `json:"executionStrategy"`
			} `json:"run"`
		} `json:"workflow"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if resp.Workflow.Run.ExecutionStrategy == nil {
		t.Fatalf("response carries no executionStrategy: %s", body)
	}
	return resp.Workflow.Run.ExecutionStrategy
}

// 19: every strategy round-trips over the API, reaches the right creation
// path, and comes back with its full provenance.
func TestWorkflowCreateRunStrategyRoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		effective string
		source    string
		reason    string
		planned   bool
	}{
		{
			name: "explicit task", body: `{"objective":"rename the flag","strategy":"task"}`,
			effective: "task", source: "explicit", reason: "explicit_request",
		},
		{
			name: "explicit autonomous", body: `{"objective":"build users","strategy":"autonomous"}`,
			effective: "autonomous", source: "explicit", reason: "explicit_request", planned: true,
		},
		{
			name: "explicit master", body: `{"objective":"rebuild billing","strategy":"master"}`,
			effective: "master", source: "explicit", reason: "explicit_request", planned: true,
		},
		{
			name:      "auto selects task for a small bounded change",
			body:      `{"objective":"typo","strategy":"auto","strategySignals":{"size":"small","expectedSteps":1}}`,
			effective: "task", source: "policy", reason: "bounded_work",
		},
		{
			name:      "auto selects autonomous for normal project work",
			body:      `{"objective":"add search","strategy":"auto","strategySignals":{"expectedSteps":6}}`,
			effective: "autonomous", source: "policy", reason: "multi_step_project", planned: true,
		},
		{
			name:      "auto selects master for a multi-workstream initiative",
			body:      `{"objective":"replatform","strategy":"auto","strategySignals":{"multiWorkstream":true}}`,
			effective: "master", source: "policy", reason: "multi_workstream_initiative", planned: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &strategyWorkflowService{}
			srv := newWorkflowTestServer(t, svc)
			body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows", tt.body)
			if status != http.StatusCreated {
				t.Fatalf("status=%d body=%s", status, body)
			}
			view := createdStrategyView(t, body)
			if view["effectiveStrategy"] != tt.effective || view["selectionSource"] != tt.source || view["reasonCode"] != tt.reason {
				t.Fatalf("executionStrategy = %v, want effective=%s source=%s reason=%s", view, tt.effective, tt.source, tt.reason)
			}
			if view["policyVersion"] != domain.ExecutionStrategyPolicyVersion {
				t.Fatalf("policyVersion = %v, want %q", view["policyVersion"], domain.ExecutionStrategyPolicyVersion)
			}
			if tt.planned {
				if svc.objectiveSel == nil || svc.taskReq != nil {
					t.Fatalf("a %s run must be created through the planner path (objective=%v task=%v)", tt.effective, svc.objectiveSel, svc.taskReq)
				}
			} else if svc.taskReq == nil || svc.objectiveSel != nil {
				t.Fatalf("a task run must not be created through the planner path (objective=%v task=%v)", svc.objectiveSel, svc.taskReq)
			}
		})
	}
}

// 20: anything outside the vocabulary is refused before a run exists.
func TestWorkflowCreateRunRejectsInvalidStrategyInputs(t *testing.T) {
	for _, tt := range []struct{ name, body, code string }{
		{"unknown strategy", `{"objective":"x","strategy":"manual"}`, "INVALID_EXECUTION_STRATEGY"},
		{"unknown size", `{"objective":"x","strategy":"auto","strategySignals":{"size":"huge"}}`, "INVALID_EXECUTION_STRATEGY"},
		{"negative counts", `{"objective":"x","strategy":"auto","strategySignals":{"expectedSteps":-1}}`, "INVALID_EXECUTION_STRATEGY"},
		{"unknown write intent", `{"objective":"x","strategy":"task","writeIntent":"maybe"}`, "INVALID_EXECUTION_STRATEGY"},
		{"unknown approval policy", `{"objective":"x","strategy":"task","approvalPolicy":"sometimes"}`, "INVALID_APPROVAL_POLICY"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &strategyWorkflowService{}
			srv := newWorkflowTestServer(t, svc)
			body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows", tt.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", status, body)
			}
			if !strings.Contains(string(body), tt.code) {
				t.Fatalf("body=%s, want error code %s", body, tt.code)
			}
			if svc.taskReq != nil || svc.objectiveSel != nil || svc.legacyPlanCall {
				t.Fatal("an invalid request created a run")
			}
		})
	}
}

// 18: approvalPolicy is the OTHER axis. It sets the plan's approval mode and
// leaves the strategy alone, for every strategy.
func TestWorkflowCreateRunApprovalPolicyIsIndependent(t *testing.T) {
	for _, tt := range []struct {
		approval string
		want     domain.WorkflowPlanApprovalMode
	}{
		{"automatic", domain.WorkflowPlanApprovalAuto},
		{"manual", domain.WorkflowPlanApprovalManual},
	} {
		for _, strategy := range []string{"autonomous", "master"} {
			svc := &strategyWorkflowService{}
			srv := newWorkflowTestServer(t, svc)
			body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows",
				`{"objective":"x","strategy":"`+strategy+`","approvalPolicy":"`+tt.approval+`"}`)
			if status != http.StatusCreated {
				t.Fatalf("status=%d body=%s", status, body)
			}
			if svc.objectiveMode != tt.want {
				t.Fatalf("%s/%s: approval mode = %q, want %q", strategy, tt.approval, svc.objectiveMode, tt.want)
			}
			if view := createdStrategyView(t, body); view["effectiveStrategy"] != strategy {
				t.Fatalf("%s/%s: approval policy changed the strategy to %v", strategy, tt.approval, view["effectiveStrategy"])
			}
		}
	}
}

// A pre-P1-A client sends no strategy at all. It must keep getting exactly
// what its masterPlan flag has always meant -- now written down.
func TestWorkflowCreateRunLegacyClientCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name, body, effective string
		planned               bool
	}{
		{"masterPlan true", `{"objective":"x","masterPlan":true,"planApprovalMode":"auto"}`, "autonomous", true},
		{"masterPlan omitted", `{"objective":"x"}`, "task", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &strategyWorkflowService{}
			srv := newWorkflowTestServer(t, svc)
			body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows", tt.body)
			if status != http.StatusCreated {
				t.Fatalf("status=%d body=%s", status, body)
			}
			view := createdStrategyView(t, body)
			if view["effectiveStrategy"] != tt.effective || view["selectionSource"] != "policy" {
				t.Fatalf("executionStrategy = %v, want a policy-selected %s", view, tt.effective)
			}
			if view["requestedStrategy"] != nil {
				t.Fatalf("a client that requested nothing must not be recorded as having requested %v", view["requestedStrategy"])
			}
			if tt.planned != (svc.objectiveSel != nil) {
				t.Fatalf("planned=%v but objective creation=%v", tt.planned, svc.objectiveSel)
			}
		})
	}
}

// The task strategy carries the two things a task needs and a planner would
// otherwise have supplied.
func TestWorkflowCreateTaskRunForwardsCriteriaAndWriteIntent(t *testing.T) {
	svc := &strategyWorkflowService{}
	srv := newWorkflowTestServer(t, svc)
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows",
		`{"objective":"audit the config","strategy":"task","writeIntent":"read_only","acceptanceCriteria":["The report lists every unvalidated field."]}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if svc.taskReq == nil {
		t.Fatal("no task run was created")
	}
	if svc.taskReq.WriteIntent != domain.WorkflowWriteIntentReadOnly {
		t.Fatalf("write intent = %q, want read_only", svc.taskReq.WriteIntent)
	}
	if len(svc.taskReq.AcceptanceCriteria) != 1 {
		t.Fatalf("acceptance criteria = %v", svc.taskReq.AcceptanceCriteria)
	}
}
