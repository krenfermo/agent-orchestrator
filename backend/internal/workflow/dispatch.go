package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Spawner is the narrow session-creation write path workflow reuses. It is
// satisfied by *session_manager.Manager; workflow never constructs sessions
// itself. Prompt delivery happens inside Spawn itself (per cfg.Prompt), so
// workflow never calls a separate send-message path for the initial task
// prompt.
type Spawner interface {
	Spawn(ctx stdctx.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error)
}

// SessionFacts is the narrow read path workflow uses to observe worker
// progress and detect an already-spawned session by natural key, without
// ever writing to sessions.
type SessionFacts interface {
	GetSession(ctx stdctx.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	FindSessionByProjectAndIssueID(ctx stdctx.Context, projectID domain.ProjectID, issueID domain.IssueID) (domain.SessionRecord, bool, error)
}

// WorkspaceFacts is the narrow read path workflow uses to observe live
// worktree state (current HEAD SHA, dirty state) without caching staleness.
type WorkspaceFacts interface {
	ObserveWorkspace(ctx stdctx.Context, info ports.WorkspaceInfo) (ports.WorkspaceObservation, error)
}

// workStepIssueID is the durable natural key correlating a workflow work
// step to the session spawned for it, independent of whether workflow's own
// outbox/checkpoint bookkeeping made it to disk.
func workStepIssueID(stepID string) domain.IssueID {
	return domain.IssueID("workflow-step:" + stepID)
}

// workStepOutboxIdempotencyKey is the deterministic idempotency key for a
// work step's spawn command, derived purely from the step id.
func workStepOutboxIdempotencyKey(stepID string) string {
	return "workflow-step-spawn:" + stepID
}

func spawnPayloadJSON(projectID, stepID string) string {
	b, _ := json.Marshal(map[string]string{
		"projectId": projectID,
		"harness":   "codex",
		"issueId":   string(workStepIssueID(stepID)),
	})
	return string(b)
}

const workDisplayNameMaxLen = 40

func workDisplayName(objective string) string {
	name := objective
	if len(name) > workDisplayNameMaxLen {
		name = name[:workDisplayNameMaxLen]
	}
	return "wf: " + name
}

// dispatchWorkStep is the single idempotent dispatch algorithm for a work
// step's Codex worker spawn (Checkpoint 8B §5). It is safe to call
// repeatedly — from StartRun, from boot recovery, and opportunistically from
// GetRun — without ever calling Spawner.Spawn more than once for the same
// step across the union of all call sites.
func (c *Coordinator) dispatchWorkStep(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, prompt string) (domain.WorkflowStep, error) {
	// Cancellation (or any other terminal transition) must never race with a
	// late-arriving dispatch.
	if run.State.Terminal() || step.State.Terminal() {
		return step, nil
	}
	// Primary, cheapest guard: a session is already durably associated.
	if step.SessionID != nil {
		return step, nil
	}
	if c.spawner == nil {
		// No spawner wired (e.g. a unit test exercising only the durable
		// foundation). Nothing to dispatch.
		return step, nil
	}

	now := c.clock()
	entry, _, err := c.store.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID:             "wfo-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &step.ID,
		IdempotencyKey: workStepOutboxIdempotencyKey(step.ID),
		CommandType:    domain.WorkflowOutboxSpawnWorkerSession,
		Payload:        spawnPayloadJSON(run.ProjectID, step.ID),
		CreatedAt:      now,
	})
	if err != nil {
		return step, err
	}

	switch entry.Status {
	case domain.WorkflowOutboxPending:
		return c.dispatchFromPending(ctx, run, step, entry, prompt)
	case domain.WorkflowOutboxDispatched, domain.WorkflowOutboxAcknowledged:
		// Dispatched: a previous attempt got at least as far as "about to call
		// Spawn," but we don't durably know if Spawn itself completed.
		// Acknowledged should be unreachable given the SessionID guard above,
		// but if somehow reached, idempotent re-adoption is correct.
		return c.adoptOrMarkAmbiguous(ctx, run, step, entry)
	case domain.WorkflowOutboxFailed:
		// Already durably recorded as failed; no auto-retry in 8B.
		return step, nil
	default:
		return step, nil
	}
}

func (c *Coordinator) dispatchFromPending(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, entry domain.WorkflowOutboxEntry, prompt string) (domain.WorkflowStep, error) {
	now := c.clock()
	if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, domain.WorkflowOutboxPending, domain.WorkflowOutboxDispatched, now, ""); err != nil {
		return step, err
	}
	// Keep the in-memory entry in sync with the CAS update above: the success/
	// failure recorders further down use entry.Status as the *expected* value
	// for their own CAS call, and a stale "pending" here would silently no-op
	// against the DB row (which is genuinely already "dispatched"), leaving
	// the outbox permanently stuck instead of advancing to acknowledged/failed.
	entry.Status = domain.WorkflowOutboxDispatched
	if step.State == domain.WorkflowStepReady {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, domain.WorkflowStepReady, domain.WorkflowStepRunning, now); err != nil {
			return step, err
		}
		step.State = domain.WorkflowStepRunning
	}

	rec, _, _, err := c.spawner.Spawn(ctx, ports.SpawnConfig{
		ProjectID:   domain.ProjectID(run.ProjectID),
		Kind:        domain.KindWorker,
		Harness:     domain.AgentHarness("codex"),
		IssueID:     workStepIssueID(step.ID),
		Prompt:      prompt,
		DisplayName: workDisplayName(run.Objective),
	})
	if err != nil {
		// Cannot distinguish from here whether the failure happened before or
		// at session-row creation vs. after (agent launch); session_create_failed
		// is the safer default per the 8B spec.
		return c.recordDispatchFailure(ctx, run, step, entry, domain.WorkflowErrorSessionCreateFailed, err)
	}
	return c.recordDispatchSuccess(ctx, run, step, entry, rec)
}

