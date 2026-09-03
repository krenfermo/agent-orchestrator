package workflow

import (
	stdctx "context"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// dispatch_reconcile.go — what a crash leaves behind, and how AO reads it.
//
// The phased dispatch (dispatch_state_machine.go) made every step of a launch
// durable in order: intent, launch, confirmation, RUNNING. That ordering is
// only worth anything if something later READS it. A daemon killed between any
// two of those phases leaves a run whose rows disagree with each other — an
// attempt open over a launch that never happened, a step RUNNING over a worker
// that was never confirmed, a confirmed worker whose process died with the
// daemon — and until this file existed nothing resolved those disagreements.
// Boot recovery re-entered dispatch and hoped; a wake fired and hoped again.
//
// So: for every non-terminal work step, read the durable launch evidence, read
// who actually owns the execution right now, and answer the ONE question that
// decides everything else — which phase of the state machine actually
// completed. Six contradictions, six deterministic answers:
//
//	(a) intent recorded, nothing launched under this key   -> close, retry per policy
//	(b) intent recorded, launch proven failed, unrouted    -> close, retry per policy
//	(c) launch proven, confirmation never persisted        -> ADOPT: confirm it durably
//	(d) step RUNNING with no launch evidence at all        -> close, retry per policy
//	(e) confirmed launch whose execution is proven gone    -> close, retry per policy
//	(f) confirmed launch, execution live                   -> UNTOUCHED. Nothing. At all.
//
// And one answer that is not a case but a rule: whenever AO cannot PROVE which
// of those it is looking at, it stops with the evidence rather than guessing.
// An unreadable session is not a dead one, an unwired liveness probe is not a
// verdict, and a retry taken on either would be a second agent started over a
// live one.
//
// Two shapes are deliberately NOT taken here, because AO already has a correct
// owner for each and a second opinion would only be a worse one:
//
//   - a confirmed launch that reached RUNNING with its session on the step is a
//     worker, and what became of it is work observation's question
//     (worker_progress.go) — including when it has exited;
//   - a dispatched outbox command with no launch boundary under it at all is
//     the pre-state-machine ambiguity, and adoptOrMarkAmbiguous already adopts
//     it on evidence and escalates it otherwise.
//
// Three properties this file is built around:
//
//   - It never launches anything. Retry means "hand the step back to the
//     bounded launch-retry policy that already exists" (worker_launch_recovery.go):
//     the outbox goes back to Pending under the same idempotency key and a
//     durable wake carries it. Reconciliation decides; dispatch dispatches.
//   - It is idempotent through the dispatch key. Before it acts it looks for a
//     live worker under that key — the step's session, the confirmation's
//     session, and the natural-key row a launch AO never managed to name — and
//     a live one ends the pass. After it acts it records a
//     `worker_dispatch_reconciled` boundary, so while that is the newest
//     evidence for the step the contradiction it answered is already answered.
//     Two wakes therefore produce one resolution and never two workers.
//   - Every needs_attention it raises goes through the SAME evidence gate every
//     other ambiguity in this package goes through (ambiguous_worker_state.go):
//     the bounded snapshot is collected and made durable BEFORE any state moves,
//     and the launch/dispatch history is part of it because the collector
//     already reads the dispatch table. There is no second evidence model here
//     and there must never be one.

// DispatchContradiction is the deterministic classification of one attempt/step
// against the durable launch evidence under it.
//
// It names what the ROWS say, not what AO would like to do about it. The remedy
// is a separate decision (DispatchReconcileAction) taken by policy, so the same
// contradiction can end in a retry on one run and a stop on another without the
// classification drifting.
type DispatchContradiction string

const (
	// ContradictionNone is agreement: the evidence and the execution say the
	// same thing. Case (f).
	ContradictionNone DispatchContradiction = "none"
	// ContradictionIntentNeverLaunched is case (a): a dispatch intent is the
	// newest boundary and nothing concluded it. The process died holding the
	// launch, and no worker exists under this dispatch key.
	ContradictionIntentNeverLaunched DispatchContradiction = "intent_never_launched"
	// ContradictionLaunchFailed is case (b): the launch is proven not to have
	// completed, and the failure was never routed into the retry policy —
	// i.e. the daemon died between recording the failure and answering it.
	ContradictionLaunchFailed DispatchContradiction = "launch_failed"
	// ContradictionLaunchUnconfirmed is case (c): a launch happened and its
	// confirmation never became durable. The one contradiction from which a
	// relaunch is forbidden and an adoption is owed.
	ContradictionLaunchUnconfirmed DispatchContradiction = "launch_unconfirmed"
	// ContradictionRunningWithoutEvidence is case (d): the step says RUNNING
	// and there is no dispatch boundary for it at all. Either the rows predate
	// the state machine, or something moved the step without launching.
	ContradictionRunningWithoutEvidence DispatchContradiction = "running_without_evidence"
	// ContradictionStaleRunning is case (e): the launch was confirmed, and the
	// execution it confirmed is provably gone.
	ContradictionStaleRunning DispatchContradiction = "stale_running"
	// ContradictionUnprovable is the honest non-answer: the evidence says one
	// thing, and AO could not read who owns the execution well enough to act on
	// it. Never folded into a neighbouring case.
	ContradictionUnprovable DispatchContradiction = "unprovable"
)

// DispatchReconcileAction is what reconciliation DID about a contradiction.
type DispatchReconcileAction string

const (
	// DispatchReconcileNoop means nothing needed doing, or nothing could be:
	// no work step, no readable dispatch evidence, or the contradiction was
	// already answered by an earlier pass.
	DispatchReconcileNoop DispatchReconcileAction = "noop"
	// DispatchReconcileProtected means a live, evidenced worker was found and
	// deliberately left alone. Not retried, not stopped, not killed, and not
	// written about: an untouched worker means untouched.
	DispatchReconcileProtected DispatchReconcileAction = "protected"
	// DispatchReconcileAdopted means a launch AO could not confirm was
	// confirmed durably now, from the ownership evidence, without launching a
	// second worker.
	DispatchReconcileAdopted DispatchReconcileAction = "adopted"
	// DispatchReconcileRetryScheduled means the stale attempt was closed and
	// the step handed back to the bounded launch-retry policy.
	DispatchReconcileRetryScheduled DispatchReconcileAction = "retry_scheduled"
	// DispatchReconcileNeedsAttention means AO stopped, with the evidence
	// snapshot recorded first.
	DispatchReconcileNeedsAttention DispatchReconcileAction = "needs_attention"
)

// Resolved reports whether this action changed durable state. It is what the
// parent-state settlement below keys off: a pass that resolved nothing must
// never move the run.
func (a DispatchReconcileAction) Resolved() bool {
	switch a {
	case DispatchReconcileAdopted, DispatchReconcileRetryScheduled, DispatchReconcileNeedsAttention:
		return true
	default:
		return false
	}
}

// DispatchReconciliation is one step's answer: what the contradiction was, what
// was done, and the identities it was about.
type DispatchReconciliation struct {
	RunID       string
	StepID      string
	AttemptID   string
	SessionID   string
	DispatchKey string

	Contradiction DispatchContradiction
	Action        DispatchReconcileAction
	// Detail is the sentence a person reads. Always set for an action that
	// moved anything.
	Detail string
}

// ---- who actually owns the execution ----------------------------------------

// ownedExecution is the live half of the answer: everything AO can currently
// read about whether a worker exists under this dispatch key.
//
// The three accessors below are the whole point. `Live` and `ProvenGone` are
// NOT complements — the gap between them is `Unprovable`, and keeping that gap
// open is what stops a session AO simply could not read from being reported as
// a dead one.
type ownedExecution struct {
	SessionID domain.SessionID
	Evidence  SessionOwnershipEvidence

	// RowFound/RowTerminated come from the session ROW, which is a different
	// question from the one the ownership proof answers and must not be folded
	// into it. The row says whether the agent's session ended; the proof says
	// whether the execution AO launched still exists. A worker that finished
	// normally satisfies the first and not the second, and telling those apart
	// is what keeps a completed worker from being read as a phantom.
	RowFound      bool
	RowTerminated bool

	// RowTurnCompletedAt is the provider's own durable receipt for the LAST
	// turn this session ran to the end, straight off the session row.
	//
	// It is the fact that separates the two endings an absent execution can
	// have, and reconciliation was deciding between them without it (P3-D §1):
	// a process that vanished under a worker still mid-turn is a phantom, and a
	// process that exited BECAUSE its turn finished is a completion. Both leave
	// the same runtime probe answer and — for as long as the lifecycle reaper's
	// recent-activity window has not elapsed — the same un-terminated row.
	//
	// Zero means the row carries no receipt, never that the turn did not
	// finish. Silence is not an ending here any more than anywhere else.
	RowTurnCompletedAt time.Time

	// LivenessKnown records that the runtime probe ANSWERED. False covers both
	// "no probe is wired" and "the probe could not tell", exactly as in
	// ObservedWorkerFacts, and neither is ever read as death.
	LivenessKnown bool
	LivenessAlive bool

	// NaturalKeyRead records that the dispatch key's own session lookup
	// actually ran and answered. Without it, "AO holds no session id" means
	// only that AO did not look.
	NaturalKeyRead bool
}

// Live reports a worker AO can prove is running under this identity.
func (o ownedExecution) Live() bool {
	if o.SessionID == "" || !o.Evidence.Observed || o.RowTerminated {
		return false
	}
	return !o.LivenessKnown || o.LivenessAlive
}

// ProvenGone reports positive evidence that nothing is running under this
// dispatch key: the session read back as absent, the row says terminated, the
// probe answered "not alive" — or the natural-key lookup ran and found no
// session at all.
func (o ownedExecution) ProvenGone() bool {
	if o.SessionID == "" {
		return o.NaturalKeyRead
	}
	switch {
	case o.Evidence.Missing, o.RowTerminated:
		return true
	case o.LivenessKnown && !o.LivenessAlive:
		return true
	default:
		return false
	}
}

// ExecutionProvenGone is the OWNERSHIP half of ProvenGone, on its own: the
// process/session AO launched demonstrably does not exist any more.
//
// It deliberately excludes the session row's own verdict. "The agent's session
// ended" and "the execution AO launched is gone" are different facts with
// different owners, and only this one belongs to dispatch.
func (o ownedExecution) ExecutionProvenGone() bool {
	if o.SessionID == "" {
		return false
	}
	return o.Evidence.Missing || (o.LivenessKnown && !o.LivenessAlive)
}

// PhantomRunning is the contradiction this file is named for: the records say a
// worker is running, and there is no execution behind them.
//
// Both halves are required, and the second one is the whole of the distinction
// between a phantom and a finished worker:
//
//   - the owned execution is PROVEN gone — the ownership proof read back absent,
//     or the runtime probe answered "not running". Silence never qualifies;
//   - and the session ROW still reads as a usable worker. That is what makes it
//     a phantom rather than a conclusion: work observation reads that row, sees
//     a worker that is fine, and therefore cannot resolve this state at all. It
//     is structurally blind to exactly this case, which is why reconciliation
//     owns it.
//
// A row that says terminated/exited is NOT a phantom, however dead the process
// is — in production every worker that finishes normally is exactly that: a
// terminated row and a runtime probe answering "not running". Whether such an
// ending was a completion or a failure is decided on commit evidence
// reconciliation never reads, so it belongs to observation, and taking it here
// would turn every finished worker into an incident.
func (o ownedExecution) PhantomRunning() bool {
	return o.ExecutionProvenGone() && o.RowFound && !o.RowTerminated
}

// TurnFinishedSince reports a durable turn receipt belonging to THIS dispatch:
// the provider stated it ran a turn to the end, at or after the moment this
// dispatch started.
//
// The `since` fence is the whole of its safety, and it is the same generation
// rule every other receipt in this package obeys. A receipt from an earlier
// generation's turn describes an execution this attempt did not start, and
// reading it here would let one dispatch's ending conclude another's. A zero
// `since` (no dispatch time AO can name) therefore answers false: an unfenced
// receipt is not this dispatch's receipt.
func (o ownedExecution) TurnFinishedSince(since time.Time) bool {
	if o.RowTurnCompletedAt.IsZero() || since.IsZero() {
		return false
	}
	return !o.RowTurnCompletedAt.Before(since)
}

// FinishedNormallySince is the ending reconciliation must NOT touch: the
// execution is gone, and there is a receipt for this dispatch saying it is gone
// because the provider finished.
//
// It outranks PhantomRunning, and has to. The two are observationally identical
// in every fact ownership can read — an absent process behind a row the
// lifecycle reaper has not caught up with yet — and the reaper cannot catch up
// promptly by design: it requires the row to have had NO activity for its
// recent-activity window, and a worker that just finished has activity seconds
// old. So there is a window, on every single successful headless run, in which
// a completed worker reads exactly like a phantom. A wake landing in it used to
// close the attempt and park the run on worker_dispatch_ambiguous with the
// worker's change sitting on disk (P3-D §1).
//
// What it is NOT is a completion verdict. Whether the finished turn produced
// anything is decided on workspace evidence this file never reads; all this
// says is that the ending belongs to work observation, which runs on the very
// next line of both callers and fails closed on its own terms.
func (o ownedExecution) FinishedNormallySince(since time.Time) bool {
	return o.ExecutionProvenGone() && o.TurnFinishedSince(since)
}

// Unprovable is the gap: neither live nor provably gone. The only correct
// answer to it is to stop with the evidence.
func (o ownedExecution) Unprovable() bool { return !o.Live() && !o.ProvenGone() }

// describe renders the ownership answer for the durable record and for the
// sentence a person reads. Same vocabulary as the evidence snapshot: a fact AO
// holds is `observed`, a fact it could not read is `unavailable`.
func (o ownedExecution) describe() string {
	switch {
	case o.Live():
		return fmt.Sprintf("session %s observed live", o.SessionID)
	case o.SessionID == "" && o.NaturalKeyRead:
		return "no session exists under this dispatch key"
	case o.SessionID == "":
		return "no session identity is recorded and none could be looked up"
	case o.RowFound && !o.RowTerminated && !o.RowTurnCompletedAt.IsZero() && o.ExecutionProvenGone():
		return fmt.Sprintf(
			"session %s completed a turn at %s and its execution has since exited",
			o.SessionID, o.RowTurnCompletedAt.UTC().Format(time.RFC3339))
	case o.Evidence.Missing && o.RowFound && !o.RowTerminated:
		return fmt.Sprintf(
			"the session row for %s still reads as a live worker and its owned execution no longer exists",
			o.SessionID)
	case o.Evidence.Missing:
		return fmt.Sprintf("session %s no longer exists", o.SessionID)
	case o.RowTerminated:
		return fmt.Sprintf("session %s is terminated", o.SessionID)
	case o.LivenessKnown && !o.LivenessAlive && o.RowFound:
		return fmt.Sprintf(
			"the session row for %s still reads as a live worker and the runtime says nothing is running under it",
			o.SessionID)
	case o.LivenessKnown && !o.LivenessAlive:
		return fmt.Sprintf("session %s answered the liveness probe as not running", o.SessionID)
	default:
		return fmt.Sprintf("session %s could not be read (%s)", o.SessionID,
			orValue(o.Evidence.Unavailable, "no reason given"))
	}
}

// observeOneOwnedExecution reads one session identity three ways: the ownership
// proof, the session row, and the runtime probe. Every one of them is optional
// and every unanswered one stays unanswered.
func (c *Coordinator) observeOneOwnedExecution(ctx stdctx.Context, id domain.SessionID) ownedExecution {
	o := ownedExecution{SessionID: id}
	o.Evidence = c.sessionOwnershipOrDefault().ObserveSessionOwnership(ctx, id)
	if c.sessionFacts != nil {
		if rec, found, err := c.sessionFacts.GetSession(ctx, id); err == nil && found {
			o.RowFound = true
			o.RowTerminated = rec.IsTerminated || rec.Activity.State == domain.ActivityExited
			o.RowTurnCompletedAt = rec.TurnCompletedAt
		}
	}
	if c.workerLiveness != nil {
		if alive, known, err := c.workerLiveness.SessionAlive(ctx, id); err == nil && known {
			o.LivenessAlive, o.LivenessKnown = alive, true
		}
	}
	return o
}

// observeDispatchOwnership answers "is there a live worker under this dispatch
// key" across every identity that key could be wearing.
//
// This IS the duplicate-wake protection. A worker launched by a pass that died
// before it could name it is findable only through the natural key, and a
// reconciler that did not look there would hand the step back to dispatch and
// put a second agent on the same worktree. A live hit anywhere ends the search:
// one live worker under a key is the answer, whichever row was holding it.
func (c *Coordinator) observeDispatchOwnership(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	status WorkerDispatchStatus,
) ownedExecution {
	var ids []domain.SessionID
	add := func(id string) {
		if id == "" {
			return
		}
		for _, existing := range ids {
			if string(existing) == id {
				return
			}
		}
		ids = append(ids, domain.SessionID(id))
	}
	add(status.SessionID)
	if step.SessionID != nil {
		add(*step.SessionID)
	}
	naturalKeyRead := false
	if c.sessionFacts != nil {
		rec, found, err := c.sessionFacts.FindSessionByProjectAndIssueID(
			ctx, domain.ProjectID(run.ProjectID), workStepIssueID(step.ID))
		if err == nil {
			naturalKeyRead = true
			if found {
				add(string(rec.ID))
			}
		}
	}

	best := ownedExecution{NaturalKeyRead: naturalKeyRead}
	for i, id := range ids {
		obs := c.observeOneOwnedExecution(ctx, id)
		obs.NaturalKeyRead = naturalKeyRead
		if obs.Live() {
			return obs
		}
		if i == 0 {
			best = obs
		}
	}
	return best
}

// ---- the sweep ---------------------------------------------------------------

// ReconcileDispatchEvidence is the boot-time and wake-triggered sweep: every
// non-terminal run's work step, read against its durable launch evidence.
//
// It returns what it resolved rather than only an error, because the caller
// (boot recovery, a wake) has to know whether the run it was about to advance
// has just been moved out from under it.
func (c *Coordinator) ReconcileDispatchEvidence(ctx stdctx.Context) ([]DispatchReconciliation, error) {
	runs, err := c.store.ListNonTerminalWorkflowRuns(ctx)
	if err != nil {
		return nil, err
	}
	var all []DispatchReconciliation
	for _, run := range runs {
		results, _, rerr := c.ReconcileRunDispatchEvidence(ctx, run)
		if rerr != nil {
			return all, rerr
		}
		all = append(all, results...)
	}
	return all, nil
}

// ReconcileRunDispatchEvidence reconciles one run's work steps and then settles
// the run itself against what that produced.
//
// The settlement is not cosmetic. A run whose only in-flight child was just
// closed or handed back to a retry is not running — nothing is running — and a
// run row that keeps saying `running` over it is the same class of lie the
// dispatch state machine was built to remove, one level up.
func (c *Coordinator) ReconcileRunDispatchEvidence(
	ctx stdctx.Context,
	run domain.WorkflowRun,
) ([]DispatchReconciliation, domain.WorkflowRun, error) {
	if run.State.Terminal() {
		return nil, run, nil
	}
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return nil, run, err
	}
	var results []DispatchReconciliation
	for _, step := range steps {
		if step.Kind != domain.WorkflowStepWork {
			continue
		}
		result, updatedRun, rerr := c.reconcileWorkStepDispatch(ctx, run, step)
		if rerr != nil {
			return results, run, rerr
		}
		run = updatedRun
		results = append(results, result)
	}
	run, err = c.settleRunAfterDispatchReconciliation(ctx, run, results)
	return results, run, err
}

