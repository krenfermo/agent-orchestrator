package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// This file is review_launch_recovery.go's counterpart for the OTHER launch AO
// performs before any agent owns any work: the work step's worker spawn.
//
// The real incident (wf-57f90ff2-3fc6-4f08-bb2a-006dee207281, task 4 of master
// wf-872e7f57): routing picked claude-code, the session lifecycle policy said
// NEW_SESSION, the tmux session was created — and the very next tmux command
// answered "no such session", because Create had unlinked the staged launch
// script out from under the pane's own shell (fixed at the root in
// adapters/runtime/tmux; see Create's comment there). What the pre-existing
// dispatch path did with that momentary, provably pre-work failure:
//
//   - recordDispatchFailure moved the work step to `failed` — terminal, zero
//     outgoing transitions;
//   - it moved the outbox entry to `failed`, which dispatchWorkStep answers with
//     "Already durably recorded as failed; no auto-retry";
//   - it parked the run in needs_attention under the flat `dispatch_failed`
//     reason, which the master mirrored as child_needs_attention.
//
// So a runtime hiccup that had created nothing, delivered nothing and started no
// worker became a permanently stuck objective that not even a human Continue
// could restart — Continue only ever re-entered dispatch for a step at
// ready/running, and this one was failed.
//
// The rules here replace that, deliberately mirroring the reviewer file so
// there is one shape of answer in AO for "the thing we were launching never
// launched":
//
//  1. Every worker-launch failure persists its deep error and a real
//     classification (class + certainty + stage), not just an error class.
//  2. A failure AO can prove happened BEFORE the worker owned anything, and
//     whose cause is transient, retries automatically — bounded, durably
//     recorded, driven by a wake, asking no one for anything.
//  3. A permanent cause (auth, a missing binary, an unusable configuration) is
//     never retried as if it were a hiccup; it stops with a named reason.
//  4. An AMBIGUOUS outcome — an outbox entry left `dispatched`, i.e. AO cannot
//     prove whether Spawn completed — is never touched here. It belongs to
//     adoptOrMarkAmbiguous, which adopts on evidence and escalates otherwise,
//     and which never calls Spawn a second time.
//  5. A human Continue can reopen a durably failed pre-work dispatch, exactly
//     once per authorized generation, bounded, and only on durable evidence
//     that this is what the failure was — including the rows already on disk
//     from before this file existed.

const (
	// workerLaunchRecordPhase is the durable_phase of the machine-readable
	// record every worker-launch failure writes: RetryState carries the
	// classification and the deep error, NextAction the human-readable form.
	// Deliberately NOT a canonical attention reason (attention.go) — the reason
	// is written separately by recordAttentionStop, so a stopped run's newest
	// checkpoint always NAMES the stop while this one explains it.
	workerLaunchRecordPhase = "worker_launch_error"

	// workerLaunchHumanRetryPhase marks a human-initiated resume of a worker
	// dispatch that had stopped. It resets the automatic retry budget —
	// attempts are only ever counted since the newest one of these — and its
	// own count is what bounds how many times one step may be reopened.
	workerLaunchHumanRetryPhase = "worker_launch_human_retry"

	// workerDispatchedDurablePhase is recordDispatchSuccess's own phase. A
	// launch-failure record older than one of these has been superseded by a
	// worker that actually launched and must never be read as current state.
	workerDispatchedDurablePhase = "worker_dispatched"

	// workerLaunchRetryDelay is the minimum age of a launch-failure record
	// before AO retries it by itself. The durable wake is what drives the
	// retry; this floor stops any other dispatch entry point (boot Reconcile, a
	// capacity wake, a master reconcile pass) from front-running it and burning
	// the whole budget inside one second, before the transient condition had any
	// chance to clear. A human-driven Continue bypasses it: a person asking now
	// means now.
	workerLaunchRetryDelay = 30 * time.Second

	// maxWorkerLaunchAttempts bounds automatic retries of one work step's
	// dispatch. The budget exists so a permanently broken environment that
	// merely *looks* transient still reaches a human instead of retrying
	// forever — and so AO can never create an unbounded number of agent
	// sessions for one step.
	maxWorkerLaunchAttempts = 3

	// maxWorkerLaunchRecoveryGenerations bounds how many times one work step may
	// be reopened by a human Continue, however many times the button is pressed.
	// Same reasoning as maxVerifyRecoveryAttempts: the bound is there for the
	// case the person's diagnosis is wrong, so "fix it and continue" on a
	// condition that was never transient is not an unbounded loop with a human
	// in it.
	maxWorkerLaunchRecoveryGenerations = 3
)

