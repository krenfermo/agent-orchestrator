package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// Spawner is the narrow session-creation write path workflow reuses. It is
// satisfied by *session_manager.Manager; workflow never constructs sessions
// itself. Prompt delivery happens inside Spawn itself (per cfg.Prompt), so
// workflow never calls a separate send-message path for the initial task
// prompt.
type Spawner interface {
	Spawn(ctx stdctx.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error)
}

// RuntimeIsolation resolves a workflow run's owner-scoped provider
// subprocess env (Checkpoint 8P-B.1) -- the single canonical
// implementation every dispatch site (worker, reviewer, planner, decision
// resolver) calls, rather than each re-deriving owner/profile lookup
// itself. See providerruntime.Resolver for the concrete implementation.
// Optional: nil means every dispatch site behaves exactly as it did before
// this checkpoint (no env override, never blocks).
// Checkpoint 8P-C extends Resolve's return with the matched
// domain.ProviderProfileID (empty if unresolved/no profile matched), so
// capacity/health scoping (workflow/health.go) and routing both key off the
// exact same resolution instead of re-deriving owner/profile independently.
type RuntimeIsolation interface {
	Resolve(ctx stdctx.Context, runID string, harness domain.AgentHarness) (env map[string]string, owner domain.UserID, profileID domain.ProviderProfileID, err error)
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
// It is read-only, and narrowed back to read-only by Task 5.
// MaterializeIntegrationCommit used to be here, because 8M.1's master-task
// promotion captured a worktree's content into an AO-owned commit. That route
// is gone — every ready task now reaches its target through the Integration
// Coordinator — and keeping the method on the interface workflow depends on
// would leave the door to it open for the next change that wanted a shortcut.
// The adapters still implement it; workflow simply no longer asks.
type WorkspaceFacts interface {
	ObserveWorkspace(ctx stdctx.Context, info ports.WorkspaceInfo) (ports.WorkspaceObservation, error)
}

// workStepIssueIDPrefix is the namespace every work-step natural key carries.
const workStepIssueIDPrefix = "workflow-step:"

// workStepIssueID is the durable natural key correlating a workflow work
// step to the session spawned for it, independent of whether workflow's own
// outbox/checkpoint bookkeeping made it to disk.
func workStepIssueID(stepID string) domain.IssueID {
	return domain.IssueID(workStepIssueIDPrefix + stepID)
}

// WorkStepIDFromIssueID reverses workStepIssueID, recovering the work step a
// spawn belongs to from the natural key it carries. It returns "" for any
// issue id this package did not mint, so a tracker-issued id is never mistaken
// for a step id.
//
// It is exported for out-of-package observers (the project-memory baseline
// instrumentation wraps Spawner and sees only ports.SpawnConfig) so the key
// format stays owned by the package that defines it instead of being
// duplicated at the observation site.
func WorkStepIDFromIssueID(id domain.IssueID) string {
	rest, ok := strings.CutPrefix(string(id), workStepIssueIDPrefix)
	if !ok {
		return ""
	}
	return rest
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
//
// humanResume marks the one call site a person (or the API's Continue) drives
// — mirroring dispatchReviewStep's parameter of the same name. It only ever
// relaxes the automatic retry PACING (worker_launch_recovery.go's
// workerLaunchRetryDelay): a person asking now means now. It never relaxes an
// idempotency guard, and reopening a durably failed dispatch is not done here
// at all but in resumeWorkerLaunchAfterFailure, before this is reached.
func (c *Coordinator) dispatchWorkStep(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, prompt string, humanResume bool) (domain.WorkflowStep, error) {
	// Cancellation (or any other terminal transition) must never race with a
	// late-arriving dispatch.
	if run.State.Terminal() || step.State.Terminal() {
		return step, nil
	}
	// Checkpoint 8K-A: an unresolved question on this step means the worker
	// is paused on a decision — never dispatch (or re-dispatch) into that.
	if open, err := c.hasOpenQuestion(ctx, run.ID, &step.ID); err != nil {
		return step, err
	} else if open {
		return step, nil
	}
	// Primary, cheapest guard: a session is already durably associated.
	if step.SessionID != nil {
		return step, nil
	}
	if c.workerLauncherOrDefault() == nil {
		// No launcher wired (e.g. a unit test exercising only the durable
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
		// A launch failure AO has scheduled its own bounded retry for owns this
		// step until that retry is due (worker_launch_recovery.go). Without this
		// floor every other entry point into dispatch — boot Reconcile, a
		// capacity wake, a master reconcile pass — would front-run the wake and
		// burn the whole attempt budget inside one second, before the transient
		// condition it is waiting out had any chance to clear.
		if rec, ok := c.latestWorkerLaunchRecord(ctx, run.ID, step.ID); ok && !rec.dueForRetry(c.clock()) && !humanResume {
			// Backing off is only safe if something will actually come back.
			// recordWorkerLaunchFailure schedules the wake that carries this
			// retry, but that write is best-effort by construction (a nil
			// scheduler, a store hiccup) and a crash can land between the
			// release and it -- leaving an outbox row pending, a retry floor
			// that every entry point respects, and nothing whatsoever due to
			// fire. Work steps are not re-entered by read-time polling, so that
			// state waits for the next daemon boot.
			//
			// Re-ensuring here costs nothing and closes it: Schedule is
			// idempotent per (run, step, reason) and leaves a row that is
			// already pending completely untouched -- it does not bump the
			// attempt count and does not push the due time out, which is the
			// exact regression the wake scheduler's own pending branch exists to
			// prevent. So N backed-off passes produce at most one row and, after
			// the first, no writes at all.
			c.scheduleWake(ctx, run, stepIDPtr(step.ID), wake.ReasonTransientRetry, step.AssignedHarness)
			return step, nil
		}
		return c.dispatchFromPending(ctx, run, step, entry, prompt)
	case domain.WorkflowOutboxDispatched, domain.WorkflowOutboxAcknowledged:
		// Dispatched: a previous attempt got at least as far as "about to call
		// Spawn," but we don't durably know if Spawn itself completed.
		// Acknowledged should be unreachable given the SessionID guard above,
		// but if somehow reached, idempotent re-adoption is correct.
		return c.adoptOrMarkAmbiguous(ctx, run, step, entry)
	case domain.WorkflowOutboxFailed:
		// Durably failed: AO's own automatic retries are spent (or the cause was
		// never retryable), so nothing here retries it. The one way back out is
		// an explicit human Continue, which reopens this exact entry — same
		// idempotency key, no second row — through
		// resumeWorkerLaunchAfterFailure before dispatch is re-entered at all.
		return step, nil
	default:
		return step, nil
	}
}

func (c *Coordinator) dispatchFromPending(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, entry domain.WorkflowOutboxEntry, prompt string) (domain.WorkflowStep, error) {
	// Checkpoint 8L: pick the initial worker harness via ExecutionRouter
	// BEFORE touching the outbox CAS, so a waiting_for_capacity decision
	// leaves the entry Pending — the next boot Reconcile/StartRun call
	// re-evaluates routing against fresh capacity rather than getting stuck
	// "dispatched" with nothing actually spawned (checkpoint brief §13: "No
	// failure. No duplicate dispatch.").
	decision, err := c.routeWorkerDispatch(ctx, run, step, 1)
	if err != nil {
		return step, err
	}
	if decision.Waiting {
		return c.markRunWaitingForCapacity(ctx, run, step)
	}

	// Checkpoint 8P-E.11: in direct-branch mode the run must own its
	// repository+branch pair before anything is spawned into it. Like the
	// capacity check above, this runs BEFORE the outbox CAS, so a run that
	// has to wait for the branch leaves the entry Pending and the next
	// wake/reconcile pass re-evaluates it cleanly -- never "dispatched" with
	// nothing actually spawned. A nil branch-lock dependency, or a project in
	// isolated-worktree mode, passes straight through.
	if ok, err := c.ensureBranchLock(ctx, run, step); err != nil {
		return step, err
	} else if !ok {
		return step, nil
	}

	// P1-C: runtime admission. Placed here for exactly the reasons the two
	// gates above are: BEFORE the outbox CAS, so a run that has to wait for a
	// slot leaves its entry Pending and the next wake/reconcile pass
	// re-evaluates it cleanly, never "dispatched" with nothing spawned.
	//
	// A refusal is not a failure: markRunWaitingForCapacity parks the run in
	// Waiting under the same durable wake the provider-capacity wait uses, and
	// spends no retry budget.
	capReq := c.workerCapacityRequest(ctx, run, step)
	if admitted, cerr := c.acquireCapacity(ctx, capReq); cerr != nil {
		return step, cerr
	} else if !admitted {
		return c.markRunWaitingForCapacity(ctx, run, step)
	}

	now := c.clock()
	// Checkpoint 8P-E.13A.2: the same argument as 8N.1's below, for the other
	// state a blocked run can be parked in. A run that reached this line has
	// passed the capacity check AND holds its branch lock, so whatever stopped
	// it earlier is demonstrably gone — but if that stop was recorded as
	// needs_attention (a legacy branch wait, or any stop AO could not name),
	// nothing here used to write the run row back, and needs_attention is a
	// one-way street for the forward transitions: only -> running is legal, so
	// this step's own later completion (-> waiting) would be dropped as an
	// invalid transition and the run would sit stopped over completed work.
	// Human decisions are never cleared here (see clearResolvedStop).
	run = c.clearResolvedStop(ctx, run, "the work step dispatched successfully")
	// Checkpoint 8N.1: a successful (non-waiting) dispatch decision means
	// capacity genuinely came back — if the run was parked in Waiting (either
	// from a prior capacity wait on this exact step, or from this run's own
	// markRunWaitingForCapacity call on an earlier attempt), it must move
	// back to Running here, at the single point that actually knows dispatch
	// succeeded. Before this fix nothing ever wrote Running back once a run
	// left it (confirmed: StartRun is the only other UpdateWorkflowRunState
	// call site that ever sets Running), so a wake-driven redispatch left
	// run.State stuck at Waiting in the DB even though the step was, in
	// fact, running again.
	if run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, domain.WorkflowRunWaiting, domain.WorkflowRunRunning, now); err != nil {
			return step, err
		}
		run.State = domain.WorkflowRunRunning
	}
	// S4/S5: the claim, and the token it is taken under.
	//
	// The generation is minted here, BEFORE the row is claimed, and is the id
	// the intent dispatch-boundary record is then written under -- so the token
	// on the outbox row and the durable record naming this launch are the same
	// identity, reconstructable from either side. A crash between the claim and
	// the intent write leaves a claimed row whose record is missing, which
	// reconciliation already reads as "intended, never confirmed" and resolves;
	// the reverse order would leave a record for a claim nobody holds, which is
	// worse because it looks like ownership.
	//
	// ClaimWorkflowOutboxDispatch replaces the plain status CAS, which
	// deliberately CLEARS both generation columns. That clearing is right for a
	// transition that ends a claim and wrong for one that takes it: an
	// unclaimed `dispatched` row is a launch nothing owns, and every later
	// transition off it -- fail, release, acknowledge -- was then satisfiable by
	// any pass at all.
	generation := "wfd-" + c.newID()
	claimed, err := c.store.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, generation)
	if err != nil {
		return step, err
	}
	if !claimed {
		// Somebody else took this row between the read that found it pending and
		// this statement. That pass owns the launch; this one must not launch a
		// second worker over it, and must not report a failure for a dispatch it
		// does not hold. The next pass re-reads the row and does the right thing
		// with whatever state the winner leaves behind.
		if c.log != nil {
			c.log.Debug("workflow: another pass claimed this work dispatch first", "run", run.ID, "step", step.ID)
		}
		return step, nil
	}
	// Keep the in-memory entry in sync with the CAS update above: the success/
	// failure recorders further down use entry.Status as the *expected* value
	// for their own CAS call, and a stale "pending" here would silently no-op
	// against the DB row (which is genuinely already "dispatched"), leaving
	// the outbox permanently stuck instead of advancing to acknowledged/failed.
	entry.Status = domain.WorkflowOutboxDispatched
	entry.DispatchGeneration = generation
	// The step deliberately does NOT move to running here. It moves in
	// recordDispatchSuccess, strictly after the dispatch confirmation is
	// durable -- see dispatch_state_machine.go. Marking it running at this
	// line is what used to make "running" mean "AO intended to launch".

	prompt = c.applyWorkLifecycleDecision(ctx, run, step, prompt)
	return c.attemptWorkHarness(ctx, run, step, entry, prompt, decision.SelectedHarness, 1, generation)
}

