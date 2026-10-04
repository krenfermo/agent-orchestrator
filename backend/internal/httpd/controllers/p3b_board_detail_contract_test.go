package controllers_test

// p3b_board_detail_contract_test.go — P3-B §29, the test that stops the Board
// and the run detail page from diverging again.
//
// It runs over the real stack the desktop app talks to: the real SQLite store,
// the real coordinator, the real router. For every run on the board it fetches
// that run's own detail page and asserts the two projections are the SAME
// values — not merely compatible ones. The whole class of bug P3-B closes is
// "the card said reviewing and the page said needs_attention", and the only
// durable defence against it is an assertion that compares the two answers.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// The subset of the presentation both surfaces send. Decoded structurally, so a
// field added to the projection is carried by both or by neither.
type p3bPresentation struct {
	Stage                 string          `json:"stage"`
	RequiresHuman         bool            `json:"requiresHuman"`
	AutomaticActionActive bool            `json:"automaticActionActive"`
	SummaryCode           string          `json:"summaryCode"`
	RecommendedAction     string          `json:"recommendedAction"`
	Actions               json.RawMessage `json:"actions"`
	Progress              json.RawMessage `json:"progress"`
	Placement             json.RawMessage `json:"placement"`
	Technical             struct {
		Phase           string `json:"phase"`
		RunState        string `json:"runState"`
		Attention       string `json:"attention"`
		AttentionReason string `json:"attentionReason"`
		WaitReason      string `json:"waitReason"`
		ErrorClass      string `json:"errorClass"`
		RepairRunID     string `json:"repairRunId"`
	} `json:"technical"`
}

type p3bBoardBody struct {
	Workflows []struct {
		WorkflowID            string           `json:"workflowId"`
		Stage                 string           `json:"stage"`
		RequiresHuman         bool             `json:"requiresHuman"`
		AutomaticActionActive bool             `json:"automaticActionActive"`
		RepairOfWorkflowID    string           `json:"repairOfWorkflowId"`
		Presentation          *p3bPresentation `json:"presentation"`
		Repairs               []struct {
			WorkflowID string `json:"workflowId"`
			Attempt    int    `json:"attempt"`
			Active     bool   `json:"active"`
		} `json:"repairs"`
	} `json:"workflows"`
	Counts struct {
		Active         int `json:"active"`
		Working        int `json:"working"`
		Waiting        int `json:"waiting"`
		NeedsAttention int `json:"needsAttention"`
		Completed      int `json:"completed"`
		Archived       int `json:"archived"`
	} `json:"counts"`
	Matched int `json:"matched"`
}

type p3bDetailBody struct {
	Workflow struct {
		Run struct {
			Stage                 string `json:"stage"`
			RequiresHuman         bool   `json:"requiresHuman"`
			AutomaticActionActive bool   `json:"automaticActionActive"`
			SummaryCode           string `json:"summaryCode"`
			RecommendedAction     string `json:"recommendedAction"`
			RepairOfWorkflowID    string `json:"repairOfWorkflowId"`
		} `json:"run"`
		Presentation *p3bPresentation `json:"presentation"`
	} `json:"workflow"`
}

func p3bBoardOf(t *testing.T, srv *httptest.Server, projectID string) p3bBoardBody {
	t.Helper()
	raw, status, _ := doRequest(t, srv, "GET", "/api/v1/projects/"+projectID+"/board", "")
	if status != http.StatusOK {
		t.Fatalf("board status=%d body=%s", status, raw)
	}
	var body p3bBoardBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode board: %v (body=%s)", err, raw)
	}
	return body
}

