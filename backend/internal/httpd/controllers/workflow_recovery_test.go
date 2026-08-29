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

// workflow_recovery_test.go — P1-B §K at the wire.
//
// Matrix 28 is the one that matters here: /continue must stay compatible AND
// stop being undocumented magic. A caller that presses it has to be told which
// recovery action AO actually took, not handed a 200 that means nothing.

// recoveryWorkflowService implements the recovery capability the routes
// type-assert for, and records what each route asked of it.
type recoveryWorkflowService struct {
	fakeWorkflowService

	assessment workflowcore.RecoveryAssessment
	assessErr  error
	assessRuns []string

	resumeReport workflowcore.ResumeReport
	resumeRuns   []string

	reuseAssessment workflowcore.PlanReuseAssessment
	reuseErr        error
	reuseRuns       []string

	regenAssessment workflowcore.PlanReuseAssessment
	regenErr        error
	regenRuns       []string

	repairPlan   workflowcore.RepairPlan
	repairIntent domain.RepairIntent
	repairErr    error
	repairRuns   []string

	repairPolicy domain.RepairMode
}

var _ workflowsvc.RecoveryManager = (*recoveryWorkflowService)(nil)

func (f *recoveryWorkflowService) AssessRecovery(_ context.Context, runID string) (workflowcore.RecoveryAssessment, error) {
	f.assessRuns = append(f.assessRuns, runID)
	return f.assessment, f.assessErr
}

func (f *recoveryWorkflowService) ResumeRun(_ context.Context, runID string) (workflowcore.RunDetail, workflowcore.ResumeReport, error) {
	f.resumeRuns = append(f.resumeRuns, runID)
	return f.detail, f.resumeReport, nil
}

func (f *recoveryWorkflowService) ReusePlan(_ context.Context, runID string) (workflowcore.RunDetail, workflowcore.PlanReuseAssessment, error) {
	f.reuseRuns = append(f.reuseRuns, runID)
	return f.detail, f.reuseAssessment, f.reuseErr
}

func (f *recoveryWorkflowService) RegeneratePlan(_ context.Context, runID string) (workflowcore.RunDetail, workflowcore.PlanReuseAssessment, error) {
	f.regenRuns = append(f.regenRuns, runID)
	return f.detail, f.regenAssessment, f.regenErr
}

func (f *recoveryWorkflowService) PlanRepair(_ context.Context, _ string) (workflowcore.RepairPlan, error) {
	return f.repairPlan, nil
}

func (f *recoveryWorkflowService) LaunchRepair(_ context.Context, runID, _ string) (domain.RepairIntent, error) {
	f.repairRuns = append(f.repairRuns, runID)
	return f.repairIntent, f.repairErr
}

func (f *recoveryWorkflowService) ApplyRepairPolicy(_ context.Context, _ string, mode domain.RepairMode) error {
	f.repairPolicy = mode
	return nil
}

func newRecoverySvc() *recoveryWorkflowService {
	svc := &recoveryWorkflowService{
		assessment: workflowcore.RecoveryAssessment{
			RunID:             "wf-1",
			RecommendedAction: domain.RecoveryRepair,
			ReasonCode:        "verify_budget_exhausted",
			Explanation:       "Verification kept failing after every automatic fix attempt.",
			PlanReusable:      domain.PlanReuseNotApplicable,
			RepairAvailable:   true,
			RepairEligibility: domain.RepairEligible,
			BlockingCondition: "the deterministic checks still fail",
			Obligation: workflowcore.ResumeObligation{
				Kind: workflowcore.ResumeObligationVerify, Explanation: "Verification is outstanding.",
			},
			Strategy:    domain.ExecutionStrategyTask,
			TargetRunID: "wf-1",
			Version:     workflowcore.RecoveryAssessmentVersion,
		},
		repairPlan: workflowcore.RepairPlan{
			Eligibility: domain.RepairEligible, Mode: domain.RepairModeSuggest, Spent: 0, Budget: 2,
		},
	}
	svc.detail = workflowcore.RunDetail{Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention}}
	return svc
}

