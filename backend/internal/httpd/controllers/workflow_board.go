package controllers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
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
	State      string `json:"state" enum:"blocked,eligible,running,needs_attention,completed,failed,cancelled"`
	WaitReason string `json:"waitReason,omitempty" enum:"waiting_for_dependencies,waiting_for_write_conflict"`
	// WorkflowID is the child run executing this task, empty until dispatched.
	WorkflowID string `json:"workflowId,omitempty"`
	// Phase is the child run's own derived lifecycle phase. Empty when the task
	// has no child run yet — a fact, not an unknown.
	Phase string                     `json:"phase,omitempty"`
	Steps []WorkflowStepProgressView `json:"steps,omitempty"`
	// ID is the plan-step id this task is known by on the run-detail endpoint
	// (WorkflowTaskView.id), so a Board row and a detail row can be matched
	// without going through the ordinal.
	ID string `json:"id,omitempty"`
	// Planner is the same per-task planner projection the run-detail endpoint
	// returns: execution strategy, dependency and integration ordering, waiting
	// reason, dispatch wave, probable write scope, AO worktree/branch and
	// integration state. It is what lets a Board row say "Waiting for conflict"
	// or "Ready to integrate" instead of only "blocked" or "running".
	Planner *WorkflowTaskPlannerView `json:"planner,omitempty"`
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
	// TasksNeedsAttention counts tasks parked on a decision only a person can
	// make — today, an integration conflict (migration 0130). Separate from
	// TasksFailed because the remedy is different: the work is very likely
	// fine and one resume releases it.
	TasksNeedsAttention int `json:"tasksNeedsAttention"`
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

	// Usage is P3-E's compact per-card figure: a token total, and a cost when
	// one is knowable. Deliberately nothing more -- a card is not a financial
	// dashboard, and it is filled from ONE grouped query for the whole board
	// rather than a query per card. Absent when the run has no usage rows, so
	// a card renders nothing rather than "0 tokens"; present with
	// `recorded: false` never happens, and `cost.known: false` means show the
	// tokens and no money at all, never "$0.00".
	Usage *CompactRunUsageResponse `json:"usage,omitempty"`

	ReviewCycles int                        `json:"reviewCycles"`
	Steps        []WorkflowStepProgressView `json:"steps,omitempty"`
	Tasks        []WorkflowBoardTaskView    `json:"tasks,omitempty"`

	// Stage, RequiresHuman and AutomaticActionActive are the compact human
	// projection, mirrored at the top level of the card for the surfaces that
	// only need those three. They are COPIES of `presentation`, taken from the
	// same value, so they cannot drift from it.
	Stage                 string `json:"stage" enum:"preparing,planning,working,reviewing,correcting,verifying,integrating,waiting,needs_attention,completed,cancelled,failed"`
	RequiresHuman         bool   `json:"requiresHuman"`
	AutomaticActionActive bool   `json:"automaticActionActive"`

	// Presentation is P3-B's contract: the Board sends the SAME projection the
	// run detail endpoint sends, built by the same mapping function from the
	// same derivation. The only difference is the bounded timeline, which a
	// card has no room for and which the detail page still carries in full — so
	// every field present here is field-for-field equal to the detail page's,
	// which is what board_detail_contract_test.go asserts.
	Presentation *WorkflowPresentationView `json:"presentation,omitempty"`
	// Strategy is the run's recorded execution strategy. Empty for a legacy run
	// whose mapping has not been reconciled yet — a fact, not an unknown.
	Strategy string `json:"strategy,omitempty" enum:"task,autonomous,master"`
	// LastMeaningfulActivityAt is when this run last did something a person
	// would recognise as having happened (§11). It is deliberately NOT
	// lastActivityAt above: that one moves for bookkeeping, and a board sorted
	// by it showed a run stalled for hours as active seconds ago.
	LastMeaningfulActivityAt time.Time `json:"lastMeaningfulActivityAt"`
	// Repairs are the automatic repairs of THIS run, inline (§6). A repair run
	// is an ordinary top-level run in the project; nesting it here is what
	// stops one incident from reading as three workflows and from being counted
	// three times in the headline.
	Repairs []WorkflowBoardRepairView `json:"repairs,omitempty"`
	// RepairOfWorkflowID/RepairGeneration are set only on a card that IS a
	// repair and could not be nested because the run it repairs is not on this
	// board. Rather than disappear, it stays visible and stays labelled.
	RepairOfWorkflowID string `json:"repairOfWorkflowId,omitempty"`
	RepairGeneration   int    `json:"repairGeneration,omitempty"`
	// ObjectiveTruncated says plainly that `objective` above is a bounded
	// summary of a longer specification (§17). Nothing is truncated in storage:
	// the run detail endpoint returns the objective in full.
	ObjectiveTruncated bool `json:"objectiveTruncated,omitempty"`
}

