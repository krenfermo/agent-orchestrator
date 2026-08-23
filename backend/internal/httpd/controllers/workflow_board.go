package controllers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ProjectBoardParam is the {projectId} path parameter for the board route.
type ProjectBoardParam struct {
	ProjectID string `path:"projectId" description:"Project identifier (registry key)."`
}

// WorkflowStepProgressView is one entry of the Board's lifecycle checklist.
// The never-executed `advance` step is excluded upstream, so a fully completed
// run's checklist reads as all-complete rather than 5 of 6.
type WorkflowStepProgressView struct {
	Kind  string `json:"kind" enum:"plan,work,review,fix,verify"`
	State string `json:"state" enum:"pending,ready,running,waiting,completed,failed,cancelled"`
}

// WorkflowBoardTaskView is one planned task of a master run.
type WorkflowBoardTaskView struct {
	Ordinal    int64  `json:"ordinal"`
	Title      string `json:"title"`
	State      string `json:"state" enum:"blocked,eligible,running,completed,failed,cancelled"`
	WaitReason string `json:"waitReason,omitempty" enum:"waiting_for_dependencies,waiting_for_write_conflict"`
	// WorkflowID is the child run executing this task, empty until dispatched.
	WorkflowID string `json:"workflowId,omitempty"`
	// Phase is the child run's own derived lifecycle phase. Empty when the task
	// has no child run yet — a fact, not an unknown.
	Phase string                     `json:"phase,omitempty"`
	Steps []WorkflowStepProgressView `json:"steps,omitempty"`
}