func decodeRunRecovery(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp struct {
		Workflow struct {
			Run struct {
				Recovery map[string]any `json:"recovery"`
			} `json:"run"`
			Resume    map[string]any `json:"resume"`
			PlanReuse map[string]any `json:"planReuse"`
		} `json:"workflow"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	out := map[string]any{"recovery": resp.Workflow.Run.Recovery, "resume": resp.Workflow.Resume, "planReuse": resp.Workflow.PlanReuse}
	return out
}

// Matrix 28: /continue keeps its shape AND says what it did.
func TestContinueRunResponseNamesTheRecoveryActionTaken(t *testing.T) {
	svc := newRecoverySvc()
	srv := newWorkflowTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/continue", "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if svc.continueCalls != 1 {
		t.Fatalf("ContinueRun called %d times, want 1: legacy behaviour must be unchanged", svc.continueCalls)
	}
	recovery, _ := decodeRunRecovery(t, body)["recovery"].(map[string]any)
	if recovery == nil {
		t.Fatalf("/continue returned a bare success with no recovery action: %s", body)
	}
	if recovery["recommendedAction"] != "repair" {
		t.Fatalf("recommendedAction = %v, want repair", recovery["recommendedAction"])
	}
	if recovery["reasonCode"] != "verify_budget_exhausted" || recovery["explanation"] == "" {
		t.Fatalf("the response must say what happened and why: %v", recovery)
	}
	if recovery["obligation"] != "verify" {
		t.Fatalf("obligation = %v, want verify", recovery["obligation"])
	}
	if recovery["automaticAllowed"] != false {
		t.Fatalf("automaticAllowed = %v, want false for a suggest-policy repair", recovery["automaticAllowed"])
	}
}

// A deployment without the recovery capability answers exactly as it did
// before P1-B: a 200 with the run, and no invented recovery block.
func TestContinueRunStaysCompatibleWithoutTheRecoveryCapability(t *testing.T) {
	svc := &fakeWorkflowService{}
	srv := newWorkflowTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/continue", "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if strings.Contains(string(body), `"recovery"`) {
		t.Fatalf("a pre-P1-B deployment invented a recovery block: %s", body)
	}
}

// GET /recovery is the read-only assessment, and it never mutates anything.
func TestGetRecoveryReturnsTheAssessmentAndTheRepairPlan(t *testing.T) {
	svc := newRecoverySvc()
	srv := newWorkflowTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "GET", "/api/v1/workflows/wf-1/recovery", "")
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp struct {
		Recovery map[string]any `json:"recovery"`
		Repair   map[string]any `json:"repair"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Recovery["recommendedAction"] != "repair" || resp.Recovery["repairAvailable"] != true {
		t.Fatalf("recovery = %v", resp.Recovery)
	}
	if resp.Repair["mode"] != "suggest" || resp.Repair["budget"] != float64(2) {
		t.Fatalf("repair = %v", resp.Repair)
	}
	if svc.continueCalls != 0 || len(svc.resumeRuns) != 0 {
		t.Fatal("reading the assessment drove the run")
	}
}

// Each recovery operation is its own route with its own verb, and each reports
// what it did rather than a bare success.
func TestRecoveryRoutesAreNamedOperations(t *testing.T) {
	t.Run("resume reports the obligation it discharged", func(t *testing.T) {
		svc := newRecoverySvc()
		svc.resumeReport = workflowcore.ResumeReport{
			Obligation: workflowcore.ResumeObligation{
				Kind: workflowcore.ResumeObligationWorkObservation, Explanation: "A worker is running.",
			},
			Performed:  true,
			Assessment: svc.assessment,
		}
		srv := newWorkflowTestServer(t, svc)
		body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/resume", "")
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, body)
		}
		if len(svc.resumeRuns) != 1 || svc.resumeRuns[0] != "wf-1" {
			t.Fatalf("resume routed to %v", svc.resumeRuns)
		}
		decoded := decodeRunRecovery(t, body)
		resume, _ := decoded["resume"].(map[string]any)
		if resume == nil || resume["obligation"] != "work_observation" || resume["performed"] != true {
			t.Fatalf("resume report = %v", resume)
		}
	})

	t.Run("reuse reports the plan revision it executed", func(t *testing.T) {
		svc := newRecoverySvc()
		svc.reuseAssessment = workflowcore.PlanReuseAssessment{
			Reusability: domain.PlanReuseExact, Revision: 1, TaskCount: 2, PlanHash: "abc",
		}
		srv := newWorkflowTestServer(t, svc)
		body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/plan/reuse", "")
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, body)
		}
		plan, _ := decodeRunRecovery(t, body)["planReuse"].(map[string]any)
		if plan == nil || plan["reusability"] != "exact" || plan["revision"] != float64(1) {
			t.Fatalf("planReuse = %v", plan)
		}
	})

	t.Run("regenerate reports the new revision", func(t *testing.T) {
		svc := newRecoverySvc()
		svc.regenAssessment = workflowcore.PlanReuseAssessment{Reusability: domain.PlanReuseNotReusable, Revision: 2}
		srv := newWorkflowTestServer(t, svc)
		body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/plan/regenerate", "")
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, body)
		}
		plan, _ := decodeRunRecovery(t, body)["planReuse"].(map[string]any)
		if plan == nil || plan["revision"] != float64(2) {
			t.Fatalf("planReuse = %v", plan)
		}
		if len(svc.regenRuns) != 1 {
			t.Fatalf("regenerate routed to %v", svc.regenRuns)
		}
	})

	t.Run("repair returns the intent it created", func(t *testing.T) {
		svc := newRecoverySvc()
		svc.repairIntent = domain.RepairIntent{
			ID: "wfr-1", TargetRunID: "wf-1", ConditionReason: "verify_budget_exhausted",
			EvidenceDigest: "deadbeef", Generation: 1, RepairRunID: "wf-repair",
			Strategy: domain.ExecutionStrategyTask, Mode: domain.RepairModeSuggest,
		}
		srv := newWorkflowTestServer(t, svc)
		body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/repair", "")
		if status != http.StatusAccepted {
			t.Fatalf("status=%d body=%s", status, body)
		}
		var resp struct {
			Intent map[string]any `json:"intent"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Intent["repairRunId"] != "wf-repair" || resp.Intent["strategy"] != "task" {
			t.Fatalf("intent = %v", resp.Intent)
		}
	})
}

// A backend refusal is a refusal at the wire, never a success the UI could
// mistake for one.
func TestRepairRefusalIsNotSuccess(t *testing.T) {
	svc := newRecoverySvc()
	svc.repairPlan = workflowcore.RepairPlan{
		Eligibility: domain.RepairIneligible, Mode: domain.RepairModeSuggest, Budget: 2,
		Reason: `"verify_approved_head_unprovable" is not a repairable condition`,
	}
	svc.repairErr = workflowcore.ErrRepairIneligible
	srv := newWorkflowTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/repair", "")
	if status != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", status, body)
	}
	if !strings.Contains(string(body), "REPAIR_NOT_AVAILABLE") {
		t.Fatalf("body=%s, want the refusal's own error code", body)
	}
}

// The repair policy chosen at creation reaches the freeze, and an unknown one
// is refused before a run exists.
func TestCreateRunFreezesTheRepairPolicy(t *testing.T) {
	svc := newRecoverySvc()
	srv := newWorkflowTestServer(t, svc)
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects/proj-1/workflows",
		`{"objective":"x","strategy":"task","repairPolicy":"automatic"}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if svc.repairPolicy != domain.RepairModeAutomatic {
		t.Fatalf("frozen repair policy = %q, want automatic", svc.repairPolicy)
	}

	svc2 := newRecoverySvc()
	srv2 := newWorkflowTestServer(t, svc2)
	body2, status2, _ := doRequest(t, srv2, "POST", "/api/v1/projects/proj-1/workflows",
		`{"objective":"x","strategy":"task","repairPolicy":"whenever"}`)
	if status2 != http.StatusBadRequest || !strings.Contains(string(body2), "INVALID_REPAIR_POLICY") {
		t.Fatalf("status=%d body=%s, want 400 INVALID_REPAIR_POLICY", status2, body2)
	}
	if svc2.repairPolicy != "" {
		t.Fatal("an invalid repair policy was frozen onto a run")
	}
}