// WorkflowBoardRepairView is one automatic repair, shown under the run it
// repairs.
//
// It deliberately carries no actions. §6: the origin owns Resume and Repair,
// and a repair offering its own copies is the duplicate remedy §5 forbids. The
// run id is here so the full repair run stays one click away.
type WorkflowBoardRepairView struct {
	WorkflowID string `json:"workflowId"`
	// Attempt/Budget are "attempt N of M", from the ORIGIN's durable repair
	// ledger — the intent's generation, never a count of attempt rows.
	Attempt int    `json:"attempt"`
	Budget  int    `json:"budget"`
	Stage   string `json:"stage" enum:"preparing,planning,working,reviewing,correcting,verifying,integrating,waiting,needs_attention,completed,cancelled,failed"`
	// SummaryCode is the same stable key the origin's card renders its sentence
	// from, so a repair says what it is doing in the same vocabulary.
	SummaryCode   string `json:"summaryCode,omitempty"`
	RequiresHuman bool   `json:"requiresHuman"`
	State         string `json:"state" enum:"pending,running,waiting,needs_attention,completed,failed,cancelled"`
	// Active is the LIFECYCLE authority: a repair run that exists and has not
	// reached a terminal state. It is never derived from an attempt row, so an
	// orphaned attempt can neither invent a repair nor keep a finished one
	// alive.
	Active                   bool      `json:"active"`
	Succeeded                bool      `json:"succeeded,omitempty"`
	Failed                   bool      `json:"failed,omitempty"`
	LastMeaningfulActivityAt time.Time `json:"lastMeaningfulActivityAt"`
}

// WorkflowBoardCountsView is the headline (§22).
//
// The numbers come from the same derived stages the cards do, and a repair
// nested under its origin is never counted: "3 workflows need attention" when
// the truth is one origin and two of its repairs is the misreport these counts
// exist to prevent.
type WorkflowBoardCountsView struct {
	Active         int `json:"active"`
	Working        int `json:"working"`
	Waiting        int `json:"waiting"`
	NeedsAttention int `json:"needsAttention"`
	Completed      int `json:"completed"`
	Archived       int `json:"archived"`
}

// WorkflowBoardResponse is the body of GET /api/v1/projects/{projectId}/board.
type WorkflowBoardResponse struct {
	Workflows []WorkflowBoardEntryView `json:"workflows"`
	// Counts are computed over the whole board, not over the returned page, so
	// the view tabs keep saying how much there is while a filter is applied.
	Counts WorkflowBoardCountsView `json:"counts"`
	// Matched is how many runs the filter selected before paging — what a pager
	// needs and what len(workflows) cannot say.
	Matched int `json:"matched"`
	Offset  int `json:"offset"`
	Limit   int `json:"limit"`
}

// ProjectBoardQuery is the query string accepted by the board route (§20).
//
// Every filter runs server-side against the same derived projection the cards
// render, so a filtered board can never disagree with an unfiltered one about
// what a run is doing.
type ProjectBoardQuery struct {
	Stage         string `query:"stage,omitempty" description:"Comma-separated derived stages to keep (preparing, working, reviewing, correcting, verifying, integrating, waiting, needs_attention, completed, cancelled, failed)."`
	RequiresHuman string `query:"requiresHuman,omitempty" enum:"true,false" description:"Keep only runs that do (true) or do not (false) need a person."`
	Strategy      string `query:"strategy,omitempty" enum:"task,autonomous,master" description:"Recorded execution strategy filter."`
	Search        string `query:"search,omitempty" description:"Case-insensitive substring of the objective or the run id."`
	Offset        string `query:"offset,omitempty" description:"Zero-based offset into the ordered result."`
	Limit         string `query:"limit,omitempty" description:"Page size; defaults to 50 and is capped at 200."`
}