// applyWorkLifecycleDecision is dispatchFromPending's Checkpoint 8N.1
// counterpart to cascade.go's applyFixLifecycleDecision: the single
// outbox-idempotency-guarded point reached exactly once per real work-step
// dispatch (whether this is the step's very first attempt or a wake-driven
// redispatch after a capacity wait) — never invoked speculatively on a mere
// poll, matching that function's own convention.
//
// A work step that reaches dispatchFromPending never has a session yet (the
// SessionID-nil guard earlier in dispatchWorkStep already returns early
// otherwise — see that function's doc comment), so
// CurrentSessionID is always empty here and DecideSessionLifecycle's own
// first, most certain rule ("no current session -> NEW_SESSION") always
// wins. That is not a limitation of this call: it is the same reasoning
// applied explicitly and durably-recorded rather than left implicit in
// "well, Spawn always creates a new session" — a capacity wake never gets to
// silently skip the policy just because a session happens not to exist yet.
// The REUSE/COMPACT branches this policy can produce are exercised by the
// fix-cycle path (cascade.go), which is reached by the exact same
// wake-driven ContinueRun cascade once a work step has a live session.
func (c *Coordinator) applyWorkLifecycleDecision(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, prompt string) string {
	attempts, _ := c.store.ListWorkflowAttempts(ctx, step.ID)
	decision := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleWorker, CurrentSessionID: "",
		SessionHealth: domain.SessionHealthUnknown, AttemptCount: len(attempts), Policy: policyForRun(run),
	})
	stepID := step.ID
	_ = c.persistSessionLifecycleDecision(ctx, run, &stepID, decision, nil)
	return prompt
}