// workerLaunchStage names how far the dispatch got before it failed. Recorded
// because "the owner's provider env could not be resolved" and "the agent
// process would not start" are different problems that can carry the same class.
type workerLaunchStage string

const (
	// workerLaunchStageIntent is a dispatch that stopped before ANY launcher or
	// session call, because the durable dispatch-intent record could not be
	// written. It is the only stage at which nothing whatsoever was invoked, so
	// it is also the only one where "nothing was created" needs no argument.
	workerLaunchStageIntent     workerLaunchStage = "intent"
	workerLaunchStageRuntimeEnv workerLaunchStage = "runtime_env"
	workerLaunchStageSpawn      workerLaunchStage = "spawn"
	// workerLaunchStagePreflight is a launch refused BEFORE anything was
	// spawned, because the provider would have stopped at an interactive
	// prompt. It is the earliest stage there is, and the only one at which AO
	// can be certain nothing was created.
	workerLaunchStagePreflight workerLaunchStage = "preflight"
)

// workerLaunchClassification is the verdict on one worker-launch failure: which
// error class it really is, how confident that is, whether AO may retry it by
// itself, and the canonical attention reason that names it when it may not.
type workerLaunchClassification struct {
	Class     domain.WorkflowErrorClass
	Certainty ClassificationCertainty
	Retryable bool
	// Reason is the canonical attention reason used when this failure is (or
	// becomes) a human decision. Always non-empty.
	Reason string
}

// classifyWorkerLaunchFailure classifies one worker-launch failure.
//
// Like classifyReviewerLaunchFailure it layers on classifyProviderFailure
// rather than re-deriving provider semantics, and answers only the
// launch-specific question: may AO retry this by itself?
//
// It reuses the reviewer file's phrase vocabularies deliberately. "Did the
// process/runtime refuse right now, or is this a configuration problem?" has
// the same answer whoever was being launched, and two drifting copies of that
// list would be two different answers to one question.
//
// The default for an unrecognised failure is retryable-but-bounded, not
// permanent: a launch that failed for a reason AO cannot name is far more often
// a momentary process/runtime problem than a configuration one, and
// maxWorkerLaunchAttempts guarantees an unnameable failure still reaches a
// human quickly instead of looping.
func classifyWorkerLaunchFailure(err error) workerLaunchClassification {
	// A refused provider preflight already carries its own proven class and its
	// own precise attention reason (provider_preflight.go). It is never
	// retryable — no amount of waiting installs a credential or trusts a folder
	// — and re-deriving it from the error text would collapse three distinct
	// remedies back into one.
	if cls, ok := classifyPreflightRefusal(err); ok {
		return cls
	}
	base := classifyProviderFailure(err)
	switch base.Class {
	case domain.WorkflowErrorAuth, domain.WorkflowErrorBinaryMissing:
		// Credentials and installation are the two things no amount of
		// retrying changes, and the two AO must never treat as a hiccup.
		// dispatch_failed is kept as the reason so these stops read exactly as
		// they always have.
		return workerLaunchClassification{Class: base.Class, Certainty: base.Certainty, Reason: ReasonDispatchFailed}
	case domain.WorkflowErrorRateLimited, domain.WorkflowErrorCapacityExhausted:
		// Time-boxed provider conditions: exactly what a bounded retry with a
		// durable wake exists for.
		return workerLaunchClassification{Class: base.Class, Certainty: base.Certainty, Retryable: true, Reason: ReasonWorkerLaunchRetriesExhausted}
	}

	text := ""
	if err != nil {
		text = strings.ToLower(err.Error())
	}
	switch {
	case containsAny(text, reviewLaunchPermanentPhrases...):
		return workerLaunchClassification{
			Class:     base.Class,
			Certainty: CertaintyInferred,
			Reason:    ReasonDispatchFailed,
		}
	case containsAny(text, reviewLaunchTransientPhrases...):
		return workerLaunchClassification{
			Class:     domain.WorkflowErrorTransient,
			Certainty: CertaintyInferred,
			Retryable: true,
			Reason:    ReasonWorkerLaunchRetriesExhausted,
		}
	case containsAny(text, reviewLaunchRuntimePhrases...):
		// The wf-57f90ff2 shape: "tmux runtime: set status ...: no such
		// session". The runtime/terminal layer itself failed, independently of
		// the provider binary.
		return workerLaunchClassification{
			Class:     domain.WorkflowErrorRuntimeFailed,
			Certainty: CertaintyInferred,
			Retryable: true,
			Reason:    ReasonWorkerLaunchRetriesExhausted,
		}
	}
	return workerLaunchClassification{
		Class:     base.Class,
		Certainty: CertaintyUnknown,
		Retryable: true,
		Reason:    ReasonWorkerLaunchRetriesExhausted,
	}
}

