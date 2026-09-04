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

// The observable worker states, in the order a healthy worker passes through
// them.
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
	// WorkerTurnCompleted is the TUI half of "the worker is done". A worker
	// whose runtime is a terminal pane does not exit when it finishes: it goes
	// idle with the pane still alive, and the only thing that distinguishes
	// "finished" from "quiet" is the turn-completion receipt the harness's own
	// Stop hook writes. That receipt is what this label stands on, and AO had
	// no name for it before -- which is why a real TUI worker could finish, be
	// observed, and be recorded as merely idle, forever.
	WorkerTurnCompleted   WorkerProgress = "worker_turn_completed"
	WorkerTerminated      WorkerProgress = "worker_terminated"
	WorkerFailed          WorkerProgress = "worker_failed"
	WorkerResultAvailable WorkerProgress = "worker_result_available"
	// WorkerReadOnlyVerified is a worker whose task the plan declared
	// read-only, which finished, and whose worktree AO has git-verified to be
	// exactly as it was at dispatch. It is a SUCCESS, and it is deliberately a
	// different word from WorkerResultAvailable: the two are reached from
	// opposite evidence -- one from a change AO can point at, the other from
	// the proven absence of one -- and a ledger that called them the same thing
	// would make the second unreadable.
	WorkerReadOnlyVerified WorkerProgress = "worker_read_only_verified"
	// WorkerReadOnlyViolated is the other half: a task declared read-only whose
	// worktree changed anyway. Not an ambiguity (AO can prove exactly what
	// happened) and not a worker error class -- a person has to decide what the
	// unexpected change is.
	WorkerReadOnlyViolated WorkerProgress = "worker_read_only_violated"
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
	// Ambiguous is set by the pure evaluator when the decision it reached IS an
	// ambiguous_worker_state stop. It deliberately does NOT carry the error
	// class: the class can only come from a raise that has a collected evidence
	// snapshot behind it, so the evaluator states the conclusion and
	// observeWorkStep is obliged to go and gather the evidence before that
	// conclusion can become durable. See ambiguous_worker_state.go.
	Ambiguous bool
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
// readOnly is the plan's own declaration that this task must not change the
// workspace, plus the git-verified verdict on whether it did. It is the ONLY
// thing that can turn "the worker finished and nothing changed" into a success,
// it is inert for every task whose plan did not declare it (which is every
// legacy plan and every standalone objective), and it can never make a
// mutation-required task complete on no evidence. See read_only_completion.go.
//
// humanInputProven is the corroboration gate on the needs-input family, and it
// is the whole of Checkpoint 8P-E.15's invariant: AO may never park a run on
// "the worker needs you" because the worker is merely still alive, or because
// no completion signal has arrived. It must hold POSITIVE evidence that a
// person is being asked something. See provenHumanInputRequest for what counts.
//
// turnCompleted is P3-A's missing terminal authority for a worker that never
// exits. Every other "the work is over" fact this evaluator reads is about the
// PROCESS -- terminated, exited, gone -- and a TUI worker satisfies none of
// them when it finishes: the pane stays alive and the session goes idle,
// exactly as it does between two turns of a conversation that is not over. So a
// real worker could finish, with its change on disk, and be evaluated forever
// as "idle, look again later". The receipt closes that: it is written only from
// a REPORTED turn boundary (the harness's Stop hook), it is CLEARED the moment
// work goes back in flight, and it is durable across a daemon restart -- see
// SessionRecord.TurnCompletedAt. It is generation-bound by its caller, which
// only sets it for a receipt at or after THIS attempt's dispatch.
//
// What it is not is a verdict. It never completes a step on its own and it
// never outranks the evidence: it makes the step CONCLUSIVE, so an outcome AO
// cannot prove becomes a bounded, honest stop instead of an unbounded silence.
func evaluateWorkStepProgress(
	sessionFound bool,
	session domain.SessionRecord,
	workspaceAvailable bool,
	obs ports.WorkspaceObservation,
	baseSHA string,
	now time.Time,
	dispatchedAt time.Time,
	humanInputProven bool,
	turnCompleted bool,
	evidence workerEvidence,
	readOnly readOnlyExpectation,
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
		// The read-only verdict outranks hasWorkEvidence() in both directions,
		// and it has to. A task whose plan permits a pre-existing dirty
		// baseline is observed dirty whether or not the worker touched
		// anything, so "the tree is dirty" cannot decide it -- only the
		// comparison against the dispatch-time fingerprint can. sessionFound
		// is required for the success half: a session row AO cannot see at all
		// is not evidence that a worker ran to completion.
		if readOnly.Violated() {
			return readOnlyViolationDecision(readOnly)
		}
		if sessionFound && readOnly.Satisfied() {
			return readOnlyVerifiedDecision()
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
		if readOnly.Violated() {
			return readOnlyViolationDecision(readOnly)
		}
		if readOnly.Satisfied() {
			return readOnlyVerifiedDecision()
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
			Progress:        WorkerObservationAmbiguous,
			NextStep:        domain.WorkflowStepWaiting,
			NextRun:         domain.WorkflowRunNeedsAttention,
			NextAction:      "worker reported waiting for input but AO could not observe any question being asked, and the session has produced no activity since — AO cannot prove what this worker is doing",
			Ambiguous:       true,
			AttentionReason: ReasonWorkerDispatchAmbiguous,
		}
	case domain.ActivityIdle:
		// The read-only rule is consulted first, and only for a worker that
		// demonstrably STARTED (a first signal). Both halves matter:
		//
		//   - first, because for a read-only task the dirty/untracked flags
		//     hasWorkEvidence() reads may be nothing but the pre-existing
		//     baseline the plan explicitly told the worker to preserve, so
		//     completing on them would be completing on the wrong fact;
		//   - only when started, because a worker that never produced a first
		//     signal has not been shown to have run at all, and "the tree is
		//     unchanged" is exactly what a worker that never started leaves
		//     behind. That case belongs to the startup reconciliation below,
		//     which weighs evidence this rule does not have.
		if !session.FirstSignalAt.IsZero() {
			if readOnly.Violated() {
				return readOnlyViolationDecision(readOnly)
			}
			if readOnly.Satisfied() {
				return readOnlyVerifiedDecision()
			}
		}
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
				// Checkpoint 8P-E.24 (incident wf-00283521 / medusa-4): an
				// absent first signal is a fact about the HOOK PIPELINE, never
				// proof the process died. Hand it to the reconciliation, which
				// weighs every independent fact AO can actually obtain and only
				// declares a startup failure on evidence of absence — see
				// worker_signal_reconcile.go.
				return reconcileMissingFirstSignal(evidence, now, dispatchedAt).Decision
			}
			return WorkStepDecision{Progress: WorkerCreated, NoChange: true}
		}
		if !workspaceAvailable {
			// Insufficient fresh evidence this call; wait for a future call
			// once the throttle window has elapsed. Not an error.
			//
			// Unless the worker has already SAID it is finished. Then there is
			// no future call worth waiting for -- nothing else is coming, the
			// receipt does not expire, and "look again later" is a promise AO
			// cannot keep. This is the exact state both P3-A smokes ended in:
			// a finished worker, its change on disk, and a work step polling
			// itself forever because the one observation that would have
			// concluded it could not be obtained. Its caller has already paid
			// for a forced, unthrottled observation before reaching here, so an
			// observation still missing at this point is a repository AO cannot
			// read rather than a throttle -- which is a thing to say out loud,
			// not to keep quiet about.
			if !turnCompleted {
				return WorkStepDecision{Progress: WorkerIdle, NoChange: true}
			}
			return WorkStepDecision{
				Progress:   WorkerTurnCompleted,
				NextStep:   domain.WorkflowStepWaiting,
				NextRun:    domain.WorkflowRunNeedsAttention,
				NextAction: "worker reported its turn finished, but AO could not observe its workspace and cannot prove what it produced",
				Ambiguous:  true,
				// Not the generic dispatch ambiguity: the turn receipt is
				// proof this worker ran its turn to the end, so the only thing
				// missing is one observation of one directory. See the reason's
				// own doc comment for why the two must not share a sentence.
				AttentionReason: ReasonWorkerWorkspaceUnreadable,
			}
		}
		// Observed, and nothing to show for it. Fail closed: a worker that says
		// it is done and left no trace is not a completed task, and a process
		// that merely stopped is not a result either. Naming the receipt in the
		// progress label keeps the two apart on the ledger.
		progress := WorkerIdle
		action := "worker idle with no verifiable change — needs human review"
		// The default reason stands for the idle half: no receipt, nothing in
		// the tree, and no way to tell a worker that did nothing from one that
		// never really started. A turn receipt removes exactly that doubt, and
		// with it the reason -- what is left is a disagreement between two
		// facts AO holds, not a gap in them.
		reason := ""
		if turnCompleted {
			progress = WorkerTurnCompleted
			action = "worker reported its turn finished, but AO can see no change in its workspace — nothing verifiable was produced"
			reason = ReasonWorkerTurnProducedNothing
		}
		return WorkStepDecision{
			Progress:        progress,
			NextStep:        domain.WorkflowStepWaiting,
			NextRun:         domain.WorkflowRunNeedsAttention,
			NextAction:      action,
			Ambiguous:       true,
			AttentionReason: reason,
		}
	default:
		// Unknown/unspecified activity: make no change rather than guess.
		return WorkStepDecision{Progress: WorkerCreated, NoChange: true}
	}
}