// ReconcileWorkStepDispatch reconciles exactly one work step. Exported so the
// wake path can reconcile the step it was woken for without sweeping the world.
func (c *Coordinator) ReconcileWorkStepDispatch(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
) (DispatchReconciliation, domain.WorkflowRun, error) {
	if step.Kind != domain.WorkflowStepWork {
		return DispatchReconciliation{RunID: run.ID, StepID: step.ID, Action: DispatchReconcileNoop}, run, nil
	}
	result, run, err := c.reconcileWorkStepDispatch(ctx, run, step)
	if err != nil {
		return result, run, err
	}
	run, err = c.settleRunAfterDispatchReconciliation(ctx, run, []DispatchReconciliation{result})
	return result, run, err
}

// ---- one step ----------------------------------------------------------------

// reconcileWorkStepDispatch is the decision itself, in the order the decision
// has to be made: is anything alive under this key (nothing else matters if so),
// has this contradiction already been answered, and only then — what does the
// evidence say happened.
func (c *Coordinator) reconcileWorkStepDispatch(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
) (DispatchReconciliation, domain.WorkflowRun, error) {
	result := DispatchReconciliation{
		RunID:         run.ID,
		StepID:        step.ID,
		DispatchKey:   workStepOutboxIdempotencyKey(step.ID),
		Contradiction: ContradictionNone,
		Action:        DispatchReconcileNoop,
	}
	if run.State.Terminal() || step.State.Terminal() {
		result.Detail = "the run or step is terminal; there is nothing in flight to reconcile"
		return result, run, nil
	}

	if _, canRecord := c.dispatchRecorder(); !canRecord {
		// A resolution AO cannot record is one a duplicate wake cannot see, and
		// an invisible resolution is the second worker this file exists to
		// prevent. A store that cannot write dispatch boundaries therefore
		// reconciles nothing at all — the same refusal beginWorkerDispatch makes
		// before it launches.
		result.Detail = "this store cannot record dispatch boundaries, so no reconciliation may be attempted"
		return result, run, nil
	}

	status := c.WorkerDispatchStatusForStep(ctx, run.ID, step.ID)
	if !status.Readable {
		// The dispatch history could not be read at all. That is not "nothing
		// was dispatched" and must never be acted on as if it were.
		result.Detail = "the dispatch evidence for this step could not be read; nothing is concluded from silence"
		return result, run, nil
	}
	attempt, hasAttempt := c.openAttemptForStep(ctx, step.ID)
	if hasAttempt {
		result.AttemptID = attempt.ID
	}
	// Nothing to reconcile: no launch evidence, no open attempt, and a step
	// that is not claiming to run. This is simply a step that has not started.
	if status.Phase == WorkerDispatchNone && !hasAttempt && step.State != domain.WorkflowStepRunning {
		result.Detail = "this step has neither launch evidence nor anything claiming to be in flight"
		return result, run, nil
	}

	owned := c.observeDispatchOwnership(ctx, run, step, status)
	result.SessionID = string(owned.SessionID)
	// The outbox command is launch evidence in its own right, and the OLDEST
	// AO has. An entry sitting at `dispatched` says a launch was attempted and
	// its outcome was never recorded — which is the pre-state-machine ambiguity
	// adoptOrMarkAmbiguous already owns, adopting on evidence and escalating
	// otherwise. Reconciliation reads it so it can tell that shape apart from
	// the ones the dispatch boundaries actually explain.
	entry, entryFound, err := c.findDispatchOutboxEntry(ctx, run, step)
	if err != nil {
		return result, run, err
	}
	launchAttempted := entryFound &&
		(entry.Status == domain.WorkflowOutboxDispatched || entry.Status == domain.WorkflowOutboxAcknowledged)

	// (f) — and the same protection extended to every other phase. A live,
	// evidenced worker ends the pass here: not retried, not stopped, not killed,
	// and nothing written about it. It is also what makes a second wake safe
	// after the first one adopted a launch: the adopted worker is now live under
	// this key, so the second pass returns right here.
	if owned.Live() {
		result.Action = DispatchReconcileProtected
		result.Detail = fmt.Sprintf("a live worker owns this dispatch key (%s); reconciliation leaves it alone",
			owned.describe())
		if status.Phase == WorkerDispatchUnconfirmed || status.Phase == WorkerDispatchIntended {
			// A launch AO never managed to confirm, whose worker is demonstrably
			// alive. That is case (c), and the answer is to make the
			// confirmation durable NOW rather than to leave the step outside
			// RUNNING over a worker that is running.
			return c.adoptLiveLaunch(ctx, run, step, attempt, hasAttempt, owned, result)
		}
		return result, run, nil
	}

	// Already answered, and still answered: the reconciliation boundary is the
	// newest thing in this step's launch history, so nothing has happened since
	// the pass that wrote it. That is what makes a duplicate wake a no-op.
	//
	// It is checked AFTER the live-worker branch above on purpose. A stop taken
	// because a session could not be READ must not become permanent if that
	// session later reads back alive — the worker would be running with AO
	// still refusing to recognise it. Liveness reopens the question; nothing
	// else does.
	if status.Record.Phase == domain.DispatchPhaseWorkerDispatchReconciled {
		result.Detail = "this contradiction was already reconciled and nothing has happened since"
		return result, run, nil
	}

	switch status.Phase {
	case WorkerDispatchConfirmed:
		if step.State == domain.WorkflowStepRunning && step.SessionID != nil && *step.SessionID != "" {
			// A confirmed launch that reached RUNNING with its session on the
			// step is one of two very different things, and the split is the
			// whole of case (e).
			//
			// PHANTOM: the session row still reads as a usable worker and the
			// execution behind it is proven gone. Work observation reads that
			// row, sees a healthy worker and leaves the step running — it is
			// structurally blind to this, because the only facts that reveal it
			// are the ownership proof and the runtime probe, neither of which it
			// consults. So reconciliation resolves it, below.
			//
			// NOT a phantom: a session row that says terminated/exited. That is
			// a worker that ENDED, and whether the ending was a completion is
			// decided on commit evidence reconciliation never reads. In
			// production every normal finish looks exactly like this — terminated
			// row, runtime probe answering "not running" — so taking it here
			// would turn every completed worker into an incident. It belongs to
			// observation, which is on the very next line of both callers.
			//
			// And the third thing it can be, which is neither and which used
			// to be read as the first: a worker that finished NORMALLY and
			// whose row the lifecycle reaper has not marked yet. Its receipt
			// says the provider ran this dispatch's turn to the end, so its
			// absent process is an ending rather than a disappearance — see
			// FinishedNormallySince, and P3-D §1 for the incident.
			if owned.FinishedNormallySince(dispatchStartedAt(attempt, hasAttempt, status)) {
				result.Detail = fmt.Sprintf(
					"this worker completed its turn for this dispatch and then exited; what it produced is work observation's question (%s)",
					owned.describe())
				return result, run, nil
			}
			if !owned.PhantomRunning() {
				result.Detail = fmt.Sprintf(
					"a confirmed, running launch whose execution is not proven gone behind a live-reading row belongs to work observation (%s)",
					owned.describe())
				return result, run, nil
			}
			result.Contradiction = ContradictionStaleRunning
			result.Detail = fmt.Sprintf(
				"attempt %s is RUNNING over session %s and there is no execution behind it: %s",
				orValue(result.AttemptID, "(none)"), *step.SessionID, owned.describe())
			break
		}
		// The other shape a confirmation can leave: the boundary is durable and
		// the transition after it is not — the step never reached RUNNING, or
		// never got its session written. That IS stale: an attempt claiming to
		// be in flight with nothing tracking what it is in flight over.
		result.Contradiction = ContradictionStaleRunning
		result.Detail = fmt.Sprintf(
			"the launch for attempt %s was confirmed, its RUNNING transition never completed, and its execution is gone: %s",
			orValue(result.AttemptID, "(none)"), owned.describe())
	case WorkerDispatchUnconfirmed:
		result.Contradiction = ContradictionLaunchUnconfirmed
		result.Detail = fmt.Sprintf(
			"a worker was launched and never durably confirmed, and it is not there now: %s", owned.describe())
	case WorkerDispatchFailed:
		if _, routed := c.latestWorkerLaunchRecord(ctx, run.ID, step.ID); routed {
			// The failure was recorded AND answered by the retry policy. The
			// wake owns this step; reconciliation must not burn its budget.
			result.Detail = "this launch failure was already routed into the bounded retry policy"
			return result, run, nil
		}
		result.Contradiction = ContradictionLaunchFailed
		result.Detail = fmt.Sprintf(
			"the launch for attempt %s is proven to have failed and its failure was never routed",
			orValue(result.AttemptID, "(none)"))
	case WorkerDispatchIntended:
		if _, routed := c.latestWorkerLaunchRecord(ctx, run.ID, step.ID); routed {
			result.Detail = "this dispatch intent was already routed into the bounded retry policy"
			return result, run, nil
		}
		// An intent is also what a launch happening RIGHT NOW looks like: the
		// record is written before the launcher is invoked, so it is the newest
		// boundary for as long as that call is in flight. Nothing is concluded
		// from one younger than the settle window — the same window
		// adoptOrMarkAmbiguous gives an in-flight dispatch, for the same reason.
		if c.clock().Sub(status.Record.CreatedAt) < dispatchReconcileSettleWindow {
			result.Detail = "a dispatch intent this recent may be a launch still in flight; nothing is concluded yet"
			return result, run, nil
		}
		result.Contradiction = ContradictionIntentNeverLaunched
		result.Detail = fmt.Sprintf(
			"a dispatch intent for attempt %s was recorded and nothing concluded it: %s",
			orValue(result.AttemptID, "(none)"), owned.describe())
	default: // WorkerDispatchNone
		if launchAttempted {
			// A dispatched command with no boundary under it at all. AO cannot
			// say from these rows whether a launch completed, and the reconciler
			// must not invent an answer the dispatch path already has a correct
			// one for: adoptOrMarkAmbiguous adopts a real session on evidence
			// and escalates otherwise, and never launches a second worker.
			result.Detail = "a dispatched command with no launch boundary under it belongs to dispatch adoption, not to reconciliation"
			return result, run, nil
		}
		if step.State != domain.WorkflowStepRunning {
			result.Detail = "this step carries no launch evidence and is not claiming to be in flight"
			return result, run, nil
		}
		result.Contradiction = ContradictionRunningWithoutEvidence
		result.Detail = fmt.Sprintf(
			"this step claims to be RUNNING and no launch was ever recorded or even commanded for it: %s",
			owned.describe())
	}

	// The rule that outranks every case above: AO could not prove which of them
	// it is looking at. Stop with the evidence rather than act on a guess.
	if owned.Unprovable() {
		result.Contradiction = ContradictionUnprovable
		result.Detail = fmt.Sprintf(
			"AO cannot prove what happened to this dispatch (%s): %s",
			orValue(string(status.Phase), "unknown phase"), owned.describe())
		return c.stopReconciledDispatch(ctx, run, step, attempt, hasAttempt, owned, result)
	}
	return c.retryReconciledDispatch(ctx, run, step, attempt, hasAttempt, owned, result)
}