// markRunWaitingForCapacity is Checkpoint 8L's read-time-derivable
// waiting_for_capacity representation (checkpoint brief §13): the run moves
// to WorkflowRunWaiting (never needs_attention, never a synthetic failure),
// the step and outbox entry are left untouched so no duplicate dispatch is
// possible, and the routing_decision checkpoint already persisted by
// routeWorkerDispatch is what explains which providers were considered.
func (c *Coordinator) markRunWaitingForCapacity(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) (domain.WorkflowStep, error) {
	if run.State == domain.WorkflowRunRunning {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunWaiting, c.clock()); err != nil {
			return step, err
		}
	}
	c.scheduleCapacityWake(ctx, run, &step, step.Kind, step.AssignedHarness)
	return step, nil
}

// scheduleCapacityWake is Checkpoint 8N's single shared hook for persisting
// a durable wake whenever a run/step parks on provider capacity — called
// from markRunWaitingForCapacity (worker/reviewer waits) and from
// decision_resolver_wiring.go's dispatchDecisionResolver (question-resolver
// waits). Never fails the caller: a nil wakeScheduler or a scheduling error
// only means no automatic wake gets scheduled — the run still correctly
// enters/stays in Waiting either way, matching this codebase's "observers
// don't invent failures" convention (compare recordAgentHealthFailure's own
// best-effort, non-fatal write).
func (c *Coordinator) scheduleCapacityWake(ctx stdctx.Context, run domain.WorkflowRun, step *domain.WorkflowStep, kind domain.WorkflowStepKind, harness string) {
	reason := wake.ReasonWorkerCapacity
	if kind == domain.WorkflowStepReview {
		reason = wake.ReasonReviewerCapacity
	}
	var stepID *domain.WorkflowStepID
	if step != nil {
		id := domain.WorkflowStepID(step.ID)
		stepID = &id
	}
	c.scheduleWake(ctx, run, stepID, reason, harness)
}

// scheduleQuestionResolverCapacityWake is decision_resolver_wiring.go's
// counterpart to scheduleCapacityWake, for the one capacity-wait producer
// that is not a work/review step: an AUTO_RESOLVABLE question whose resolver
// currently has no usable provider. q.WorkflowStepID (the worker step that
// asked the question, if any) is used as the wake's step scope purely so a
// worker step and a question tied to it don't collide on the same
// idempotency key as a plain run-scoped wake would; the harness is left
// empty since selectDecisionResolverProvider's "no provider available"
// outcome (unlike a single harness's own recorded cooldown) has no single
// harness to look up a known reset for.
func (c *Coordinator) scheduleQuestionResolverCapacityWake(ctx stdctx.Context, run domain.WorkflowRun, q domain.WorkflowQuestion) {
	c.scheduleWake(ctx, run, q.WorkflowStepID, wake.ReasonQuestionResolverCapacity, "")
}