// p3bRun creates a run and moves it into the state named, refusing to continue
// if the durable transition did not take.
func p3bRun(t *testing.T, fx *cancelFixture, projectID, objective string, state domain.WorkflowRunState) string {
	t.Helper()
	ctx := context.Background()
	created, err := fx.coord.CreateRun(ctx, projectID, objective)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	from := domain.WorkflowRunPending
	for _, to := range p3bPathTo(state) {
		ok, err := fx.store.UpdateWorkflowRunState(ctx, created.Run.ID, from, to, fx.now)
		if err != nil || !ok {
			t.Fatalf("fixture could not move %s from %s to %s (ok=%v err=%v)", created.Run.ID, from, to, ok, err)
		}
		from = to
	}
	return created.Run.ID
}

func p3bPathTo(state domain.WorkflowRunState) []domain.WorkflowRunState {
	switch state {
	case domain.WorkflowRunPending:
		return nil
	case domain.WorkflowRunCompleted, domain.WorkflowRunFailed:
		return []domain.WorkflowRunState{domain.WorkflowRunRunning, state}
	default:
		return []domain.WorkflowRunState{state}
	}
}

// restartedCoordinator is a second coordinator over the same database: what a
// daemon restart leaves behind, with no carried state of any kind.
func (f *cancelFixture) restartedCoordinator(t *testing.T) *workflowcore.Coordinator {
	t.Helper()
	clock := func() time.Time { return f.now }
	mgr := branchlock.New(branchlock.Deps{Store: f.store, OwnerToken: "owner-restart", NewID: newIDSeq("bl2"), Clock: clock})
	coord := workflowcore.New(workflowcore.Deps{
		Store: f.store, Projects: f.store, SessionFacts: f.store, ReviewRuns: f.store,
		BranchLocks: realBranchLocks{mgr: mgr},
		Clock:       clock, NewID: newIDSeq("id2"),
	})
	mgr.SetClassifier(realLockClassifier{coord: coord})
	return coord
}

// The contract itself. For every card on the board, the run's own page must
// report the same stage, the same summary, the same recommendation and the same
// technical account.
func TestBoardAndRunDetailProjectTheSameRun(t *testing.T) {
	fx := newCancelStack(t)
	srv := newWorkflowTestServer(t, workflowsvc.New(fx.coord))

	// Three real shapes: a run stopped on a person, a run in flight, and a run
	// that finished.
	fx.stoppedHolder(t, "agent-orchestrator")
	p3bRun(t, fx, "other", "work in flight", domain.WorkflowRunRunning)
	p3bRun(t, fx, "other", "finished work", domain.WorkflowRunCompleted)

	for _, projectID := range []string{"agent-orchestrator", "other"} {
		raw, status, _ := doRequest(t, srv, "GET", "/api/v1/projects/"+projectID+"/board", "")
		if status != http.StatusOK {
			t.Fatalf("board status=%d body=%s", status, raw)
		}
		var board p3bBoardBody
		if err := json.Unmarshal(raw, &board); err != nil {
			t.Fatalf("decode board: %v (body=%s)", err, raw)
		}
		if len(board.Workflows) == 0 {
			t.Fatalf("project %q returned an empty board; the fixture did not reach the state under test", projectID)
		}
		for _, card := range board.Workflows {
			if card.Presentation == nil {
				t.Fatalf("card %q carries no presentation: the Board is not projecting the shared model", card.WorkflowID)
			}
			detailRaw, detailStatus, _ := doRequest(t, srv, "GET", "/api/v1/workflows/"+card.WorkflowID, "")
			if detailStatus != http.StatusOK {
				t.Fatalf("detail status=%d body=%s", detailStatus, detailRaw)
			}
			var detail p3bDetailBody
			if err := json.Unmarshal(detailRaw, &detail); err != nil {
				t.Fatalf("decode detail: %v (body=%s)", err, detailRaw)
			}
			if detail.Workflow.Presentation == nil {
				t.Fatalf("run %q has no presentation on its detail page", card.WorkflowID)
			}
			assertSamePresentation(t, card.WorkflowID, *card.Presentation, *detail.Workflow.Presentation)

			// The flat mirrors on both surfaces are copies of the same value
			// and must agree with it too — they are what the compact card and
			// the page header actually render.
			if card.Stage != card.Presentation.Stage ||
				detail.Workflow.Run.Stage != card.Presentation.Stage {
				t.Fatalf("run %q: flat stage disagrees with the projection (card=%q detail=%q projection=%q)",
					card.WorkflowID, card.Stage, detail.Workflow.Run.Stage, card.Presentation.Stage)
			}
			if card.RequiresHuman != detail.Workflow.Run.RequiresHuman {
				t.Fatalf("run %q: board says requiresHuman=%v, its page says %v",
					card.WorkflowID, card.RequiresHuman, detail.Workflow.Run.RequiresHuman)
			}
			if card.Presentation.RecommendedAction != detail.Workflow.Run.RecommendedAction {
				t.Fatalf("run %q: board recommends %q, its page recommends %q",
					card.WorkflowID, card.Presentation.RecommendedAction, detail.Workflow.Run.RecommendedAction)
			}
		}
	}
}