// workerLaunchRecord is the decoded form of a workerLaunchRecordPhase
// checkpoint's RetryState — the durable memory of the last launch failure for
// one work step, and the thing that makes the retry gate survive a restart.
type workerLaunchRecord struct {
	Attempt   int    `json:"attempt"`
	Class     string `json:"class"`
	Certainty string `json:"certainty"`
	Retryable bool   `json:"retryable"`
	Stage     string `json:"stage"`
	Harness   string `json:"harness"`
	// Error is the deep/root error text, verbatim (bounded), so the actual
	// cause survives the process that observed it.
	Error string `json:"error"`

	// RecordedAt is the checkpoint's own created_at, not part of the JSON —
	// filled in by latestWorkerLaunchRecord so the retry gate is a durable time
	// comparison rather than in-memory state.
	RecordedAt time.Time `json:"-"`
}

// dueForRetry reports whether an automatic retry of this launch failure may run
// now: it must be retryable, and the wake-driven delay must have elapsed.
func (r workerLaunchRecord) dueForRetry(now time.Time) bool {
	return r.Retryable && !now.Before(r.RecordedAt.Add(workerLaunchRetryDelay))
}

// workerLaunchErrorMaxLen bounds the persisted error text. Launch errors are
// short in practice; this only guards against a runtime dumping a whole process
// log into one row.
const workerLaunchErrorMaxLen = 4000