// WorkflowBoardEntryView is one Board card.
//
// The two attention fields are the point of this endpoint. `attention` says
// whether AO is handling a problem itself ("ao_internal") or genuinely cannot
// proceed without the user ("human_decision"); only the latter may render as
// "Te necesita". A review that asked for changes AO is about to apply is the
// first kind, and reporting it as the second is the misreport this endpoint
// exists to prevent.
type WorkflowBoardEntryView struct {
	WorkflowID string `json:"workflowId"`
	ProjectID  string `json:"projectId"`
	Objective  string `json:"objective"`
	State      string `json:"state" enum:"pending,running,waiting,needs_attention,completed,failed,cancelled"`
	// Phase is the derived lifecycle vocabulary. For a master run with a
	// running task it is that child's phase, so the card says "Reviewing"
	// rather than the vaguer, equally true "running".
	Phase string `json:"phase" enum:"queued,planning,running,reviewing,fixing,verifying,waiting,waiting_for_capacity,retrying,blocked,needs_attention,completed,failed,cancelled"`
	// Attention separates AO's own recoverable problems from real requests for
	// a human decision. Empty means neither applies.
	Attention string `json:"attention,omitempty" enum:"ao_internal,human_decision"`
	// AttentionReason is the machine-readable kind of the stop. Empty when a
	// run is in needs_attention with genuinely nothing recorded — never a
	// synthesized reason.
	AttentionReason string `json:"attentionReason,omitempty"`
	// AttentionAction is what the user has to do. Populated only for
	// human_decision, and only when AO actually knows the remedy.
	AttentionAction string `json:"attentionAction,omitempty"`

	WaitReason     string     `json:"waitReason,omitempty"`
	NextWakeAt     *time.Time `json:"nextWakeAt,omitempty"`
	LastActivityAt time.Time  `json:"lastActivityAt"`
	ErrorClass     string     `json:"errorClass,omitempty"`

	ExecutionMode string `json:"executionMode" enum:"autonomous,manual"`
	Harness       string `json:"harness,omitempty"`
	Model         string `json:"model,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`

	// TasksTotal is 0 for a single-task run, which has no planned tasks at all.
	TasksTotal     int `json:"tasksTotal"`
	TasksCompleted int `json:"tasksCompleted"`
	TasksRunning   int `json:"tasksRunning"`
	TasksBlocked   int `json:"tasksBlocked"`
	TasksEligible  int `json:"tasksEligible"`
	// TasksFailed counts tasks whose child run ended failed or cancelled
	// (Checkpoint 8P-E.13). Non-zero is why a master run with nothing running
	// is nonetheless not going to finish on its own.
	TasksFailed int `json:"tasksFailed"`
	// CurrentTaskOrdinal/CurrentTaskTitle name the running task, so a card can
	// say "Task 2 of 7 — Backend backup API" from facts.
	CurrentTaskOrdinal int64  `json:"currentTaskOrdinal,omitempty"`
	CurrentTaskTitle   string `json:"currentTaskTitle,omitempty"`

	// BranchWait is the repository+branch this card is queued on, when it is.
	// A blocked card without it can only say "Blocked"; with it, it can say
	// which branch, who holds it, and whether anyone has to do anything.
	BranchWait *WorkflowBranchWaitView `json:"branchWait,omitempty"`

	// ArchivedAt is set on a run a human cancelled and archived. Present only
	// in the archived view; the active board never returns an archived run.
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`

	ReviewCycles int                        `json:"reviewCycles"`
	Steps        []WorkflowStepProgressView `json:"steps,omitempty"`
	Tasks        []WorkflowBoardTaskView    `json:"tasks,omitempty"`
}

// WorkflowBoardResponse is the body of GET /api/v1/projects/{projectId}/board.
type WorkflowBoardResponse struct {
	Workflows []WorkflowBoardEntryView `json:"workflows"`
}

func workflowBoardEntryView(e workflowcore.BoardEntry) WorkflowBoardEntryView {
	view := WorkflowBoardEntryView{
		WorkflowID:         e.Run.ID,
		ProjectID:          e.Run.ProjectID,
		Objective:          e.Run.Objective,
		State:              string(e.Run.State),
		Phase:              string(e.ActivePhase),
		Attention:          string(e.Lifecycle.Attention),
		AttentionReason:    e.Lifecycle.AttentionReason,
		AttentionAction:    e.Lifecycle.AttentionAction,
		WaitReason:         e.Lifecycle.WaitReason,
		NextWakeAt:         e.Lifecycle.NextWakeAt,
		LastActivityAt:     e.Lifecycle.LastActivityAt,
		ErrorClass:         string(e.ErrorClass),
		ExecutionMode:      e.ExecutionMode,
		Harness:            e.Harness,
		Model:              e.Model,
		SessionID:          e.SessionID,
		TasksTotal:         e.Tasks.Total,
		TasksCompleted:     e.Tasks.Completed,
		TasksRunning:       e.Tasks.Running,
		TasksBlocked:       e.Tasks.Blocked,
		TasksEligible:      e.Tasks.Eligible,
		TasksFailed:        e.Tasks.Failed + e.Tasks.Cancelled,
		CurrentTaskOrdinal: e.Tasks.CurrentNumber,
		CurrentTaskTitle:   e.Tasks.CurrentTitle,
		ArchivedAt:         e.Run.ArchivedAt,
		ReviewCycles:       e.ReviewCycles,
		Steps:              stepProgressViews(e.Steps),
		BranchWait:         workflowBranchWaitView(e.BranchWait),
	}
	for _, t := range e.ChildTasks {
		view.Tasks = append(view.Tasks, WorkflowBoardTaskView{
			Ordinal:    t.Ordinal,
			Title:      t.Title,
			State:      string(t.State),
			WaitReason: t.WaitReason,
			WorkflowID: t.RunID,
			Phase:      string(t.Phase),
			Steps:      stepProgressViews(t.Steps),
		})
	}
	return view
}

func stepProgressViews(steps []workflowcore.StepProgress) []WorkflowStepProgressView {
	if len(steps) == 0 {
		return nil
	}
	out := make([]WorkflowStepProgressView, 0, len(steps))
	for _, s := range steps {
		out = append(out, WorkflowStepProgressView{Kind: string(s.Kind), State: string(s.State)})
	}
	return out
}

// boardHistory serves the archived ("Mostrar archivados") projection: the runs
// a human has cancelled and archived, newest first. Same entry shape as the
// active board, so history renders with the same card.
func (c *WorkflowsController) boardHistory(w http.ResponseWriter, r *http.Request) {
	archiver, ok := c.Svc.(workflowsvc.RunArchiver)
	if c.Svc == nil || !ok {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{projectId}/board/history")
		return
	}
	projectID := strings.TrimSpace(chi.URLParam(r, "projectId"))
	entries, err := archiver.ProjectBoardHistory(r.Context(), projectID, workflowsvc.BoardHistoryLimit)
	if errors.Is(err, workflowsvc.ErrArchiveUnsupported) {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{projectId}/board/history")
		return
	}
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "BOARD_HISTORY_FAILED", err.Error(), nil)
		return
	}
	c.writeBoard(w, r, entries)
}

// board serves the project Board projection.
func (c *WorkflowsController) board(w http.ResponseWriter, r *http.Request) {
	reader, ok := c.Svc.(workflowsvc.BoardReader)
	if c.Svc == nil || !ok {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{projectId}/board")
		return
	}
	projectID := strings.TrimSpace(chi.URLParam(r, "projectId"))
	entries, err := reader.ProjectBoard(r.Context(), projectID, workflowsvc.BoardTerminalRetention)
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "BOARD_FAILED", err.Error(), nil)
		return
	}
	c.writeBoard(w, r, entries)
}

// writeBoard applies ownership scoping and serializes a board projection. The
// active board and the archived history share it so an archived run can never
// become visible to a user the active board would have hidden it from.
func (c *WorkflowsController) writeBoard(w http.ResponseWriter, r *http.Request, entries []workflowcore.BoardEntry) {
	out := WorkflowBoardResponse{Workflows: make([]WorkflowBoardEntryView, 0, len(entries))}
	for _, e := range entries {
		// Ownership scoping mirrors the run-detail route exactly: a run whose
		// owner is somebody else is simply absent from the board, never a
		// redacted placeholder.
		if c.scopingEnforced() {
			user, found := identity.FromContext(r.Context())
			if !found || !c.runVisible(r.Context(), e.Run.ID, user.ID) {
				continue
			}
		}
		out.Workflows = append(out.Workflows, workflowBoardEntryView(e))
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}