// dispatchStartedAt is the moment THIS dispatch began, and the fence every
// receipt in this file is read against.
//
// The open attempt's start is the truest answer: the attempt row is opened at
// dispatch intent, before the launcher is invoked, so it dates the dispatch
// rather than anything that happened to it afterwards. The durable boundary's
// own timestamp is the fallback for a step whose attempt has already been
// concluded. A step with neither is one AO cannot date, and it answers zero —
// which every caller reads as "no receipt belongs to this dispatch".
func dispatchStartedAt(attempt domain.WorkflowAttempt, hasAttempt bool, status WorkerDispatchStatus) time.Time {
	if hasAttempt && !attempt.StartedAt.IsZero() {
		return attempt.StartedAt
	}
	return status.Record.CreatedAt
}

// openAttemptForStep returns the step's currently open (unconcluded) attempt.
// An attempt row with no outcome is the thing that claims work is in flight,
// which is exactly what reconciliation is here to check.
func (c *Coordinator) openAttemptForStep(ctx stdctx.Context, stepID string) (domain.WorkflowAttempt, bool) {
	attempts, err := c.store.ListWorkflowAttempts(ctx, stepID)
	if err != nil || len(attempts) == 0 {
		return domain.WorkflowAttempt{}, false
	}
	latest := attempts[len(attempts)-1]
	if latest.Outcome != "" {
		return domain.WorkflowAttempt{}, false
	}
	return latest, true
}

