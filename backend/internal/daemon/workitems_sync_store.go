package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// workitems_sync_store.go — the ONE producer of external work-item sync
// intents (P4-E §1).
//
// WHERE THIS SITS, AND WHY THERE. AO's canonical authority for "a run changed
// state" is the compare-and-swap in the store: UpdateWorkflowRunState returns
// whether the row actually moved, and every one of the two dozen call sites in
// internal/workflow goes through it. The same is true of workflow tasks.
//
// So the sync intent is emitted HERE, decorating that CAS, rather than at the
// call sites. Three properties follow from that placement and are the whole
// reason for it:
//
//   - **One producer.** attention_notify.go documents the alternative and why
//     it was rejected for notifications: "the state write is scattered across
//     two dozen call sites". A new call site cannot forget to emit, because
//     there is nothing at a call site to forget.
//   - **Exactly once per real transition.** The emit is behind `moved`. A
//     retry, a second observer, a reconcile pass that re-derives the same
//     conclusion, and a restart that re-runs a convergence check all see
//     moved == false and emit nothing. That is the same fence the completion
//     notification already rides on (completion_notify.go).
//   - **The workflow package stays unaware.** internal/workflow does not
//     import internal/service/workitems and does not know Plane exists. The
//     dependency points from the composition root inward, which is what keeps
//     "AO remains canonical" a structural fact rather than a convention.
//
// ORDERING IS THE §4 REQUIREMENT, and it is satisfied by construction: the CAS
// has already committed before the emit is reached. AO's transition is durable
// first; the sync intent is a durable row written after it; the worker
// delivers later. Nothing here can fail an AO transition — every path returns
// the CAS's own result, and the emit's own error is logged and dropped.
//
// A NIL SYNC MAKES THIS A PASS-THROUGH. A daemon built without the integration
// wraps nothing (see maybeSyncingStore), so an installation that does not use
// it pays not even an interface indirection.

// workItemSyncer is the slice of the work-items service this decorator needs.
// Narrow on purpose: the store may ANNOUNCE a transition and can do nothing
// else with the integration.
type workItemSyncer interface {
	EnqueueRunState(ctx context.Context, projectID domain.ProjectID, runID string, state domain.WorkflowRunState, detail string) error
	EnqueueTaskState(ctx context.Context, projectID domain.ProjectID, taskID string, state domain.WorkflowTaskState, detail string) error
}

// syncingStore is *sqlite.Store with the two lifecycle CAS methods decorated.
//
// It embeds the CONCRETE store rather than an interface, and that is
// load-bearing: workflow.Coordinator obtains its plan store by type-asserting
// its Store to an unexported masterPlanStore interface
// (`d.Store.(masterPlanStore)`). Embedding the concrete type promotes every
// method, so the assertion still succeeds through the decorator. Embedding a
// narrower interface would make the assertion fail and silently leave the
// master coordinator with no plan store at all — a failure that would not
// announce itself.
type syncingStore struct {
	*sqlite.Store
	sync workItemSyncer
	log  *slog.Logger
}

// maybeSyncingStore wraps the store only when there is something to emit to.
func maybeSyncingStore(store *sqlite.Store, sync workItemSyncer, log *slog.Logger) *syncingStore {
	if store == nil || sync == nil {
		return nil
	}
	return &syncingStore{Store: store, sync: sync, log: log}
}

// UpdateWorkflowRunState performs the canonical CAS, then announces it.
//
// The announcement is strictly after the durable transition and strictly
// conditional on it. An error from the announcement is logged and discarded:
// the run really did change state, and reporting a failure here would turn a
// planning-tool outage into a workflow failure — the exact inversion §4
// forbids.
func (s *syncingStore) UpdateWorkflowRunState(
	ctx context.Context, id string, expected, next domain.WorkflowRunState, now time.Time,
) (bool, error) {
	moved, err := s.Store.UpdateWorkflowRunState(ctx, id, expected, next, now)
	if err != nil || !moved {
		return moved, err
	}
	s.emitRun(ctx, id, next)
	return true, nil
}

// UpdateWorkflowTaskState performs the canonical task CAS, then announces it.
func (s *syncingStore) UpdateWorkflowTaskState(
	ctx context.Context, id string, expected, next domain.WorkflowTaskState, now time.Time,
) (bool, error) {
	moved, err := s.Store.UpdateWorkflowTaskState(ctx, id, expected, next, now)
	if err != nil || !moved {
		return moved, err
	}
	s.emitTask(ctx, id, next)
	return true, nil
}

