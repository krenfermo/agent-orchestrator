package controllers_test

// Cancel-and-archive, proven at the boundary the Board button actually calls.
//
// These reuse newCancelStack (workflow_cancel_branch_lock_test.go): the real
// SQLite store, the real branchlock.Manager, the real wake.Scheduler and the
// real coordinator, driven over the real HTTP router. The whole point of the
// action is what it does to durable rows — a fake store could not tell a
// released lock from a deleted one.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

type boardResponseBody struct {
	Workflows []struct {
		WorkflowID string  `json:"workflowId"`
		State      string  `json:"state"`
		ArchivedAt *string `json:"archivedAt"`
	} `json:"workflows"`
}

func decodeBoard(t *testing.T, raw []byte) []string {
	t.Helper()
	var body boardResponseBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode board: %v (body=%s)", err, raw)
	}
	ids := make([]string, 0, len(body.Workflows))
	for _, w := range body.Workflows {
		ids = append(ids, w.WorkflowID)
	}
	return ids
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// The end-to-end guarantee the Board depends on: after one POST the stale
// workflow is gone from the active board, present in history, stopped, and its
// branch is free — with every durable row still on disk.
func TestCancelAndArchiveOverHTTPRetiresAStaleWorkflow(t *testing.T) {
	fx := newCancelStack(t)
	ctx := context.Background()
	srv := newWorkflowTestServer(t, workflowsvc.New(fx.coord))
	holder := fx.stoppedHolder(t, "agent-orchestrator")
	lock, ok := fx.heldLock(t, holder)
	if !ok {
		t.Fatal("fixture did not reach the state under test: holder owns no lock")
	}

	raw, status, _ := doRequest(t, srv, "GET", "/api/v1/projects/agent-orchestrator/board", "")
	if status != http.StatusOK || !hasID(decodeBoard(t, raw), holder) {
		t.Fatalf("precondition: stale workflow not on the active board (status=%d body=%s)", status, raw)
	}

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/"+holder+"/cancel-archive", "")
	if status != http.StatusOK {
		t.Fatalf("cancel-archive status=%d body=%s", status, body)
	}

	run, found, err := fx.store.GetWorkflowRun(ctx, holder)
	if err != nil || !found {
		t.Fatalf("the run row was deleted: found=%v err=%v", found, err)
	}
	if run.State != domain.WorkflowRunCancelled {
		t.Fatalf("run state = %q, want cancelled", run.State)
	}
	if !run.Archived() {
		t.Fatal("run is not marked archived")
	}
	if fx.lockRow(t, lock.ID).State != domain.BranchLockReleased {
		t.Fatal("the branch is still locked to the archived workflow")
	}

	raw, status, _ = doRequest(t, srv, "GET", "/api/v1/projects/agent-orchestrator/board", "")
	if status != http.StatusOK {
		t.Fatalf("board status=%d body=%s", status, raw)
	}
	if hasID(decodeBoard(t, raw), holder) {
		t.Fatalf("archived workflow still on the active board: %s", raw)
	}

	raw, status, _ = doRequest(t, srv, "GET", "/api/v1/projects/agent-orchestrator/board/history", "")
	if status != http.StatusOK {
		t.Fatalf("history status=%d body=%s", status, raw)
	}
	if !hasID(decodeBoard(t, raw), holder) {
		t.Fatalf("archived workflow missing from history: %s", raw)
	}

	// Steps and checkpoints are untouched — this is an archive, not a delete.
	steps, err := fx.store.ListWorkflowSteps(ctx, holder)
	if err != nil || len(steps) == 0 {
		t.Fatalf("workflow steps were removed: %d steps, err=%v", len(steps), err)
	}
	cps, err := fx.store.ListWorkflowCheckpoints(ctx, holder)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	audit := false
	for _, cp := range cps {
		if cp.DurablePhase == "run_cancelled_and_archived" {
			audit = true
		}
	}
	if !audit {
		t.Fatal("no durable cancellation audit checkpoint survives on the archived run")
	}
}