// ---- (c) adoption ------------------------------------------------------------

// adoptLiveLaunch is case (c): a launch AO could not confirm, whose worker is
// alive right now. It rejoins the state machine at phase 3 through the SAME
// confirmation path a fresh launch takes (confirmWorkerDispatch), so an adopted
// worker and a launched one leave identical durable trails — and so RUNNING is
// still gated on a confirmation that is durable first.
//
// Nothing is launched here, and nothing can be: the launcher is not reachable
// from this file.
func (c *Coordinator) adoptLiveLaunch(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
	hasAttempt bool,
	owned ownedExecution,
	result DispatchReconciliation,
) (DispatchReconciliation, domain.WorkflowRun, error) {
	if c.sessionFacts == nil {
		return result, run, nil
	}
	rec, readable := c.readableSessionRecord(ctx, owned.SessionID)
	if !readable {
		// The ownership proof said live and the session row cannot be read
		// back. That is a contradiction of its own, and the safe answer is the
		// one this whole file defaults to: leave the live worker alone.
		return result, run, nil
	}
	// Adopt the launch that is ALIVE -- not merely a session row under the same
	// dispatch key.
	//
	// A session id survives a daemon restart; the process behind it does not.
	// RuntimeLaunchID is the only identity that separates those two: the
	// supervisor carries it in its own argv, so the runtime can answer "is the
	// process I started still running" rather than "does a pane with this name
	// exist". If the durable launch evidence names a launch id and the session
	// now carries a different one, this row has been relaunched by something
	// that is not the launch AO recorded, and confirming it would bind this
	// step's attempt to an execution nobody here authorized.
	//
	// Silence is not a mismatch. Evidence or a session row that names no launch
	// id at all (an older row, a runtime that does not report one) is not
	// contradicted by anything, and adoption proceeds on the liveness proof it
	// always did -- the fence refuses only a stated DISAGREEMENT.
	if recorded := c.recordedLaunchIDForStep(ctx, run.ID, step.ID); recorded != "" &&
		rec.Metadata.RuntimeLaunchID != "" && recorded != rec.Metadata.RuntimeLaunchID {
		result.Contradiction = ContradictionUnprovable
		result.Detail = fmt.Sprintf(
			"session %s is alive under runtime launch %s, but the launch AO recorded for this step was %s — this is not the execution AO started, and reconciliation may not adopt it",
			owned.SessionID, shortFingerprint(rec.Metadata.RuntimeLaunchID), shortFingerprint(recorded))
		return c.stopReconciledDispatch(ctx, run, step, attempt, hasAttempt, owned, result)
	}
	entry, err := c.dispatchOutboxEntry(ctx, run, step)
	if err != nil {
		return result, run, err
	}
	result.Contradiction = ContradictionLaunchUnconfirmed
	result.Action = DispatchReconcileAdopted
	result.Detail = fmt.Sprintf(
		"worker session %s was launched and never confirmed; its confirmation is recorded now, without relaunching",
		owned.SessionID)
	if err := c.recordDispatchReconciliation(ctx, run, step, entry, attempt, owned, result,
		domain.LaunchStageConfirm, domain.LaunchOutcomeIntended); err != nil {
		return result, run, err
	}
	if !hasAttempt {
		opened, oerr := c.openWorkerAttempt(ctx, step.ID, rec.Harness, c.clock())
		if oerr != nil {
			return result, run, oerr
		}
		attempt = opened
		result.AttemptID = opened.ID
	}
	// A step parked by an earlier reconciliation stop has to come back before
	// the confirmation lands on it: confirmWorkerDispatch moves ready -> running
	// and nothing else, so a `waiting` step would end up holding a live worker's
	// session while still saying it is not running.
	if step.State == domain.WorkflowStepWaiting {
		if _, serr := c.store.UpdateWorkflowStepState(ctx, step.ID,
			domain.WorkflowStepWaiting, domain.WorkflowStepRunning, c.clock()); serr != nil {
			return result, run, serr
		}
		step.State = domain.WorkflowStepRunning
	}
	// The adoption joins the claim ALREADY on the row -- it is the claim the
	// launch being adopted was made under, and re-stamping it would make this
	// adopter look like a different launch to every generation-fenced
	// transition. An entry claimed before the worker path stamped tokens carries
	// "", which is what completes an unclaimed row.
	intent := workerDispatchIntent{attempt: attempt, harness: rec.Harness, generation: entry.DispatchGeneration}
	_, err = c.confirmWorkerDispatch(ctx, run, step, entry, intent,
		WorkerLaunchResult{Session: rec}, owned.Evidence)
	if err != nil {
		return result, run, err
	}
	if refreshed, ok, rerr := c.store.GetWorkflowRun(ctx, run.ID); rerr == nil && ok {
		run = refreshed
	}
	return result, run, nil
}

