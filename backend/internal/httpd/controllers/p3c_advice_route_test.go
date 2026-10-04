package controllers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p3c_advice_route_test.go — P3-C §15/§24 at the wire.
//
// Two properties, and they are the ones a client depends on. The advice route
// composes the answer server-side so no frontend has to; and a mutating action
// computed against an earlier reading is REFUSED with a reason, never
// duplicated silently.

// advisorWorkflowService adds the advisor capability the P3-C routes
// type-assert for, and records what each route asked of it.
type advisorWorkflowService struct {
	recoveryWorkflowService

	advice      workflowcore.Advice
	adviceErr   error
	adviceRuns  []string
	mismatch    workflowcore.ActionAuthorityMismatch
	revalidated []workflowcore.ActionID
	expected    []workflowcore.AdviceAuthority
}

var _ workflowsvc.AdvisorManager = (*advisorWorkflowService)(nil)

func (f *advisorWorkflowService) AdviceFor(_ context.Context, runID string) (workflowcore.Advice, error) {
	f.adviceRuns = append(f.adviceRuns, runID)
	return f.advice, f.adviceErr
}

func (f *advisorWorkflowService) AdviceForDetail(_ context.Context, _ workflowcore.RunDetail) (workflowcore.Advice, error) {
	return f.advice, f.adviceErr
}

func (f *advisorWorkflowService) DispatchAutomaticRecovery(_ context.Context, runID string) (workflowcore.AutomaticRecoveryOutcome, error) {
	return workflowcore.AutomaticRecoveryOutcome{RunID: runID}, nil
}

func (f *advisorWorkflowService) RevalidateActionAuthority(
	_ context.Context, _ string, action workflowcore.ActionID, expected workflowcore.AdviceAuthority,
) (workflowcore.ActionAuthorityMismatch, error) {
	f.revalidated = append(f.revalidated, action)
	f.expected = append(f.expected, expected)
	return f.mismatch, nil
}

func (f *advisorWorkflowService) ApplyAutonomyPolicy(_ context.Context, _ string, _ domain.QuestionAutonomyMode) error {
	return nil
}

func newAdvisorSvc() *advisorWorkflowService {
	svc := &advisorWorkflowService{recoveryWorkflowService: *newRecoverySvc()}
	svc.advice = workflowcore.Advice{
		RunID:             "wf-1",
		TargetRunID:       "wf-1",
		Category:          workflowcore.AdviceHumanAction,
		Stage:             workflowcore.StageNeedsAttention,
		ReasonCode:        "verify_budget_exhausted",
		SummaryCode:       "verify_budget_exhausted",
		Summary:           "AO has used every automatic attempt it is allowed. The next step is yours.",
		Explanation:       "Verification kept failing after every automatic fix attempt.",
		RequiresHuman:     true,
		RecommendedAction: workflowcore.ActionRepair,
		AvailableActions:  []workflowcore.ActionID{workflowcore.ActionRepair, workflowcore.ActionCancel},
		BlockedActions: []workflowcore.BlockedAction{
			{ID: workflowcore.ActionContinue, Reason: "not_recoverable"},
		},
		ExpectedNextStage: workflowcore.StageCorrecting,
		Repairable:        true,
		RepairEligibility: domain.RepairEligible,
		RepairBudget:      2,
		Authority: workflowcore.AdviceAuthority{
			RepairGeneration: 1, StopPhase: "verify_budget_exhausted",
			RunState: domain.WorkflowRunNeedsAttention,
		},
		Version: workflowcore.AdviceVersion,
	}
	return svc
}

// §24: the composition happens on the server, and every field a client needs to
// avoid re-deriving one is on the wire.
func TestAdviceRouteServesTheWholeAnswer(t *testing.T) {
	svc := newAdvisorSvc()
	srv := newWorkflowTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "GET", "/api/v1/workflows/wf-1/advice", "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp struct {
		Advice map[string]any `json:"advice"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	for _, key := range []string{
		"category", "summaryCode", "requiresHuman", "recommendedAction",
		"availableActions", "blockedActions", "expectedNextStage", "authority", "version",
	} {
		if _, ok := resp.Advice[key]; !ok {
			t.Errorf("advice is missing %q, so a client would have to derive it: %s", key, body)
		}
	}
	if resp.Advice["category"] != "human_action" {
		t.Fatalf("category = %v, want human_action", resp.Advice["category"])
	}
	blocked, _ := resp.Advice["blockedActions"].([]any)
	if len(blocked) != 1 {
		t.Fatalf("blocked actions were hidden rather than reported: %s", body)
	}
}

// §15: a Repair click computed against a state the run has moved past is
// refused 409 with the reason, and the repair is never launched.
func TestStaleRepairClickIsRefusedAtTheWire(t *testing.T) {
	svc := newAdvisorSvc()
	svc.mismatch = workflowcore.AuthorityMismatchRepairActive
	srv := newWorkflowTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/repair",
		`{"repairGeneration":1,"stopPhase":"verify_budget_exhausted"}`)
	if status != http.StatusConflict {
		t.Fatalf("status=%d, want 409: %s", status, body)
	}
	var apiErr struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &apiErr); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if apiErr.Code != "ACTION_SUPERSEDED" {
		t.Fatalf("error code = %q, want ACTION_SUPERSEDED: %s", apiErr.Code, body)
	}
	if apiErr.Message == "" {
		t.Fatal("a refusal with no sentence a person can read")
	}
	if len(svc.repairRuns) != 0 {
		t.Fatalf("a superseded click still launched a repair: %v", svc.repairRuns)
	}
	// And the proof the caller sent was actually compared, not ignored.
	if len(svc.expected) != 1 || svc.expected[0].RepairGeneration != 1 ||
		svc.expected[0].StopPhase != "verify_budget_exhausted" {
		t.Fatalf("the authority proof was not passed through: %+v", svc.expected)
	}
}

// §15: a Continue that arrives while AO is repairing is refused the same way,
// rather than re-entering a resume path the repair owns.
func TestStaleContinueClickIsRefusedAtTheWire(t *testing.T) {
	svc := newAdvisorSvc()
	svc.mismatch = workflowcore.AuthorityMismatchRepairActive
	srv := newWorkflowTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/continue", `{}`)
	if status != http.StatusConflict {
		t.Fatalf("status=%d, want 409: %s", status, body)
	}
	if svc.continueCalls != 0 {
		t.Fatalf("a superseded Continue still re-entered the resume path (%d calls)", svc.continueCalls)
	}
}

// A caller that sends no proof gets exactly the pre-P3-C behaviour: the check
// runs, finds nothing to refuse, and the action proceeds.
func TestActionsWithoutAnAuthorityProofStillProceed(t *testing.T) {
	svc := newAdvisorSvc()
	svc.mismatch = workflowcore.AuthorityMismatchNone
	srv := newWorkflowTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/continue", "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if svc.continueCalls != 1 {
		t.Fatalf("ContinueRun called %d times, want 1", svc.continueCalls)
	}
}