// recordWorkerLaunchFailure is the single terminal-for-this-attempt path for a
// work-step dispatch that failed before any worker session existed. It replaces
// recordDispatchFailure at every pre-work launch site.
//
// The pre-work property is what licenses everything below, and it is a
// structural fact rather than an assumption: this is only ever called from
// attemptWorkHarness, whose two failure sites are "the owner's runtime env
// could not be resolved" and "Spawner.Spawn returned an error". Session
// creation is transactional from workflow's point of view — Spawn either
// returns a session record or returns an error having left none — so a failure
// here means no worker, no workspace and no branch ownership was ever handed
// out. The natural-key re-check in resumeWorkerLaunchAfterFailure re-proves it
// from storage before any retry actually spawns anything.
func (c *Coordinator) recordWorkerLaunchFailure(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	harness domain.AgentHarness,
	stage workerLaunchStage,
	cause error,
) (domain.WorkflowStep, error) {
	now := c.clock()
	cls := classifyWorkerLaunchFailure(cause)
	deep := ""
	if cause != nil {
		deep = cause.Error()
	}
	if len(deep) > workerLaunchErrorMaxLen {
		deep = deep[:workerLaunchErrorMaxLen]
	}

	attempt := c.workerLaunchAttemptCount(ctx, run.ID, step.ID) + 1
	retry := c.workerLaunchRetryAllowed(ctx, run.ID, step.ID, cls)

	detail := fmt.Sprintf("work dispatch failed at stage %s (%s, %s, attempt %d/%d): %s",
		stage, cls.Class, cls.Certainty, attempt, maxWorkerLaunchAttempts, deep)

	// Persist the deep error and the classification, always — this is the record
	// the retry gate, the next daemon boot, and a human debugging the run all
	// read.
	stepID := step.ID
	state, _ := json.Marshal(workerLaunchRecord{
		Attempt: attempt, Class: string(cls.Class), Certainty: string(cls.Certainty),
		Retryable: retry, Stage: string(stage), Harness: string(harness), Error: deep,
	})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction:     detail,
		DurablePhase:   workerLaunchRecordPhase,
		PayloadVersion: "v1",
		RetryState:     string(state),
		CreatedAt:      now,
	}); err != nil {
		return step, err
	}

	if retry {
		// Retryable: the outbox entry goes back to Pending under the SAME
		// idempotency key, so the retry re-enters dispatchFromPending exactly
		// once (never a second outbox row, never a parallel dispatch), and a
		// durable wake makes the retry happen without a human or a poll. The
		// step stays `running` and the run stays where it is: nothing about a
		// launch AO is about to redo is a stop, and moving the run to
		// needs_attention here is precisely what used to make the master mirror
		// a hiccup as a human decision.
		if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxPending, now, string(cls.Class)); err != nil {
			return step, err
		}
		c.scheduleWake(ctx, run, stepIDPtr(step.ID), wake.ReasonTransientRetry, string(harness))
		c.recordAttentionStop(ctx, run, &stepID, ReasonWorkerLaunchRetry, detail)
		if c.log != nil {
			c.log.Warn("workflow: worker launch failed, retry scheduled",
				"step", step.ID, "stage", stage, "class", cls.Class, "attempt", attempt, "err", cause)
		}
		return step, nil
	}

	// Permanent, or out of automatic budget: the same durable failure shape this
	// path has always written (outbox failed, step failed, run needs_attention),
	// so existing readers and the rows already on disk keep their meaning — but
	// now with a truthful reason, and with resumeWorkerLaunchAfterFailure as the
	// documented way back out.
	reason := cls.Reason
	if cls.Retryable {
		// Retryable in kind, but the budget is gone: the honest reason is that
		// every automatic retry was used, not that the cause was permanent.
		reason = ReasonWorkerLaunchRetriesExhausted
	}
	if c.log != nil {
		c.log.Warn("workflow: worker launch failed permanently",
			"step", step.ID, "stage", stage, "class", cls.Class, "reason", reason, "attempt", attempt, "err", cause)
	}
	return c.recordDispatchFailure(ctx, run, step, entry, cls.Class, reason, cause)
}

// workerLaunchRetryAllowed is the bounded-retry policy itself, as one
// expression: a cause AO may retry, and budget left to retry it with.
//
// It is a function rather than an inlined condition because the crash/restart
// reconciler has to ASK the policy before it acts — a reconciled contradiction
// that the policy would not retry must stop with its evidence rather than be
// pushed through the failure path and stop without it (see
// dispatch_reconcile.go). Two copies of this expression would be two policies.
func (c *Coordinator) workerLaunchRetryAllowed(
	ctx stdctx.Context,
	runID, stepID string,
	cls workerLaunchClassification,
) bool {
	return cls.Retryable && c.workerLaunchAttemptCount(ctx, runID, stepID)+1 < maxWorkerLaunchAttempts
}

