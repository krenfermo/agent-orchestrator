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

// This file is the recovery lifecycle for one specific, previously fatal
// moment: the reviewer launch failed AFTER the review/review_run rows and the
// trigger_review outbox entry were durably created, but BEFORE any reviewer
// session existed.
//
// The real incident (wf-6d290889-3a7f-41e1-9aa7-b2b265b2ad13) went like this:
// work completed, policy required review, Codex was selected, the review target
// was observed and checkpointed, review + review_run rows were created, the
// outbox entry was created — and then the launch failed. What the old
// recordReviewDispatchFailure did with that:
//
//   - it recorded the outbox error_class as the flat literal
//     "reviewer_launch_failed", and left the ACTUAL error only in a log line, so
//     the deep/root cause did not survive the process;
//   - it left the review_run it had just inserted at status "running" forever —
//     a durable lie, and (because that row occupies the (session, target_sha)
//     unique index) also the thing that would make any retry adopt the orphan
//     instead of launching a reviewer;
//   - it moved the review step to "failed", a terminal state with no outgoing
//     transitions, so even a human who fixed the underlying cause had nothing
//     left to resume; and
//   - it treated every cause identically: a temporary spawn failure got the same
//     needs_attention dead end as a missing binary.
//
// The rules here replace that:
//
//  1. Every launch failure persists its deep error and a real classification
//     (class + certainty + stage), not just "reviewer_launch_failed".
//  2. No partial durable state outlives the failure: a review_run created for a
//     reviewer that never launched is closed out as failed (CAS-guarded on
//     status='running', so it can never clobber a verdict that landed in the
//     same instant).
//  3. Transient causes retry automatically, bounded, with a durable wake, and
//     never ask a person for anything.
//  4. Permanent causes stop with a named reason and a concrete action.
//  5. The review step is left non-terminal in both cases, so a retry — automatic
//     or human — always has somewhere to resume from.

const (
	// reviewLaunchRecordPhase is the durable_phase of the machine-readable
	// record every launch failure writes: RetryState carries the classification
	// and the deep error, NextAction carries the human-readable form. It is
	// deliberately NOT a canonical attention reason (attention.go) — the reason
	// is written separately by recordAttentionStop, so the newest checkpoint a
	// stopped run has always NAMES the stop, while this one explains it.
	reviewLaunchRecordPhase = "reviewer_launch_error"

	// reviewLaunchHumanRetryPhase marks a human-initiated resume of a review
	// launch that had stopped permanently. It resets the automatic retry budget:
	// attempts are only ever counted since the newest one of these.
	reviewLaunchHumanRetryPhase = "reviewer_launch_human_retry"

	// reviewDispatchedDurablePhase is recordReviewDispatchSuccess's own phase.
	// A launch-failure record older than one of these has been superseded by a
	// reviewer that actually launched, and must never be read as current state.
	reviewDispatchedDurablePhase = "review_dispatched"

	// reviewLaunchRetryDelay is the minimum age of a launch-failure record
	// before AO retries it by itself. The durable wake this file schedules is
	// what actually drives the retry; this delay is what stops the read path
	// from front-running it — GetRun polls every 2s while a run is non-terminal,
	// and without a floor here a "retry" would mean three launches inside one
	// second and an exhausted budget before the transient condition had any
	// chance to clear. A human-driven Continue bypasses it: a person asking now
	// means now.
	reviewLaunchRetryDelay = 30 * time.Second

	// maxReviewerLaunchAttempts bounds automatic retries of one review cycle's
	// reviewer launch. The budget exists so a permanently broken environment
	// that merely *looks* transient still reaches a human instead of retrying
	// forever (same reasoning as WakePolicy.MaxAttempts in dispatch.go).
	maxReviewerLaunchAttempts = 3
)

// reviewLaunchStage names how far the dispatch got before it failed. It is
// recorded because "the review row could not be written" and "the reviewer
// process would not start" are different problems with the same class.
type reviewLaunchStage string

const (
	reviewLaunchStageReviewRow  reviewLaunchStage = "review_row"
	reviewLaunchStageReviewRun  reviewLaunchStage = "review_run"
	reviewLaunchStagePreflight  reviewLaunchStage = "preflight"
	reviewLaunchStageRuntimeEnv reviewLaunchStage = "runtime_env"
	reviewLaunchStageLaunch     reviewLaunchStage = "launch"
)

// reviewLaunchClassification is the verdict on one launch failure: which error
// class it really is, how confident that is, whether AO may retry it by itself,
// and — when it may not — the canonical attention reason that names it.
type reviewLaunchClassification struct {
	Class     domain.WorkflowErrorClass
	Certainty ClassificationCertainty
	Retryable bool
	// Reason is the canonical attention reason used when this failure is (or
	// becomes) a human decision. Always non-empty.
	Reason string
}

// permanent launch signals: configuration/policy problems no amount of retrying
// can change. Deliberately a short, explicit list — an unrecognised failure is
// treated as retryable-but-bounded (below), never silently declared permanent.
var reviewLaunchPermanentPhrases = []string{
	"unsupported", "not supported", "unsupported configuration",
	"invalid configuration", "invalid config", "misconfigured",
	"unknown harness", "unknown reviewer", "no such reviewer",
	"policy violation", "violates policy", "forbidden by policy", "not permitted",
}

// transient launch signals: the process/runtime/transport refused right now.
var reviewLaunchTransientPhrases = []string{
	"temporarily unavailable", "resource temporarily unavailable", "try again",
	"connection refused", "connection reset", "broken pipe", "eof",
	"timeout", "timed out", "deadline exceeded", "unavailable",
	"too many open files", "device or resource busy", "cannot allocate memory",
	"no space left", "signal: killed",
}

// runtime/transport launch signals: the terminal/runtime layer itself failed,
// independent of the reviewer binary (transient in the same bounded way, but
// worth its own honest class).
var reviewLaunchRuntimePhrases = []string{
	"tmux", "pty", "pane", "terminal", "runtime", "spawn", "fork/exec", "transport",
}

// classifyReviewerLaunchFailure classifies one reviewer-launch failure.
//
// It layers on classifyProviderFailure (failure_classifier.go) rather than
// re-deriving provider semantics: typed sentinels (auth, missing binary,
// provider profile) keep the exact meaning they have everywhere else in
// workflow, and only the launch-specific question — "may AO retry this by
// itself?" — is answered here.
//
// The default for an unrecognised failure is retryable-but-bounded, not
// permanent: a launch that failed for a reason AO cannot name is far more often
// a momentary process/runtime problem than a configuration one, and the
// maxReviewerLaunchAttempts budget guarantees an unnameable failure still
// reaches a human quickly instead of looping.
func classifyReviewerLaunchFailure(err error) reviewLaunchClassification {
	// An orphaned session outranks every other reading of a launch failure.
	//
	// Ordinary launch failures mean nothing is running, so retrying them is
	// free. This one means a session may still BE running that AO cannot
	// identify — and therefore cannot adopt or terminate. Retrying it would pile
	// a second reviewer on top of the first, so it is never retryable and always
	// goes to a person.
	if errors.Is(err, ports.ErrRuntimeOrphanedSession) {
		return reviewLaunchClassification{
			Class:     domain.WorkflowErrorReviewerLaunchFailed,
			Certainty: CertaintyUnknown,
			Retryable: false,
			Reason:    ReasonReviewStateAmbiguous,
		}
	}
	base := classifyProviderFailure(err)
	switch base.Class {
	case domain.WorkflowErrorAuth:
		return reviewLaunchClassification{Class: base.Class, Certainty: base.Certainty, Reason: ReasonReviewerAuthInvalid}
	case domain.WorkflowErrorBinaryMissing:
		return reviewLaunchClassification{Class: base.Class, Certainty: base.Certainty, Reason: ReasonReviewerBinaryMissing}
	case domain.WorkflowErrorRateLimited, domain.WorkflowErrorCapacityExhausted:
		// Time-boxed provider conditions: exactly what a bounded retry with a
		// durable wake exists for.
		return reviewLaunchClassification{Class: base.Class, Certainty: base.Certainty, Retryable: true, Reason: ReasonReviewerLaunchRetriesExhausted}
	}

	text := ""
	if err != nil {
		text = strings.ToLower(err.Error())
	}
	switch {
	case containsAny(text, reviewLaunchPermanentPhrases...):
		return reviewLaunchClassification{
			Class:     domain.WorkflowErrorReviewerLaunchFailed,
			Certainty: CertaintyInferred,
			Reason:    ReasonReviewerLaunchUnsupported,
		}
	case containsAny(text, reviewLaunchTransientPhrases...):
		return reviewLaunchClassification{
			Class:     domain.WorkflowErrorTransient,
			Certainty: CertaintyInferred,
			Retryable: true,
			Reason:    ReasonReviewerLaunchRetriesExhausted,
		}
	case containsAny(text, reviewLaunchRuntimePhrases...):
		return reviewLaunchClassification{
			Class:     domain.WorkflowErrorRuntimeFailed,
			Certainty: CertaintyInferred,
			Retryable: true,
			Reason:    ReasonReviewerLaunchRetriesExhausted,
		}
	}
	return reviewLaunchClassification{
		Class:     base.Class,
		Certainty: CertaintyUnknown,
		Retryable: true,
		Reason:    ReasonReviewerLaunchRetriesExhausted,
	}
}

