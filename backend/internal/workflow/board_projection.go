package workflow

import (
	stdctx "context"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// board_projection.go — P3-B: the Board answers with the SAME projection the
// run detail page answers with.
//
// P3-A gave one run one human projection (presentation.go). What it did not do
// was make every surface use it: the Board still mapped a phase to a card
// itself, GET /workflows returned a raw run state, and a repair run — which is
// an ordinary top-level run in the project, created by the Repair Agent —
// appeared as a card of its own beside the run it was repairing. So one
// incident read as three workflows, "3 need attention" meant one origin and two
// repairs, and a card could say `reviewing` while the page it opened said
// `needs_attention`.
//
// Everything below is derived, at read time, from the same durable rows
// DerivePresentation reads. Nothing here is stored, nothing is a second
// vocabulary, and the one contract test that matters (board_detail_contract_
// test.go) asserts the Board's fields ARE the detail's fields for the same run.

// BoardRepair is one automatic repair, projected under the run it repairs.
//
// §6: a repair is not a sibling workflow. It is AO acting on this run, it is
// shown inline beneath its origin, and it carries no Resume/Repair buttons of
// its own — offering a second remedy for a problem AO is already answering is
// exactly what §5 forbids. RunID is kept so the full repair run stays one click
// away for anybody who wants the technical view.
type BoardRepair struct {
	RunID string
	// Attempt/Budget are the "attempt N of M" a person actually needs. Attempt
	// is the repair INTENT's generation — a durable, ledger-backed identity —
	// never a count of attempt rows, which is what §31's deferral is about.
	Attempt int
	Budget  int
	// Stage/SummaryCode/RequiresHuman are the repair run's own presentation,
	// derived by DerivePresentation exactly like any other run's.
	Stage         Stage
	SummaryCode   string
	RequiresHuman bool
	State         domain.WorkflowRunState
	// Active reports a repair that can still act. It is the LIFECYCLE
	// authority — a repair run that exists and has not reached a terminal
	// state — and never an attempt row, so an orphaned attempt can neither
	// invent a repair nor keep a finished one alive (§31).
	Active                   bool
	Succeeded                bool
	Failed                   bool
	LastMeaningfulActivityAt time.Time
}

// BoardCounts are the headline numbers, computed from the same rules the cards
// are (§22).
//
// A repair is never counted here: it is not a workflow the project asked for,
// and counting it would turn one incident into "3 workflows need attention".
// Archived runs are counted only in Archived.
type BoardCounts struct {
	// Active is everything not terminal: working, waiting and needing a person.
	Active         int
	Working        int
	Waiting        int
	NeedsAttention int
	Completed      int
	Archived       int
}

// BoardQuery is the server-side filter and page (§20).
//
// It exists so a Board with several hundred runs does not have to ship the
// whole history to draw twenty cards. Every filter is applied to the SAME
// derived projection the cards render, so a filtered board can never disagree
// with an unfiltered one about what a run is doing.
type BoardQuery struct {
	// Retention is how long a finished run stays on the active board.
	Retention time.Duration
	// Archived selects the archived view instead of the active one. The two are
	// disjoint by construction: an archived run is never on the active board and
	// an active run is never in the history.
	Archived bool
	// Stages, when non-empty, keeps only runs whose derived stage is listed.
	Stages []Stage
	// RequiresHuman, when non-nil, keeps only runs matching the flag.
	RequiresHuman *bool
	// Strategy filters on the run's recorded execution strategy.
	Strategy string
	// Search is a case-insensitive substring of the objective or the run id.
	Search string
	// Offset/Limit page the RESULT, after ordering. Limit <= 0 means no paging.
	Offset int
	Limit  int
}

// BoardView is one Board response: the page, the counts the whole board would
// have produced, and how many runs matched the filter.
type BoardView struct {
	Entries []BoardEntry
	Counts  BoardCounts
	// Matched is how many entries the filter selected, before paging — what a
	// pager needs and what a page length cannot say.
	Matched int
	Offset  int
	Limit   int
}

// maxBoardObjectiveBytes bounds the objective a Board card carries (§17).
//
// A task's objective is its full specification and may be 128 KiB. The card
// shows a title and a first line; shipping the whole specification for every
// card would make a 500-run board a multi-megabyte response for text nothing
// renders. Nothing is truncated in storage — the run detail endpoint still
// returns the objective in full, and ObjectiveTruncated says plainly that this
// copy is a summary.
const maxBoardObjectiveBytes = 2000

// ProjectBoardView is the Board projection with hierarchy, ordering, counts and
// paging applied.
func (c *Coordinator) ProjectBoardView(ctx stdctx.Context, projectID string, q BoardQuery) (BoardView, error) {
	runs, err := c.boardCandidateRuns(ctx, projectID, q)
	if err != nil {
		return BoardView{}, err
	}
	// One query for the whole repair vocabulary, not one per run: the store
	// already indexes runs by checkpoint phase, and asking "which of these runs
	// is a repair" per card is precisely the N+1 §19 forbids.
	repairOf := c.repairOriginIndex(ctx, runs)

	byID := make(map[string]*BoardEntry, len(runs))
	order := make([]string, 0, len(runs))
	deferred := make([]domain.WorkflowRun, 0, 4)
	for _, run := range runs {
		if _, isRepair := repairOf[run.ID]; isRepair {
			deferred = append(deferred, run)
			continue
		}
		entry, err := c.boardEntry(ctx, run)
		if err != nil {
			return BoardView{}, err
		}
		byID[run.ID] = &entry
		order = append(order, run.ID)
	}

	// A repair goes under its origin. One that has no origin ON THIS BOARD —
	// the origin aged out, was archived, or lives in another project — becomes a
	// top-level card rather than disappearing: §26's "a hidden repair child is
	// still reachable" is not satisfied by hiding it under nothing.
	for _, run := range deferred {
		link := repairOf[run.ID]
		entry, err := c.boardEntry(ctx, run)
		if err != nil {
			return BoardView{}, err
		}
		entry.RepairOfRunID = link.OriginRunID
		entry.RepairGeneration = link.Generation
		if origin, ok := byID[link.OriginRunID]; ok {
			origin.Repairs = append(origin.Repairs, boardRepairOf(entry, link, origin.RepairBudget))
			continue
		}
		byID[run.ID] = &entry
		order = append(order, run.ID)
	}

	entries := make([]BoardEntry, 0, len(order))
	for _, id := range order {
		e := byID[id]
		sort.SliceStable(e.Repairs, func(i, j int) bool { return e.Repairs[i].Attempt < e.Repairs[j].Attempt })
		entries = append(entries, *e)
	}

	counts := boardCounts(entries)
	entries = filterBoardEntries(entries, q)
	sortBoardEntries(entries)
	matched := len(entries)
	entries = pageBoardEntries(entries, q)
	return BoardView{Entries: entries, Counts: counts, Matched: matched, Offset: q.Offset, Limit: q.Limit}, nil
}

// boardCandidateRuns is the set of runs the board may show, before hierarchy.
func (c *Coordinator) boardCandidateRuns(ctx stdctx.Context, projectID string, q BoardQuery) ([]domain.WorkflowRun, error) {
	if q.Archived {
		store, ok := c.archiveStore()
		if !ok {
			return nil, ErrArchiveUnsupported
		}
		limit := q.Limit
		if limit <= 0 {
			limit = 100
		}
		return store.ListArchivedWorkflowRuns(ctx, projectID, q.Offset+limit)
	}
	runs, err := c.store.ListWorkflowRuns(ctx, projectID)
	if err != nil {
		return nil, err
	}
	cutoff := c.clock().Add(-q.Retention)
	out := make([]domain.WorkflowRun, 0, len(runs))
	for _, run := range runs {
		// A child of a master is not a card: it appears under its parent.
		if run.ParentWorkflowID != nil {
			continue
		}
		// Archiving is a human decision, not a derived state: unlike the
		// retention rule below, an archived run never comes back.
		if run.Archived() {
			continue
		}
		if run.State.Terminal() && !terminalWithin(run, cutoff) {
			continue
		}
		out = append(out, run)
	}
	return out, nil
}

// repairOriginIndex maps every repair run in `runs` to the run it repairs.
//
// The phase index is one query for the whole daemon; the per-run checkpoint
// read that follows happens only for runs that phase actually named, so the
// cost is proportional to the number of repairs rather than to the size of the
// board.
func (c *Coordinator) repairOriginIndex(ctx stdctx.Context, runs []domain.WorkflowRun) map[string]repairLink {
	out := map[string]repairLink{}
	ids, err := c.store.ListWorkflowRunIDsByCheckpointPhase(ctx, repairRunOriginPhase)
	if err != nil || len(ids) == 0 {
		// A store that cannot answer leaves every run an ordinary one, which is
		// deliberately the direction this fails in: a repair shown as its own
		// card is untidy, a workflow hidden as somebody's repair would be a run
		// gone missing. That is also why it returns no error — there is no
		// failure a caller could do anything about.
		return out
	}
	repairs := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		repairs[id] = struct{}{}
	}
	for _, run := range runs {
		if _, ok := repairs[run.ID]; !ok {
			continue
		}
		link, ok := c.repairOriginLink(ctx, run.ID)
		if !ok || link.OriginRunID == run.ID {
			continue
		}
		out[run.ID] = link
	}
	return out
}