// ---- close + retry per policy -------------------------------------------------

// retryReconciledDispatch closes the stale attempt and hands the step back to
// the bounded launch-retry policy — or, when that policy would not retry, stops
// with the evidence.
//
// The policy is ASKED rather than pushed through, because the two outcomes have
// different obligations: a retry is not an incident and owes no snapshot, while
// a stop is one and owes the full one. Letting the failure path decide would
// produce a stop whose evidence was never collected, which is precisely the
// unevidenced dead end the evidence gate exists to abolish.
func (c *Coordinator) retryReconciledDispatch(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
	hasAttempt bool,
	owned ownedExecution,
	result DispatchReconciliation,
) (DispatchReconciliation, domain.WorkflowRun, error) {
	// A step that durably owns a session cannot be re-dispatched: dispatch
	// refuses to launch over an associated session (and rightly — that guard is
	// what stops two workers on one worktree), and no reconciler may release
	// another component's ownership. So this is a human decision, with the
	// evidence.
	//
	// This is the phantom's route. The contradiction is still CLOSED here — the
	// stop below concludes the open attempt, so the fossil that told every
	// downstream guard "a writer is live in this tree" is gone, and the step
	// leaves RUNNING — but the retry half is genuinely unavailable while the
	// step owns a session AO has no way to release. The bounded needs_attention
	// path carries the full launch evidence to a person instead.
	if step.SessionID != nil && *step.SessionID != "" {
		result.Detail = fmt.Sprintf("%s — and the step still owns session %s, which reconciliation may not release",
			result.Detail, *step.SessionID)
		return c.stopReconciledDispatch(ctx, run, step, attempt, hasAttempt, owned, result)
	}

	cause := fmt.Errorf("crash reconciliation: %s", result.Detail)
	cls := classifyWorkerLaunchFailure(cause)
	if !c.workerLaunchRetryAllowed(ctx, run.ID, step.ID, cls) {
		result.Detail = fmt.Sprintf("%s — and the bounded launch-retry budget is spent", result.Detail)
		return c.stopReconciledDispatch(ctx, run, step, attempt, hasAttempt, owned, result)
	}

	entry, err := c.dispatchOutboxEntry(ctx, run, step)
	if err != nil {
		return result, run, err
	}
	result.Action = DispatchReconcileRetryScheduled
	if err := c.recordDispatchReconciliation(ctx, run, step, entry, attempt, owned, result,
		reconcileLaunchStage(result.Contradiction), domain.LaunchOutcomeFailed); err != nil {
		return result, run, err
	}
	if hasAttempt {
		// The attempt row is what claims work is in flight. It is closed BEFORE
		// the retry is scheduled, so no guard downstream can read a fossil as a
		// live writer while the retry is pending.
		// The claim form: exactly one caller may conclude an attempt, and a
		// pass that loses is a no-op rather than an overwriter of somebody
		// else's outcome. See concludeWorkerAttemptFailure.
		if _, aerr := c.store.ClaimWorkflowAttemptOutcome(ctx, attempt.ID, c.clock(),
			domain.WorkflowAttemptFailed, domain.WorkflowErrorRuntimeFailed); aerr != nil {
			return result, run, aerr
		}
	}
	// A step parked at `waiting` is never re-entered by dispatch, so a retry
	// scheduled over one would never fire. It comes back to `running` — the
	// same state the launch-retry policy leaves a step it is about to redo, and
	// the state dispatch re-enters. (`ready` would be truer still and the domain
	// has no edge back to it; `running` here means "a retry owns this", which is
	// exactly what the closed attempt and the pending outbox row say.)
	if step.State == domain.WorkflowStepWaiting {
		if _, serr := c.store.UpdateWorkflowStepState(ctx, step.ID,
			domain.WorkflowStepWaiting, domain.WorkflowStepRunning, c.clock()); serr != nil {
			return result, run, serr
		}
		step.State = domain.WorkflowStepRunning
	}
	// recordWorkerLaunchFailure owns the rest: the durable classification, the
	// outbox back to Pending under the same idempotency key, and the wake that
	// carries the retry. Reconciliation never launches anything itself.
	if _, ferr := c.recordWorkerLaunchFailure(ctx, run, step, entry,
		reconcileHarness(attempt, hasAttempt), workerLaunchStageIntent, cause); ferr != nil {
		return result, run, ferr
	}
	if refreshed, ok, rerr := c.store.GetWorkflowRun(ctx, run.ID); rerr == nil && ok {
		run = refreshed
	}
	return result, run, nil
}

