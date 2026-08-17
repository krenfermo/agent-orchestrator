package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Checkpoint 8M.1: master task git state propagation.
//
// dispatchMasterTask (master_coordinator.go) already gives task N+1 a
// text-only SessionContextPack recap of its completed dependencies, but its
// worker session got a brand-new worktree branched from the project's
// default branch — task N's actual file changes were physically absent.
// This file closes that gap: once a task passes the SAME gate that already
// completes its execution run (review approved-or-skipped, verify passed,
// fingerprint stable, no open question — see verify.go's maybeVerify), its
// worktree content is captured into an AO-owned internal git commit under
// refs/ao/workflows/<masterRunID>/integration, and every subsequent task's
// worktree is based on that ref instead of the project's default branch
// (see attemptWorkHarness in dispatch.go). No new table: promotions are
// recorded as one more DurablePhase on the existing append-only
// workflow_checkpoints ledger, the same convention
// persistSessionLifecycleDecision (session_context_pack.go) already uses.
//
// V1 linearization: reconcileMasterTasks's dispatch loop already only ever
// starts one eligible task at a time regardless of the declared dependency
// graph shape (master_coordinator.go's loop returns immediately after the
// first dispatchMasterTask call). Integration state matches that exact
// execution order: one single integration ref per master run, advancing
// monotonically in dispatch order. Every task after the first is based on
// the integration state containing ALL previously completed tasks, not only
// its own declared Dependencies — an explicit, documented V1 simplification,
// not a silent DAG violation, since execution was already fully sequential
// before this checkpoint.
const (
	masterIntegrationDurablePhase        = "master_integration_promotion"
	masterIntegrationFailureDurablePhase = "master_integration_promotion_failed"
	masterIntegrationPayloadVersion      = "v1"
)

// MasterIntegrationState is derived (never stored directly) by folding every
// master_integration_promotion checkpoint recorded for a master run, in
// order. CompletedTaskIDs is the natural-key idempotency ledger: a task
// whose ID already appears here has already been promoted and must never be
// materialized again.
type MasterIntegrationState struct {
	WorkflowRunID    string
	RefName          string
	BaseSHA          string
	CurrentSHA       string
	CompletedTaskIDs []string
	LastErrorTaskID  string
	LastErrorClass   string
	LastErrorReason  string
}

// masterIntegrationPromotionPayload is master_integration_promotion's
// RetryState JSON payload — the same field-repurposing convention
// sessionLifecycleRecord already uses for session_lifecycle_decision.
type masterIntegrationPromotionPayload struct {
	TaskID  string `json:"taskId"`
	TreeSHA string `json:"treeSha"`
	RefName string `json:"refName"`
	Reused  bool   `json:"reused"`
}

// masterIntegrationFailurePayload is master_integration_promotion_failed's
// RetryState JSON payload.
type masterIntegrationFailurePayload struct {
	TaskID     string `json:"taskId"`
	ErrorClass string `json:"errorClass"`
	Reason     string `json:"reason"`
}

// masterTaskBaseRef returns the integration ref a master child task's worker
// worktree should be based on (Checkpoint 8M.1 §8), or "" to keep the
// existing project-default-branch behavior. Empty whenever run isn't a
// master child task, or is the first task in its master run (nothing has
// been promoted to integration state yet, so there is nothing to base on
// beyond the project default). Read-only, best-effort: any lookup error
// falls back to "" rather than blocking dispatch — a missing integration
// state means no dependency's code needs to reach this worker yet.
func (c *Coordinator) masterTaskBaseRef(ctx stdctx.Context, run domain.WorkflowRun) string {
	if run.ParentWorkflowID == nil {
		return ""
	}
	state, err := c.getMasterIntegrationState(ctx, *run.ParentWorkflowID)
	if err != nil || state.CurrentSHA == "" {
		return ""
	}
	return state.RefName
}

func masterIntegrationRefName(masterRunID string) string {
	return "refs/ao/workflows/" + masterRunID + "/integration"
}

// getMasterIntegrationState folds the master run's checkpoint ledger into
// the current integration state. Read-only, no side effects — safe to call
// as often as needed (e.g. once per reconcile pass).
func (c *Coordinator) getMasterIntegrationState(ctx stdctx.Context, masterRunID string) (MasterIntegrationState, error) {
	state := MasterIntegrationState{WorkflowRunID: masterRunID, RefName: masterIntegrationRefName(masterRunID)}
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, masterRunID)
	if err != nil {
		return state, err
	}
	for _, cp := range checkpoints {
		switch cp.DurablePhase {
		case masterIntegrationDurablePhase:
			var payload masterIntegrationPromotionPayload
			if json.Unmarshal([]byte(cp.RetryState), &payload) != nil {
				continue
			}
			if state.BaseSHA == "" {
				state.BaseSHA = cp.BaseSHA
			}
			state.CurrentSHA = cp.HeadSHA
			state.CompletedTaskIDs = append(state.CompletedTaskIDs, payload.TaskID)
			state.LastErrorTaskID, state.LastErrorClass, state.LastErrorReason = "", "", ""
		case masterIntegrationFailureDurablePhase:
			var payload masterIntegrationFailurePayload
			if json.Unmarshal([]byte(cp.RetryState), &payload) != nil {
				continue
			}
			state.LastErrorTaskID = payload.TaskID
			state.LastErrorClass = payload.ErrorClass
			state.LastErrorReason = payload.Reason
		}
	}
	return state, nil
}