// repairLink is the durable "this run is generation N of the repair of run X",
// read from the marker the Repair Agent writes before the repair run starts.
type repairLink struct {
	OriginRunID string
	Generation  int
}

// repairOriginLink reads the origin and generation this repair run was created
// for. Not-ok when the marker is missing or unreadable — in which case the run
// is treated as an ordinary one rather than nested under a guess.
func (c *Coordinator) repairOriginLink(ctx stdctx.Context, repairRunID string) (repairLink, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, repairRunID)
	if err != nil {
		return repairLink{}, false
	}
	for _, cp := range cps {
		if cp.DurablePhase != repairRunOriginPhase {
			continue
		}
		var body struct {
			OriginRunID string `json:"originRunId"`
			Generation  int    `json:"generation"`
		}
		if json.Unmarshal([]byte(cp.RetryState), &body) == nil && body.OriginRunID != "" {
			return repairLink{OriginRunID: body.OriginRunID, Generation: body.Generation}, true
		}
	}
	return repairLink{}, false
}

// boardRepairOf compacts a repair run's own board entry into the inline form.
//
// Attempt and budget come from the ORIGIN's ledger, not from the repair run:
// "attempt 2 of 3" is a fact about the run being repaired, and the repair run's
// own repair lifecycle would answer a different question entirely.
func boardRepairOf(e BoardEntry, link repairLink, budget int) BoardRepair {
	return BoardRepair{
		RunID:                    e.Run.ID,
		Attempt:                  link.Generation,
		Budget:                   budget,
		Stage:                    e.Presentation.Stage,
		SummaryCode:              e.Presentation.SummaryCode,
		RequiresHuman:            e.Presentation.RequiresHuman,
		State:                    e.Run.State,
		Active:                   !e.Run.State.Terminal(),
		Succeeded:                e.Run.State == domain.WorkflowRunCompleted,
		Failed:                   e.Run.State == domain.WorkflowRunFailed || e.Run.State == domain.WorkflowRunCancelled,
		LastMeaningfulActivityAt: e.Presentation.LastMeaningfulActivityAt,
	}
}