// MarkCapacityRetryExhausted is Checkpoint 8N.1's explicit, observable
// terminal-for-now outcome when a wake's own retry budget
// (WakePolicy.MaxAttempts) has been exhausted with capacity still
// unavailable: rather than leaving a run silently parked in Waiting forever
// with no further wake ever firing (checkpoint brief §26: "no loop
// infinito... workflow queda en estado conservador explícito"), the run
// moves to NeedsAttention with a checkpoint recording exactly why, using the
// existing domain.WorkflowErrorCapacityExhausted class rather than inventing
// a new one. Called by the wake poller (wakepoller.Poller.RunDueOnce), never
// by Schedule/Fail themselves, since only the poller — not the wake package,
// which is deliberately Coordinator-agnostic — knows which run a budget-
// exhausted wake belonged to and is allowed to mutate workflow state. A run
// no longer Waiting (already recovered by some other path, or already
// terminal) is left untouched: this is a best-effort, idempotent nudge, not
// a state machine transition that must always apply.
func (c *Coordinator) MarkCapacityRetryExhausted(ctx stdctx.Context, runID, reason string) error {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return err
	}
	if !ok || run.State != domain.WorkflowRunWaiting {
		return nil
	}
	now := c.clock()
	if _, err := c.store.UpdateWorkflowRunState(ctx, runID, domain.WorkflowRunWaiting, domain.WorkflowRunNeedsAttention, now); err != nil {
		return err
	}
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: runID,
		ProjectID:     run.ProjectID,
		NextAction: fmt.Sprintf(
			"capacity_retry_exhausted: %s — wake retry budget exhausted with capacity still unavailable; a human must decide whether to wait longer, switch provider, or cancel",
			reason,
		),
		DurablePhase:   string(domain.WorkflowErrorCapacityExhausted),
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	})
	return err
}

// scheduleWake is the common body shared by every Checkpoint 8N capacity-wait
// producer: it persists a durable wake so a daemon poller can resume the run
// automatically. Never fails the caller: a nil wakeScheduler or a scheduling
// error only means no automatic wake gets scheduled — the run still
// correctly enters/stays in its waiting state either way, matching this
// codebase's "observers don't invent failures" convention (compare
// recordAgentHealthFailure's own best-effort, non-fatal write).
func (c *Coordinator) scheduleWake(ctx stdctx.Context, run domain.WorkflowRun, stepID *domain.WorkflowStepID, reason wake.Reason, harness string) {
	if c.wakeScheduler == nil {
		return
	}
	// Checkpoint 8N: never fabricate known_reset_at. Only a real, recorded
	// AgentHealthEvent.CooldownUntil for the harness that was just attempted
	// counts as a known reset; today's failover.go/health.go recording path
	// does not populate CooldownUntil for workflow's TUI worker sessions (see
	// health.go's own doc comment), so this will be nil in practice — that
	// is expected and correct, not a bug.
	var knownResetAt *time.Time
	if harness != "" {
		_, owner, profileID, _ := c.resolveRuntimeEnv(ctx, run.ID, domain.AgentHarness(harness))
		if health, err := c.agentHealth(ctx, domain.AgentHarness(harness), healthScope{userID: owner, profileID: profileID}); err == nil {
			knownResetAt = health.CooldownUntil
		}
	}

	sch, err := c.wakeScheduler.Schedule(ctx, domain.WorkflowRunID(run.ID), stepID, reason, knownResetAt)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: wake schedule failed", "run", run.ID, "reason", reason, "err", err)
		}
		return
	}
	if c.log != nil {
		c.log.Info("workflow: wake scheduled", "run", run.ID, "reason", reason, "scheduledAt", sch.ScheduledAt, "attempt", sch.AttemptCount)
	}
}