// assertSamePresentation compares the two projections field by field. The raw
// JSON comparisons are deliberate: they catch a field the two surfaces render
// differently without this test having to enumerate every one.
func assertSamePresentation(t *testing.T, runID string, board, detail p3bPresentation) {
	t.Helper()
	if board.Stage != detail.Stage {
		t.Fatalf("run %q: board stage %q, detail stage %q", runID, board.Stage, detail.Stage)
	}
	if board.SummaryCode != detail.SummaryCode {
		t.Fatalf("run %q: board summary %q, detail summary %q", runID, board.SummaryCode, detail.SummaryCode)
	}
	if board.RequiresHuman != detail.RequiresHuman {
		t.Fatalf("run %q: board requiresHuman %v, detail %v", runID, board.RequiresHuman, detail.RequiresHuman)
	}
	if board.AutomaticActionActive != detail.AutomaticActionActive {
		t.Fatalf("run %q: board automaticActionActive %v, detail %v", runID, board.AutomaticActionActive, detail.AutomaticActionActive)
	}
	if board.RecommendedAction != detail.RecommendedAction {
		t.Fatalf("run %q: board recommends %q, detail recommends %q", runID, board.RecommendedAction, detail.RecommendedAction)
	}
	if !jsonEqual(board.Actions, detail.Actions) {
		t.Fatalf("run %q: the two surfaces offer different actions\n board:  %s\n detail: %s", runID, board.Actions, detail.Actions)
	}
	if !jsonEqual(board.Progress, detail.Progress) {
		t.Fatalf("run %q: the two surfaces show different progressions\n board:  %s\n detail: %s", runID, board.Progress, detail.Progress)
	}
	if !jsonEqual(board.Placement, detail.Placement) {
		t.Fatalf("run %q: the two surfaces show different placements\n board:  %s\n detail: %s", runID, board.Placement, detail.Placement)
	}
	if board.Technical != detail.Technical {
		t.Fatalf("run %q: the two surfaces show different technical accounts\n board:  %+v\n detail: %+v", runID, board.Technical, detail.Technical)
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(nonNullJSON(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal(nonNullJSON(b), &bv); err != nil {
		return false
	}
	return fmt.Sprintf("%v", av) == fmt.Sprintf("%v", bv)
}

func nonNullJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// §3: the list endpoint used to read run ROWS, which cannot tell `running` from
// `reviewing`, so the same workflow read differently in three places. It now
// projects the same model.
func TestWorkflowListAgreesWithTheBoardAndTheDetailPage(t *testing.T) {
	fx := newCancelStack(t)
	srv := newWorkflowTestServer(t, workflowsvc.New(fx.coord))
	stopped := fx.stoppedHolder(t, "agent-orchestrator")

	raw, status, _ := doRequest(t, srv, "GET", "/api/v1/workflows?projectId=agent-orchestrator", "")
	if status != http.StatusOK {
		t.Fatalf("list status=%d body=%s", status, raw)
	}
	var list struct {
		Workflows []struct {
			ID                string `json:"id"`
			Stage             string `json:"stage"`
			SummaryCode       string `json:"summaryCode"`
			RequiresHuman     bool   `json:"requiresHuman"`
			RecommendedAction string `json:"recommendedAction"`
		} `json:"workflows"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode list: %v (body=%s)", err, raw)
	}
	var row *struct {
		ID                string `json:"id"`
		Stage             string `json:"stage"`
		SummaryCode       string `json:"summaryCode"`
		RequiresHuman     bool   `json:"requiresHuman"`
		RecommendedAction string `json:"recommendedAction"`
	}
	for i := range list.Workflows {
		if list.Workflows[i].ID == stopped {
			row = &list.Workflows[i]
		}
	}
	if row == nil {
		t.Fatalf("the stopped run is missing from the list (body=%s)", raw)
	}
	if row.Stage == "" {
		t.Fatal("the list still returns a run row with no derived stage")
	}

	detailRaw, _, _ := doRequest(t, srv, "GET", "/api/v1/workflows/"+stopped, "")
	var detail p3bDetailBody
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if row.Stage != detail.Workflow.Run.Stage ||
		row.SummaryCode != detail.Workflow.Run.SummaryCode ||
		row.RequiresHuman != detail.Workflow.Run.RequiresHuman ||
		row.RecommendedAction != detail.Workflow.Run.RecommendedAction {
		t.Fatalf("list row %+v contradicts the run page %+v", *row, detail.Workflow.Run)
	}
}

// §24: after a restart, the Board must be rebuilt from the durable rows alone
// and mean the same thing. A second coordinator over the same database has no
// carried state at all, which is exactly what a restart is.
func TestBoardIsSemanticallyIdenticalAfterARestart(t *testing.T) {
	fx := newCancelStack(t)
	srv := newWorkflowTestServer(t, workflowsvc.New(fx.coord))
	fx.stoppedHolder(t, "agent-orchestrator")

	before := p3bBoardOf(t, srv, "agent-orchestrator")
	restarted := newWorkflowTestServer(t, workflowsvc.New(fx.restartedCoordinator(t)))
	after := p3bBoardOf(t, restarted, "agent-orchestrator")

	if len(before.Workflows) != len(after.Workflows) {
		t.Fatalf("restart changed the board size: %d then %d", len(before.Workflows), len(after.Workflows))
	}
	if before.Counts != after.Counts {
		t.Fatalf("restart changed the counts: %+v then %+v", before.Counts, after.Counts)
	}
	for i := range before.Workflows {
		b, a := before.Workflows[i], after.Workflows[i]
		if b.WorkflowID != a.WorkflowID {
			t.Fatalf("restart reordered the board: %q then %q", b.WorkflowID, a.WorkflowID)
		}
		assertSamePresentation(t, b.WorkflowID, *b.Presentation, *a.Presentation)
	}
}

// §14: a repair run has no independent existence on the Board. It cannot be
// archived on its own — that would leave its origin showing an inline repair
// whose history a person had quietly retired — and archiving the origin takes
// it with it.
func TestArchivingAnOriginTakesItsRepairsAndARepairCannotBeArchivedAlone(t *testing.T) {
	fx := newCancelStack(t)
	ctx := context.Background()
	srv := newWorkflowTestServer(t, workflowsvc.New(fx.coord))
	origin := fx.stoppedHolder(t, "agent-orchestrator")
	repair := p3bRun(t, fx, "agent-orchestrator", "repair of the holder", domain.WorkflowRunRunning)
	p3bLinkRepair(t, fx, origin, repair, 1)

	board := p3bBoardOf(t, srv, "agent-orchestrator")
	var originCard *struct {
		WorkflowID            string           `json:"workflowId"`
		Stage                 string           `json:"stage"`
		RequiresHuman         bool             `json:"requiresHuman"`
		AutomaticActionActive bool             `json:"automaticActionActive"`
		RepairOfWorkflowID    string           `json:"repairOfWorkflowId"`
		Presentation          *p3bPresentation `json:"presentation"`
		Repairs               []struct {
			WorkflowID string `json:"workflowId"`
			Attempt    int    `json:"attempt"`
			Active     bool   `json:"active"`
		} `json:"repairs"`
	}
	for i := range board.Workflows {
		if board.Workflows[i].WorkflowID == origin {
			originCard = &board.Workflows[i]
		}
		if board.Workflows[i].WorkflowID == repair {
			t.Fatal("the repair run appeared as a top-level card beside the run it repairs")
		}
	}
	if originCard == nil {
		t.Fatalf("the origin is missing from the board (%+v)", board.Workflows)
	}
	if len(originCard.Repairs) != 1 || originCard.Repairs[0].WorkflowID != repair {
		t.Fatalf("the repair is not inline under its origin: %+v", originCard.Repairs)
	}
	if !originCard.Repairs[0].Active {
		t.Fatal("a repair whose run is still running reported inactive")
	}

	// Archiving the repair alone is refused, and says what to do instead.
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/"+repair+"/cancel-archive", "")
	if status == http.StatusOK {
		t.Fatalf("archiving a repair run on its own was allowed (body=%s)", body)
	}

	// Archiving the origin takes the repair with it, so neither is left on the
	// active board and the archived view keeps the pair together.
	if body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/"+origin+"/cancel-archive", ""); status != http.StatusOK {
		t.Fatalf("cancel-archive of the origin status=%d body=%s", status, body)
	}
	after := p3bBoardOf(t, srv, "agent-orchestrator")
	for _, card := range after.Workflows {
		if card.WorkflowID == origin || card.WorkflowID == repair {
			t.Fatalf("%q is still on the active board after its origin was archived", card.WorkflowID)
		}
	}
	repairRun, found, err := fx.store.GetWorkflowRun(ctx, repair)
	if err != nil || !found {
		t.Fatalf("the repair run row was deleted: found=%v err=%v", found, err)
	}
	if !repairRun.Archived() {
		t.Fatal("the repair was left unarchived while its origin was archived: the pair is now inconsistent")
	}
	if !repairRun.State.Terminal() {
		t.Fatalf("the repair was left running after its origin was cancelled and archived (state=%q)", repairRun.State)
	}
}

// p3bLinkRepair writes the two durable rows the Repair Agent writes: the
// dispatch intent on the origin, and the origin marker on the repair run.
func p3bLinkRepair(t *testing.T, fx *cancelFixture, originID, repairID string, generation int) {
	t.Helper()
	ctx := context.Background()
	intent, err := json.Marshal(domain.RepairIntent{
		ID: fmt.Sprintf("ri-%s-%d", originID, generation), WorkflowRunID: originID,
		TargetRunID: originID, RepairRunID: repairID, Generation: generation,
		ProjectID: "agent-orchestrator", ConditionReason: workflowcore.ReasonFixBudgetExhausted,
	})
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	for _, cp := range []domain.WorkflowCheckpoint{
		{
			ID: "wfc-dispatch-" + repairID, WorkflowRunID: originID, ProjectID: "agent-orchestrator",
			DurablePhase: "workflow_repair_dispatched", PayloadVersion: "v1",
			RetryState: string(intent), CreatedAt: fx.now,
		},
		{
			ID: "wfc-origin-" + repairID, WorkflowRunID: repairID, ProjectID: "agent-orchestrator",
			DurablePhase: "workflow_repair_run_origin", PayloadVersion: "v1",
			RetryState: fmt.Sprintf(`{"originRunId":%q,"generation":%d}`, originID, generation),
			CreatedAt:  fx.now,
		},
	} {
		if _, err := fx.store.CreateWorkflowCheckpoint(ctx, cp); err != nil {
			t.Fatalf("checkpoint %s: %v", cp.DurablePhase, err)
		}
	}
}
