package workflow

import (
	stdctx "context"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ArchiveStore is the optional durable surface behind cancel-and-archive.
//
// It is type-asserted off the coordinator's Store rather than added to the
// Store interface itself, following this package's existing narrow-optional
// convention: a store (or test double) that predates archiving keeps
// compiling, and simply reports archiving as unsupported.
type ArchiveStore interface {
	// ArchiveWorkflowRun stamps the archive marker. False means "already
	// archived, or not terminal" — never an error, and never a delete.
	ArchiveWorkflowRun(ctx stdctx.Context, id string, now time.Time) (bool, error)
	// ListChildWorkflowRuns lists a master's child runs in creation order.
	ListChildWorkflowRuns(ctx stdctx.Context, parentRunID string) ([]domain.WorkflowRun, error)
	// ListArchivedWorkflowRuns lists a project's archived top-level runs,
	// newest archive first.
	ListArchivedWorkflowRuns(ctx stdctx.Context, projectID string, limit int) ([]domain.WorkflowRun, error)
}

// ErrArchiveUnsupported is returned when the configured store cannot archive.
var ErrArchiveUnsupported = errors.New("workflow archiving is not supported by this store")

// ArchivedRunCheckpointPhase is the durable phase written once per run when a
// human cancels and archives it. It is the audit trail of the action: who
// stopped what, and the explicit record that the history below it was kept.
const ArchivedRunCheckpointPhase = "run_cancelled_and_archived"

func (c *Coordinator) archiveStore() (ArchiveStore, bool) {
	as, ok := c.store.(ArchiveStore)
	return as, ok
}

// CancelAndArchiveRun stops a workflow for good and moves it off the active
// Board, without deleting a single row.
//
// It exists because "cancel" and "stop showing me this" were not the same
// operation and only the first had a button. A run parked in needs_attention —
// child_failed, master_integration_promotion_failed, a cancelled child chain —
// is not terminal, so it never aged out of the Board's completion retention and
// stayed on the active lane indefinitely, long after the incident behind it had
// been superseded by later runs.
//
// Everything it does to actually stop the workflow is CancelRun, unchanged and
// reused: CancelRun is the canonical lifecycle that releases branch locks
// through the branch-lock lifecycle, cancels non-terminal steps, cancels open
// and in-flight questions, cancels pending wakes so nothing resurrects the run
// later, and writes the left-running-session checkpoints. This function adds
// exactly two things on top: the cascade over child runs (a master's children
// each go through that same canonical cancellation, so no orphan child is left
// running while holding a branch), and the archive marker.
//
// Idempotent by construction. Every step is either CAS'd (the run-state
// transition, the archive marker) or best-effort-and-repeatable (lock release,
// wake cancellation), so calling it twice — or retrying a failed API request —
// converges on the same state and writes the audit checkpoint exactly once.
func (c *Coordinator) CancelAndArchiveRun(ctx stdctx.Context, runID string) (RunDetail, error) {
	store, ok := c.archiveStore()
	if !ok {
		return RunDetail{}, ErrArchiveUnsupported
	}
	run, found, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !found {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	// A child run is not a Board card and has no independent existence to
	// archive: archiving one would hide nothing and would leave its parent
	// showing a task whose run a human had quietly retired. Cancel the master.
	if run.ParentWorkflowID != nil && *run.ParentWorkflowID != "" {
		return RunDetail{}, fmt.Errorf(
			"%w: workflow run %q is a child of %q; cancel and archive the parent workflow instead",
			ErrInvalid, runID, *run.ParentWorkflowID)
	}

	// A repair run is not a card either, for the same reason and with a
	// different link: it has no parent_workflow_id, but it exists only because
	// another run stopped, and archiving it alone would leave that run showing
	// an inline repair whose own history had been retired out from under it.
	// P3-B §14: the origin is the thing to archive, and archiving it takes its
	// repairs with it.
	if link, ok := c.repairOriginLink(ctx, runID); ok && link.OriginRunID != runID {
		return RunDetail{}, fmt.Errorf(
			"%w: workflow run %q is an automatic repair of %q; cancel and archive the run it repairs instead",
			ErrInvalid, runID, link.OriginRunID)
	}

	// 1. Cascade first, parent last. A child cancelled after its parent would
	// briefly be a running child of an archived master — the exact orphan this
	// action exists to prevent — and the parent's own cancellation is what
	// stops new children from being dispatched behind us.
	children, err := store.ListChildWorkflowRuns(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	// Repairs of this run cascade with it. They are top-level rows the Board
	// only ever showed nested, so leaving one behind would resurrect it as a
	// card of its own the moment its origin left the active lane.
	children = append(children, c.repairRunsOf(ctx, run)...)
	for _, child := range children {
		if err := c.cancelForArchive(ctx, child); err != nil {
			return RunDetail{}, err
		}
	}

	// 2. The master itself, through the same canonical path.
	if err := c.cancelForArchive(ctx, run); err != nil {
		return RunDetail{}, err
	}

	// 3. The audit checkpoint, written before the marker and exactly once: a
	// run already archived has one, and re-stamping would make the history
	// read as two separate cancellations.
	if !run.Archived() {
		if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID:            "wfc-" + c.newID(),
			WorkflowRunID: runID,
			ProjectID:     run.ProjectID,
			NextAction: fmt.Sprintf(
				"cancelled and archived by request from %s; %d child run(s) cascaded. "+
					"Execution is stopped and the branch lock released; every run, step, attempt, "+
					"checkpoint and review record is retained and stays queryable via the archived view",
				run.State, len(children),
			),
			DurablePhase:   ArchivedRunCheckpointPhase,
			PayloadVersion: "v1",
			RetryState:     "{}",
			CreatedAt:      c.clock(),
		}); err != nil {
			return RunDetail{}, err
		}
	}

	// 4. The marker. Children are stamped too so the archived view of a master
	// and its fan-out is internally consistent, but only the parent is what the
	// Board ever filtered on.
	for _, child := range children {
		if _, err := store.ArchiveWorkflowRun(ctx, child.ID, c.clock()); err != nil {
			return RunDetail{}, err
		}
	}
	if _, err := store.ArchiveWorkflowRun(ctx, runID, c.clock()); err != nil {
		return RunDetail{}, err
	}

	return c.GetRun(ctx, runID)
}

// cancelForArchive runs one run through the canonical cancellation, then
// re-asserts the two guarantees that outlive it.
//
// The re-assertion is not redundancy for its own sake: a run that was ALREADY
// terminal when the user hit the button never enters CancelRun at all, and one
// that reached a terminal state through a crash-recovery path may have been
// left holding both. Both calls are idempotent — the release SQL is CAS'd on
// state='held' and wake cancellation only ever touches pending/claimed rows —
// so running them on an already-clean run costs one query and changes nothing.
func (c *Coordinator) cancelForArchive(ctx stdctx.Context, run domain.WorkflowRun) error {
	if !run.State.Terminal() {
		if _, err := c.CancelRun(ctx, run.ID); err != nil && !errors.Is(err, ErrAlreadyTerminal) {
			return err
		}
	}
	c.releaseBranchLocks(ctx, run.ID, "workflow run cancelled and archived")
	if c.wakeScheduler != nil {
		if _, err := c.wakeScheduler.CancelAllForRun(ctx, domain.WorkflowRunID(run.ID)); err != nil && c.log != nil {
			c.log.Warn("workflow: archive wake cancellation failed", "run", run.ID, "err", err)
		}
	}
	return nil
}

// ProjectBoardHistory projects a project's archived runs — the "Mostrar
// archivados" view. Same BoardEntry shape as the active Board, so the history
// list renders with the same card and the same derived vocabulary rather than a
// second, divergent projection of the same rows.
func (c *Coordinator) ProjectBoardHistory(ctx stdctx.Context, projectID string, limit int) ([]BoardEntry, error) {
	store, ok := c.archiveStore()
	if !ok {
		return nil, ErrArchiveUnsupported
	}
	runs, err := store.ListArchivedWorkflowRuns(ctx, projectID, limit)
	if err != nil {
		return nil, err
	}
	entries := make([]BoardEntry, 0, len(runs))
	for _, run := range runs {
		entry, err := c.boardEntry(ctx, run)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// repairRunsOf lists the automatic repair runs created for this run.
//
// Read from the run's OWN ledger — the repair dispatch intents it recorded
// before each repair started — rather than by scanning the project for runs
// that claim it. The intent is written first and is the durable link; a run
// that claims an origin the origin does not claim back is exactly the
// provenance the cession chain refuses to trust, and archiving is not the place
// to start trusting it.
func (c *Coordinator) repairRunsOf(ctx stdctx.Context, run domain.WorkflowRun) []domain.WorkflowRun {
	var out []domain.WorkflowRun
	seen := map[string]bool{}
	for _, intent := range c.repairIntents(ctx, run.ID) {
		if intent.RepairRunID == "" || seen[intent.RepairRunID] {
			continue
		}
		seen[intent.RepairRunID] = true
		repair, found, err := c.store.GetWorkflowRun(ctx, intent.RepairRunID)
		if err != nil || !found {
			continue
		}
		out = append(out, repair)
	}
	return out
}