func workflowBoardEntryView(e workflowcore.BoardEntry) WorkflowBoardEntryView {
	view := WorkflowBoardEntryView{
		WorkflowID:          e.Run.ID,
		ProjectID:           e.Run.ProjectID,
		Objective:           e.Run.Objective,
		State:               string(e.Run.State),
		Phase:               string(e.ActivePhase),
		Attention:           string(e.Lifecycle.Attention),
		AttentionReason:     e.Lifecycle.AttentionReason,
		AttentionAction:     e.Lifecycle.AttentionAction,
		WaitReason:          e.Lifecycle.WaitReason,
		NextWakeAt:          e.Lifecycle.NextWakeAt,
		LastActivityAt:      e.Lifecycle.LastActivityAt,
		ErrorClass:          string(e.ErrorClass),
		ExecutionMode:       e.ExecutionMode,
		Harness:             e.Harness,
		Model:               e.Model,
		SessionID:           e.SessionID,
		TasksTotal:          e.Tasks.Total,
		TasksCompleted:      e.Tasks.Completed,
		TasksRunning:        e.Tasks.Running,
		TasksBlocked:        e.Tasks.Blocked,
		TasksEligible:       e.Tasks.Eligible,
		TasksFailed:         e.Tasks.Failed + e.Tasks.Cancelled,
		TasksNeedsAttention: e.Tasks.NeedsAttention,
		CurrentTaskOrdinal:  e.Tasks.CurrentNumber,
		CurrentTaskTitle:    e.Tasks.CurrentTitle,
		ArchivedAt:          e.Run.ArchivedAt,
		ReviewCycles:        e.ReviewCycles,
		Steps:               stepProgressViews(e.Steps),
		BranchWait:          workflowBranchWaitView(e.BranchWait),
	}
	// P3-B: one projection, sent once. The three flat fields below are copies
	// taken from the same value, never a second derivation -- the whole reason
	// a card and its page could disagree before was that each mapped the facts
	// itself.
	presentation := workflowPresentationView(e.Presentation)
	// The timeline is the one field a card drops: it is unbounded in length,
	// nothing on a card renders it, and the run detail page carries it in full.
	presentation.Timeline = nil
	view.Presentation = &presentation
	view.Stage = presentation.Stage
	view.RequiresHuman = presentation.RequiresHuman
	view.AutomaticActionActive = presentation.AutomaticActionActive
	view.Strategy = e.Strategy
	view.LastMeaningfulActivityAt = e.Presentation.LastMeaningfulActivityAt
	view.RepairOfWorkflowID = e.RepairOfRunID
	view.RepairGeneration = e.RepairGeneration
	view.ObjectiveTruncated = e.ObjectiveTruncated
	if e.ObjectiveTruncated {
		// §17: the card carries a bounded summary. The full specification stays
		// on disk and on the run detail endpoint, untouched.
		view.Objective = e.ObjectiveSummary
	}
	for _, r := range e.Repairs {
		view.Repairs = append(view.Repairs, WorkflowBoardRepairView{
			WorkflowID: r.RunID, Attempt: r.Attempt, Budget: r.Budget,
			Stage: string(r.Stage), SummaryCode: r.SummaryCode,
			RequiresHuman: r.RequiresHuman, State: string(r.State),
			Active: r.Active, Succeeded: r.Succeeded, Failed: r.Failed,
			LastMeaningfulActivityAt: r.LastMeaningfulActivityAt,
		})
	}
	// The planner projection speaks in task ids; every other id on this endpoint
	// is a plan-step id, and mixing the two in one response is how a client ends
	// up rendering an opaque internal id next to a readable one.
	planIDByTask := make(map[string]string, len(e.ChildTasks))
	for _, t := range e.ChildTasks {
		planIDByTask[t.TaskID] = t.PlanStepID
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
			ID:         t.PlanStepID,
			Planner:    workflowTaskPlannerView(t.Planner, planIDByTask),
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

// boardQuery parses §20's server-side filter and page off the request.
//
// Every value it cannot understand is IGNORED rather than refused: a board is a
// read, and a stage name a client sent that this daemon does not know should
// narrow nothing rather than 400 the whole screen. The one thing it enforces is
// the page ceiling, because that is a promise about response size rather than a
// preference.
func boardQuery(r *http.Request, archived bool) workflowcore.BoardQuery {
	q := workflowcore.BoardQuery{
		Retention: workflowsvc.BoardTerminalRetention,
		Archived:  archived,
		Limit:     workflowsvc.BoardPageLimit,
	}
	values := r.URL.Query()
	for _, raw := range strings.Split(values.Get("stage"), ",") {
		if stage := strings.TrimSpace(raw); stage != "" {
			q.Stages = append(q.Stages, workflowcore.Stage(stage))
		}
	}
	switch strings.TrimSpace(values.Get("requiresHuman")) {
	case "true":
		yes := true
		q.RequiresHuman = &yes
	case "false":
		no := false
		q.RequiresHuman = &no
	}
	q.Strategy = strings.TrimSpace(values.Get("strategy"))
	q.Search = strings.TrimSpace(values.Get("search"))
	if n, err := strconv.Atoi(strings.TrimSpace(values.Get("offset"))); err == nil && n > 0 {
		q.Offset = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(values.Get("limit"))); err == nil && n > 0 {
		if n > workflowsvc.BoardPageLimitMax {
			n = workflowsvc.BoardPageLimitMax
		}
		q.Limit = n
	}
	return q
}

// boardHistory serves the archived ("Mostrar archivados") projection: the runs
// a human has cancelled and archived, newest first. Same entry shape and the
// same derivation as the active board, so history renders with the same card
// and cannot describe a run differently from the way the active board did the
// moment before it was archived.
func (c *WorkflowsController) boardHistory(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(chi.URLParam(r, "projectId"))
	if viewer, ok := c.Svc.(workflowsvc.BoardViewReader); ok && c.Svc != nil {
		q := boardQuery(r, true)
		if q.Limit > workflowsvc.BoardHistoryLimit {
			q.Limit = workflowsvc.BoardHistoryLimit
		}
		view, err := viewer.ProjectBoardView(r.Context(), projectID, q)
		if errors.Is(err, workflowsvc.ErrArchiveUnsupported) {
			apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{projectId}/board/history")
			return
		}
		if err != nil {
			envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "BOARD_HISTORY_FAILED", err.Error(), nil)
			return
		}
		c.writeBoardView(w, r, view)
		return
	}
	archiver, ok := c.Svc.(workflowsvc.RunArchiver)
	if c.Svc == nil || !ok {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{projectId}/board/history")
		return
	}
	entries, err := archiver.ProjectBoardHistory(r.Context(), projectID, workflowsvc.BoardHistoryLimit)
	if errors.Is(err, workflowsvc.ErrArchiveUnsupported) {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{projectId}/board/history")
		return
	}
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "BOARD_HISTORY_FAILED", err.Error(), nil)
		return
	}
	c.writeBoardView(w, r, workflowcore.BoardView{Entries: entries, Matched: len(entries)})
}