// readOnlyVerifiedDecision is the single construction of the accepted
// no-change completion.
//
// It lands on the SAME transition an implementation task's completion lands on,
// with the same "start_review" next action, and that is deliberate: everything
// downstream -- the review dispatch's target fingerprint, the review policy,
// and above all the verification step that actually runs the plan's declared
// commands and file checks -- must behave identically. A read-only task is not
// trusted here; it is merely allowed to reach the thing that judges it.
func readOnlyVerifiedDecision() WorkStepDecision {
	return WorkStepDecision{
		Progress:   WorkerReadOnlyVerified,
		NextStep:   domain.WorkflowStepCompleted,
		NextRun:    domain.WorkflowRunWaiting,
		NextAction: "start_review",
	}
}

// readOnlyViolationDecision is the other single construction: a task the plan
// declared read-only whose worktree changed anyway.
//
// It carries no error class on purpose. An error class asserts that an attempt
// failed for a reason AO can name in the failure vocabulary; what happened here
// is that a declared contract was broken, AO can say exactly how, and a person
// has to decide whether the change is wanted. That is an attention reason, in
// the same shape blockedOnHumanDecision uses -- and, like that one, it leaves
// the step Waiting so an ordinary Continue can reopen it.
func readOnlyViolationDecision(e readOnlyExpectation) WorkStepDecision {
	return WorkStepDecision{
		Progress:        WorkerReadOnlyViolated,
		NextStep:        domain.WorkflowStepWaiting,
		NextRun:         domain.WorkflowRunNeedsAttention,
		NextAction:      e.Detail,
		AttentionReason: ReasonReadOnlyWorkspaceMutated,
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

	// Checkpoint 8P-E.24: the two decisions that can END a work step on the
	// ABSENCE of evidence — "terminated with nothing to show" and "no first
	// signal" — must never be taken against a throttled observation. The
	// throttle exists to keep a healthy poll cheap; a step that is quietly
	// polling writes no checkpoints, so it can keep the one fact that would
	// have proven the work exists permanently out of reach. When either of
	// those two decisions is in play AO pays for the git observation, every
	// time, and gathers the independent liveness facts alongside it.
	terminalish := !found || sess.IsTerminated || sess.Activity.State == domain.ActivityExited
	missingFirstSignal := found && sess.FirstSignalAt.IsZero() &&
		!dispatchedAt.IsZero() && now.Sub(dispatchedAt) > workStepFirstSignalTimeout
	// P3-A: the third decision that must never be taken against a throttled
	// observation, and the one that had no forcing at all. A TUI worker that
	// reported its turn finished is a work step with a conclusion due NOW: the
	// receipt does not expire, no further signal is coming, and the throttle --
	// which is keyed off the newest checkpoint and which a quietly polling step
	// never refreshes -- can otherwise keep the observation that would conclude
	// it permanently out of reach. Same argument as the two above, same
	// remedy: when a conclusion is in play, AO pays for the git observation.
	turnCompleted := workerTurnCompleted(found, sess, dispatchedAt)
	var evidence workerEvidence
	if terminalish || missingFirstSignal {
		evidence, obs, workspaceAvailable = c.workerEvidenceFor(
			ctx, run, found, sess, obs, workspaceAvailable, baseSHA, dispatchedAt)
	} else if turnCompleted {
		obs, workspaceAvailable = c.forceWorkspaceObservation(ctx, sess, obs, workspaceAvailable)
	}

	// The read-only expectation is resolved only for an observation it could
	// actually decide: a session that has ended, or one sitting idle/uncorroborated
	// with a fresh observation in hand. A healthy run polling an active worker
	// pays for none of it.
	//
	// It is resolved AFTER the forced observation above, so the fingerprint it
	// compares is the same one the decision is taken on rather than a throttled
	// older reading.
	var readOnly readOnlyExpectation
	if terminalish || sess.Activity.State == domain.ActivityIdle || sess.Activity.State == domain.ActivityWaitingInput {
		readOnly = c.resolveReadOnlyExpectation(ctx, run, step, workspaceAvailable, obs)
	}

	decision := evaluateWorkStepProgress(found, sess, workspaceAvailable, obs, baseSHA, now, dispatchedAt, humanInputProven, turnCompleted, evidence, readOnly)
	if missingFirstSignal {
		// The reconciliation's verdict is durable whether or not it changes any
		// state: "AO looked again and decided to keep waiting" is exactly the
		// fact whose absence made the medusa-4 incident unreadable.
		verdict := reconcileMissingFirstSignal(evidence, now, dispatchedAt)
		c.recordFirstSignalReconciliation(ctx, run, step, verdict, evidence, latestCP)
	}
	if decision.NoChange {
		return step, nil
	}

	// §12: before an empty turn is recorded as the worker's fault, ask whether
	// the worker was even looking at the right code. Done BEFORE the ambiguity
	// gate so the reason it writes down, the evidence snapshot it collects and
	// the attention stop below all carry the same corrected answer.
	decision = c.reclassifyRepairWorkspaceStop(ctx, run, decision)

	// The ambiguity gate. A decision that concluded "AO cannot prove what this
	// worker is doing" may not become durable on that conclusion alone: the
	// raise collects the bounded evidence snapshot, records it, and is the only
	// thing that can hand back the error class. See ambiguous_worker_state.go.
	var ambiguity AmbiguousWorkerState
	if decision.Ambiguous {
		var observation *ports.WorkspaceObservation
		if workspaceAvailable {
			o := obs
			observation = &o
		}
		// The observation this decision was actually taken on, plus a liveness
		// answer, are handed to the gate so it can write them down BEFORE the
		// snapshot is built. The collector reads only durable rows, so an
		// observation that stayed in memory would be one nobody could see
		// afterwards. See observedWorkerFactsFor.
		observed := c.observedWorkerFactsFor(ctx, sessionIDIfFound(found, sess), observation)
		reason := decision.AttentionReason
		if reason == "" {
			reason = ReasonWorkerDispatchAmbiguous
		}
		raised, rerr := c.raiseAmbiguousWorkerState(ctx, run, step, reason, decision.NextAction, observed)
		if rerr != nil {
			return step, rerr
		}
		ambiguity = raised
		decision.ErrorClass = raised.ErrorClass()
	}
	if err := assertAmbiguousEvidence(decision.ErrorClass, ambiguity); err != nil {
		return step, err
	}

	// A read-only outcome is a conclusion drawn from a comparison of two
	// fingerprints, so both sides of that comparison are written down before
	// the transition that stands on them. Best-effort: unlike the ambiguity
	// gate, failing to record this must not strand a step that AO can prove
	// finished correctly -- the decision itself is re-derivable from the
	// dispatch record and a fresh observation, which is exactly what the
	// ambiguity snapshot is not.
	// Gated on the DECISION rather than on the expectation: a read-only verdict
	// that was resolved but not acted on (e.g. satisfied, but with no session
	// row, so the terminated path still failed the step) decided nothing, and
	// recording it would put a conclusion on the ledger that no transition
	// stands on.
	if decision.Progress == WorkerReadOnlyVerified || decision.Progress == WorkerReadOnlyViolated {
		c.recordReadOnlyCompletionEvidence(ctx, run, step, readOnly)
	}

	if !domain.ValidWorkflowStepTransition(step.State, decision.NextStep) {
		if c.log != nil {
			c.log.Info("workflow: skipping invalid work-step observation transition (benign race)",
				"step", step.ID, "from", step.State, "to", decision.NextStep)
		}
		return step, nil
	}
	// The CAS is the exactly-once boundary, and its answer is load-bearing.
	//
	// Three mechanisms observe the same worker -- the autonomous heartbeat,
	// Continue, and boot recovery -- and all three can reach this line for one
	// completion. Exactly one of them moves the row; the losers must then do
	// NOTHING, because every write below this point is a side effect of having
	// moved it: clearing the stop, transitioning the run, and above all writing
	// the completion checkpoint that review dispatch resolves its worktree and
	// its target fingerprint from. A second checkpoint for one completion ties
	// with the first on created_at, which makes "the latest checkpoint for this
	// step" ambiguous -- so the loser observing the winner's own result is not
	// merely tidier, it is the difference between one review dispatch and a
	// coin flip between two. Ignoring the CAS result is what made this a race
	// rather than a decision.
	moved, err := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, decision.NextStep, now)
	if err != nil {
		return step, err
	}
	if !moved {
		if current, ok, gerr := c.getWorkflowStep(ctx, run.ID, step.ID); gerr == nil && ok {
			return current, nil
		}
		return step, nil
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
			// Two different writes share this line, and only one of them is a
			// finalization.
			//
			// When the attempt is still OPEN this is the finalization, and it
			// goes through the claim: exactly one caller may conclude an
			// attempt, and last-writer-wins over the row that says whether work
			// is in flight is how one pass's verdict came to overwrite
			// another's. A losing caller is a no-op, which is the outcome it
			// wanted anyway.
			//
			// When the attempt is already CONCLUDED this is a deliberate
			// REFINEMENT of its recorded error class (e.g. to
			// ambiguous_worker_state) by a later fact-based observation, and it
			// must still land — the claim would match zero rows and silently
			// drop it, hiding the ambiguity from the UI exactly as the stranded
			// row above used to.
			if latestAttempt.Outcome == "" && latestAttempt.FinishedAt == nil {
				_, _ = c.store.ClaimWorkflowAttemptOutcome(ctx, latestAttempt.ID, finishedAt, outcome, errClass)
			} else {
				_ = c.store.UpdateWorkflowAttemptOutcome(ctx, latestAttempt.ID, finishedAt, outcome, errClass)
			}
		}
	}

	return step, nil
}