// latestWorkerLaunchRecord returns the newest launch-failure record for a work
// step, and whether it is still the operative state — i.e. no worker has
// actually dispatched since. Reading the ledger (rather than the step's latest
// checkpoint) is what makes this survive the routing/session-lifecycle
// checkpoints every retry pass writes before it gets to Spawn.
func (c *Coordinator) latestWorkerLaunchRecord(ctx stdctx.Context, runID, stepID string) (workerLaunchRecord, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return workerLaunchRecord{}, false
	}
	// ListWorkflowCheckpoints is ordered oldest-first (created_at, id), so the
	// last match wins. Index order — not timestamp comparison — is deliberate:
	// several checkpoints of one dispatch attempt share a clock reading.
	recordIdx, dispatchedIdx := -1, -1
	for i, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		switch cp.DurablePhase {
		case workerLaunchRecordPhase:
			recordIdx = i
		case workerDispatchedDurablePhase:
			dispatchedIdx = i
		}
	}
	if recordIdx < 0 || dispatchedIdx > recordIdx {
		return workerLaunchRecord{}, false
	}
	var rec workerLaunchRecord
	if json.Unmarshal([]byte(cps[recordIdx].RetryState), &rec) != nil {
		return workerLaunchRecord{}, false
	}
	rec.RecordedAt = cps[recordIdx].CreatedAt
	return rec, true
}

// workerLaunchAttemptCount counts the automatic launch attempts already burned
// for one work step, since the newest human-initiated retry (which resets the
// budget — a person who fixed the cause is entitled to a fresh set of attempts).
func (c *Coordinator) workerLaunchAttemptCount(ctx stdctx.Context, runID, stepID string) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return 0
	}
	count := 0
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		switch cp.DurablePhase {
		case workerLaunchHumanRetryPhase, workerDispatchedDurablePhase:
			count = 0
		case workerLaunchRecordPhase:
			count++
		}
	}
	return count
}

// workerLaunchRecoveryGenerations counts how many times a human Continue has
// already reopened this work step's dispatch.
func (c *Coordinator) workerLaunchRecoveryGenerations(ctx stdctx.Context, runID, stepID string) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return maxWorkerLaunchRecoveryGenerations // failing to read is never a licence to reopen
	}
	count := 0
	for _, cp := range cps {
		if cp.WorkflowStepID != nil && *cp.WorkflowStepID == stepID && cp.DurablePhase == workerLaunchHumanRetryPhase {
			count++
		}
	}
	return count
}

// workerLaunchStopReasons are the stops a pre-work dispatch failure can park a
// run on. resumeWorkerLaunchAfterFailure un-parks exactly these and nothing
// else, which is the whole guarantee that unrelated parent/child attention is
// never cleared by this file.
var workerLaunchStopReasons = map[string]bool{
	ReasonDispatchFailed:               true,
	ReasonWorkerLaunchRetry:            true,
	ReasonWorkerLaunchRetriesExhausted: true,
	unclassifiedStop:                   true,
}

// preWorkDispatchEvidence is what resumeWorkerLaunchAfterFailure proved before
// it is allowed to reopen anything, recorded on the authorization checkpoint so
// the reopen is auditable rather than a state change nobody can account for.
type preWorkDispatchEvidence struct {
	Generation int    `json:"generation"`
	Class      string `json:"class"`
	Stage      string `json:"stage"`
	Source     string `json:"source"`
	Error      string `json:"error,omitempty"`
}

