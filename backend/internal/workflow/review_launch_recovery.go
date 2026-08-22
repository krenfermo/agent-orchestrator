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
	// Error is the deep/root error text, verbatim (bounded), so the actual
	// cause survives the process that observed it.
	Error string `json:"error"`

	// RecordedAt is the checkpoint's own created_at, not part of the JSON —
	// filled in by latestReviewLaunchRecord so the retry gate can be a durable
	// time comparison rather than in-memory state.
	RecordedAt time.Time `json:"-"`
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

	// 1. Clean up the partial durable state first: whatever happens to the step
	// and the run below, a review_run whose reviewer never started is not
	// running, and saying so is not optional.
	c.failPartialReviewRun(ctx, reviewRunID, cls, stage, deep)

	attempt := c.reviewLaunchAttemptCount(ctx, run.ID, reviewStep.ID, cycleNumber) + 1
	retry := cls.Retryable && attempt < maxReviewerLaunchAttempts

	detail := fmt.Sprintf("reviewer launch failed at stage %s (%s, %s, attempt %d/%d): %s",
		stage, cls.Class, cls.Certainty, attempt, maxReviewerLaunchAttempts, deep)

	// 2. Persist the deep error and the classification, always — this is the
	// record the retry path, the next daemon boot, and a human debugging the run
	// all read.
	stepID := reviewStep.ID
	state, _ := json.Marshal(reviewLaunchRecord{
		Cycle: cycleNumber, Attempt: attempt, Class: string(cls.Class),
		Certainty: string(cls.Certainty), Retryable: retry, Stage: string(stage),
		Harness: string(harness), TargetSHA: targetSHA, Error: deep,
	})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
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
		if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxPending, now, string(cls.Class)); err != nil {
			return reviewStep, err
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
	if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxFailed, now, string(cls.Class)); err != nil {
		return reviewStep, err
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
	return rec, true
}

// reviewLaunchAttemptCount counts the automatic launch attempts already burned
// for one review cycle, since the newest human-initiated retry (which resets the
// budget — a person who fixed the cause is entitled to a fresh set of attempts).
func (c *Coordinator) reviewLaunchAttemptCount(ctx stdctx.Context, runID, stepID string, cycleNumber int) int {
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
		case reviewLaunchHumanRetryPhase:
			count = 0
		case reviewLaunchRecordPhase:
			var rec reviewLaunchRecord
			if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.Cycle == cycleNumber {
				count++
			}
		}
	}
	return count
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
func (c *Coordinator) resumeReviewLaunchAfterFailure(ctx stdctx.Context, run domain.WorkflowRun, reviewStep domain.WorkflowStep, entry domain.WorkflowOutboxEntry) bool {
	rec, ok := c.latestReviewLaunchRecord(ctx, run.ID, reviewStep.ID)
	if !ok {
		return false
	}
	now := c.clock()
	moved, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxPending, now, string(rec.Class))
	if err != nil || !moved {
		return false
	}
	stepID := reviewStep.ID
	state, _ := json.Marshal(map[string]any{"cycle": rec.Cycle, "resumedFrom": rec.Class})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction:     "reviewer_launch_human_retry: resuming the reviewer launch after a human continue (previous failure: " + rec.Error + ")",
		DurablePhase:   reviewLaunchHumanRetryPhase,
		PayloadVersion: "v1",
		RetryState:     string(state),
		CreatedAt:      now,
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: recording a human reviewer-launch retry failed", "run", run.ID, "err", err)
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
