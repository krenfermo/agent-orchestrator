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

// workStepFirstSignalTimeout bounds how long a work step may sit idle with
// FirstSignalAt unset before AO gives up waiting for the worker to ever
// start (Checkpoint 8P-E.3). Without this, a worker whose process launched
// but never progressed past its own startup (e.g. stuck at an interactive
// prompt, a launch error masked by an otherwise-successful Spawn, or an
// unauthenticated provider) leaves the work step "running" and the
// autonomous heartbeat rescheduling itself forever (maybeScheduleAutonomousHeartbeat
// only stops once the run reaches a terminal or needs_attention state).
// Sized well above the wake scheduler's own backoff cadence (WakePolicy's
// InitialBackoffSeconds=60, MaxBackoffSeconds=1800 — see
// domain/workflow_wake_policy.go) so a normal, merely-slow CLI cold start
// (model warmup, network, hook install) is never mistaken for a stuck
// worker, while still reaching a deterministic failure state in bounded
// time rather than polling indefinitely.
const workStepFirstSignalTimeout = 10 * time.Minute

// WorkerProgress is a workflow-internal interpretation label for a work
// step's Codex worker. It exists purely to make the conservative completion
// rule below legible and testable; it is never persisted (no sessions column,
// no new DB table). The resulting WorkflowStepState/WorkflowRunState and
// checkpoint next_action text are what gets stored.
type WorkerProgress string

const (
	WorkerCreated WorkerProgress = "worker_created"
	WorkerStarted WorkerProgress = "worker_started"
	// WorkerActive covers both "actively executing" and "actively
	// reasoning/streaming". AO deliberately does not split them: no harness
	// signal distinguishes the two, and nothing downstream would do anything
	// different if it did — both mean "the worker has a turn in flight, leave
	// it alone".
	WorkerActive WorkerProgress = "worker_active"
	// WorkerIdle is idle but healthy: the session is alive and its turn ended.
	WorkerIdle WorkerProgress = "worker_idle"
	// WorkerAwaitingHuman is the ONLY progress value that may park a run on
	// ReasonWorkerBlocked, and it requires positive, corroborated evidence that
	// a person is actually being asked something — see
	// evaluateWorkStepProgress. Before it existed, a decision to stop the run
	// on a "blocked" worker was recorded under WorkerActive, so the incident's
	// own ledger read "worker_observed_worker_active / worker awaiting input".
	WorkerAwaitingHuman WorkerProgress = "worker_awaiting_human"
	// WorkerObservationAmbiguous is AO admitting it cannot tell what the
	// session is doing: a needs-input reading it could not corroborate, on a
	// session that has also stopped producing any activity at all. Truthful,
	// bounded, and never phrased as "the worker is waiting for you".
	WorkerObservationAmbiguous WorkerProgress = "worker_observation_ambiguous"
	WorkerTerminated           WorkerProgress = "worker_terminated"
	WorkerFailed               WorkerProgress = "worker_failed"
	WorkerResultAvailable      WorkerProgress = "worker_result_available"
)

// workerNeedsInputCorroborationWindow bounds how long an UNCORROBORATED
// needs-input reading may stand before AO stops calling the state healthy and
// records an honest ambiguity instead.
//
// It is not a "wait longer and hope" timeout: an uncorroborated reading never
// becomes a worker-blocked stop no matter how long it lasts, and a corroborated
// one stops the run immediately without consulting this at all. It exists only
// so the ambiguous tail is bounded rather than polled forever, and it is
// measured from the session's last activity reading — which, for a worker that
// is genuinely still working, keeps moving (the terminal activity observer
// re-asserts `active` from the pane), so an actively working agent can never
// age into it.
const workerNeedsInputCorroborationWindow = 15 * time.Minute