// ---- stop, with the evidence --------------------------------------------------

// stopReconciledDispatch is the only way this file produces a needs_attention,
// and it produces it through the package's one evidence gate: the bounded
// snapshot is collected and made durable BEFORE any state moves, and the raise
// is refused outright if it could not be.
//
// The launch/dispatch evidence travels automatically — the collector already
// reads the dispatch table, and the reconciliation boundary this function
// records first is part of what it reads. There is no second evidence model
// here, which is the whole point: the Incident Advisor gets the launch history
// for a reconciliation stop by the same route it gets it for every other stop.
func (c *Coordinator) stopReconciledDispatch(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
	hasAttempt bool,
	owned ownedExecution,
	result DispatchReconciliation,
) (DispatchReconciliation, domain.WorkflowRun, error) {
	entry, err := c.dispatchOutboxEntry(ctx, run, step)
	if err != nil {
		return result, run, err
	}
	result.Action = DispatchReconcileNeedsAttention
	// Recorded FIRST, so the snapshot collected below contains it: a stop whose
	// evidence does not include the reconciliation that caused it is a stop
	// nobody can check.
	if err := c.recordDispatchReconciliation(ctx, run, step, entry, attempt, owned, result,
		reconcileLaunchStage(result.Contradiction), domain.LaunchOutcomeAmbiguous); err != nil {
		return result, run, err
	}

	detail := fmt.Sprintf("%s: %s", ReasonWorkerDispatchAmbiguous, result.Detail)
	raise, rerr := c.raiseAmbiguousWorkerState(ctx, run, step,
		ReasonWorkerDispatchAmbiguous, detail,
		c.observedWorkerFactsFor(ctx, owned.SessionID, nil))
	if rerr != nil {
		// The gate refused because the evidence could not be made durable. The
		// step is left exactly as it was and the next pass tries again — an
		// unevidenced stop on the ledger would be permanent, while an
		// unresolved contradiction is still true in three seconds.
		return result, run, rerr
	}
	now := c.clock()
	if hasAttempt {
		if _, aerr := c.store.ClaimWorkflowAttemptOutcome(ctx, attempt.ID, now,
			domain.WorkflowAttemptFailed, raise.ErrorClass()); aerr != nil {
			return result, run, aerr
		}
	}
	if step.State == domain.WorkflowStepRunning || step.State == domain.WorkflowStepReady {
		if _, serr := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State,
			domain.WorkflowStepWaiting, now); serr != nil {
			return result, run, serr
		}
		step.State = domain.WorkflowStepWaiting
	}
	c.recordAttentionStop(ctx, run, &step.ID, ReasonWorkerDispatchAmbiguous, result.Detail)
	if c.log != nil {
		c.log.Warn("workflow: dispatch reconciliation stopped with evidence",
			"run", run.ID, "step", step.ID, "contradiction", result.Contradiction, "detail", result.Detail)
	}
	return result, run, nil
}