// Retrying the request must be safe. /cancel answers 409 on an already-terminal
// run; the archive action deliberately does not — a user pressing the button
// twice, or a client retrying a request whose response was lost, must not see a
// failure for work that is already done.
func TestCancelAndArchiveOverHTTPIsIdempotent(t *testing.T) {
	fx := newCancelStack(t)
	srv := newWorkflowTestServer(t, workflowsvc.New(fx.coord))
	holder := fx.stoppedHolder(t, "agent-orchestrator")

	var first, second string
	for i, target := range []*string{&first, &second} {
		body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/"+holder+"/cancel-archive", "")
		if status != http.StatusOK {
			t.Fatalf("call %d status=%d body=%s", i+1, status, body)
		}
		var parsed struct {
			Workflow struct {
				Run struct {
					ArchivedAt string `json:"archivedAt"`
				} `json:"run"`
			} `json:"workflow"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("decode call %d: %v", i+1, err)
		}
		*target = parsed.Workflow.Run.ArchivedAt
	}
	if first == "" {
		t.Fatal("response carries no archivedAt")
	}
	if first != second {
		t.Fatalf("archive timestamp moved between retries: %q -> %q", first, second)
	}
}

// A pending wake must not survive: the poller would otherwise pick the run back
// up and hand it to the dispatcher after the user retired it.
func TestCancelAndArchiveOverHTTPCancelsPendingWakes(t *testing.T) {
	fx := newCancelStack(t)
	ctx := context.Background()
	srv := newWorkflowTestServer(t, workflowsvc.New(fx.coord))
	holder := fx.stoppedHolder(t, "agent-orchestrator")
	if _, err := fx.wake.Schedule(ctx, domain.WorkflowRunID(holder), nil, wake.ReasonBranchLock, nil); err != nil {
		t.Fatalf("seed wake: %v", err)
	}
	if next, err := fx.wake.NextForRun(ctx, domain.WorkflowRunID(holder)); err != nil || next == nil {
		t.Fatalf("precondition: no open wake to cancel (next=%v err=%v)", next, err)
	}

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/"+holder+"/cancel-archive", "")
	if status != http.StatusOK {
		t.Fatalf("cancel-archive status=%d body=%s", status, body)
	}

	next, err := fx.wake.NextForRun(ctx, domain.WorkflowRunID(holder))
	if err != nil {
		t.Fatalf("NextForRun: %v", err)
	}
	if next != nil {
		t.Fatalf("a wake survived archiving and would resurrect the run: %+v", next)
	}
}

// The safety half: archiving one workflow leaves every other one alone, and a
// project's own board is unaffected by another project's cleanup.
func TestCancelAndArchiveOverHTTPLeavesOtherWorkflowsOnTheBoard(t *testing.T) {
	fx := newCancelStack(t)
	ctx := context.Background()
	srv := newWorkflowTestServer(t, workflowsvc.New(fx.coord))
	stale := fx.stoppedHolder(t, "agent-orchestrator")
	live, err := fx.coord.CreateRun(ctx, "other", "a workflow that is still going")
	if err != nil {
		t.Fatalf("CreateRun live: %v", err)
	}

	if body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/"+stale+"/cancel-archive", ""); status != http.StatusOK {
		t.Fatalf("cancel-archive status=%d body=%s", status, body)
	}

	raw, status, _ := doRequest(t, srv, "GET", "/api/v1/projects/other/board", "")
	if status != http.StatusOK {
		t.Fatalf("board status=%d body=%s", status, raw)
	}
	if !hasID(decodeBoard(t, raw), live.Run.ID) {
		t.Fatalf("the untouched workflow disappeared from its board: %s", raw)
	}
	after, _, err := fx.store.GetWorkflowRun(ctx, live.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if after.Archived() || after.State != domain.WorkflowRunPending {
		t.Fatalf("untouched run was modified: state=%s archived=%v", after.State, after.Archived())
	}
}