// adoptOrMarkAmbiguous handles a retry/recovery call that found the outbox
// entry already dispatched (or, defensively, acknowledged). It never calls
// Spawn again. A natural-key match with a populated workspace is real
// evidence and is adopted; anything else is surfaced as ambiguous rather
// than silently resolved ("nunca asumir éxito").
func (c *Coordinator) adoptOrMarkAmbiguous(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, entry domain.WorkflowOutboxEntry) (domain.WorkflowStep, error) {
	if c.sessionFacts == nil {
		return step, nil
	}
	rec, found, err := c.sessionFacts.FindSessionByProjectAndIssueID(ctx, domain.ProjectID(run.ProjectID), workStepIssueID(step.ID))
	if err != nil {
		return step, err
	}
	if found && (rec.Metadata.WorkspacePath != "" || rec.Metadata.Branch != "") {
		return c.recordDispatchSuccess(ctx, run, step, entry, rec)
	}
	// Session creation persists the natural-key row before workspace
	// provisioning finishes. A concurrent GetRun/reconcile can therefore see a
	// freshly dispatched command plus a real but not-yet-populated session. Give
	// that in-flight provisioning window time to settle; old/unknown dispatched
	// commands still take the conservative ambiguous path below.
	if entry.DispatchedAt != nil && c.clock().Sub(*entry.DispatchedAt) < 30*time.Second {
		return step, nil
	}

	now := c.clock()
	nextAction := "ambiguous_worker_state: no session found for dispatched command"
	if found {
		nextAction = fmt.Sprintf("ambiguous_worker_state: orphaned session %s with no workspace after restart", rec.ID)
	}
	if step.State == domain.WorkflowStepRunning {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, domain.WorkflowStepRunning, domain.WorkflowStepWaiting, now); err != nil {
			return step, err
		}
		step.State = domain.WorkflowStepWaiting
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return step, err
		}
	}
	stepID := step.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction:     nextAction,
		DurablePhase:   "worker_dispatch_ambiguous",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		return step, err
	}
	// Outbox intentionally stays "dispatched": a human/future checkpoint
	// decides whether to clean up the orphan and retry.
	return step, nil
}

func (c *Coordinator) recordDispatchSuccess(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, entry domain.WorkflowOutboxEntry, rec domain.SessionRecord) (domain.WorkflowStep, error) {
	now := c.clock()

	attempts, err := c.store.ListWorkflowAttempts(ctx, step.ID)
	if err != nil {
		return step, err
	}
	if len(attempts) == 0 {
		if _, err := c.store.CreateWorkflowAttempt(ctx, "wfa-"+c.newID(), step.ID, string(rec.Harness), "", now); err != nil {
			return step, err
		}
	}

	if _, err := c.store.UpdateWorkflowStepSession(ctx, step.ID, string(rec.ID), now); err != nil {
		return step, err
	}
	sid := string(rec.ID)
	step.SessionID = &sid

	if entry.Status != domain.WorkflowOutboxAcknowledged {
		if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxAcknowledged, now, ""); err != nil {
			return step, err
		}
	}

	baseSHA := ""
	branch := rec.Metadata.Branch
	worktree := rec.Metadata.WorkspacePath
	if c.workspaceFacts != nil && worktree != "" {
		if obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
			Path: worktree, Branch: branch, SessionID: rec.ID, ProjectID: rec.ProjectID,
		}); err == nil {
			baseSHA = obs.HeadSHA
		}
	}
	stepID := step.ID
	sessID := string(rec.ID)
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		SessionID:      &sessID,
		Branch:         branch,
		WorktreePath:   worktree,
		BaseSHA:        baseSHA,
		DurablePhase:   "worker_dispatched",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		return step, err
	}
	return step, nil
}

func (c *Coordinator) recordDispatchFailure(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, entry domain.WorkflowOutboxEntry, errClass domain.WorkflowErrorClass, cause error) (domain.WorkflowStep, error) {
	now := c.clock()
	if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxFailed, now, string(errClass)); err != nil {
		return step, err
	}
	attempts, err := c.store.ListWorkflowAttempts(ctx, step.ID)
	if err != nil {
		return step, err
	}
	if len(attempts) == 0 {
		attempt, err := c.store.CreateWorkflowAttempt(ctx, "wfa-"+c.newID(), step.ID, "codex", "", now)
		if err != nil {
			return step, err
		}
		if err := c.store.UpdateWorkflowAttemptOutcome(ctx, attempt.ID, now, domain.WorkflowAttemptFailed, errClass); err != nil {
			return step, err
		}
	}
	if step.State == domain.WorkflowStepRunning {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, domain.WorkflowStepRunning, domain.WorkflowStepFailed, now); err != nil {
			return step, err
		}
		step.State = domain.WorkflowStepFailed
	}
	if run.State == domain.WorkflowRunRunning {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, domain.WorkflowRunRunning, domain.WorkflowRunNeedsAttention, now); err != nil {
			return step, err
		}
	}
	if c.log != nil {
		c.log.Warn("workflow: work step dispatch failed", "step", step.ID, "err", cause)
	}
	return step, nil
}