// promoteTaskToIntegration materializes task's verified worktree content into
// the master run's integration ref, once task has reached the SAME
// completion gate maybeVerify already enforces for its own execution run
// (review approved-or-skipped, verify passed, fingerprint stable, no open
// question) — reconcileMasterTasks only calls this immediately before
// marking the task Completed, so that gate is guaranteed to already hold.
//
// Idempotent: if task.ID is already in the integration state's
// CompletedTaskIDs, this is a no-op. If a previous attempt updated the git
// ref but crashed before this checkpoint write landed, MaterializeIntegrationCommit
// itself is idempotent (write-tree is content-deterministic; identical
// content reuses the existing commit rather than creating a new one), so a
// retry here is always safe.
func (c *Coordinator) promoteTaskToIntegration(ctx stdctx.Context, parent domain.WorkflowRun, task domain.WorkflowTask, child RunDetail) error {
	if c.workspaceFacts == nil || c.projects == nil {
		return errors.New("workflow: integration promotion requires workspaceFacts and projects")
	}
	state, err := c.getMasterIntegrationState(ctx, parent.ID)
	if err != nil {
		return err
	}
	for _, id := range state.CompletedTaskIDs {
		if id == task.ID {
			return nil
		}
	}

	project, ok, err := c.projects.GetProject(ctx, parent.ProjectID)
	if err != nil {
		return err
	}
	if !ok {
		return c.recordIntegrationFailure(ctx, parent, task, "project not found")
	}
	if project.Kind.WithDefault() == domain.ProjectKindWorkspace {
		// V1 scope guard (Checkpoint 8M.1 §8): multi-repo workspace-kind
		// projects are explicitly out of scope, not silently degraded.
		return c.recordIntegrationFailure(ctx, parent, task, "integration_unsupported_project_kind: multi-repo workspace projects are not supported in 8M.1")
	}

	var workStepID string
	for _, s := range child.Steps {
		if s.Step.Kind == domain.WorkflowStepWork {
			workStepID = s.Step.ID
			break
		}
	}
	if workStepID == "" {
		return c.recordIntegrationFailure(ctx, parent, task, "execution run has no work step")
	}
	workCP, hasCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStepID)
	if err != nil {
		return err
	}
	if !hasCP || workCP.WorktreePath == "" || workCP.SessionID == nil {
		return c.recordIntegrationFailure(ctx, parent, task, "worktree/session facts are missing")
	}

	message := fmt.Sprintf("AO internal integration checkpoint: task %s (%s)", task.ID, task.Title)
	commitSHA, treeSHA, reused, err := c.workspaceFacts.MaterializeIntegrationCommit(ctx,
		ports.WorkspaceInfo{Path: workCP.WorktreePath, Branch: workCP.Branch, SessionID: domain.SessionID(*workCP.SessionID), ProjectID: domain.ProjectID(parent.ProjectID)},
		state.RefName, state.CurrentSHA, message, EphemeralArtifactExcludePatterns())
	if err != nil {
		return c.recordIntegrationFailure(ctx, parent, task, err.Error())
	}

	payload, _ := json.Marshal(masterIntegrationPromotionPayload{TaskID: task.ID, TreeSHA: treeSHA, RefName: state.RefName, Reused: reused})
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  parent.ID,
		ProjectID:      parent.ProjectID,
		SessionID:      workCP.SessionID,
		BaseSHA:        state.CurrentSHA,
		HeadSHA:        commitSHA,
		RetryState:     string(payload),
		DurablePhase:   masterIntegrationDurablePhase,
		PayloadVersion: masterIntegrationPayloadVersion,
		CreatedAt:      c.clock(),
	})
	return err
}

// recordIntegrationFailure persists a diagnostic checkpoint (best-effort —
// a failure to record the failure itself is swallowed, never masking the
// original error) and returns a non-nil error so the caller never marks the
// task completed or advances the integration state.
func (c *Coordinator) recordIntegrationFailure(ctx stdctx.Context, parent domain.WorkflowRun, task domain.WorkflowTask, reason string) error {
	payload, _ := json.Marshal(masterIntegrationFailurePayload{TaskID: task.ID, ErrorClass: string(domain.WorkflowErrorIntegrationFailed), Reason: reason})
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  parent.ID,
		ProjectID:      parent.ProjectID,
		RetryState:     string(payload),
		DurablePhase:   masterIntegrationFailureDurablePhase,
		PayloadVersion: masterIntegrationPayloadVersion,
		CreatedAt:      c.clock(),
	})
	return fmt.Errorf("%w: %s", errIntegrationFailed, reason)
}

var errIntegrationFailed = errors.New("workflow: integration promotion failed")

// MasterIntegrationSummary is RunDetail's live-derived surface of the
// integration state, following the same read-live-at-GetRun-time pattern
// StepDetail.ReviewPolicy/Routing already use.
type MasterIntegrationSummary struct {
	RefName         string
	CurrentSHA      string
	TasksIntegrated int
	LatestTaskID    string
	Status          string // "ok" | "integration_failed" | "none"
	ErrorClass      string
}

// buildIntegrationSummary derives MasterIntegrationSummary for a master run.
// Only meaningful for runs with a plan (getMasterRun's caller guards this).
func (c *Coordinator) buildIntegrationSummary(ctx stdctx.Context, masterRunID string) (*MasterIntegrationSummary, error) {
	state, err := c.getMasterIntegrationState(ctx, masterRunID)
	if err != nil {
		return nil, err
	}
	summary := &MasterIntegrationSummary{
		RefName:         state.RefName,
		CurrentSHA:      state.CurrentSHA,
		TasksIntegrated: len(state.CompletedTaskIDs),
		Status:          "none",
	}
	if len(state.CompletedTaskIDs) > 0 {
		summary.LatestTaskID = state.CompletedTaskIDs[len(state.CompletedTaskIDs)-1]
		summary.Status = "ok"
	}
	if state.LastErrorClass != "" {
		summary.Status = "integration_failed"
		summary.ErrorClass = state.LastErrorClass
	}
	return summary, nil
}