// WorkStepDecision is the outcome of evaluating a work step's session/
// workspace facts: what to do next, expressed independent of persistence.
type WorkStepDecision struct {
	Progress   WorkerProgress
	NextStep   domain.WorkflowStepState
	NextRun    domain.WorkflowRunState
	NextAction string
	ErrorClass domain.WorkflowErrorClass
	// AttentionReason is Checkpoint 8P-E.13's canonical name for a stop this
	// decision is about to cause. Set only when NextRun is needs_attention and
	// ErrorClass alone would not name the stop — a worker blocked on its own
	// interactive prompt is not an "error", so it has no error class, and
	// before this field it reached the Board as an unexplained needs_attention.
	AttentionReason string
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
//
// humanInputProven is the corroboration gate on the needs-input family, and it
// is the whole of Checkpoint 8P-E.15's invariant: AO may never park a run on
// "the worker needs you" because the worker is merely still alive, or because
// no completion signal has arrived. It must hold POSITIVE evidence that a
// person is being asked something. See provenHumanInputRequest for what counts.
func evaluateWorkStepProgress(
	sessionFound bool,
	session domain.SessionRecord,
	workspaceAvailable bool,
	obs ports.WorkspaceObservation,
	baseSHA string,
	now time.Time,
	dispatchedAt time.Time,
	humanInputProven bool,
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
	case domain.ActivityBlocked:
		// `blocked` is proof on its own, and is the one needs-input state that
		// is. It is only ever entered from a permission dialog whose blocking
		// tool AO correlated, and lifecycle's tool-precedence rule clears it as
		// soon as that tool's post-tool-use lands or the turn ends — so a
		// session reading `blocked` has a dialog open RIGHT NOW. Nothing about
		// it can go stale while the agent works, which is exactly what
		// distinguishes it from waiting_input below.
		return blockedOnHumanDecision()

	case domain.ActivityWaitingInput:
		// `waiting_input` is a HINT, not proof, and treating it as proof is
		// what caused incident wf-57f90ff2. It is by construction the state for
		// a needs-input signal that carries NO tool identity (see the
		// claude-code adapter's own note on why agent_needs_input must not map
		// to blocked), which means nothing can correlate its resolution and it
		// can only lift at a turn boundary. On Codex it is worse still: every
		// PermissionRequest lands here, Codex installs no resolving hook at
		// all, and the reading therefore latches for an entire working turn.
		//
		// So it needs corroboration from something that actually observed a
		// question being asked.
		if humanInputProven {
			return blockedOnHumanDecision()
		}
		// Uncorroborated. The worker is alive and inside a turn; "alive with no
		// completion signal" is explicitly not grounds to stop a run, so the
		// default is to leave it alone and look again.
		if session.Activity.LastActivityAt.IsZero() ||
			now.Sub(session.Activity.LastActivityAt) <= workerNeedsInputCorroborationWindow {
			return WorkStepDecision{Progress: WorkerActive, NoChange: true}
		}
		// The reading has neither been corroborated nor refreshed for the whole
		// window: the session has gone completely silent in a state AO cannot
		// explain. Say that, rather than claiming the worker is waiting on the
		// user — a claim AO has no evidence for and which sends the person to
		// look for a prompt that may not exist. Bounded and reopenable by a
		// normal Continue, like every other ambiguous stop.
		if hasWorkEvidence() {
			return WorkStepDecision{
				Progress:   WorkerResultAvailable,
				NextStep:   domain.WorkflowStepCompleted,
				NextRun:    domain.WorkflowRunWaiting,
				NextAction: "start_review",
			}
		}
		return WorkStepDecision{
			Progress:        WorkerObservationAmbiguous,
			NextStep:        domain.WorkflowStepWaiting,
			NextRun:         domain.WorkflowRunNeedsAttention,
			NextAction:      "worker reported waiting for input but AO could not observe any question being asked, and the session has produced no activity since — AO cannot prove what this worker is doing",
			ErrorClass:      domain.WorkflowErrorAmbiguousWorkerState,
			AttentionReason: ReasonWorkerDispatchAmbiguous,
		}
	case domain.ActivityIdle:
		// Real, git-verified work evidence always wins, regardless of
		// FirstSignalAt: a worker can (and often does) finish its turn and go
		// idle before AO ever observes an intermediate hook signal, and that
		// is still a genuinely completed task, not a stuck startup.
		if workspaceAvailable && hasWorkEvidence() {
			return WorkStepDecision{
				Progress:   WorkerResultAvailable,
				NextStep:   domain.WorkflowStepCompleted,
				NextRun:    domain.WorkflowRunWaiting,
				NextAction: "start_review",
			}
		}
		// A newly launched TUI session is stored as idle before its first hook
		// callback. In particular, this window can span a daemon restart while
		// the agent is already working. Without a signal or workspace evidence,
		// idle is only an initialization default—not proof the worker finished.
		// It is also not proof the worker will *ever* start: a Spawn() that
		// returns success only proves the runtime process launched, not that
		// the agent progressed past its own startup (Checkpoint 8P-E.3 found a
		// real worker stuck at Claude Code's interactive "do you trust this
		// folder?" prompt, which never fires a hook and never produces
		// FirstSignalAt). Past workStepFirstSignalTimeout since dispatch, treat
		// that absence itself as evidence of a startup failure rather than
		// waiting forever — checked ahead of the workspaceAvailable gate below
		// because a worker that never started will also never produce fresh
		// workspace evidence, so waiting on that would wait forever too.
		if session.FirstSignalAt.IsZero() {
			if !dispatchedAt.IsZero() && now.Sub(dispatchedAt) > workStepFirstSignalTimeout {
				return WorkStepDecision{
					Progress:   WorkerFailed,
					NextStep:   domain.WorkflowStepFailed,
					NextRun:    domain.WorkflowRunNeedsAttention,
					NextAction: "worker produced no first signal within " + workStepFirstSignalTimeout.String() + " of dispatch — startup likely failed (e.g. blocked on an interactive prompt, auth, or a launch error)",
					ErrorClass: domain.WorkflowErrorAgentStartFailed,
				}
			}
			return WorkStepDecision{Progress: WorkerCreated, NoChange: true}
		}
		if !workspaceAvailable {
			// Insufficient fresh evidence this call; wait for a future call
			// once the throttle window has elapsed. Not an error.
			return WorkStepDecision{Progress: WorkerIdle, NoChange: true}
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

// blockedOnHumanDecision is the single construction of the one stop that says
// "a person is being asked something". Both callers reach it only with positive
// evidence, and no other site in this file may build it.
func blockedOnHumanDecision() WorkStepDecision {
	return WorkStepDecision{
		Progress:        WorkerAwaitingHuman,
		NextStep:        domain.WorkflowStepWaiting,
		NextRun:         domain.WorkflowRunNeedsAttention,
		NextAction:      "worker awaiting input/blocked — needs human attention",
		AttentionReason: ReasonWorkerBlocked,
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

	var dispatchedAt time.Time
	if latestAttempt, hasAttempt, aerr := c.store.GetLatestWorkflowAttempt(ctx, step.ID); aerr == nil && hasAttempt {
		dispatchedAt = latestAttempt.StartedAt
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

	// The corroboration gate for the needs-input family. Read unconditionally
	// (a cheap row scan on the run's own questions) rather than lazily inside
	// the evaluator, so the evaluator stays a pure function of facts.
	humanInputProven, err := c.provenHumanInputRequest(ctx, run.ID, step.ID)
	if err != nil {
		return step, err
	}

	decision := evaluateWorkStepProgress(found, sess, workspaceAvailable, obs, baseSHA, now, dispatchedAt, humanInputProven)
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

	// Checkpoint 8P-E.13A.2: a decision that moves the run FORWARD (work
	// completed, so: start the review) cannot be applied on top of a stale
	// needs_attention — needs_attention -> waiting is not a legal transition,
	// so the update below would be skipped as a benign race and the run would
	// stay stopped with a completed work step and a "start_review" checkpoint
	// under it, which is precisely the deadlock this checkpoint fixes. The
	// worker having produced git-verified work is itself the proof that
	// whatever parked the run has been remediated; a stop that is a human
	// decision is still never cleared (see clearResolvedStop).
	if decision.NextRun != domain.WorkflowRunNeedsAttention {
		run = c.clearResolvedStop(ctx, run, "the worker produced verifiable work after the run was parked")
	}
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
	fingerprintAfter := ""
	if workspaceAvailable {
		headSHA = obs.HeadSHA
		// Checkpoint 8D: on the completion checkpoint specifically, also
		// record the WorkspaceFingerprint of the completed state. Reused
		// (not duplicated) by dispatchReviewStep as cycle 1's target_sha —
		// see design decision 3's identity-column reuse: a cycle 1 review
		// target_sha must be a fingerprint hash too, or the fix step's
		// "did the workspace genuinely change" comparison (fingerprintBefore
		// == the addressed review_run's target_sha) would compare a real
		// SHA256 hash against a raw (often empty) HeadSHA and never match.
		if decision.NextStep == domain.WorkflowStepCompleted {
			fingerprintAfter = WorkspaceFingerprint(obs)
		}
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
		// Branch/WorktreePath must carry forward from session facts on every
		// checkpoint, not just the initial "worker_dispatched" one — a later
		// checkpoint is what "the latest checkpoint for this step" resolves
		// to (e.g. Checkpoint 8C's review dispatch reads it to find the
		// worktree to launch the reviewer against), so dropping these here
		// silently loses them the moment work observation writes its own
		// checkpoint on completion. Found via 8C's real E2E run: the
		// reviewer launch failed with "workspace path is required" because
		// this checkpoint had gone through without them.
		Branch:           sess.Metadata.Branch,
		WorktreePath:     sess.Metadata.WorkspacePath,
		BaseSHA:          baseSHA,
		HeadSHA:          headSHA,
		FingerprintAfter: fingerprintAfter,
		NextAction:       decision.NextAction,
		DurablePhase:     "worker_observed_" + string(decision.Progress),
		PayloadVersion:   "v1",
		RetryState:       "{}",
		CreatedAt:        now,
	}); err != nil {
		return step, err
	}

	// Checkpoint 8P-E.13: a work observation that stops the run records why in
	// the canonical vocabulary. The generic "worker_observed_<progress>" phase
	// written above says what AO was looking at, not what it decided.
	if decision.NextRun == domain.WorkflowRunNeedsAttention && decision.AttentionReason != "" {
		c.recordAttentionStop(ctx, run, &stepID, decision.AttentionReason, decision.NextAction)
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
			switch {
			case decision.NextStep == domain.WorkflowStepCompleted:
				outcome = domain.WorkflowAttemptSucceeded
				finishedAt = now
			// A decision carrying an ErrorClass is AO giving up on this
			// attempt, and that is true even when the step lands on
			// Waiting rather than a terminal state — the ambiguous-worker
			// case below is the only such decision today. It must finalize
			// the attempt for the same reason the terminal cases do:
			// observeWorkStep's own guard only re-enters while the step is
			// Running, and nothing resumes a *work* step out of Waiting
			// (only the plan and verify steps have a waiting->running
			// caller), so an attempt left with outcome/finished_at unset
			// here stays unset forever. Before this fix that stranded row
			// read as still in-flight, and — because
			// task_checkpoint_summary only surfaces ActiveErrors for
			// attempts whose outcome is failed — hid the ambiguity from
			// the UI entirely, leaving the run sitting in needs_attention
			// with nothing on screen explaining why.
			//
			// "failed" is the honest outcome, not a guess about the
			// worker: the attempt demonstrably did not produce a usable
			// result, and ErrorClass (ambiguous_worker_state) is what
			// records that AO could not prove *why*. The outcome enum has
			// no "ambiguous" member, and leaving it NULL asserts something
			// stronger and falser — that the attempt is still running.
			case decision.NextStep == domain.WorkflowStepFailed, decision.ErrorClass != "":
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