// workerTurnCompleted reports whether this attempt's worker has REPORTED that
// its turn ended, and is the whole of the ownership/generation guard on that
// fact.
//
// Two conditions, and the second is the one that makes an old completion unable
// to close a new work step. The receipt must exist, and it must not predate
// THIS attempt's dispatch. A worker relaunched for attempt N+1 carries the
// session row of attempt N, and that row can still hold a receipt from the
// earlier turn if nothing has cleared it yet; concluding on it would close the
// new attempt on the old attempt's word. (The receipt is in fact cleared the
// moment work goes back in flight -- see SessionRecord.TurnCompletedAt -- so
// this is a second lock on a door that is already shut. Both are cheap, and
// only one of them lives in this package.)
//
// A dispatch time AO does not know is not treated as permission: with no
// attempt row there is nothing to bind the receipt to, and an unbound receipt
// concludes nothing.
func workerTurnCompleted(sessionFound bool, sess domain.SessionRecord, dispatchedAt time.Time) bool {
	if !sessionFound || sess.TurnCompletedAt.IsZero() || dispatchedAt.IsZero() {
		return false
	}
	return !sess.TurnCompletedAt.Before(dispatchedAt)
}

// forceWorkspaceObservation pays for the git observation the throttle skipped.
//
// Extracted from workerEvidenceFor, which has always done exactly this and for
// exactly the same reason, so the two forcing paths cannot drift: one of them
// also wants the independent liveness facts, and one of them only wants to be
// able to see the repository. Already-available observations are returned
// untouched, so calling it is idempotent and free.
func (c *Coordinator) forceWorkspaceObservation(
	ctx stdctx.Context, sess domain.SessionRecord,
	obs ports.WorkspaceObservation, workspaceAvailable bool,
) (ports.WorkspaceObservation, bool) {
	if workspaceAvailable || c.workspaceFacts == nil || sess.Metadata.WorkspacePath == "" {
		return obs, workspaceAvailable
	}
	o, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      sess.Metadata.WorkspacePath,
		Branch:    sess.Metadata.Branch,
		SessionID: sess.ID,
		ProjectID: sess.ProjectID,
	})
	if err != nil {
		return obs, workspaceAvailable
	}
	return o, true
}

// sessionIDIfFound is the session to probe and to stamp on a recorded
// observation, or empty when there is no session row at all. AO never probes a
// runtime for a session it cannot see.
func sessionIDIfFound(found bool, sess domain.SessionRecord) domain.SessionID {
	if !found {
		return ""
	}
	return sess.ID
}