// boardCounts computes the headline numbers from the derived stages (§22).
func boardCounts(entries []BoardEntry) BoardCounts {
	var counts BoardCounts
	for _, e := range entries {
		if e.Run.Archived() {
			counts.Archived++
			continue
		}
		// A repair nested under an origin is not counted; one that surfaced as
		// its own card is, because on this board it IS the only thing
		// representing that work.
		switch {
		case e.Presentation.RequiresHuman:
			counts.NeedsAttention++
			counts.Active++
		case e.Presentation.Stage == StageWaiting:
			counts.Waiting++
			counts.Active++
		case e.Presentation.Stage == StageCompleted:
			counts.Completed++
		case e.Presentation.Stage.Terminal():
			// Cancelled and failed are terminal and are neither active nor
			// completed. They are deliberately absent from every bucket rather
			// than folded into one they are not.
		default:
			counts.Working++
			counts.Active++
		}
	}
	return counts
}

func filterBoardEntries(entries []BoardEntry, q BoardQuery) []BoardEntry {
	stages := map[Stage]bool{}
	for _, s := range q.Stages {
		stages[s] = true
	}
	search := strings.ToLower(strings.TrimSpace(q.Search))
	strategy := strings.TrimSpace(q.Strategy)
	out := entries[:0:0]
	for _, e := range entries {
		if len(stages) > 0 && !stages[e.Presentation.Stage] {
			continue
		}
		if q.RequiresHuman != nil && e.Presentation.RequiresHuman != *q.RequiresHuman {
			continue
		}
		if strategy != "" && e.Strategy != strategy {
			continue
		}
		if search != "" &&
			!strings.Contains(strings.ToLower(e.Run.Objective), search) &&
			!strings.Contains(strings.ToLower(e.Run.ID), search) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// boardOrderBucket is §21's stable ranking: what a person has to act on, then
// what AO is fixing by itself, then what is moving, then what is parked, then
// what is over.
func boardOrderBucket(e BoardEntry) int {
	switch {
	case e.Presentation.RequiresHuman:
		return 0
	case e.Presentation.AutomaticActionActive && !e.Presentation.Stage.Terminal():
		return 1
	case e.Presentation.Stage.Terminal():
		return 4
	case e.Presentation.Stage == StageWaiting:
		return 3
	default:
		return 2
	}
}

// sortBoardEntries orders the board deterministically.
//
// The tie-break is the run id, not the database's row order: two runs with the
// same bucket and the same activity timestamp must come back in the same order
// on every poll, or cards swap places under the cursor for no reason.
func sortBoardEntries(entries []BoardEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		bi, bj := boardOrderBucket(entries[i]), boardOrderBucket(entries[j])
		if bi != bj {
			return bi < bj
		}
		ai := entries[i].Presentation.LastMeaningfulActivityAt
		aj := entries[j].Presentation.LastMeaningfulActivityAt
		if !ai.Equal(aj) {
			return ai.After(aj)
		}
		return entries[i].Run.ID < entries[j].Run.ID
	})
}

