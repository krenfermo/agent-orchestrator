package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// run_summary.go — P3-B §3: closing the ListWorkflows gap.
//
// GET /api/v1/workflows returned a run ROW: id, objective, run state. The Board
// returned a derived stage and the run detail page returned a full
// presentation, so the same workflow could read
//
//	list   → running
//	board  → reviewing
//	detail → correcting
//
// all three at the same instant and all three "correct" about a different
// vocabulary. The list was the odd one out because it never read the run's
// steps, and a run state alone cannot tell `running` from `reviewing`.
//
// RunSummary is the compact projection that closes it. It is not a fourth
// vocabulary: every field below is copied out of the SAME Presentation the
// board card and the detail page render, so the list can be less detailed than
// they are and can never contradict them.

// RunSummary is one row of the workflow list.
type RunSummary struct {
	Run domain.WorkflowRun
	// Presentation carries the compact half of the projection. Progress,
	// timeline and actions are deliberately dropped by the API projection
	// above this layer — a list has no room for them — but they are derived
	// here, from the same call, so nothing in the list is computed by a second
	// rule.
	Presentation Presentation
	Strategy     string
	// RepairOfRunID names the run this one is an automatic repair of, empty for
	// ordinary work. A list is flat by nature, so the link is how a repair stays
	// distinguishable from a workflow somebody asked for.
	RepairOfRunID string
}

// ListRunSummariesFilter bounds the list.
//
// The limit exists because the previous endpoint had none: it returned every
// run a project had ever had, and now that each row carries a derived stage
// that is a promise to project unbounded history on every poll.
type ListRunSummariesFilter struct {
	ProjectID string
	Offset    int
	Limit     int
}

// ListRunSummaries projects runs onto the shared human status model.
func (c *Coordinator) ListRunSummaries(ctx stdctx.Context, f ListRunSummariesFilter) ([]RunSummary, int, error) {
	runs, err := c.store.ListWorkflowRuns(ctx, f.ProjectID)
	if err != nil {
		return nil, 0, err
	}
	total := len(runs)
	page := runs
	if f.Offset > 0 {
		if f.Offset >= len(page) {
			return nil, total, nil
		}
		page = page[f.Offset:]
	}
	if f.Limit > 0 && len(page) > f.Limit {
		page = page[:f.Limit]
	}
	// The repair index is read once for the page rather than per row, for the
	// same reason the Board reads it once: "is this a repair" is a question
	// about a checkpoint phase the store already indexes.
	repairOf := c.repairOriginIndex(ctx, page)
	out := make([]RunSummary, 0, len(page))
	for _, run := range page {
		detail, derr := c.readOnlyDetail(ctx, run)
		if derr != nil {
			return nil, 0, derr
		}
		life := DeriveLifecycle(LifecycleInput{Detail: detail, Questions: detail.Questions})
		summary := RunSummary{Run: run, Presentation: c.presentationFor(ctx, detail, life)}
		if sel, ok := RecordedExecutionStrategy(run); ok {
			summary.Strategy = string(sel.Effective)
		}
		summary.RepairOfRunID = repairOf[run.ID].OriginRunID
		out = append(out, summary)
	}
	return out, total, nil
}