// attemptWorkHarness tries one provider for one work-step dispatch attempt
// (Checkpoint 8H). On failure it classifies the error and — only if the
// class is failover-eligible, the step is still within its policy attempt
// budget, and the fallback harness is healthy — tries exactly one fallback
// harness (V1's fixed codex->claude-code order) before giving up. This is
// deliberately synchronous within one dispatchWorkStep call rather than
// waiting for a later poll/reconcile pass: dispatchWorkStep is only re-
// entered by StartRun and boot Reconcile, never by GetRun's read-time
// polling, so a mid-uptime Spawn failure has no other opportunity to retry
// before the checkpoint's own attempt budget would otherwise go unused.
//
// generation is the claim token dispatchFromPending took the outbox row under,
// and it is threaded through every provider attempt of THIS claim -- including
// a fallback. A fallback is a second provider under one claim, not a second
// claim, so it must not mint its own token: doing so would fence the fallback
// out of the very row its predecessor holds.
func (c *Coordinator) attemptWorkHarness(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, entry domain.WorkflowOutboxEntry, prompt string, harness domain.AgentHarness, attemptNumber int, generation string) (domain.WorkflowStep, error) {
	// PHASE 1 -- INTENT. Durable before anything is invoked: the attempt row
	// and the dispatch-intent record. A store that cannot write this cannot
	// launch, because a launch AO holds no record of is a launch no restart can
	// reconcile. See dispatch_state_machine.go.
	// The intent record's own id is the generation ONLY on the first provider
	// attempt of this claim -- that is the record the token names. A fallback
	// writes its own intent row under a fresh id while carrying the same token.
	intentRecordID := ""
	if attemptNumber == 1 {
		intentRecordID = generation
	}
	intent, err := c.beginWorkerDispatch(ctx, run, step, entry, harness, generation, intentRecordID)
	if err != nil {
		if intent.attempt.ID != "" {
			// The attempt row was opened before the intent write failed; it
			// describes a launch that never happened, so it concludes here
			// rather than staying open over nothing.
			if aerr := c.concludeWorkerAttemptFailure(ctx, intent, domain.WorkflowErrorRuntimeFailed, c.clock()); aerr != nil {
				return step, aerr
			}
		}
		return c.recordWorkerLaunchFailure(ctx, run, step, entry, harness, workerLaunchStageIntent, err)
	}

	runtimeEnv, owner, profileID, err := c.resolveRuntimeEnv(ctx, run.ID, harness)
	scope := healthScope{userID: owner, profileID: profileID}
	if err != nil {
		classification := classifyProviderFailure(err)
		now := c.clock()
		c.recordAgentHealthFailure(ctx, harness, scope, classification, now)
		if aerr := c.concludeWorkerAttemptFailure(ctx, intent, classification.Class, now); aerr != nil {
			return step, aerr
		}
		c.recordLaunchFailureBoundary(ctx, run, step, entry, intent, domain.LaunchStageRuntimeEnv, classification.Class, err)
		return c.recordWorkerLaunchFailure(ctx, run, step, entry, harness, workerLaunchStageRuntimeEnv, err)
	}
	// Checkpoint 8P-E.24: before an UNATTENDED launch, ask whether this
	// provider can actually start without an operator. A refusal here is a
	// pre-work failure exactly like a failed Spawn — nothing was created, no
	// worker owns anything — so it travels through the same
	// recordWorkerLaunchFailure path and lands with its own proven class and
	// its own precise attention reason. A missing or unrunnable checker is not
	// a refusal; see preflightWorkerDispatch.
	if perr := c.preflightWorkerDispatch(ctx, WorkerPreflightRequest{
		Harness:       harness,
		WorkspacePath: c.projectPathFor(ctx, run.ProjectID),
		ProjectID:     run.ProjectID,
		RunID:         run.ID,
		StepID:        step.ID,
		RuntimeEnv:    runtimeEnv,
		Owner:         owner,
		ProfileID:     profileID,
	}); perr != nil {
		now := c.clock()
		class := classifyWorkerLaunchFailure(perr).Class
		if aerr := c.concludeWorkerAttemptFailure(ctx, intent, class, now); aerr != nil {
			return step, aerr
		}
		c.recordLaunchFailureBoundary(ctx, run, step, entry, intent, domain.LaunchStagePreflight, class, perr)
		return c.recordWorkerLaunchFailure(ctx, run, step, entry, harness, workerLaunchStagePreflight, perr)
	}

	// PHASE 2 -- LAUNCH. Through the injectable launcher, with the process/
	// session ownership proof read back through the injectable prober.
	result, ownership, err := c.launchWorker(ctx, WorkerLaunchRequest{
		RunID:       run.ID,
		StepID:      step.ID,
		AttemptID:   intent.attempt.ID,
		ProjectID:   domain.ProjectID(run.ProjectID),
		Harness:     harness,
		IssueID:     workStepIssueID(step.ID),
		Prompt:      prompt,
		DisplayName: workDisplayName(run.Objective),
		BaseRef:     c.masterTaskBaseRef(ctx, run),
		RuntimeEnv:  runtimeEnv,
		Owner:       owner,
		// This run already holds the direct-branch execution lock (see
		// ensureBranchLock above), so the worker session must not try to
		// acquire it as a task in its own right and queue behind its own run.
		WorkflowRunID: run.ID,
	})
	if errors.Is(err, errLaunchWithoutEvidence) {
		// The launcher answered "fine" and named no session. Nothing is retried
		// and nothing is confirmed: the outbox stays `dispatched` with no
		// session on the step, which is precisely the shape adoptOrMarkAmbiguous
		// resolves from evidence — adopt what is really there, escalate
		// otherwise, never launch a second worker over the first.
		c.recordAmbiguousLaunchBoundary(ctx, run, step, entry, intent)
		if c.log != nil {
			c.log.Warn("workflow: worker launch reported success without a session identity",
				"step", step.ID, "harness", harness)
		}
		return step, nil
	}
	if err != nil {
		classification := classifyProviderFailure(err)
		now := c.clock()
		c.recordAgentHealthFailure(ctx, harness, scope, classification, now)
		// Always conclude this attempt first — audit history, never deleted or
		// overwritten, regardless of whether a fallback follows.
		if aerr := c.concludeWorkerAttemptFailure(ctx, intent, classification.Class, now); aerr != nil {
			return step, aerr
		}
		c.recordLaunchFailureBoundary(ctx, run, step, entry, intent, domain.LaunchStageSpawn, classification.Class, err)
		if fallback, ok := c.selectFallbackForWork(ctx, run, step.ID, harness, attemptNumber, classification); ok {
			if c.log != nil {
				c.log.Warn("workflow: work step failing over to fallback harness", "step", step.ID, "from", harness, "to", fallback, "class", classification.Class)
			}
			return c.attemptWorkHarness(ctx, run, step, entry, prompt, fallback, attemptNumber+1, generation)
		}
		// Every provider this attempt was allowed to try has now failed, and
		// none of them left a worker behind: the launcher either returns a
		// session record or returns an error having created none. That pre-work
		// property is what worker_launch_recovery.go's bounded retry stands on.
		return c.recordWorkerLaunchFailure(ctx, run, step, entry, harness, workerLaunchStageSpawn, err)
	}
	if attemptNumber > 1 && c.log != nil {
		c.log.Info("workflow: work step provider failover succeeded", "step", step.ID, "harness", harness, "attempt", attemptNumber)
	}
	c.recordAgentHealthSuccess(ctx, harness, scope, c.clock())
	// PHASES 3 and 4 -- CONFIRMATION, then RUNNING, in that order.
	return c.confirmWorkerDispatch(ctx, run, step, entry, intent, result, ownership)
}

