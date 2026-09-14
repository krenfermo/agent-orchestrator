package controllers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ownershipRecoverySvc is the recovery fake with P9's readback capability.
type ownershipRecoverySvc struct {
	*recoveryWorkflowService
	rows  []workflowcore.WorkerOwnershipReadback
	calls int
}

func (s *ownershipRecoverySvc) WorkerOwnershipFor(_ context.Context, _ string) ([]workflowcore.WorkerOwnershipReadback, error) {
	s.calls++
	return s.rows, nil
}

// P9 §25: GET /recovery carries the ownership readback -- proof, identities and
// the recovery decision with its reason code -- and reading it drives nothing.
func TestGetRecoveryIncludesTheWorkerOwnershipReadback(t *testing.T) {
	signal := time.Date(2026, 9, 13, 21, 0, 0, 0, time.UTC)
	svc := &ownershipRecoverySvc{recoveryWorkflowService: newRecoverySvc(), rows: []workflowcore.WorkerOwnershipReadback{{
		StepID: "wfs-work", StepKind: domain.WorkflowStepWork, StepState: domain.WorkflowStepWaiting,
		SessionID: "proj-1-1", Ownership: domain.WorkerOwnershipUnproven, Proof: domain.WorkerRuntimeOwnerMismatch,
		RuntimeInstanceID: "$4", ObservedInstanceID: "$4",
		LaunchState: domain.WorkflowOutboxDispatched, DispatchGeneration: "wfd-1", DispatchPhase: workflowcore.WorkerDispatchIntended,
		AttemptID: "wfa-1", LastSignalAt: signal,
		Decision: workflowcore.WorkerRecoveryFailClosed, Reason: workflowcore.WorkerReasonOwnerMismatch,
		Detail: "runtime incarnation $4 carries an ownership token that is not this launch's",
	}}}
	srv := newWorkflowTestServer(t, svc)
	body, status, _ := doRequest(t, srv, "GET", "/api/v1/workflows/wf-1/recovery", "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp struct {
		WorkerOwnership []map[string]any `json:"workerOwnership"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.WorkerOwnership) != 1 {
		t.Fatalf("workerOwnership = %v", resp.WorkerOwnership)
	}
	row := resp.WorkerOwnership[0]
	for key, want := range map[string]any{
		"ownership": "unproven", "proof": "owner_mismatch", "recoveryDecision": "fail_closed",
		"recoveryReason": "owner_mismatch", "dispatchGeneration": "wfd-1", "attemptId": "wfa-1",
		"sessionId": "proj-1-1", "runtimeInstanceId": "$4", "lastSignalAt": "2026-09-13T21:00:00Z",
	} {
		if row[key] != want {
			t.Fatalf("%s = %v, want %v (row %v)", key, row[key], want, row)
		}
	}
	if svc.continueCalls != 0 || len(svc.resumeRuns) != 0 {
		t.Fatal("reading the ownership readback drove the run")
	}
	for _, forbidden := range []string{"prompt", "env", "command", "token"} {
		if strings.Contains(strings.ToLower(string(body)), `"`+forbidden) {
			t.Fatalf("the readback exposes a %q field: %s", forbidden, body)
		}
	}
}