// ---- the durable boundary ------------------------------------------------------

// recordDispatchReconciliation appends the reconciliation's own row to the SAME
// append-only dispatch history every other launch boundary is written to. It
// carries the contradiction, the remedy, and the ownership answer that decided
// between them.
//
// It is mandatory, not best-effort. A reconciliation AO cannot record is one a
// duplicate wake cannot see, and a duplicate wake that cannot see it is the
// second worker this file exists to prevent.
func (c *Coordinator) recordDispatchReconciliation(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	attempt domain.WorkflowAttempt,
	owned ownedExecution,
	result DispatchReconciliation,
	stage domain.WorkflowLaunchStage,
	outcome domain.WorkflowLaunchOutcome,
) error {
	return c.recordDispatchBoundary(ctx, dispatchBoundary{
		run: run, step: step, entry: entry,
		attempt: result.AttemptID,
		harness: domain.AgentHarness(attempt.Harness),
		phase:   domain.DispatchPhaseWorkerDispatchReconciled,
		stage:   stage,
		outcome: outcome,
		// The error class is deliberately NOT set here. ambiguous_worker_state
		// may only come from the evidence gate, and no other class is a truthful
		// description of a reconciliation.
		sessionID:       string(owned.SessionID),
		detail:          result.Detail,
		runtimeHandleID: owned.Evidence.RuntimeHandleID,
		runtimeLaunchID: owned.Evidence.RuntimeLaunchID,
		agentSessionID:  owned.Evidence.AgentSessionID,
		branch:          owned.Evidence.Branch,
		worktreePath:    owned.Evidence.WorktreePath,
		baseSHA:         owned.Evidence.BaseSHA,
		evidence: map[string]string{
			"contradiction": string(result.Contradiction),
			"action":        string(result.Action),
			"dispatchKey":   result.DispatchKey,
			"ownership":     ownershipEvidenceStatus(owned.Evidence),
			"execution":     owned.describe(),
			"stepState":     string(step.State),
		},
	})
}

// reconcileLaunchStage maps a contradiction back to the launch stage it is
// about, so the reconciliation row sits in the same stage vocabulary as the
// boundary it is answering.
func reconcileLaunchStage(contradiction DispatchContradiction) domain.WorkflowLaunchStage {
	switch contradiction {
	case ContradictionLaunchFailed:
		return domain.LaunchStageSpawn
	case ContradictionLaunchUnconfirmed, ContradictionStaleRunning:
		return domain.LaunchStageConfirm
	default:
		return domain.LaunchStageIntent
	}
}

// reconcileHarness names the harness the stale attempt was for. Empty when
// there is no attempt to name one: an invented harness on a retry record would
// route the next launch at a provider nobody chose.
func reconcileHarness(attempt domain.WorkflowAttempt, hasAttempt bool) domain.AgentHarness {
	if !hasAttempt {
		return ""
	}
	return domain.AgentHarness(attempt.Harness)
}

// dispatchReconcileSettleWindow is how recent a dispatch boundary may be before
// reconciliation refuses to conclude anything from it. It mirrors
// adoptOrMarkAmbiguous's own in-flight window because it answers the same
// question — "could this still be happening right now?" — and two different
// answers to that would be two different definitions of a launch in flight.
const dispatchReconcileSettleWindow = 30 * time.Second