// resolveRuntimeEnv is the single call site every dispatcher (worker,
// reviewer, planner, decision resolver) uses to derive the workflow
// owner's isolated provider subprocess env (Checkpoint 8P-B.1). A nil
// runtimeIsolation (not yet wired) is a permanent, unconditional no-op --
// exactly today's pre-8P-B.1 behavior.
func (c *Coordinator) resolveRuntimeEnv(ctx stdctx.Context, runID string, harness domain.AgentHarness) (map[string]string, domain.UserID, domain.ProviderProfileID, error) {
	if c.runtimeIsolation == nil {
		return nil, "", "", nil
	}
	return c.runtimeIsolation.Resolve(ctx, runID, harness)
}

// concludeWorkerAttemptFailure terminates the attempt opened at intent time
// (Checkpoint 8H). Always called before any fallback decision, so the losing
// provider's attempt is never lost even when a fallback attempt subsequently
// succeeds ("no borres el intento Codex").
//
// It concludes rather than creates: since the phased dispatch opens the attempt
// row before the launcher is invoked, creating another one here would leave two
// rows per provider attempt — one open forever, describing a launch that
// already failed.
func (c *Coordinator) concludeWorkerAttemptFailure(ctx stdctx.Context, intent workerDispatchIntent, errClass domain.WorkflowErrorClass, now time.Time) error {
	// ClaimWorkflowAttemptOutcome, not UpdateWorkflowAttemptOutcome. The
	// unconditional update is last-writer-wins over a row whose whole job is to
	// say whether work is in flight, so a slow pass could overwrite the outcome
	// a faster one had already recorded -- turning a succeeded attempt into a
	// failed one, or one provider's failure class into another's. The claim
	// matches only `finished_at IS NULL`, so exactly one caller concludes the
	// attempt and the rest are no-ops. Losing is not an error: it means somebody
	// else already closed this attempt, which is the outcome this call wanted.
	_, err := c.store.ClaimWorkflowAttemptOutcome(ctx, intent.attempt.ID, now, domain.WorkflowAttemptFailed, errClass)
	return err
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
	// The evidence gate. This path says "ambiguous_worker_state" in the sentence
	// a person reads, so it owes the same bounded snapshot every other
	// ambiguity raise owes — collected and recorded BEFORE any state moves, so a
	// daemon that dies here leaves readable evidence rather than an unexplained
	// stop. See ambiguous_worker_state.go.
	if _, rerr := c.raiseAmbiguousWorkerState(ctx, run, step, ReasonWorkerDispatchAmbiguous, nextAction,
		c.observedWorkerFactsFor(ctx, sessionIDIfFound(found, rec), nil)); rerr != nil {
		return step, rerr
	}
	// `ready` parks exactly like `running`: a daemon that died between the
	// dispatch intent and its confirmation leaves the step at ready with the
	// outbox already dispatched, and leaving it there would have every later
	// reconcile pass re-enter this branch and raise the same ambiguity again.
	if step.State == domain.WorkflowStepRunning || step.State == domain.WorkflowStepReady {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, domain.WorkflowStepWaiting, now); err != nil {
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

// recordDispatchSuccess is the entry point for a dispatch whose session is
// already in hand: an adoption at recovery time (adoptOrMarkAmbiguous,
// resumeWorkerLaunchAfterFailure) rather than a launch this process performed.
//
// It rejoins the phased state machine at phase 3 — the same confirmation write,
// the same RUNNING gate — so an adopted worker and a freshly launched one leave
// exactly the same durable trail. What it cannot claim is LaunchedAt: a session
// found after a restart was launched at a time this process never observed, and
// filling that in from the current clock would date the launch to the recovery.
func (c *Coordinator) recordDispatchSuccess(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, entry domain.WorkflowOutboxEntry, rec domain.SessionRecord) (domain.WorkflowStep, error) {
	attempt, err := c.openWorkerAttempt(ctx, step.ID, rec.Harness, c.clock())
	if err != nil {
		return step, err
	}
	// The adoption joins the claim that is ALREADY on the row rather than
	// minting one: this entry was claimed by the dispatch whose launch is being
	// adopted, and re-stamping it would make the adopter look like a different
	// launch to every generation-fenced transition below. An entry claimed
	// before the worker path stamped tokens carries "", and an empty token is
	// exactly what completes an unclaimed row.
	intent := workerDispatchIntent{attempt: attempt, harness: rec.Harness, generation: entry.DispatchGeneration}
	ownership := c.sessionOwnershipOrDefault().ObserveSessionOwnership(ctx, rec.ID)
	return c.confirmWorkerDispatch(ctx, run, step, entry, intent, WorkerLaunchResult{Session: rec}, ownership)
}

// confirmWorkerDispatch is phase 3 followed by phase 4, and the ordering
// between them is the whole contract: the dispatch confirmation is durable
// BEFORE the step is running, the session is written onto the step, or the
// outbox is acknowledged.
//
// If the confirmation write fails, none of those four things happen. What is
// left behind is the deliberately distinct "launched, not confirmed" state
// (recordUnconfirmedLaunch): the outbox still `dispatched`, the step still
// without a session and still not running, and a durable ledger record naming
// the session that WAS launched — so the next pass adopts it instead of
// launching a second worker over it.
func (c *Coordinator) confirmWorkerDispatch(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	intent workerDispatchIntent,
	result WorkerLaunchResult,
	ownership SessionOwnershipEvidence,
) (domain.WorkflowStep, error) {
	now := c.clock()
	rec := result.Session
	// PHASE 3, PRECONDITION -- do we still own the launch we are about to
	// confirm?
	//
	// Everything below runs on state read some calls ago, across a real process
	// launch. A pass can lose its claim in that window: reconciliation releases
	// a dispatch it judged stale, a second pass reclaims the row, and this pass
	// then wakes up holding a session whose row is owned by somebody else.
	// Confirming there would license RUNNING off a confirmation belonging to a
	// different launch of the same step. The generation-fenced acknowledge below
	// is the durable arbiter; this read is what stops the writes BEFORE it from
	// landing on a launch this pass no longer holds.
	if !c.stillOwnsWorkerDispatch(ctx, run, entry, intent) {
		c.recordUnconfirmedLaunch(ctx, run, step, entry, intent, result, ownership,
			unconfirmedClaimLost, nil)
		return step, nil
	}
	branch, worktree, baseSHA := launchWorkspaceFacts(result, ownership)
	fingerprint := ""
	if c.workspaceFacts != nil && worktree != "" {
		if obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
			Path: worktree, Branch: branch, SessionID: rec.ID, ProjectID: rec.ProjectID,
		}); err == nil {
			// The live tree wins over the session row's recorded base: it is the
			// commit the launch is actually sitting on right now.
			baseSHA = obs.HeadSHA
			fingerprint = WorkspaceFingerprint(obs)
		}
	}

	// PHASE 3 -- CONFIRMATION. Mandatory, and the gate everything below stands
	// on.
	//
	// Both halves of the evidence are required, not one. A session identity is
	// the launcher's WORD that it started something; the ownership read-back is
	// the only thing that says that session exists, belongs to this launch, and
	// is fenced by a launch generation AO can tell apart from a session row that
	// merely outlived the process behind it. Confirming on the id alone would
	// license RUNNING off an unverified claim — which is the precise shape of
	// the bug this whole state machine exists to remove — so an ownership proof
	// AO could not read routes to the unconfirmed state instead, with the step
	// left out of running and the launched session named for a later adoption.
	if !ownership.Observed {
		c.recordUnconfirmedLaunch(ctx, run, step, entry, intent, result, ownership,
			unconfirmedOwnershipUnproven, nil)
		return step, nil
	}
	var launchedAt *time.Time
	if !result.LaunchedAt.IsZero() {
		at := result.LaunchedAt
		launchedAt = &at
	}
	evidence := map[string]string{
		"attemptId":         intent.attempt.ID,
		"attemptGeneration": intent.generation,
		"ownership":         ownershipEvidenceStatus(ownership),
	}
	if err := c.recordDispatchBoundary(ctx, dispatchBoundary{
		run: run, step: step, entry: entry, attempt: intent.attempt.ID, harness: rec.Harness,
		phase:           domain.DispatchPhaseWorkerDispatched,
		stage:           domain.LaunchStageConfirm,
		outcome:         domain.LaunchOutcomeDispatched,
		sessionID:       string(rec.ID),
		detail:          fmt.Sprintf("worker session %s confirmed for attempt %s", rec.ID, intent.attempt.ID),
		branch:          branch,
		worktreePath:    worktree,
		baseSHA:         baseSHA,
		fingerprint:     fingerprint,
		runtimeHandleID: ownership.RuntimeHandleID,
		runtimeLaunchID: ownership.RuntimeLaunchID,
		agentSessionID:  ownership.AgentSessionID,
		launchedAt:      launchedAt,
		evidence:        evidence,
	}); err != nil {
		c.recordUnconfirmedLaunch(ctx, run, step, entry, intent, result, ownership,
			unconfirmedWriteFailed, err)
		return step, err
	}

	// The ledger's own worker_dispatched row. Kept, and kept in this shape,
	// because it is what work_adoption.go and worker_launch_recovery.go read as
	// "a worker actually launched for this step"; the dispatch record above is
	// the evidence, this is the phase marker those readers already know.
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
		DurablePhase:   workerDispatchedDurablePhase,
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		return step, err
	}

	// PHASE 4 -- RUNNING. Only now, and only through the claim.
	//
	// The acknowledge comes BEFORE the session write and the step transition,
	// and it names the generation. That ordering is the durable arbiter of the
	// whole phase: a pass that no longer holds the claim matches zero rows here
	// and stops, having written only append-only evidence -- rather than
	// stamping a session and a RUNNING state for a launch that is not its own.
	//
	// A crash between the acknowledge and the session write leaves an
	// acknowledged entry with no session on the step, which dispatchWorkStep's
	// `acknowledged` arm already resolves by idempotent re-adoption.
	if entry.Status != domain.WorkflowOutboxAcknowledged {
		acked, err := c.store.AcknowledgeWorkflowOutboxDispatch(ctx, entry.ID, entry.Status, now, intent.generation)
		if err != nil {
			return step, err
		}
		if !acked {
			// The claim moved. Everything written above is evidence and stays;
			// nothing that asserts ownership is written. The surviving state --
			// a launched session named on the ledger, no session on the step --
			// is exactly the shape adoption resolves.
			c.recordUnconfirmedLaunch(ctx, run, step, entry, intent, result, ownership,
				unconfirmedClaimLost, nil)
			return step, nil
		}
		entry.Status = domain.WorkflowOutboxAcknowledged
	}
	if _, err := c.store.UpdateWorkflowStepSession(ctx, step.ID, sessID, now); err != nil {
		return step, err
	}
	step.SessionID = &sessID

	// Checkpoint 8P-E.11: point any branch lock this run holds at the session
	// that now occupies it, so "currently used by" can name the live session
	// and not just the run. Best-effort by construction (Renew never fails a
	// caller): ownership is decided by lock state, never by heartbeat freshness.
	if c.branchLocks != nil {
		c.branchLocks.Renew(ctx, run.ID, step.ID, sessID)
	}

	if step.State == domain.WorkflowStepReady {
		// Licensed by the confirmation, and by the session that confirmation
		// wrote -- not merely by order of execution. See
		// StartWorkflowStepForSession.
		started, err := c.store.StartWorkflowStepForSession(ctx, step.ID, sessID, now)
		if err != nil {
			return step, err
		}
		if !started {
			// The step is no longer ready, or holds a session that is not the
			// one this pass confirmed. Either way this pass may not call it
			// running; the ledger keeps everything it wrote, and the next
			// reconciliation reads the state that actually exists.
			if c.log != nil {
				c.log.Warn("workflow: refusing to mark a work step running over a session this pass did not confirm",
					"run", run.ID, "step", step.ID, "session", sessID)
			}
			return step, nil
		}
		step.State = domain.WorkflowStepRunning
	}
	return step, nil
}