func pageBoardEntries(entries []BoardEntry, q BoardQuery) []BoardEntry {
	if q.Offset > 0 {
		if q.Offset >= len(entries) {
			return nil
		}
		entries = entries[q.Offset:]
	}
	if q.Limit > 0 && len(entries) > q.Limit {
		entries = entries[:q.Limit]
	}
	return entries
}

// boardObjective bounds the objective a card carries (§17).
//
// It returns the first line as the title, a bounded body, and whether the body
// is a summary of something longer. It never cuts a UTF-8 sequence in half.
func boardObjective(objective string) (title, summary string, truncated bool) {
	trimmed := strings.TrimSpace(objective)
	first := trimmed
	if idx := strings.IndexByte(trimmed, '\n'); idx >= 0 {
		first = strings.TrimSpace(trimmed[:idx])
	}
	if first == "" {
		first = trimmed
	}
	if len(trimmed) <= maxBoardObjectiveBytes {
		return first, trimmed, false
	}
	cut := maxBoardObjectiveBytes
	for cut > 0 && !utf8.RuneStart(trimmed[cut]) {
		cut--
	}
	return first, trimmed[:cut], true
}

// RepairRunOrigin reports whether this run is an automatic repair created by
// the Repair Agent, and of what.
//
// It is the exported form of the marker the Board nests cards by, so the run
// detail page can label a repair the same way the card that opened it did. Two
// surfaces answering "is this a repair" from two different markers is exactly
// the divergence P3-B exists to remove.
func (c *Coordinator) RepairRunOrigin(ctx stdctx.Context, runID string) (originRunID string, generation int, ok bool) {
	link, found := c.repairOriginLink(ctx, runID)
	if !found || link.OriginRunID == runID {
		return "", 0, false
	}
	return link.OriginRunID, link.Generation, true
}