// findDispatchOutboxEntry reads the step's outbox command WITHOUT creating one.
// Reconciliation asks this before it classifies anything: an absent command and
// a dispatched one are different facts, and enqueueing here would destroy the
// difference before it could be read.
func (c *Coordinator) findDispatchOutboxEntry(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
) (domain.WorkflowOutboxEntry, bool, error) {
	entries, err := c.store.ListWorkflowOutboxByRun(ctx, run.ID)
	if err != nil {
		return domain.WorkflowOutboxEntry{}, false, err
	}
	key := workStepOutboxIdempotencyKey(step.ID)
	for _, e := range entries {
		if e.IdempotencyKey == key {
			return e, true, nil
		}
	}
	return domain.WorkflowOutboxEntry{}, false, nil
}

// dispatchOutboxEntry resolves the step's outbox command — the dispatch key's
// own row. Enqueue is idempotent on that key, so a step whose entry was lost
// (or never written) gets the same single row back rather than a second one.
func (c *Coordinator) dispatchOutboxEntry(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
) (domain.WorkflowOutboxEntry, error) {
	key := workStepOutboxIdempotencyKey(step.ID)
	entries, err := c.store.ListWorkflowOutboxByRun(ctx, run.ID)
	if err != nil {
		return domain.WorkflowOutboxEntry{}, err
	}
	for _, e := range entries {
		if e.IdempotencyKey == key {
			return e, nil
		}
	}
	entry, _, err := c.store.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID:             "wfo-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &step.ID,
		IdempotencyKey: key,
		CommandType:    domain.WorkflowOutboxSpawnWorkerSession,
		Payload:        spawnPayloadJSON(run.ProjectID, step.ID),
		CreatedAt:      c.clock(),
	})
	return entry, err
}

// ---- the parent -----------------------------------------------------------------

// settleRunAfterDispatchReconciliation makes the run row agree with what
// reconciliation just did to its children.
//
// Derived, not asserted: the run's state is read back off the steps after the
// fact rather than assumed from the action taken, because a run with two work
// steps can have one closed and one still live and only the steps know that.
//
//	any child stopped for a human      -> needs_attention
//	any child actually running         -> running
//	otherwise, something was retried   -> waiting (nothing is in flight; a wake owns it)
//
// A pass that resolved nothing never writes, and an illegal transition is never
// forced: needs_attention has no edge back to waiting, and a run parked there
// stays parked until something legitimately restarts it.
func (c *Coordinator) settleRunAfterDispatchReconciliation(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	results []DispatchReconciliation,
) (domain.WorkflowRun, error) {
	resolved := false
	stopped := false
	retried := false
	adopted := false
	reconciled := map[string]bool{}
	for _, r := range results {
		if !r.Action.Resolved() {
			continue
		}
		resolved = true
		reconciled[r.StepID] = true
		switch r.Action {
		case DispatchReconcileNeedsAttention:
			stopped = true
		case DispatchReconcileRetryScheduled:
			retried = true
		case DispatchReconcileAdopted:
			adopted = true
		}
	}
	if !resolved {
		return run, nil
	}
	if refreshed, ok, err := c.store.GetWorkflowRun(ctx, run.ID); err == nil && ok {
		run = refreshed
	}
	if run.State.Terminal() {
		return run, nil
	}
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return run, err
	}
	// A step this pass RETRIED is not in flight, whatever its row says: the
	// retry is what proved nothing is running in it, and a step left at
	// `running` so the bounded retry can re-enter dispatch must not be counted
	// as a live child. Only steps reconciliation did not touch answer for
	// themselves.
	inFlight := false
	for _, step := range steps {
		if step.Kind != domain.WorkflowStepWork || reconciled[step.ID] {
			continue
		}
		if step.State == domain.WorkflowStepRunning {
			inFlight = true
			break
		}
	}

	var next domain.WorkflowRunState
	switch {
	case stopped:
		next = domain.WorkflowRunNeedsAttention
	case adopted || inFlight:
		next = domain.WorkflowRunRunning
	case retried:
		next = domain.WorkflowRunWaiting
	default:
		return run, nil
	}
	return c.moveRunTo(ctx, run, next, c.clock())
}

// moveRunTo applies one derived run transition, refusing anything the domain
// does not allow rather than asking the store to reject it.
func (c *Coordinator) moveRunTo(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	next domain.WorkflowRunState,
	now time.Time,
) (domain.WorkflowRun, error) {
	if next == run.State || !domain.ValidWorkflowRunTransition(run.State, next) {
		return run, nil
	}
	moved, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, next, now)
	if err != nil {
		return run, err
	}
	if moved {
		run.State = next
	}
	return run, nil
}

// readableSessionRecord reads one session row, reporting only whether it could
// be read at all. The read error is deliberately not returned: every caller here
// treats "could not read" and "not there" the same way — by doing nothing — and
// a reconciliation that failed because a session lookup hiccuped would take the
// whole boot sweep down with it.
func (c *Coordinator) readableSessionRecord(
	ctx stdctx.Context,
	id domain.SessionID,
) (domain.SessionRecord, bool) {
	if c.sessionFacts == nil || id == "" {
		return domain.SessionRecord{}, false
	}
	rec, found, err := c.sessionFacts.GetSession(ctx, id)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: reading a session for dispatch reconciliation failed",
				"session", id, "err", err)
		}
		return domain.SessionRecord{}, false
	}
	return rec, found
}

// getWorkflowStep re-reads one step by id. Reconciliation may have moved the
// step under its caller, and a caller that went on using the value it had would
// dispatch against a state that no longer exists.
func (c *Coordinator) getWorkflowStep(
	ctx stdctx.Context,
	runID, stepID string,
) (domain.WorkflowStep, bool, error) {
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return domain.WorkflowStep{}, false, err
	}
	for _, step := range steps {
		if step.ID == stepID {
			return step, true, nil
		}
	}
	return domain.WorkflowStep{}, false, nil
}

// recordedLaunchIDForStep returns the runtime launch id AO durably recorded for
// this step's newest launch, or "" when nothing recorded one.
//
// It reads the dispatch table first because that is where a confirmation and an
// unconfirmed launch both write the id, and falls back to the ledger's own
// unconfirmed record for the one case the dispatch table cannot hold (the write
// that failed BECAUSE that table refused it). "" means AO holds no statement
// about which launch this step's session should be running under, which is a
// different fact from a mismatch and is never treated as one.
func (c *Coordinator) recordedLaunchIDForStep(ctx stdctx.Context, runID, stepID string) string {
	if ps, ok := c.provenanceStore(); ok && stepID != "" {
		if records, err := ps.ListWorkflowDispatchCheckpointsByStep(ctx, stepID); err == nil {
			for i := len(records) - 1; i >= 0; i-- {
				if id := strings.TrimSpace(records[i].RuntimeLaunchID); id != "" {
					return id
				}
			}
		}
	}
	if rec, ok := c.latestUnconfirmedLaunchRecord(ctx, runID, stepID); ok {
		return strings.TrimSpace(rec.RuntimeLaunchID)
	}
	return ""
}