// stillOwnsWorkerDispatch re-reads this step's outbox entry and reports whether
// the claim this pass took is still the claim on the row.
//
// An entry that has already reached `acknowledged` is treated as still owned:
// that is this same launch's own completed confirmation being re-entered
// idempotently (the crash-between-acknowledge-and-session-write window), not a
// different pass's claim.
//
// A read it cannot perform answers TRUE. This is a pre-check whose job is to
// stop obviously-lost passes early; the durable arbiter is the generation-fenced
// acknowledge, and failing closed here would turn an unreadable outbox into a
// refusal to confirm a launch that is genuinely this pass's own.
func (c *Coordinator) stillOwnsWorkerDispatch(
	ctx stdctx.Context, run domain.WorkflowRun, entry domain.WorkflowOutboxEntry, intent workerDispatchIntent,
) bool {
	entries, err := c.store.ListWorkflowOutboxByRun(ctx, run.ID)
	if err != nil {
		return true
	}
	for _, e := range entries {
		if e.ID != entry.ID {
			continue
		}
		// Only a PROVEN disagreement refuses. A row still `dispatched` under a
		// different token is somebody else's live launch and this pass must not
		// confirm over it. Every other shape -- already acknowledged (this same
		// launch's own confirmation being re-entered idempotently), or a
		// `failed` row an adoption is legitimately completing -- is left to the
		// generation-fenced acknowledge, which compare-and-swaps against the
		// exact status its caller read.
		if e.Status == domain.WorkflowOutboxDispatched && e.DispatchGeneration != intent.generation {
			return false
		}
		return true
	}
	return true
}