// workerLaunchFailureEvidence decides whether a work step's durable state is
// the one thing this file may reopen: a dispatch that failed BEFORE any worker
// session existed.
//
// It recognises two shapes, and only these two:
//
//   - the record recordWorkerLaunchFailure writes (source "launch_record"); and
//   - the shape already on disk from before this file existed (source
//     "legacy_dispatch_failed"): an outbox entry durably `failed`, and the run's
//     newest canonical stop being ReasonDispatchFailed — which
//     recordDispatchFailure is the only writer of, and which it only ever
//     reaches from attemptWorkHarness, i.e. before the worker owned anything.
//     This is what makes wf-57f90ff2's already-persisted rows recoverable
//     without recognising that run, that step, or that tmux error string.
//
// Anything else — an entry still `dispatched` (ambiguous: adoptOrMarkAmbiguous
// owns it), a step that already has a session, a run stopped for some other
// reason — is not this function's business and answers false.
func (c *Coordinator) workerLaunchFailureEvidence(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, entry domain.WorkflowOutboxEntry,
) (preWorkDispatchEvidence, bool) {
	if entry.Status != domain.WorkflowOutboxFailed {
		return preWorkDispatchEvidence{}, false
	}
	if rec, ok := c.latestWorkerLaunchRecord(ctx, run.ID, step.ID); ok {
		return preWorkDispatchEvidence{
			Class: rec.Class, Stage: rec.Stage, Source: "launch_record", Error: rec.Error,
		}, true
	}
	reason, ok := c.latestCanonicalStopReason(ctx, run.ID)
	if !ok || reason != ReasonDispatchFailed {
		return preWorkDispatchEvidence{}, false
	}
	return preWorkDispatchEvidence{
		Class: entry.ErrorClass, Stage: string(workerLaunchStageSpawn), Source: "legacy_dispatch_failed",
	}, true
}