// reviewLaunchRecord is the decoded form of a reviewLaunchRecordPhase
// checkpoint's RetryState — the durable memory of the last launch failure for
// one review cycle.
type reviewLaunchRecord struct {
	Cycle     int    `json:"cycle"`
	Attempt   int    `json:"attempt"`
	Class     string `json:"class"`
	Certainty string `json:"certainty"`
	Retryable bool   `json:"retryable"`
	Stage     string `json:"stage"`
	Harness   string `json:"harness"`
	TargetSHA string `json:"targetSha"`

	// --- the durable outbox correlation ---------------------------------
	//
	// A launch failure is not merely "something that happened to this step".
	// It is the failure OF ONE OUTBOX GENERATION: one claim, one cycle, one
	// budget epoch, one attempt. Without those four written down, the only
	// question the ledger could answer was "what is the newest launch error on
	// this step?" — and that is the wrong question for a human resume.
	//
	// The entry is REUSED across retries (failed -> pending -> failed under the
	// same idempotency key), so the key alone does not separate one generation
	// from the next; the record's own id does. These fields are what let a
	// resume ask "is the failure I observed still the current one?" instead of
	// "is there a failure?".
	OutboxID       string `json:"outboxId,omitempty"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	StepID         string `json:"stepId,omitempty"`
	// Epoch is the budget generation this failure was produced in. A human
	// resume opens the next one, so a failure carrying epoch N is by
	// construction a different generation from one carrying epoch N+1.
	Epoch int `json:"epoch,omitempty"`
	// Error is the deep/root error text, verbatim (bounded), so the actual
	// cause survives the process that observed it.
	Error string `json:"error"`

	// RecordedAt is the checkpoint's own created_at, not part of the JSON —
	// filled in by latestReviewLaunchRecord so the retry gate can be a durable
	// time comparison rather than in-memory state.
	RecordedAt time.Time `json:"-"`

	// RecordID is this record's own checkpoint id, likewise filled in on read.
	//
	// Together with IdempotencyKey it IS the identity of the failed outbox
	// generation. Exactly one of these records is written per launch failure,
	// immediately before the outbox entry moves to `failed`, so "the failure
	// that put this entry in the state a human is now resuming" has a durable,
	// unique name — which is what the human-resume claim is taken against.
	//
	// It is never resolved by "newest error on the step": see
	// reviewLaunchFailureForEntry.
	RecordID string `json:"-"`
}

// dueForRetry reports whether an automatic retry of this launch failure may run
// now: it must be retryable, and the wake-driven delay must have elapsed.
func (r reviewLaunchRecord) dueForRetry(now time.Time) bool {
	return r.Retryable && !now.Before(r.RecordedAt.Add(reviewLaunchRetryDelay))
}

// reviewLaunchErrorMaxLen bounds the persisted error text. Launch errors are
// short in practice; this only guards against a runtime dumping an entire
// process log into one row.
const reviewLaunchErrorMaxLen = 4000

// recordReviewLaunchFailure is the single terminal-for-this-attempt path for a
// reviewer launch that failed before any reviewer session existed. It replaces
// recordReviewDispatchFailure for every launch-stage failure.
//
// reviewRunID is the review_run this attempt had already inserted, or "" if the
// failure happened before that point. It is the whole partial-durable-state
// cleanup: a review_run for a reviewer that never launched must not stay
// "running".
func (c *Coordinator) recordReviewLaunchFailure(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	harness domain.ReviewerHarness,
	reviewRunID, targetSHA string,
	cycleNumber int,
	stage reviewLaunchStage,
	cause error,
) (domain.WorkflowStep, error) {
	now := c.clock()
	cls := classifyReviewerLaunchFailure(cause)
	deep := ""
	if cause != nil {
		deep = cause.Error()
	}
	if len(deep) > reviewLaunchErrorMaxLen {
		deep = deep[:reviewLaunchErrorMaxLen]
	}

	// 0. ATTRIBUTE THE FAILURE BEFORE CAUSING IT.
	//
	// Everything below is the same two-write shape the abandon protocol exists
	// for: the review_run is marked failed, and then the outbox claim moves. A
	// crash between them leaves failed row + dispatched outbox, which without a
	// marker is unattributable — recovery cannot tell an interrupted cleanup
	// from a failure it must not touch, so the run rests in needs_attention with
	// no reviewer and no way to get one.
	//
	// The marker names this exact claim, so a restart finishes the cleanup and
	// the launch protocol resumes. The retry budget is durable (it is counted
	// from the ledger, not from memory), so resuming cannot outrun it.
	// 0. ALLOCATE THE ATTEMPT, AND MAKE IT DURABLE, BEFORE ANYTHING ELSE.
	//
	// The attempt number is read first and written into the abandon intent —
	// the first durable write of this whole sequence — so the budget is spent
	// before any state exists that could later authorise another launch. Doing
	// the accounting afterwards is what let a crash mid-cleanup return an
	// attempt to the pool and, repeated, produce review generations without end.
	// The attempt this failure belongs to was allocated at CLAIM time, before any
	// of the work that could fail. This only reads it back.
	//
	// FAIL CLOSED on an unreadable or undecodable ledger: an unknown attempt
	// number is not attempt 1, and treating it as one would let a cycle that has
	// spent everything look untouched.
	//
	// The history is read ONCE here, because this record must state both the
	// attempt it spent and the EPOCH it spent it in: those two, with the claim
	// and the cycle, are the durable identity of the outbox generation this
	// failure produced.
	h, cerr := c.reviewLaunchAttempts(ctx, run.ID, reviewStep.ID)
	if cerr != nil {
		if c.log != nil {
			c.log.Warn("workflow: cannot read the reviewer retry history; refusing to consume budget",
				"run", run.ID, "step", reviewStep.ID, "err", cerr)
		}
		return reviewStep, cerr
	}
	attempt := h.attemptForClaim(entry.IdempotencyKey, cycleNumber)
	retry := cls.Retryable && attempt < maxReviewerLaunchAttempts

	if reviewRunID != "" {
		// And the review row, when there is one, gets its own attributable
		// close-out — see abandonUnlaunchedReviewRun.
		if err := c.recordReviewLaunchAttemptAbandon(ctx, run, reviewStep, entry, reviewRunID,
			cycleNumber, attempt,
			fmt.Sprintf("reviewer launch failed at stage %s", stage)); err != nil {
			// Unattributable is the one outcome worse than an unrecorded
			// failure, so nothing is changed and the next pass tries again.
			return reviewStep, err
		}
	}

	// 1. Clean up the partial durable state first: whatever happens to the step
	// and the run below, a review_run whose reviewer never started is not
	// running, and saying so is not optional.
	c.failPartialReviewRun(ctx, reviewRunID, cls, stage, deep)

	detail := fmt.Sprintf("reviewer launch failed at stage %s (%s, %s, attempt %d/%d): %s",
		stage, cls.Class, cls.Certainty, attempt, maxReviewerLaunchAttempts, deep)

	// 2. Persist the deep error and the classification, always — this is the
	// record the retry path, the next daemon boot, and a human debugging the run
	// all read.
	stepID := reviewStep.ID
	// The record's id is allocated HERE, before the write, because it is half of
	// the generation identity this failure is about to stamp onto the outbox
	// row. The row and the ledger must name the same failure, so the name is
	// chosen once and used for both.
	recordID := "wfc-" + c.newID()
	generation := entry.IdempotencyKey + "|" + recordID
	state, _ := json.Marshal(reviewLaunchRecord{
		Cycle: cycleNumber, Attempt: attempt, Class: string(cls.Class),
		Certainty: string(cls.Certainty), Retryable: retry, Stage: string(stage),
		Harness: string(harness), TargetSHA: targetSHA, Error: deep,
		// The correlation. This record is the identity of ONE failed outbox
		// generation, and a human resume is bound to the generation it
		// observed — so the generation has to be written down, not inferred
		// later from whichever launch error happens to be newest.
		OutboxID: entry.ID, IdempotencyKey: entry.IdempotencyKey,
		StepID: stepID, Epoch: h.epoch,
	})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             recordID,
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		HeadSHA:        targetSHA,
		NextAction:     detail,
		DurablePhase:   reviewLaunchRecordPhase,
		PayloadVersion: "v1",
		RetryState:     string(state),
		CreatedAt:      now,
	}); err != nil {
		return reviewStep, err
	}

	// 3. The review step rests at "waiting" in BOTH cases, never "failed":
	// failed is terminal with no outgoing transitions, which is what made the
	// original incident unrecoverable even after the cause was fixed.
	if domain.ValidWorkflowStepTransition(reviewStep.State, domain.WorkflowStepWaiting) {
		if _, err := c.store.UpdateWorkflowStepState(ctx, reviewStep.ID, reviewStep.State, domain.WorkflowStepWaiting, now); err != nil {
			return reviewStep, err
		}
		reviewStep.State = domain.WorkflowStepWaiting
	}

	if retry {
		// Retryable: the outbox entry goes back to Pending under the SAME
		// idempotency key, so the retry re-enters dispatchReviewFromPending
		// exactly once (never a second outbox row, never a parallel dispatch),
		// and a durable wake makes the retry happen without a human or a poll.
		// THROUGH THE GATED RELEASE, like every other dispatched -> pending
		// transition for a review claim. Moving the outbox directly was a way
		// around the retry budget: this is the ordinary retry path, so it is the
		// one that most needs the gate rather than the one that may skip it.
		if ok, reason, berr := c.reviewLaunchBudgetRemains(ctx, run, reviewStep, entry); berr != nil || !ok {
			return c.markReviewAmbiguous(ctx, run, reviewStep, reason)
		}
		// Ownership-conditioned, exactly like the permanent branch below: a
		// release is a transition off `dispatched` that only this dispatch's
		// claim entitles it to make. A stale one would hand a live dispatch's
		// row back to the pending pool.
		released, rerr := c.store.ReleaseDispatchedWorkflowOutboxGeneration(
			ctx, entry.ID, string(cls.Class), entry.DispatchGeneration)
		if rerr != nil {
			return reviewStep, rerr
		}
		if !released {
			if c.log != nil {
				c.log.Warn("workflow: not releasing a review claim this dispatch no longer owns",
					"run", run.ID, "step", reviewStep.ID, "key", entry.IdempotencyKey,
					"generation", entry.DispatchGeneration)
			}
			return reviewStep, nil
		}
		c.scheduleWake(ctx, run, stepIDPtr(reviewStep.ID), wake.ReasonTransientRetry, string(harness))
		c.recordAttentionStop(ctx, run, &stepID, ReasonReviewerLaunchRetry, detail)
		if c.log != nil {
			c.log.Warn("workflow: reviewer launch failed, retry scheduled",
				"step", reviewStep.ID, "stage", stage, "class", cls.Class, "attempt", attempt, "err", cause)
		}
		return reviewStep, nil
	}

	// Permanent (or out of automatic budget): durable failure, named stop, and a
	// concrete action for the person who now owns it.
	//
	// The row is stamped with THIS failure's generation in the same statement
	// that fails it. That stamp is the only thing that later distinguishes this
	// failed state from the next one on the same row — and a human resume is
	// compare-and-swapped against it, so it may never be written separately,
	// afterwards, or not at all.
	//
	// AND it proves this dispatch still owns the row. `entry.Status` alone is
	// the state, not the owner: this call can arrive after the caller paused —
	// its abandon evidence and its launch-error record already written — while
	// recovery released the claim and a SECOND dispatch took the row. The row is
	// `dispatched` in both worlds. Only the claim token separates them, and
	// without it this failure would be stamped onto a live generation that never
	// failed, which a human resume could then reopen into a third dispatch.
	failed, err := c.store.FailWorkflowOutboxWithGeneration(
		ctx, entry.ID, entry.Status, now, string(cls.Class), generation, entry.DispatchGeneration)
	if err != nil {
		return reviewStep, err
	}
	if !failed {
		// This dispatch no longer owns the row. It writes NOTHING to the outbox
		// and parks nothing: the generation that holds the claim now is somebody
		// else's business, and its own failure record stands as history.
		if c.log != nil {
			c.log.Warn("workflow: not failing a review claim this dispatch no longer owns",
				"run", run.ID, "step", reviewStep.ID, "key", entry.IdempotencyKey,
				"generation", entry.DispatchGeneration)
		}
		return reviewStep, nil
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return reviewStep, err
		}
	}
	reason := cls.Reason
	if cls.Retryable {
		// Retryable in kind, but the budget is gone: the honest reason is that
		// every automatic retry was used, not that the cause was permanent.
		reason = ReasonReviewerLaunchRetriesExhausted
	}
	c.recordAttentionStop(ctx, run, &stepID, reason, detail)
	if c.log != nil {
		c.log.Warn("workflow: reviewer launch failed permanently",
			"step", reviewStep.ID, "stage", stage, "class", cls.Class, "reason", reason, "attempt", attempt, "err", cause)
	}
	return reviewStep, nil
}

func stepIDPtr(id string) *domain.WorkflowStepID {
	sid := domain.WorkflowStepID(id)
	return &sid
}

// failPartialReviewRun closes out the review_run a failed launch had already
// inserted. CAS-guarded in SQL on status='running' (UpdateReviewRunResult), so
// it is idempotent, restart-safe, and can never overwrite a verdict.
//
// "failed" (not "cancelled") is load-bearing: migration 0014 excludes failed
// rows from the (session_id, target_sha) unique index precisely so a retry of
// the same target can insert a fresh run while the failed attempt stays visible
// in history. A cancelled row would keep occupying that key and the retry would
// adopt the orphan instead of launching a reviewer.
func (c *Coordinator) failPartialReviewRun(ctx stdctx.Context, reviewRunID string, cls reviewLaunchClassification, stage reviewLaunchStage, deep string) {
	if c.reviewRuns == nil || reviewRunID == "" {
		return
	}
	body := fmt.Sprintf("reviewer_launch_failed at stage %s (%s): %s — no reviewer session was ever created for this run", stage, cls.Class, deep)
	if _, err := c.reviewRuns.UpdateReviewRunResult(ctx, reviewRunID, domain.ReviewRunFailed, domain.VerdictNone, body, "", false); err != nil && c.log != nil {
		c.log.Warn("workflow: closing out a partial review run failed", "reviewRun", reviewRunID, "err", err)
	}
}

// latestReviewLaunchRecord returns the newest launch-failure record for a review
// step, and whether it is still the operative state — i.e. no reviewer has
// actually dispatched since. Reading the ledger (rather than the step's latest
// checkpoint) is what makes this survive the intermediate checkpoints a routing
// decision or a review-target observation writes on every retry pass.
func (c *Coordinator) latestReviewLaunchRecord(ctx stdctx.Context, runID, stepID string) (reviewLaunchRecord, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return reviewLaunchRecord{}, false
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
		case reviewLaunchRecordPhase:
			recordIdx = i
		case reviewDispatchedDurablePhase:
			dispatchedIdx = i
		}
	}
	if recordIdx < 0 || dispatchedIdx > recordIdx {
		return reviewLaunchRecord{}, false
	}
	var rec reviewLaunchRecord
	if json.Unmarshal([]byte(cps[recordIdx].RetryState), &rec) != nil {
		return reviewLaunchRecord{}, false
	}
	rec.RecordedAt = cps[recordIdx].CreatedAt
	rec.RecordID = cps[recordIdx].ID
	return rec, true
}

// reviewLaunchGeneration is the durable identity of ONE failed outbox
// generation: the claim, the outbox row, the exact launch failure that failed
// it, and the cycle/epoch that failure belongs to.
//
// It exists because a review outbox entry is REUSED. failed -> pending ->
// failed happens under one idempotency key on one row, so neither the key nor
// the row id separates the failure a person looked at from the failure that
// replaced it while they were looking. The launch-failure record's own id does,
// and that is why it is half of this value.
type reviewLaunchGeneration struct {
	OutboxID       string
	IdempotencyKey string
	// RecordID is the reviewer_launch_error checkpoint that produced this
	// generation.
	RecordID string
	Cycle    int
	Epoch    int
	// Stamped reports whether the failure that produced this generation also
	// wrote it onto the outbox row (every failure recorded by this build does).
	// A failure recorded before the column existed did not, and its row still
	// holds the empty stamp — so that is the value its resume must swap on, or
	// durable state written by an older build would become unresumable forever.
	Stamped bool
}

// valid reports whether this names a generation at all. An incomplete
// observation is never treated as "any generation" — it authorises nothing.
func (g reviewLaunchGeneration) valid() bool {
	return g.OutboxID != "" && g.IdempotencyKey != "" && g.RecordID != ""
}

// key is the string form the reset claim is taken against.
func (g reviewLaunchGeneration) key() string { return g.IdempotencyKey + "|" + g.RecordID }

// casValue is what the outbox row must currently hold for this generation to be
// reopenable — the value that goes into the UPDATE's WHERE clause.
//
// It is the generation key for anything this build failed, and the empty stamp
// for a pre-column failure, whose row was never stamped at all. It is NEVER a
// wildcard: an unstamped row and a stamped one are different states, and each
// resume may only move the one it observed.
func (g reviewLaunchGeneration) casValue() string {
	if !g.Stamped {
		return ""
	}
	return g.key()
}

// reviewLaunchFailureForEntry resolves the launch failure that belongs to the
// EXACT outbox generation `entry` is in — never "the newest launch error on this
// step".
//
// That distinction is the whole blocker. Two failures of the same claim are two
// generations; a resume that observed the first and arrives after the second has
// observed a state that no longer exists, and step-level "latest" quietly
// promotes it to the newer failure — a stale human action reopening a generation
// no human ever looked at.
//
// Matching is on the correlation the failure record now persists:
//
//   - a record naming THIS outbox row (and, when it says so, this claim) is a
//     candidate;
//   - a record naming a DIFFERENT row or a different claim is not, however new
//     it is;
//   - a legacy record naming nothing is a candidate only when no correlated
//     record exists at all, so histories written before the correlation still
//     resume rather than becoming unresumable.
//
// A record superseded by a reviewer that actually dispatched is not current
// state, exactly as in latestReviewLaunchRecord.
func (c *Coordinator) reviewLaunchFailureForEntry(
	ctx stdctx.Context, runID, stepID string, entry domain.WorkflowOutboxEntry,
) (reviewLaunchRecord, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// FAIL CLOSED: an unreadable ledger cannot prove which generation is
		// current, and "cannot prove" must never become "the newest one".
		return reviewLaunchRecord{}, false
	}
	correlatedIdx, legacyIdx, dispatchedIdx := -1, -1, -1
	decoded := map[int]reviewLaunchRecord{}
	for i, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		switch cp.DurablePhase {
		case reviewDispatchedDurablePhase:
			dispatchedIdx = i
		case reviewLaunchRecordPhase:
			var rec reviewLaunchRecord
			if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
				continue
			}
			decoded[i] = rec
			switch {
			case rec.OutboxID != "" || rec.IdempotencyKey != "":
				if rec.OutboxID != "" && rec.OutboxID != entry.ID {
					continue
				}
				if rec.IdempotencyKey != "" && rec.IdempotencyKey != entry.IdempotencyKey {
					continue
				}
				correlatedIdx = i
			default:
				legacyIdx = i
			}
		}
	}
	idx := correlatedIdx
	if idx < 0 {
		idx = legacyIdx
	}
	if idx < 0 || dispatchedIdx > idx {
		return reviewLaunchRecord{}, false
	}
	rec := decoded[idx]
	rec.RecordedAt = cps[idx].CreatedAt
	rec.RecordID = cps[idx].ID
	return rec, true
}

// observeFailedReviewLaunchGeneration names the failed generation a caller is
// looking at, AT THE MOMENT IT LOOKS.
//
// This is the observation a human resume is bound to. It is taken with the
// outbox snapshot the caller is acting on, and carried down into the resume
// unchanged — so however long the caller is delayed afterwards, what it may act
// on is still the generation it actually saw.
func (c *Coordinator) observeFailedReviewLaunchGeneration(
	ctx stdctx.Context, runID, stepID string, entry domain.WorkflowOutboxEntry,
) (reviewLaunchGeneration, bool) {
	if entry.Status != domain.WorkflowOutboxFailed || entry.ID == "" || entry.IdempotencyKey == "" {
		return reviewLaunchGeneration{}, false
	}
	rec, ok := c.reviewLaunchFailureForEntry(ctx, runID, stepID, entry)
	if !ok {
		return reviewLaunchGeneration{}, false
	}
	return reviewLaunchGeneration{
		OutboxID:       entry.ID,
		IdempotencyKey: entry.IdempotencyKey,
		RecordID:       rec.RecordID,
		Cycle:          rec.Cycle,
		Epoch:          rec.Epoch,
		// A record that carries the outbox correlation was written by a build
		// that also stamps the row; one that does not, was not.
		Stamped: rec.OutboxID != "",
	}, true
}

// currentOutboxEntry re-reads one outbox row as it stands NOW. A resume is only
// ever entitled to move the state it can still see, so the snapshot it was
// handed is evidence of what a person decided about — never authority over what
// the row currently is.
func (c *Coordinator) currentOutboxEntry(ctx stdctx.Context, runID, entryID string) (domain.WorkflowOutboxEntry, bool) {
	entries, err := c.store.ListWorkflowOutboxByRun(ctx, runID)
	if err != nil {
		return domain.WorkflowOutboxEntry{}, false
	}
	for _, e := range entries {
		if e.ID == entryID {
			return e, true
		}
	}
	return domain.WorkflowOutboxEntry{}, false
}

// reviewLaunchAttemptCount counts the automatic launch attempts already burned
// for one review cycle, since the newest human-initiated retry (which resets the
// budget — a person who fixed the cause is entitled to a fresh set of attempts).
// reviewLaunchAttemptPhase is the DURABLE ALLOCATION of one reviewer launch
// attempt, written before any of the work that attempt performs.
//
// It exists because every earlier place the budget could be inferred from came
// too late. `reviewer_launch_error` is written after the review run is marked
// failed; the abandon intent needs a review run to name. An attempt that failed
// while CREATING that row therefore left no trace at all, and recovery handed
// out another launch as though nothing had happened — repeat the crash and the
// budget is unbounded.
//
// This record depends on nothing but the dispatch claim, so it can be written
// before the row exists, before the launch, before anything can fail.
const reviewLaunchAttemptPhase = "review_launch_attempt"

// reviewLaunchAttemptRecord is one consumed attempt.
//
// Every field recovery needs is EXPLICIT. An earlier version recovered the cycle
// by parsing it back out of the idempotency key, which is fragile in exactly the
// case that matters: a replacement claim need not carry a cycle in its key at
// all, and a cycle that parses to zero matches no history, so a spent budget
// reads as untouched.
type reviewLaunchAttemptRecord struct {
	IdempotencyKey string `json:"idempotencyKey"`
	Cycle          int    `json:"cycle"`
	Attempt        int    `json:"attempt"`
	// Epoch is the budget generation this attempt belongs to. A human resume
	// opens a NEW epoch, and only attempts in the current epoch count against
	// the budget — which is what stops a delayed second reset from hiding
	// attempts that a newer epoch had already spent.
	Epoch int `json:"epoch"`
	// ReviewRunID is empty when the attempt died before the row existed. That is
	// the whole point of this record.
	ReviewRunID string `json:"reviewRunId,omitempty"`
	Why         string `json:"why,omitempty"`
}

// reviewLaunchResetRecord is a human-driven budget reset, and the epoch it opens.
type reviewLaunchResetRecord struct {
	IdempotencyKey string `json:"idempotencyKey"`
	Cycle          int    `json:"cycle"`
	Epoch          int    `json:"epoch"`
	// FailedGeneration names the EXACT failed outbox generation this reset was
	// won against: the claim's idempotency key plus the identity of the launch
	// failure that put the entry into `failed`.
	//
	// It is the reset's uniqueness key, not the epoch number, and that is the
	// whole point. Keying uniqueness on the epoch asks "has anybody opened
	// epoch N?", which a delayed resume can always answer no to — it reads a
	// history that already contains the winner's epoch, computes the NEXT one,
	// and its insert therefore never collides. It then opens a second epoch that
	// hides the attempts the first one spent, and hands the budget back twice.
	//
	// Keying on the generation asks the question that actually bounds this:
	// "has this failure already been resumed?" One failed generation can open at
	// most one epoch, and a duplicate resume of it is a no-op no matter how late
	// it arrives or which epoch it computed.
	FailedGeneration string `json:"failedGeneration"`
	// OutboxID is the row that generation lives on, recorded so the reset is
	// legible on its own rather than only in correlation with the failure
	// record. It is not validated: resets written before it exists are still
	// perfectly good claims.
	OutboxID    string `json:"outboxId,omitempty"`
	ResumedFrom string `json:"resumedFrom,omitempty"`
}

// reviewLaunchResetGeneration names the failed outbox generation a resume is
// acting on: the claim being resumed, and the launch failure that failed it.
//
// Both halves are load-bearing. The idempotency key ties the reset to the exact
// claim, and the record id ties it to the exact failure — so a LATER failure of
// the same claim is a genuinely different generation that a person may resume
// again, while the failure already resumed can never be resumed twice.
func reviewLaunchResetGeneration(entry domain.WorkflowOutboxEntry, rec reviewLaunchRecord) string {
	return entry.IdempotencyKey + "|" + rec.RecordID
}

// reviewLaunchResetHeadSHA is where that generation lives on the row. The
// partial UNIQUE index in migration 0136 is over (workflow_step_id, head_sha),
// so putting the generation here is what makes the database — not a read
// followed by a write — pick the single winner.
func reviewLaunchResetHeadSHA(generation string) string {
	return "review-launch-reset-gen-" + generation
}

// validate applies the same strict rules the other budget records get. A reset
// is the one record that can HAND BACK budget, so a malformed one is the most
// dangerous to accept, not the least.
func (r reviewLaunchResetRecord) validate(id string) error {
	switch {
	case r.IdempotencyKey == "":
		return fmt.Errorf("workflow: reviewer reset record %s names no claim", id)
	case r.Cycle <= 0:
		return fmt.Errorf("workflow: reviewer reset record %s names no review cycle", id)
	case r.Epoch <= 0:
		return fmt.Errorf("workflow: reviewer reset record %s names no budget epoch", id)
	case r.FailedGeneration == "":
		// A reset that cannot say WHICH failed generation it was won against
		// cannot be shown to have won anything. Honouring it would re-admit the
		// exact hole the generation key closes.
		return fmt.Errorf("workflow: reviewer reset record %s names no failed generation", id)
	}
	return nil
}

// validate rejects an attempt record that is syntactically fine and
// semantically empty. Every field it needs is required, because each missing one
// makes the record count for less than it should.
func (r reviewLaunchAttemptRecord) validate(id string) error {
	switch {
	case r.IdempotencyKey == "":
		return fmt.Errorf("workflow: reviewer attempt record %s names no claim", id)
	case r.Cycle <= 0:
		return fmt.Errorf("workflow: reviewer attempt record %s names no review cycle", id)
	case r.Attempt <= 0:
		return fmt.Errorf("workflow: reviewer attempt record %s names no attempt number", id)
	case r.Epoch <= 0:
		return fmt.Errorf("workflow: reviewer attempt record %s names no budget epoch", id)
	}
	return nil
}

// reviewLaunchHistory is every durably consumed attempt for one step.
type reviewLaunchHistory struct {
	// byCycle maps a cycle number to the distinct attempts spent in the CURRENT
	// epoch. Attempts from superseded epochs are excluded as they are read.
	byCycle map[int]map[int]bool
	// epoch is the current budget generation for this step.
	epoch int
	// cycleOfClaim maps an idempotency key to the cycle its attempts were
	// recorded under, so recovery can correlate on a durable field rather than
	// on the shape of the key.
	cycleOfClaim map[string]int
	// attemptOfClaim maps an idempotency key to the HIGHEST attempt allocated to
	// it, so a failure can read back the attempt its dispatch was given.
	attemptOfClaim map[string]int
	// resetGenerations records every failed generation a durable reset has
	// already been won against, so a duplicate resume can see that this failure
	// is already resumed rather than opening a second epoch for it.
	//
	// Keyed by generation and NOT by epoch number: a late resume computes a
	// fresh epoch by construction, so an epoch-keyed check can never see itself
	// as a duplicate. The database index agrees with this map exactly (both key
	// on the generation), so the check is an optimisation and never the
	// authority — a resume that races past it is still refused by the index.
	resetGenerations map[string]bool
	// claimGeneration counts the durable claims taken on each idempotency key.
	//
	// A key is REUSED across retries — the outbox goes back to pending under the
	// same key — so the key alone cannot make allocation idempotent: it would
	// hand every retry the same attempt number and the budget would never
	// advance. Each dispatch writes exactly one review_launch_claimed record, so
	// counting them numbers the generations, and the attempt is allocated once
	// per generation.
	claimGeneration map[string]int
}

func (h reviewLaunchHistory) spentIn(cycle int) int { return len(h.byCycle[cycle]) }

// highestIn is the largest attempt number ever recorded for a cycle. The next
// allocation is one past it, so an attempt identity is never reused even when
// the recorded numbers are not contiguous.
func (h reviewLaunchHistory) highestIn(cycle int) int {
	highest := 0
	for attempt := range h.byCycle[cycle] {
		if attempt > highest {
			highest = attempt
		}
	}
	return highest
}

// reviewLaunchAttempts reads the durable attempt history for one step.
//
// It FAILS CLOSED twice over. An unreadable ledger is an error, never a budget
// of zero; and a record that is relevant to the budget but cannot be DECODED is
// also an error, never a record to skip. Skipping it would silently compute a
// smaller count than the truth, which is the same failure as not reading at all.
func (c *Coordinator) reviewLaunchAttempts(
	ctx stdctx.Context, runID, stepID string,
) (reviewLaunchHistory, error) {
	h := reviewLaunchHistory{
		byCycle: map[int]map[int]bool{}, cycleOfClaim: map[string]int{},
		attemptOfClaim: map[string]int{}, claimGeneration: map[string]int{},
		resetGenerations: map[string]bool{}, epoch: 1,
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return h, err
	}
	note := func(cycle, attempt, epoch int, key string) {
		if attempt <= 0 {
			return
		}
		// An attempt from a SUPERSEDED epoch no longer bounds the budget; one
		// from the current epoch (or a later one, if records arrive out of
		// order) does. Comparing rather than clearing is what stops a delayed
		// reset from erasing attempts a newer epoch already spent.
		if epoch > 0 && epoch < h.epoch {
			return
		}
		if h.byCycle[cycle] == nil {
			h.byCycle[cycle] = map[int]bool{}
		}
		h.byCycle[cycle][attempt] = true
		if key != "" {
			h.cycleOfClaim[key] = cycle
			if attempt > h.attemptOfClaim[key] {
				h.attemptOfClaim[key] = attempt
			}
		}
	}
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		switch cp.DurablePhase {
		case reviewLaunchHumanRetryPhase:
			// A person re-opened this step, which OPENS A NEW EPOCH.
			//
			// It is validated exactly like every other budget record — more
			// carefully, if anything, because this is the one record that can
			// hand budget back. A malformed reset that cleared history would be
			// an unbounded retry loop written by corruption.
			var rec reviewLaunchResetRecord
			if uerr := json.Unmarshal([]byte(cp.RetryState), &rec); uerr != nil {
				return h, fmt.Errorf("workflow: undecodable reviewer reset record %s: %w", cp.ID, uerr)
			}
			if verr := rec.validate(cp.ID); verr != nil {
				return h, verr
			}
			h.resetGenerations[rec.FailedGeneration] = true
			if rec.Epoch > h.epoch {
				h.epoch = rec.Epoch
			}
			// Attempts belonging to superseded epochs no longer bound the
			// budget. Attempts belonging to THIS epoch or later are kept — a
			// delayed reset must not erase what a newer epoch already spent.
			for cycle, attempts := range h.byCycle {
				for attempt := range attempts {
					delete(h.byCycle[cycle], attempt)
				}
			}
			h.cycleOfClaim = map[string]int{}
			h.attemptOfClaim = map[string]int{}
			h.claimGeneration = map[string]int{}
		case reviewLaunchClaimedPhase:
			var rec struct {
				IdempotencyKey string `json:"idempotencyKey"`
			}
			if uerr := json.Unmarshal([]byte(cp.RetryState), &rec); uerr != nil {
				return h, fmt.Errorf("workflow: undecodable reviewer claim record %s: %w", cp.ID, uerr)
			}
			if rec.IdempotencyKey == "" {
				return h, fmt.Errorf("workflow: reviewer claim record %s names no claim", cp.ID)
			}
			h.claimGeneration[rec.IdempotencyKey]++
		case reviewLaunchAttemptPhase:
			var rec reviewLaunchAttemptRecord
			if uerr := json.Unmarshal([]byte(cp.RetryState), &rec); uerr != nil {
				return h, fmt.Errorf("workflow: undecodable reviewer attempt record %s: %w", cp.ID, uerr)
			}
			// SEMANTIC validation, not merely syntactic. `{}` parses perfectly
			// and describes nothing: attempt zero, no claim, no cycle. Skipping
			// it computes a smaller budget than the truth, which is the same
			// failure as not reading the ledger at all — so an attempt record
			// that does not identify an attempt is an error.
			if verr := rec.validate(cp.ID); verr != nil {
				return h, verr
			}
			note(rec.Cycle, rec.Attempt, rec.Epoch, rec.IdempotencyKey)
		case reviewLaunchRecordPhase:
			var rec reviewLaunchRecord
			if uerr := json.Unmarshal([]byte(cp.RetryState), &rec); uerr != nil {
				return h, fmt.Errorf("workflow: undecodable reviewer failure record %s: %w", cp.ID, uerr)
			}
			if rec.Attempt <= 0 || rec.Cycle <= 0 {
				return h, fmt.Errorf(
					"workflow: reviewer failure record %s is incomplete (cycle %d, attempt %d)",
					cp.ID, rec.Cycle, rec.Attempt)
			}
			// Failure records predate epochs and carry none; they are bounded
			// by the reset's own clearing of prior history.
			note(rec.Cycle, rec.Attempt, 0, "")
		case reviewLaunchAbandonedPhase:
			var rec reviewLaunchAbandonRecord
			if uerr := json.Unmarshal([]byte(cp.RetryState), &rec); uerr != nil {
				return h, fmt.Errorf("workflow: undecodable reviewer abandon record %s: %w", cp.ID, uerr)
			}
			// Every abandon must name the exact claim it authorises releasing.
			if rec.IdempotencyKey == "" {
				return h, fmt.Errorf("workflow: reviewer abandon record %s names no claim", cp.ID)
			}
			// Only abandons that BELONG to a launch attempt carry an attempt
			// number. Abandoning for another reason (a lost authorization, a
			// legacy record with no reviewer behind it) consumes no launch
			// budget and leaves both fields EXACTLY zero.
			//
			// Anything else is corruption, including negatives: a `-1` is not
			// "absent", and treating it as absent skips the record and computes
			// a budget smaller than the truth. Absence is zero; every present
			// value must be a real one.
			if rec.Attempt != 0 || rec.Cycle != 0 {
				if rec.Attempt <= 0 || rec.Cycle <= 0 {
					return h, fmt.Errorf(
						"workflow: reviewer abandon record %s carries an invalid attempt (cycle %d, attempt %d)",
						cp.ID, rec.Cycle, rec.Attempt)
				}
			}
			note(rec.Cycle, rec.Attempt, 0, rec.IdempotencyKey)
		}
	}
	return h, nil
}

// reviewLaunchAttemptCount reports how much of one cycle's retry budget is
// already spent, failing closed on anything it cannot read or decode.
func (c *Coordinator) reviewLaunchAttemptCount(
	ctx stdctx.Context, runID, stepID string, cycleNumber int,
) (int, error) {
	h, err := c.reviewLaunchAttempts(ctx, runID, stepID)
	if err != nil {
		return 0, err
	}
	return h.spentIn(cycleNumber), nil
}

// recordReviewLaunchAttempt durably consumes one attempt against the DISPATCH
// CLAIM, before the work that attempt performs.
//
// Idempotent per (claim, attempt): a replay that already allocated this attempt
// does not allocate it twice.
func (c *Coordinator) recordReviewLaunchAttempt(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, cycle, attempt int, reviewRunID, why string,
) error {
	h, err := c.reviewLaunchAttempts(ctx, run.ID, reviewStep.ID)
	if err != nil {
		return err
	}
	return c.recordReviewLaunchAttemptInEpoch(ctx, run, reviewStep, entry, cycle, attempt, h.epoch, reviewRunID, why)
}

// recordReviewLaunchAttemptInEpoch writes the attempt against an epoch the
// caller has already resolved, so allocation reads the history exactly once.
func (c *Coordinator) recordReviewLaunchAttemptInEpoch(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, cycle, attempt, epoch int, reviewRunID, why string,
) error {
	h, err := c.reviewLaunchAttempts(ctx, run.ID, reviewStep.ID)
	if err != nil {
		return err
	}
	if h.byCycle[cycle][attempt] {
		return nil
	}
	stepID := reviewStep.ID
	payload, _ := json.Marshal(reviewLaunchAttemptRecord{
		IdempotencyKey: entry.IdempotencyKey, Cycle: cycle, Attempt: attempt, Epoch: epoch,
		ReviewRunID: reviewRunID, Why: why,
	})
	_, cerr := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction: fmt.Sprintf(
			"review_launch_attempt: attempt %d/%d of review cycle %d is spent (%s)",
			attempt, maxReviewerLaunchAttempts, cycle, why),
		DurablePhase:   reviewLaunchAttemptPhase,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	})
	return cerr
}

// resumeReviewLaunchAfterFailure re-opens a review cycle whose outbox entry was
// durably failed by a permanent launch failure, for an explicitly human-driven
// resume only (ContinueRun / the API's Continue). It resets the entry to Pending
// under the same idempotency key — no new outbox row, no second dispatch — and
// records the human retry, which also resets the automatic attempt budget.
//
// It refuses anything that is not exactly that situation: an entry failed for
// some other reason, or one whose launch failure has already been superseded by
// a reviewer that dispatched, is left alone.
//
// A RESUME BINDS TO THE GENERATION ITS CALLER OBSERVED, OR IT DOES NOTHING.
//
// The caller passes the failed generation it saw (observeFailedReviewLaunchGeneration),
// and this function re-reads the row and re-resolves that row's current failure
// before writing anything. If the current generation is not the observed one,
// the resume writes nothing at all. It is never resolved as "the newest
// reviewer_launch_error on this step": that reading let a delayed resume which
// observed failure F1 act on a later failure F2 — produced by somebody else's
// resume of F1, its own dispatch, and its own launch failure — and open a third
// epoch that no person ever asked for.
//
// ONE FAILED GENERATION MAY OPEN AT MOST ONE RESET EPOCH.
//
// That is the whole invariant, and it is why the claim below is taken against
// the failed generation rather than against the epoch it is about to open.
// Read-current-epoch then write-next-epoch cannot enforce it: two Continues on
// one failed entry need not collide at all. The winner opens epoch 2, reopens
// the entry and spends an attempt; the loser arrives afterwards, reads epoch 2,
// computes epoch 3 — a key nobody holds — writes it, and only THEN discovers it
// has lost the stale failed->pending swap. Its reopen changed nothing, but its
// reset is durable, and being newer it hides every attempt epoch 2 spent. The
// budget is handed back a second time by a resume that resumed nothing.
//
// So the reset row IS the ownership claim, keyed by generation:
//
//  1. the generation names this claim and the exact launch failure that failed
//     it (reviewLaunchResetGeneration);
//  2. inserting the reset claims it, and migration 0136's partial UNIQUE index
//     over (workflow_step_id, head_sha) admits exactly one such insert;
//  3. only that winner opens an epoch;
//  4. a duplicate — concurrent or arbitrarily delayed — collides, writes
//     nothing, opens no epoch, changes no budget, and no-ops.
//
// The ordering also makes the swap safe rather than merely checked afterwards.
// Nothing else in the system moves a review outbox entry out of `failed`; only
// the holder of that generation's reset does. So winning the claim is what
// EARNS the swap, instead of the swap being attempted and its loss discovered
// once a durable reset has already been written.
func (c *Coordinator) resumeReviewLaunchAfterFailure(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, observed reviewLaunchGeneration,
) bool {
	// A resume acts on the generation its caller OBSERVED. An observation that
	// does not name a generation authorises nothing — it is not a licence to
	// resume whatever is failed now.
	if !observed.valid() {
		return false
	}
	// Only a durably failed entry has a generation to resume, and the
	// observation must be about THIS row.
	if entry.Status != domain.WorkflowOutboxFailed || observed.OutboxID != entry.ID {
		return false
	}

	// RE-READ, THEN BIND. Everything above came from a snapshot that may be
	// arbitrarily old; from here on the row is read as it stands now.
	current, ok := c.currentOutboxEntry(ctx, run.ID, entry.ID)
	if !ok {
		// FAIL CLOSED: unreadable is not "unchanged".
		return false
	}
	if current.Status != domain.WorkflowOutboxFailed || current.IdempotencyKey != observed.IdempotencyKey {
		// The generation this resume was about is gone: somebody already
		// reopened it, or the claim was replaced. Idempotent no-op — nothing is
		// written, no epoch opens, no budget moves.
		return false
	}
	rec, ok := c.reviewLaunchFailureForEntry(ctx, run.ID, reviewStep.ID, current)
	if !ok {
		return false
	}
	// THE BINDING ITSELF, and the whole of this fix.
	//
	// The failure that is current for this exact outbox row must be the failure
	// that was observed. It is not enough that the row is failed: between the
	// observation and now, another resume may have reopened this same row,
	// dispatched, and failed AGAIN. The row is failed either way; the generation
	// is not the same one.
	//
	// A resume that observed F1 may only claim F1, or no-op. It may NEVER
	// upgrade itself to F2 — that is a duplicate human action opening an epoch
	// no human asked for, on a failure nobody looked at. Resolving the failure
	// by "newest on the step" is exactly what performed that upgrade.
	if rec.RecordID != observed.RecordID {
		if c.log != nil {
			c.log.Info("workflow: the reviewer launch failure this resume observed is no longer current; ignoring it",
				"run", run.ID, "step", reviewStep.ID,
				"observed", observed.RecordID, "current", rec.RecordID)
		}
		return false
	}
	entry = current

	now := c.clock()
	stepID := reviewStep.ID

	// The generation IS the observation, now confirmed against the live row: the
	// claim that is still failed, and the exact failure that failed it. A resume
	// arriving after the claim failed again observed that later failure itself,
	// and is entitled to resume it; a duplicate resume of the SAME failure gets
	// the same key the winner already holds.
	generation := reviewLaunchResetGeneration(entry, rec)

	// INTENT FIRST: the budget reset is made durable BEFORE the outbox reopens.
	//
	// The reverse order had a trap with no way out. Reopening first and then
	// failing to record the reset left the entry pending under a budget that was
	// still exhausted — so the retry could not proceed, and a second human
	// resume could not help either, because the entry was no longer `failed` and
	// this function refuses anything that is not exactly that. The person had no
	// remaining move.
	//
	// Recording first inverts it: a reset that could not be written changes
	// nothing, the entry stays failed, and the human can simply try again.
	h, herr := c.reviewLaunchAttempts(ctx, run.ID, stepID)
	if herr != nil {
		// FAIL CLOSED. AO cannot prove whether a reset already exists, and
		// "cannot read proof" must never become "proof exists" — that would
		// reopen the entry on an unverified reset and strand it as before.
		if c.log != nil {
			c.log.Warn("workflow: cannot read the reviewer retry history; refusing to reopen the launch",
				"run", run.ID, "step", stepID, "err", herr)
		}
		return false
	}

	if !h.resetGenerations[generation] {
		// This failure has not been resumed yet as far as the ledger shows.
		// Claim it. The insert is the claim; the index is the arbiter.
		state, _ := json.Marshal(reviewLaunchResetRecord{
			IdempotencyKey:   entry.IdempotencyKey,
			OutboxID:         entry.ID,
			Cycle:            rec.Cycle,
			Epoch:            h.epoch + 1,
			FailedGeneration: generation,
			ResumedFrom:      string(rec.Class),
		})
		_, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID:             "wfc-" + c.newID(),
			WorkflowRunID:  run.ID,
			WorkflowStepID: &stepID,
			ProjectID:      run.ProjectID,
			// head_sha carries the generation so the partial UNIQUE index in
			// migration 0136 can make it single-winner without a schema change.
			HeadSHA:        reviewLaunchResetHeadSHA(generation),
			NextAction:     "reviewer_launch_human_retry: resuming the reviewer launch after a human continue (previous failure: " + rec.Error + ")",
			DurablePhase:   reviewLaunchHumanRetryPhase,
			PayloadVersion: "v1",
			RetryState:     string(state),
			CreatedAt:      now,
		})
		switch {
		case err == nil:
			// Won it. This resume, and only this resume, opened the epoch.
		case errors.Is(err, domain.ErrDuplicateWorkflowCheckpoint):
			// Lost it: somebody else already resumed this exact failure between
			// the read and the write. NO EPOCH WAS OPENED HERE and no budget
			// moved — the winner's reset stands alone. Fall through, because the
			// winner may have crashed before its own reopen and this pass can
			// still finish that.
			if c.log != nil {
				c.log.Info("workflow: another resume already reset this reviewer launch failure",
					"run", run.ID, "step", stepID, "generation", generation)
			}
		default:
			if c.log != nil {
				c.log.Warn("workflow: could not record the reviewer-launch budget reset; leaving the entry failed",
					"run", run.ID, "step", stepID, "err", err)
			}
			return false
		}
	}

	// CONFIRMED: a reset for this generation is on the ledger, so reopening is
	// safe. A crash between the two leaves a recorded reset over a still-failed
	// entry, which the next resume finishes — it sees the generation already
	// claimed, opens no second epoch, and completes the reopen instead.
	//
	// THE SWAP NAMES THE GENERATION, IN SQL.
	//
	// `WHERE id = ? AND status = 'failed'` is not the condition this resume
	// earned. The row is reused, so that predicate is satisfied by any failure
	// of it — including one produced AFTER this resume validated its own, by
	// somebody else's resume, dispatch and launch failure. Everything checked in
	// Go above is a read; between the last read and this write the generation can
	// still turn over, and the swap would then reopen a failure no person ever
	// decided about, spending a fresh epoch on it.
	//
	// So the generation goes into the UPDATE itself. SQLite decides, atomically,
	// whether the failed state being reopened is still the observed one; if it is
	// not, zero rows change and this is an idempotent no-op.
	moved, err := c.store.ReopenFailedWorkflowOutboxGeneration(
		ctx, entry.ID, string(rec.Class), observed.casValue())
	if err != nil || !moved {
		// Not moved means this generation is no longer the row's failed state —
		// already reopened, or replaced by a later failure. Either way it is the
		// duplicate's no-op, not a failure: nothing was written, nothing was
		// spent, and the caller does not dispatch.
		if err == nil && c.log != nil {
			c.log.Info("workflow: the failed generation this resume owns is no longer the outbox state; not reopening",
				"run", run.ID, "step", stepID, "generation", generation)
		}
		return false
	}
	return true
}

// reviewLaunchStopReasons are the canonical stops this file can park a run on.
// clearReviewLaunchStop un-parks exactly these and nothing else.
var reviewLaunchStopReasons = map[string]bool{
	ReasonReviewerLaunchRetry:            true,
	ReasonReviewerLaunchRetriesExhausted: true,
	ReasonReviewerAuthInvalid:            true,
	ReasonReviewerBinaryMissing:          true,
	ReasonReviewerLaunchUnsupported:      true,
	ReasonReviewerLaunchFailed:           true,
}

// clearReviewLaunchStop releases a run parked on a reviewer-launch failure once
// a reviewer has demonstrably launched. Like clearIntegrationStop, it is only
// ever called from a site that has just PROVEN the condition is gone — the
// launch this stop was about has now succeeded — and it touches exactly the
// reasons this file writes: a run stopped for anything else is left alone.
//
// This is the "clear stale attention/failed partial state" half of a successful
// retry: without it a run whose reviewer is genuinely running would keep
// reporting "needs attention" from a stop that no longer exists — and, because
// needs_attention only transitions forward to running, the review step's own
// later completion would be dropped as an invalid transition.
func (c *Coordinator) clearReviewLaunchStop(ctx stdctx.Context, run domain.WorkflowRun) domain.WorkflowRun {
	if run.State != domain.WorkflowRunNeedsAttention {
		return run
	}
	reason, ok := c.latestCanonicalStopReason(ctx, run.ID)
	if !ok || !reviewLaunchStopReasons[reason] {
		return run
	}
	return c.unparkRun(ctx, run, reason, "the reviewer launched successfully")
}

// latestCanonicalStopReason returns the newest durable_phase that is a canonical
// attention reason. It deliberately does NOT reuse stopReason, which reads only
// the single newest checkpoint: by the time a launch retry succeeds, the newest
// checkpoint is this file's own bookkeeping (the launch-error record, or a human
// retry marker), and the stop being cleared is one or two rows further back.
func (c *Coordinator) latestCanonicalStopReason(ctx stdctx.Context, runID string) (string, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return "", false
	}
	reason := ""
	for _, cp := range cps {
		if cp.DurablePhase == attentionClearedPhase {
			// A resume already cleared whatever came before it.
			reason = ""
			continue
		}
		if _, ok := attentionDispositions[cp.DurablePhase]; ok {
			reason = cp.DurablePhase
		}
	}
	return reason, reason != ""
}

// errReviewLaunchBudgetExhausted reports that a cycle has spent every reviewer
// launch attempt it is allowed. It is a decision, not a failure: the caller
// stops retrying and hands the run to a person.
var errReviewLaunchBudgetExhausted = errors.New(
	"workflow: this review cycle has spent every reviewer launch attempt")

// allocateReviewLaunchAttempt consumes one attempt for a claim AT CLAIM TIME,
// and refuses when the cycle has none left.
//
// Idempotent for the exact claim: a replay that already allocated an attempt for
// this idempotency key returns the same number rather than spending another.
// That is what makes a duplicate reconcile safe while a genuine re-dispatch of a
// released claim still advances.
func (c *Coordinator) allocateReviewLaunchAttempt(
	ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, cycleNumber int,
) (int, error) {
	h, err := c.reviewLaunchAttempts(ctx, run.ID, reviewStep.ID)
	if err != nil {
		return 0, err
	}
	// MONOTONIC AND UNIQUE. The next attempt is one past the HIGHEST ever
	// recorded for this cycle — never derived from how many generations have
	// been claimed, which could hand out a number that was already used.
	//
	// Deriving it from a count assumes the numbers are contiguous. They need not
	// be: a legacy record, a partially written history, or an attempt recorded
	// by one path and not another all leave gaps, and "count + 1" then lands on
	// a number some earlier dispatch already spent — two physical launches
	// sharing one attempt identity, and a budget that never advances past it.
	// EXHAUSTION IS MEASURED BY THE HIGHEST ATTEMPT, NOT BY HOW MANY RECORDS
	// SURVIVE.
	//
	// Counting distinct records assumes none were lost. A gapped history — one
	// record missing, one written by a path that no longer exists — then reports
	// fewer than the limit while the highest number already reached it, and the
	// allocator hands out attempt 4 over a budget of 3. Gaps must fail toward
	// exhaustion, never away from it.
	if h.highestIn(cycleNumber) >= maxReviewerLaunchAttempts ||
		h.spentIn(cycleNumber) >= maxReviewerLaunchAttempts {
		return 0, errReviewLaunchBudgetExhausted
	}
	attempt := h.highestIn(cycleNumber) + 1
	if werr := c.recordReviewLaunchAttemptInEpoch(ctx, run, reviewStep, entry, cycleNumber, attempt,
		h.epoch, "", "claim acquired"); werr != nil {
		return 0, werr
	}
	return attempt, nil
}

// reviewLaunchAttemptForClaim reads back the attempt a claim was allocated.
//
// A claim with no allocation on record is not attempt 1 by default — that is the
// assumption this whole mechanism exists to remove — so it reports the cycle's
// spend plus one only when the ledger is otherwise readable, and errors when it
// is not.
func (c *Coordinator) reviewLaunchAttemptForClaim(
	ctx stdctx.Context, runID, stepID, idempotencyKey string, cycleNumber int,
) (int, error) {
	h, err := c.reviewLaunchAttempts(ctx, runID, stepID)
	if err != nil {
		return 0, err
	}
	return h.attemptForClaim(idempotencyKey, cycleNumber), nil
}

// attemptForClaim is that read against an already-loaded history: the attempt
// this claim was allocated, or — for a legacy dispatch that predates claim-time
// allocation — the cycle's own spend plus one, which never under-counts.
func (h reviewLaunchHistory) attemptForClaim(idempotencyKey string, cycleNumber int) int {
	if n, ok := h.attemptOfClaim[idempotencyKey]; ok {
		return n
	}
	return h.spentIn(cycleNumber) + 1
}