// ParkWorkflowTaskForAttention is the OTHER way a task reaches
// needs_attention: a dedicated conditional write rather than the general CAS,
// so decorating only UpdateWorkflowTaskState would miss every parked task.
func (s *syncingStore) ParkWorkflowTaskForAttention(
	ctx context.Context, id string, expected domain.WorkflowTaskState, expectedAttempt int,
	reason string, attention domain.WorkflowTaskAttention, now time.Time,
) (bool, error) {
	parked, err := s.Store.ParkWorkflowTaskForAttention(ctx, id, expected, expectedAttempt, reason, attention, now)
	if err != nil || !parked {
		return parked, err
	}
	s.emitTask(ctx, id, domain.WorkflowTaskNeedsAttention)
	return true, nil
}

// emitRun resolves the run's project and enqueues the intent.
//
// The project read is the one cost this decorator adds to a transition, and it
// is paid only on a real move. It is deliberately not cached: a stale project
// id would attach a sync to the wrong project, which is the one mistake here
// that could cross a tenant boundary.
func (s *syncingStore) emitRun(ctx context.Context, runID string, next domain.WorkflowRunState) {
	// Nothing to say about this state? Then not even the project read happens.
	if _, ok := domain.WorkItemSyncEventForRun(next); !ok {
		return
	}
	run, found, err := s.GetWorkflowRun(ctx, runID)
	if err != nil || !found {
		return
	}
	detail := runSyncDetail(run, next)
	if err := s.sync.EnqueueRunState(ctx, domain.ProjectID(run.ProjectID), runID, next, detail); err != nil && s.log != nil {
		s.log.Debug("work items: could not enqueue a run sync",
			"run", runID, "state", next, "err", err)
	}
}

// emitTask resolves the task's project through its run and enqueues the intent.
func (s *syncingStore) emitTask(ctx context.Context, taskID string, next domain.WorkflowTaskState) {
	if _, ok := domain.WorkItemSyncEventForTask(next); !ok {
		return
	}
	task, found, err := s.GetWorkflowTask(ctx, taskID)
	if err != nil || !found {
		return
	}
	// A task's tenancy is its run's project's tenancy. Reading it here rather
	// than trusting a caller is the same rule domain.AuthzResource follows for
	// projects: the authority is a durable row, never an argument.
	run, found, err := s.GetWorkflowRun(ctx, task.WorkflowRunID)
	if err != nil || !found {
		return
	}
	detail := taskSyncDetail(task, next)
	if err := s.sync.EnqueueTaskState(ctx, domain.ProjectID(run.ProjectID), taskID, next, detail); err != nil && s.log != nil {
		s.log.Debug("work items: could not enqueue a task sync",
			"task", taskID, "state", next, "err", err)
	}
}

// runSyncDetail is the one line that travels into the external comment.
//
// It is derived from durable facts the store already has — the objective, the
// state — and never from terminal output, a transcript or an error chain. The
// service bounds it again before posting; this keeps it meaningful as well as
// short.
func runSyncDetail(run domain.WorkflowRun, next domain.WorkflowRunState) string {
	objective := firstLineOf(run.Objective, 200)
	switch next {
	case domain.WorkflowRunRunning:
		return objective
	case domain.WorkflowRunCompleted:
		return objective
	case domain.WorkflowRunNeedsAttention:
		// The stop REASON lives on a checkpoint, not on the run row, and
		// reading it here would make a state write depend on a second query
		// whose absence is normal. The objective is what a person reading a
		// planning board needs to recognise which work stopped; the reason is
		// one click away in AO.
		return objective
	default:
		return objective
	}
}

func taskSyncDetail(task domain.WorkflowTask, next domain.WorkflowTaskState) string {
	title := firstLineOf(task.Title, 200)
	if next == domain.WorkflowTaskNeedsAttention && task.AttentionReason != "" {
		// A parked task DOES carry its reason on its own row, so it costs
		// nothing to say why.
		return title + " — " + firstLineOf(task.AttentionReason, 120)
	}
	return title
}

// firstLineOf bounds a durable string to one line. Belt and braces with the
// service's own bound: a value that arrives with a newline is a summary
// somebody pasted, and neither layer should be the only one that notices.
func firstLineOf(s string, limit int) string {
	for i, r := range s {
		if r == '\n' || r == '\r' {
			s = s[:i]
			break
		}
	}
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}