// board serves the project Board projection.
func (c *WorkflowsController) board(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(chi.URLParam(r, "projectId"))
	if viewer, ok := c.Svc.(workflowsvc.BoardViewReader); ok && c.Svc != nil {
		view, err := viewer.ProjectBoardView(r.Context(), projectID, boardQuery(r, false))
		if err != nil {
			envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "BOARD_FAILED", err.Error(), nil)
			return
		}
		c.writeBoardView(w, r, view)
		return
	}
	reader, ok := c.Svc.(workflowsvc.BoardReader)
	if c.Svc == nil || !ok {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{projectId}/board")
		return
	}
	entries, err := reader.ProjectBoard(r.Context(), projectID, workflowsvc.BoardTerminalRetention)
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "BOARD_FAILED", err.Error(), nil)
		return
	}
	c.writeBoardView(w, r, workflowcore.BoardView{Entries: entries, Matched: len(entries)})
}

// writeBoardView applies ownership scoping and serializes a board projection.
// The active board and the archived history share it so an archived run can
// never become visible to a user the active board would have hidden it from.
func (c *WorkflowsController) writeBoardView(w http.ResponseWriter, r *http.Request, view workflowcore.BoardView) {
	// P3-E: ONE grouped query for the whole board, not one per card. A board
	// of fifty runs must stay one round trip, which is why CompactForProject
	// folds every run in the project at once. A failure here costs the cards
	// their token figure and nothing else -- a board that cannot price itself
	// must still render.
	usageByRun := map[string]domain.CompactRunUsage{}
	if c.UsageLedger != nil && len(view.Entries) > 0 {
		usageByRun = c.boardUsage.get(r.Context(), view.Entries[0].Run.ProjectID, c.UsageLedger.CompactForProject)
	}
	out := WorkflowBoardResponse{
		Workflows: make([]WorkflowBoardEntryView, 0, len(view.Entries)),
		Counts: WorkflowBoardCountsView{
			Active: view.Counts.Active, Working: view.Counts.Working,
			Waiting: view.Counts.Waiting, NeedsAttention: view.Counts.NeedsAttention,
			Completed: view.Counts.Completed, Archived: view.Counts.Archived,
		},
		Matched: view.Matched, Offset: view.Offset, Limit: view.Limit,
	}
	for _, e := range view.Entries {
		// Ownership scoping mirrors the run-detail route exactly: a run whose
		// owner is somebody else is simply absent from the board, never a
		// redacted placeholder.
		if c.scopingEnforced() {
			user, found := identity.FromContext(r.Context())
			if !found || !c.runVisible(r.Context(), e.Run.ID, user.ID) {
				continue
			}
		}
		card := workflowBoardEntryView(e)
		if u, ok := usageByRun[card.WorkflowID]; ok && u.Recorded {
			compact := compactRunUsageResponse(u)
			card.Usage = &compact
		}
		out.Workflows = append(out.Workflows, card)
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}