// resumeWorkerLaunchAfterFailure is the human-driven way out of a durably
// failed pre-work dispatch. ContinueRun is its only caller: read-time polling
// (GetRun/Board) must never reopen a terminal state, however often it
// re-derives it.
//
// It returns the (possibly updated) run and step and whether it reopened
// anything. The order of operations is what makes it restart-safe:
//
//  1. Adoption first. If a session already exists for this step's natural key,
//     the failure record is stale and a retry would be a SECOND worker — so the
//     existing one is adopted through the ordinary success path and nothing is
//     spawned. This is the "never duplicate worker execution if startup may
//     actually have succeeded" rule, re-proved against storage at the moment of
//     the retry rather than trusted from the failure record.
//  2. Evidence, then budget. Both are read-only.
//  3. The authorization checkpoint, written BEFORE any mutation, so a daemon
//     that dies mid-reopen still knows on the next pass that this generation was
//     authorized and counted.
//  4. The step reopen (a compare-and-swap on state='failed', so a second caller
//     matches no row and reopens nothing).
//  5. The outbox reopen (a compare-and-swap on status='failed', same property).
//     A crash between 4 and 5 leaves ready+failed, which the next pass finishes;
//     a crash after 5 leaves ready+pending, which is exactly a dispatchable
//     step and needs no recovery at all.
//  6. The un-park, restricted to the stops this file's failures produce.
func (c *Coordinator) resumeWorkerLaunchAfterFailure(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
) (domain.WorkflowRun, domain.WorkflowStep, bool, error) {
	if step.Kind != domain.WorkflowStepWork || step.State != domain.WorkflowStepFailed || step.SessionID != nil {
		return run, step, false, nil
	}
	if run.State.Terminal() {
		return run, step, false, nil
	}
	// Fetch (never create) this step's dispatch command. The key is derived
	// purely from the step id, and Enqueue is an idempotent insert-or-get, so
	// this returns the very row the failed dispatch used.
	entry, created, err := c.store.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID:             "wfo-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &step.ID,
		IdempotencyKey: workStepOutboxIdempotencyKey(step.ID),
		CommandType:    domain.WorkflowOutboxSpawnWorkerSession,
		Payload:        spawnPayloadJSON(run.ProjectID, step.ID),
		CreatedAt:      c.clock(),
	})
	if err != nil {
		return run, step, false, err
	}
	if created {
		// No dispatch was ever attempted for this step, so its failure is not a
		// launch failure and this file has nothing to say about it.
		return run, step, false, nil
	}

	// 1. Adoption before retry.
	if c.sessionFacts != nil {
		rec, found, ferr := c.sessionFacts.FindSessionByProjectAndIssueID(ctx, domain.ProjectID(run.ProjectID), workStepIssueID(step.ID))
		if ferr != nil {
			return run, step, false, ferr
		}
		if found && (rec.Metadata.WorkspacePath != "" || rec.Metadata.Branch != "") {
			if _, rerr := c.store.ReopenFailedWorkflowStep(ctx, step.ID, c.clock()); rerr != nil {
				return run, step, false, rerr
			}
			step.State = domain.WorkflowStepReady
			adopted, aerr := c.recordDispatchSuccess(ctx, run, step, entry, rec)
			if aerr != nil {
				return run, step, false, aerr
			}
			run = c.unparkWorkerLaunchStop(ctx, run, "an existing worker session for this step was adopted; no second worker was started")
			return run, adopted, true, nil
		}
	}

	// 2. Evidence, then budget.
	evidence, ok := c.workerLaunchFailureEvidence(ctx, run, step, entry)
	if !ok {
		return run, step, false, nil
	}
	generation := c.workerLaunchRecoveryGenerations(ctx, run.ID, step.ID) + 1
	if generation > maxWorkerLaunchRecoveryGenerations {
		c.recordAttentionStopOnce(ctx, run, &step.ID, ReasonWorkerLaunchRetriesExhausted,
			fmt.Sprintf("work dispatch was reopened %d times and failed to start a worker every time (%s); AO will not reopen it again",
				maxWorkerLaunchRecoveryGenerations, evidence.Class))
		return run, step, false, nil
	}
	evidence.Generation = generation

	// 3. Authorization, durably, before anything is mutated.
	stepID := step.ID
	state, _ := json.Marshal(evidence)
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction: fmt.Sprintf(
			"worker_launch_human_retry: reopening the work dispatch after a human continue (generation %d/%d, previous failure: %s)",
			generation, maxWorkerLaunchRecoveryGenerations, evidence.Class),
		DurablePhase:   workerLaunchHumanRetryPhase,
		PayloadVersion: "v1",
		RetryState:     string(state),
		CreatedAt:      c.clock(),
	}); err != nil {
		return run, step, false, err
	}

	// 4. Step out of its terminal state.
	if _, err := c.store.ReopenFailedWorkflowStep(ctx, step.ID, c.clock()); err != nil {
		return run, step, false, err
	}
	step.State = domain.WorkflowStepReady

	// 5. Outbox back to Pending under the SAME idempotency key: no second row,
	// no second command, one dispatch.
	if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, domain.WorkflowOutboxFailed, domain.WorkflowOutboxPending, c.clock(), evidence.Class); err != nil {
		return run, step, false, err
	}

	// 6. Un-park, so the redispatch is not immediately dropped as an invalid
	// transition out of needs_attention (which only ever moves forward to
	// running).
	run = c.unparkWorkerLaunchStop(ctx, run,
		fmt.Sprintf("a person reopened the work dispatch that never started a worker (%s)", evidence.Class))
	if c.log != nil {
		c.log.Info("workflow: work step dispatch reopened by a human continue",
			"run", run.ID, "step", step.ID, "generation", generation, "class", evidence.Class, "evidence", evidence.Source)
	}
	return run, step, true, nil
}

// unparkWorkerLaunchStop releases a run parked by a pre-work dispatch failure.
// Like clearReviewLaunchStop it is only ever called from a site that has just
// established the condition is being acted on, and it touches exactly the
// reasons a pre-work dispatch failure writes: a run stopped for anything else
// (a dirty worktree, an exhausted fix budget, a child decision) is left exactly
// where it is.
func (c *Coordinator) unparkWorkerLaunchStop(ctx stdctx.Context, run domain.WorkflowRun, evidence string) domain.WorkflowRun {
	if run.State != domain.WorkflowRunNeedsAttention {
		return run
	}
	reason, ok := c.latestCanonicalStopReason(ctx, run.ID)
	if !ok || !workerLaunchStopReasons[reason] {
		return run
	}
	return c.unparkRun(ctx, run, reason, evidence)
}
