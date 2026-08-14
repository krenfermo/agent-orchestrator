package workflow

import (
	stdctx "context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// observationThrottle bounds how often observeWorkStep pays for the
// expensive ObserveWorkspace git shell-out. Session fact lookups (a DB read)
// are cheap enough to always do; only the live git observation is throttled,
// keyed off the work step's latest checkpoint timestamp.
const observationThrottle = 3 * time.Second

// WorkerProgress is a workflow-internal interpretation label for a work
// step's Codex worker. It exists purely to make the conservative completion
// rule below legible and testable; it is never persisted (no sessions column,
// no new DB table). The resulting WorkflowStepState/WorkflowRunState and
// checkpoint next_action text are what gets stored.
type WorkerProgress string

const (
	WorkerCreated         WorkerProgress = "worker_created"
	WorkerStarted         WorkerProgress = "worker_started"
	WorkerActive          WorkerProgress = "worker_active"
	WorkerIdle            WorkerProgress = "worker_idle"
	WorkerTerminated      WorkerProgress = "worker_terminated"
	WorkerFailed          WorkerProgress = "worker_failed"
	WorkerResultAvailable WorkerProgress = "worker_result_available"
)

// WorkStepDecision is the outcome of evaluating a work step's session/
// workspace facts: what to do next, expressed independent of persistence.
type WorkStepDecision struct {
	Progress   WorkerProgress
	NextStep   domain.WorkflowStepState
	NextRun    domain.WorkflowRunState
	NextAction string
	ErrorClass domain.WorkflowErrorClass
	// NoChange is true when the facts do not yet justify any transition (e.g.
	// still actively working, or workspace evidence was throttled/unavailable
	// this call). The caller must leave step/run state untouched.
	NoChange bool
}

// evaluateWorkStepProgress implements the conservative, fact-only work-step
// completion rule (Checkpoint 8B §8). It never trusts agent transcript text;
// only IsTerminated, ActivityState, and (when available) a live workspace
// observation's HeadSHA vs. the checkpoint's recorded BaseSHA are evidence.
//
// workspaceAvailable is false when ObserveWorkspace was throttled/skipped
// this call (see observationThrottle) or otherwise could not be obtained;
// obs is only read when workspaceAvailable is true.
func evaluateWorkStepProgress(
	sessionFound bool,
	session domain.SessionRecord,
	workspaceAvailable bool,
	obs ports.WorkspaceObservation,
	baseSHA string,
) WorkStepDecision {
	// hasWorkEvidence checks the AO guardrail prompt explicitly tells the
	// worker not to commit/push/merge (Checkpoint 8B §4), so real, verifiable
	// work often lands as uncommitted worktree changes rather than a new
	// commit. A HEAD SHA that differs from the checkpointed base SHA is
	// sufficient evidence on its own, but it is not necessary: an actually
	// dirty/staged/untracked worktree (per the live git status observation,
	// never the agent's own words) is equally real, git-verified evidence
	// that concrete work happened.
	hasWorkEvidence := func() bool {
		if !workspaceAvailable {
			return false
		}
		if obs.HeadSHA != "" && obs.HeadSHA != baseSHA {
			return true
		}
		return obs.Dirty || obs.Staged || obs.Untracked || len(obs.Changes) > 0
	}

	terminatedOrExited := !sessionFound || session.IsTerminated || session.Activity.State == domain.ActivityExited

	if terminatedOrExited {
		if hasWorkEvidence() {
			return WorkStepDecision{
				Progress:   WorkerResultAvailable,
				NextStep:   domain.WorkflowStepCompleted,
				NextRun:    domain.WorkflowRunWaiting,
				NextAction: "start_review",
			}
		}
		return WorkStepDecision{
			Progress:   WorkerFailed,
			NextStep:   domain.WorkflowStepFailed,
			NextRun:    domain.WorkflowRunNeedsAttention,
			NextAction: "worker session terminated with no verifiable work (no commit, dirty, staged, or untracked change)",
			ErrorClass: domain.WorkflowErrorWorkerTerminatedUnexpectedly,
		}
	}

	switch session.Activity.State {
	case domain.ActivityActive:
		return WorkStepDecision{Progress: WorkerActive, NoChange: true}
	case domain.ActivityWaitingInput, domain.ActivityBlocked:
		return WorkStepDecision{
			Progress:   WorkerActive,
			NextStep:   domain.WorkflowStepWaiting,
			NextRun:    domain.WorkflowRunNeedsAttention,
			NextAction: "worker awaiting input/blocked — needs human attention",
		}
	case domain.ActivityIdle:
		if !workspaceAvailable {
			// Insufficient fresh evidence this call; wait for a future call
			// once the throttle window has elapsed. Not an error.
			return WorkStepDecision{Progress: WorkerIdle, NoChange: true}
		}
		if hasWorkEvidence() {
			return WorkStepDecision{
				Progress:   WorkerResultAvailable,
				NextStep:   domain.WorkflowStepCompleted,
				NextRun:    domain.WorkflowRunWaiting,
				NextAction: "start_review",
			}
		}
		return WorkStepDecision{
			Progress:   WorkerIdle,
			NextStep:   domain.WorkflowStepWaiting,
			NextRun:    domain.WorkflowRunNeedsAttention,
			NextAction: "worker idle with no verifiable change — needs human review",
			ErrorClass: domain.WorkflowErrorAmbiguousWorkerState,
		}
	default:
		// Unknown/unspecified activity: make no change rather than guess.
		return WorkStepDecision{Progress: WorkerCreated, NoChange: true}
	}
}

// observeWorkStep is the single fact-based work-step evaluation function
// used both by GetRun (opportunistic observation, throttled) and by boot
// recovery (Reconcile). It never trusts agent transcript text: only session
// facts (IsTerminated/ActivityState) and, when fresh enough, a live
// ObserveWorkspace call are evidence. All resulting transitions go through
// ValidWorkflowStepTransition/ValidWorkflowRunTransition; an invalid target
// (a benign race) is skipped rather than erroring the read path.
func (c *Coordinator) observeWorkStep(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) (domain.WorkflowStep, error) {
	if step.Kind != domain.WorkflowStepWork || step.State != domain.WorkflowStepRunning {
		return step, nil
	}
	if c.sessionFacts == nil || step.SessionID == nil {
		return step, nil
	}
	now := c.clock()

	sess, found, err := c.sessionFacts.GetSession(ctx, domain.SessionID(*step.SessionID))
	if err != nil {
		return step, err
	}

	latestCP, hasCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID)
	if err != nil {
		return step, err
	}
	baseSHA := ""
	if hasCP {
		baseSHA = latestCP.BaseSHA
	}

	workspaceAvailable := false
	var obs ports.WorkspaceObservation
	if c.workspaceFacts != nil && found && sess.Metadata.WorkspacePath != "" {
		stale := true
		if hasCP {
			stale = now.Sub(latestCP.CreatedAt) > observationThrottle
		}
		if stale {
			if o, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
				Path:      sess.Metadata.WorkspacePath,
				Branch:    sess.Metadata.Branch,
				SessionID: sess.ID,
				ProjectID: sess.ProjectID,
			}); err == nil {
				obs = o
				workspaceAvailable = true
			}
		}
	}

	decision := evaluateWorkStepProgress(found, sess, workspaceAvailable, obs, baseSHA)
	if decision.NoChange {
		return step, nil
	}

	if !domain.ValidWorkflowStepTransition(step.State, decision.NextStep) {
		if c.log != nil {
			c.log.Info("workflow: skipping invalid work-step observation transition (benign race)",
				"step", step.ID, "from", step.State, "to", decision.NextStep)
		}
		return step, nil
	}
	if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, decision.NextStep, now); err != nil {
		return step, err
	}
	step.State = decision.NextStep

	if domain.ValidWorkflowRunTransition(run.State, decision.NextRun) {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, decision.NextRun, now); err != nil {
			return step, err
		}
	} else if c.log != nil && run.State != decision.NextRun {
		c.log.Info("workflow: skipping invalid run transition from work-step observation (benign race)",
			"run", run.ID, "from", run.State, "to", decision.NextRun)
	}

	stepID := step.ID
	headSHA := ""
	if workspaceAvailable {
		headSHA = obs.HeadSHA
	}
	var sessionIDPtr *string
	if step.SessionID != nil {
		sid := *step.SessionID
		sessionIDPtr = &sid
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		SessionID:      sessionIDPtr,
		BaseSHA:        baseSHA,
		HeadSHA:        headSHA,
		NextAction:     decision.NextAction,
		DurablePhase:   "worker_observed_" + string(decision.Progress),
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		return step, err
	}

	if decision.ErrorClass != "" || decision.NextStep.Terminal() {
		latestAttempt, hasAttempt, aerr := c.store.GetLatestWorkflowAttempt(ctx, step.ID)
		if aerr == nil && hasAttempt {
			finishedAt := time.Time{}
			if latestAttempt.FinishedAt != nil {
				finishedAt = *latestAttempt.FinishedAt
			}
			outcome := latestAttempt.Outcome
			errClass := latestAttempt.ErrorClass
			switch decision.NextStep {
			case domain.WorkflowStepCompleted:
				outcome = domain.WorkflowAttemptSucceeded
				finishedAt = now
			case domain.WorkflowStepFailed:
				outcome = domain.WorkflowAttemptFailed
				finishedAt = now
			}
			if decision.ErrorClass != "" {
				errClass = decision.ErrorClass
			}
			_ = c.store.UpdateWorkflowAttemptOutcome(ctx, latestAttempt.ID, finishedAt, outcome, errClass)
		}
	}

	return step, nil
}