// recordDispatchFailure writes the terminal-for-AO shape of a failed work
// dispatch: outbox failed, step failed, run parked. reason is the canonical
// attention reason the caller's classification chose — the flat
// ReasonDispatchFailed for a permanent cause (which is what every row written
// before worker_launch_recovery.go existed carries), or
// ReasonWorkerLaunchRetriesExhausted when the cause was transient but the
// automatic budget is spent. Both are reopenable by an explicit human
// Continue; see resumeWorkerLaunchAfterFailure.
//
// failureGeneration is the id of the durable launch record that explains this
// failure, and it is stamped onto the row IN THE SAME STATEMENT as the failure
// itself. Two properties come from that. First, ownership: `id + status` is not
// proof, because a dispatch that paused after recording its launch error can
// find the row dispatched again to somebody else, and would then fail a live
// generation and stamp its own failure onto it. Second, resumability: a human
// resume must reopen the failure the person actually saw, and a token written
// after the state it describes would leave a crash window where a failed row
// exists whose generation nobody can prove.
func (c *Coordinator) recordDispatchFailure(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, entry domain.WorkflowOutboxEntry, errClass domain.WorkflowErrorClass, reason string, cause error, failureGeneration string) (domain.WorkflowStep, error) {
	now := c.clock()
	failed, err := c.store.FailWorkflowOutboxWithGeneration(
		ctx, entry.ID, entry.Status, now, string(errClass), failureGeneration, entry.DispatchGeneration)
	if err != nil {
		return step, err
	}
	if !failed {
		// The claim moved out from under this pass. It may not fail, park or
		// stop a launch it does not own; the evidence it already wrote stands,
		// and whoever holds the row settles it.
		if c.log != nil {
			c.log.Warn("workflow: a work dispatch failure could not be stamped on a claim it no longer holds",
				"run", run.ID, "step", step.ID, "class", errClass)
		}
		return step, nil
	}
	// The failing (and any tried-and-failed fallback) attempt row was
	// already concluded by attemptWorkHarness/concludeWorkerAttemptFailure
	// before this terminal path was reached — never duplicated here.
	//
	// `ready` is now as ordinary a starting point as `running`: a launch that
	// never got past its own intent leaves the step exactly where it was, and a
	// step whose launch permanently failed is a failed step whether or not
	// anything ever ran in it. See ValidWorkflowStepTransition.
	if step.State == domain.WorkflowStepRunning || step.State == domain.WorkflowStepReady {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, domain.WorkflowStepFailed, now); err != nil {
			return step, err
		}
		step.State = domain.WorkflowStepFailed
	}
	if run.State == domain.WorkflowRunRunning {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, domain.WorkflowRunRunning, domain.WorkflowRunNeedsAttention, now); err != nil {
			return step, err
		}
	}
	if reason == "" {
		reason = ReasonDispatchFailed
	}
	c.recordAttentionStop(ctx, run, &step.ID, reason,
		fmt.Sprintf("work dispatch failed (%s): %v", errClass, cause))
	if c.log != nil {
		c.log.Warn("workflow: work step dispatch failed", "step", step.ID, "err", cause)
	}
	return step, nil
}

// projectPathFor resolves a run's project checkout path for the provider
// preflight's workspace-trust question. Empty when it cannot be read, which the
// checker is free to treat as "no path-scoped question to ask".
func (c *Coordinator) projectPathFor(ctx stdctx.Context, projectID string) string {
	if c.projects == nil {
		return ""
	}
	proj, ok, err := c.projects.GetProject(ctx, projectID)
	if err != nil || !ok {
		return ""
	}
	return proj.Path
}
