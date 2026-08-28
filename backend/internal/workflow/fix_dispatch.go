package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// MessageSender is the narrow existing-session messaging path workflow
// reuses to deliver fix findings to the SAME Codex worker, never a new one.
// Satisfied by *session_manager.Manager (the same Manager.Send that backs
// `ao send` and the orchestrator's delegated-task title-refinement message).
//
// Critical, research-confirmed fact (from Checkpoint 8B's investigation):
// Manager.Send's public API has NO idempotency key (the private variant with
// a clientMessageID is only used by the interface-transition outbox
// internally). So workflow must never rely on Send itself for idempotency —
// the same outbox-guarded dispatch pattern as dispatch.go/review_dispatch.go
// applies: enqueue a workflow_outbox entry first, only call Send from the
// "pending" branch, and treat a recovered "dispatched" entry as ambiguous
// rather than ever calling Send a second time for the same cycle.
type MessageSender interface {
	Send(ctx stdctx.Context, id domain.SessionID, message string, attachment *ports.SpawnAttachment) error
}

// SubmissionReportingSender is the optional capability that lets workflow tell
// "the prompt reached the agent's composer" apart from "the agent was given a
// turn" — Checkpoint 8P-E.17. Satisfied by *session_manager.Manager.
//
// Workflow degrades cleanly without it: a sender that only implements
// MessageSender behaves exactly as it always did, and every verdict below is
// simply unavailable rather than assumed.
type SubmissionReportingSender interface {
	MessageSender
	// SendReportingSubmission delivers and then says what it could prove.
	SendReportingSubmission(ctx stdctx.Context, id domain.SessionID, message string, attachment *ports.SpawnAttachment) (ports.PromptSubmission, error)
	// SubmitPending submits a draft already in the composer, writing no new
	// payload — the only safe retry for a prompt that is loaded but not sent.
	SubmitPending(ctx stdctx.Context, id domain.SessionID) (ports.PromptSubmission, error)
	// ComposerState reports what the composer holds, writing nothing.
	ComposerState(ctx stdctx.Context, id domain.SessionID) ports.PromptSubmission
}

// maxFixSubmitRetries bounds the submit-only retries for one delivery. Each is
// an Enter with no payload, so it can never duplicate a prompt; the bound is
// here because a composer that will not clear after three submits is a
// condition to report, not to keep poking.
const maxFixSubmitRetries = 2

// deliverAndConfirm writes the prompt and returns the strongest statement AO can
// make about whether the agent actually received a turn.
//
// The submit-only retries matter more than they look. The transport now refuses
// to write into a pane whose keys it cannot deliver, but a TUI can still absorb
// a submit behind a large paste, and the ONLY correct response to that is to
// submit again — never to re-send, which appends a second copy of the prompt to
// the draft that is already there. That is precisely how session
// agent-orchestrator-29 ended up holding two copies of the same 15 KB fix
// prompt in one composer.
func (c *Coordinator) deliverAndConfirm(ctx stdctx.Context, sessionID domain.SessionID, prompt string) (ports.PromptSubmission, error) {
	reporter, ok := c.messageSender.(SubmissionReportingSender)
	if !ok {
		return ports.PromptSubmissionUnset, c.messageSender.Send(ctx, sessionID, prompt, nil)
	}
	submission, err := reporter.SendReportingSubmission(ctx, sessionID, prompt, nil)
	if err != nil {
		return submission, err
	}
	for i := 0; i < maxFixSubmitRetries && submission == ports.PromptLoadedNotSubmitted; i++ {
		if c.log != nil {
			c.log.Info("workflow: fix prompt is loaded but unsubmitted; submitting again without re-sending it",
				"session", sessionID, "attempt", i+1, "max", maxFixSubmitRetries)
		}
		next, serr := reporter.SubmitPending(ctx, sessionID)
		if serr != nil {
			// The submit could not be issued. Nothing was written either way,
			// so the caller still owns the loaded-not-submitted verdict.
			return submission, nil
		}
		submission = next
	}
	return submission, nil
}

// fixStepOutboxIdempotencyKey is the deterministic, cycle-specific
// idempotency key for a fix step's send-message command. Cycle-specific for
// the same reason review dispatch is (design decision 2): a second fix cycle
// for the same step must get its own outbox row / single-flight guard.
//
// Checkpoint 8P-E.13C adds the transport attempt to the key. A dispatch whose
// transport refused the message outright (ports.ErrPromptUndelivered — nothing
// reached the agent) is allowed a bounded number of retries, and each retry is
// genuinely a NEW command rather than a re-run of a failed one. Deriving the
// attempt from the durable prompt_transport_retry checkpoints keeps the key
// stable across a restart, so recovery reconstructs the same key rather than
// inventing a second concurrent delivery.
func fixStepOutboxIdempotencyKey(stepID string, cycleNumber, transportAttempt int) string {
	key := "workflow-step-fix:" + stepID + ":cycle" + strconv.Itoa(cycleNumber)
	if transportAttempt > 0 {
		key += ":transport" + strconv.Itoa(transportAttempt)
	}
	return key
}

// fixDispatchedPhase is the durable phase of a completed fix-cycle dispatch:
// the record that names the session the cycle went to and the workspace
// fingerprint it was dispatched against. Both the observation path and the
// re-delivery rule read it, so it has a name rather than a repeated literal.
const fixDispatchedPhase = "fix_dispatched"

// fixTransportRetryPhase is the durable phase of a bounded, self-remediable
// prompt-transport retry. It is a canonical attention reason (attention.go) so
// a run resting on one reads as "retrying", never as a human decision.
const fixTransportRetryPhase = "prompt_transport_retry"

// maxFixTransportRetries bounds those retries. A transport that refuses three
// times in a row is not a transient hiccup, and the fourth failure falls
// through to the ordinary dispatch_failed stop a person can act on.
const maxFixTransportRetries = 3

// promptDeliveryRecord is Checkpoint 8P-E.13C's prompt-size observability: the
// facts needed to diagnose a delivery problem from the durable ledger alone.
// Deliberately NOT the prompt itself — the objective, the acceptance criteria
// and the reviewer's findings are all already durable elsewhere (the run, the
// plan artifact, the review run), and copying them here would duplicate exactly
// the payload whose size is under investigation.
type promptDeliveryRecord struct {
	PromptBytes      int                   `json:"promptBytes"`
	Transport        ports.PromptTransport `json:"transport"`
	ContextPack      bool                  `json:"contextPack"`
	CycleNumber      int                   `json:"cycleNumber"`
	TransportAttempt int                   `json:"transportAttempt,omitempty"`
	Reason           string                `json:"reason,omitempty"`
	// Submission is what the transport could prove about the submit itself:
	// whether the payload left the agent's composer. Empty means no check ran.
	Submission ports.PromptSubmission `json:"submission,omitempty"`
	// PromptReceipt is the digest of the exact bytes the session will record as
	// its LatestUserPrompt once this prompt is written into it. Recorded BEFORE
	// delivery, it is what lets recovery prove after a restart that this
	// session received THIS cycle's prompt rather than some other message. See
	// fix_delivery_recovery.go.
	PromptReceipt string `json:"promptReceipt,omitempty"`
	// Findings is the durable proof of WHICH reviewer findings this prompt
	// carried, and whether they are in it verbatim. See
	// fix_findings_evidence.go — it is the field that lets an operator tell a
	// worker that ignored its findings from a worker that never got them.
	Findings FixFindingsEvidence `json:"findings"`
	// FixAttemptID names the workflow_attempt row this delivery produced, so a
	// fix attempt is durably bound to the delivery record that authorized it.
	// Empty on the intent record, which is written before the attempt exists.
	FixAttemptID string `json:"fixAttemptId,omitempty"`
}

func newPromptDeliveryRecord(prompt string, cycleNumber, transportAttempt int, contextPack bool) promptDeliveryRecord {
	return promptDeliveryRecord{
		PromptBytes:      len(prompt),
		Transport:        ports.PromptTransportFor(len(prompt)),
		ContextPack:      contextPack,
		CycleNumber:      cycleNumber,
		TransportAttempt: transportAttempt,
		PromptReceipt:    promptReceiptDigest(prompt),
	}
}

// promptDeliveryRecordFromJSON re-reads a delivery record off a checkpoint,
// returning the zero value when the checkpoint carries none.
func promptDeliveryRecordFromJSON(raw string) promptDeliveryRecord {
	var rec promptDeliveryRecord
	if json.Unmarshal([]byte(raw), &rec) != nil {
		return promptDeliveryRecord{}
	}
	return rec
}

func (r promptDeliveryRecord) json() string {
	b, err := json.Marshal(r)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// fixTransportRetryCount folds the run's ledger into "how many times has this
// exact fix cycle's prompt been refused by the transport". Durable by
// construction, so it survives a daemon restart mid-retry.
func (c *Coordinator) fixTransportRetryCount(ctx stdctx.Context, runID, stepID string, cycleNumber int) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return 0
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase != fixTransportRetryPhase || cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		var rec promptDeliveryRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil || rec.CycleNumber != cycleNumber {
			continue
		}
		n++
	}
	return n
}

// dispatchFixStep is Checkpoint 8D's single idempotent dispatch algorithm for
// delivering one cycle of fix findings to the existing worker session. Safe
// to call repeatedly — from GetRun's cascade, from Reconcile, and from a
// future ContinueRun nudge — without ever sending the same cycle's findings
// twice: the primary, cheapest guard is that this cycle's workflow_attempt
// already exists (mirrors dispatchWorkStep's SessionID guard / dispatchReviewStep's
// ReviewRunID guard, adapted to a step that is dispatched repeatedly across
// cycles rather than once).
func (c *Coordinator) dispatchFixStep(ctx stdctx.Context, run domain.WorkflowRun, workStep, fixStep domain.WorkflowStep, reviewRun domain.ReviewRun, cycleNumber int, prompt string, findings fixFindingsRef) (domain.WorkflowStep, error) {
	if run.State.Terminal() || fixStep.State.Terminal() {
		return fixStep, nil
	}
	// Checkpoint 8K-A: never send fix findings into a session paused on an
	// unresolved question.
	if open, err := c.hasOpenQuestion(ctx, run.ID, &fixStep.ID); err != nil {
		return fixStep, err
	} else if open {
		return fixStep, nil
	}
	if c.messageSender == nil {
		return fixStep, nil
	}
	attempts, err := c.store.ListWorkflowAttempts(ctx, fixStep.ID)
	if err != nil {
		return fixStep, err
	}
	if int64(len(attempts)) >= int64(cycleNumber) {
		// This cycle already has its attempt row: either fully dispatched
		// and progressing, or ambiguous and awaiting attention. Either way,
		// never re-enter dispatch for it.
		return fixStep, nil
	}

	now := c.clock()
	transportAttempt := c.fixTransportRetryCount(ctx, run.ID, fixStep.ID, cycleNumber)
	entry, _, err := c.store.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID:             "wfo-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &fixStep.ID,
		IdempotencyKey: fixStepOutboxIdempotencyKey(fixStep.ID, cycleNumber, transportAttempt),
		CommandType:    domain.WorkflowOutboxSendMessage,
		Payload:        fixPayloadJSON(fixStep.ID, reviewRun.ID, cycleNumber),
		CreatedAt:      now,
	})
	if err != nil {
		return fixStep, err
	}

	switch entry.Status {
	case domain.WorkflowOutboxPending:
		return c.dispatchFixFromPending(ctx, run, workStep, fixStep, entry, reviewRun, cycleNumber, transportAttempt, prompt, findings)
	case domain.WorkflowOutboxDispatched, domain.WorkflowOutboxAcknowledged:
		// Boundary B: a previous attempt got at least as far as "about to call
		// Send". Send still has no idempotency key, so this must never call it
		// blindly — but "no idempotency key" was never the same thing as "no
		// evidence". resolveFixDeliveryAfterRestart reads the durable
		// pre-delivery record and the session's own facts, and re-sends only
		// when it can PROVE nothing was delivered, adopts the cycle when it can
		// prove it was, and escalates only what is genuinely unprovable — once.
		// Before this, every pass through here parked the run and wrote another
		// identical checkpoint, which is the wf-6528a538 incident.
		return c.resolveFixDeliveryAfterRestart(ctx, run, workStep, fixStep, entry, reviewRun, cycleNumber, transportAttempt, prompt, findings)
	case domain.WorkflowOutboxFailed:
		return fixStep, nil
	default:
		return fixStep, nil
	}
}

func (c *Coordinator) dispatchFixFromPending(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	workStep, fixStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	reviewRun domain.ReviewRun,
	cycleNumber int,
	transportAttempt int,
	prompt string,
	findings fixFindingsRef,
) (domain.WorkflowStep, error) {
	now := c.clock()
	if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, domain.WorkflowOutboxPending, domain.WorkflowOutboxDispatched, now, ""); err != nil {
		return fixStep, err
	}
	entry.Status = domain.WorkflowOutboxDispatched

	return c.deliverFixPrompt(ctx, run, workStep, fixStep, entry, reviewRun, cycleNumber, transportAttempt, prompt, findings)
}

// runFixStep puts the fix step into the state a delivery is made from. Both
// delivery entry points go through it, so a cycle delivered on the first pass
// and one delivered by recovery after a restart leave the step identically.
func (c *Coordinator) runFixStep(ctx stdctx.Context, fixStep domain.WorkflowStep) (domain.WorkflowStep, error) {
	now := c.clock()
	if fixStep.State == domain.WorkflowStepPending {
		if _, err := c.store.UpdateWorkflowStepState(ctx, fixStep.ID, domain.WorkflowStepPending, domain.WorkflowStepReady, now); err != nil {
			return fixStep, err
		}
		fixStep.State = domain.WorkflowStepReady
	}
	// ready->running (cycle 1) and waiting->running (cycle N+1, and a cycle
	// re-entered by delivery recovery) are all valid transitions.
	if fixStep.State == domain.WorkflowStepReady || fixStep.State == domain.WorkflowStepWaiting {
		if _, err := c.store.UpdateWorkflowStepState(ctx, fixStep.ID, fixStep.State, domain.WorkflowStepRunning, now); err != nil {
			return fixStep, err
		}
		fixStep.State = domain.WorkflowStepRunning
	}
	return fixStep, nil
}

// deliverFixPrompt is the one place a fix prompt is ever handed to the
// transport. Both entry points use it — the ordinary pending dispatch, and the
// recovery path that has PROVEN a previous attempt never got this far — so
// "deliver this cycle" has exactly one implementation, one pre-delivery record
// and one set of outcomes, whichever side of a restart it happens on.
//
// The caller owns the outbox entry and must have already moved it out of
// `pending`; this function never enqueues one, so it can never mint a second
// delivery identity for the same logical cycle.
func (c *Coordinator) deliverFixPrompt(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	workStep, fixStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	reviewRun domain.ReviewRun,
	cycleNumber int,
	transportAttempt int,
	prompt string,
	findings fixFindingsRef,
) (domain.WorkflowStep, error) {
	// The last gate before a worktree is deliberately changed: is the authority
	// this cycle was derived from still the authority the run holds? An approved
	// review, or a review the step no longer speaks for, does not authorize a
	// mutation — see fix_authority.go. Refusing is inert: nothing durable has
	// moved yet, the outbox entry stays where it is, and the next poll re-derives
	// the cycle from whatever authority is current then.
	if refusal := c.fixAuthorityRefusal(ctx, run, fixStep, reviewRun); refusal != "" {
		if c.log != nil {
			c.log.Info("workflow: refusing a fix delivery whose review authority is stale",
				"run", run.ID, "step", fixStep.ID, "cycle", cycleNumber, "reason", refusal)
		}
		return fixStep, nil
	}

	fixStep, err := c.runFixStep(ctx, fixStep)
	if err != nil {
		return fixStep, err
	}
	// Checkpoint 8M §12/§27: apply the session lifecycle decision (and
	// persist it) right here — the single outbox-idempotency-guarded point
	// reached exactly once per real cycle dispatch, never once per poll.
	prompt, contextPack := c.applyFixLifecycleDecision(ctx, run, fixStep, reviewRun, cycleNumber, prompt)
	delivery := newPromptDeliveryRecord(prompt, cycleNumber, transportAttempt, contextPack)
	// Computed against the FINAL prompt — after applyFixLifecycleDecision may
	// have prepended a context pack — so `Embedded` is a statement about the
	// exact bytes deliverAndConfirm is about to write, not about what the
	// builder intended.
	delivery.Findings = findings.evidence(prompt)

	// The durable pre-delivery record, written STRICTLY before Send and fatal
	// if it fails. Its presence or absence is the fact recovery reasons from
	// after a restart, so a delivery AO could not first write down is a
	// delivery AO does not make — see recordFixDispatchIntent.
	if err := c.recordFixDispatchIntent(ctx, run, fixStep, reviewRun, reviewRun.TargetSHA, delivery); err != nil {
		return fixStep, err
	}

	submission, err := c.deliverAndConfirm(ctx, reviewRun.SessionID, prompt)
	if err != nil {
		// Checkpoint 8P-E.13C: a transport that refused the message before any
		// of it reached the agent is a transport problem, not a workflow
		// verdict. Nothing was delivered, so re-sending is provably safe, and
		// asking a human to resolve "command too long" was never a decision
		// anyone could make. Bounded, durable, self-remediable.
		if errors.Is(err, ports.ErrPromptUndelivered) && transportAttempt < maxFixTransportRetries {
			return c.recordFixTransportRetry(ctx, run, fixStep, entry, delivery, err)
		}
		return c.recordFixDispatchFailure(ctx, run, fixStep, entry, domain.WorkflowErrorPromptDeliveryFailed, err)
	}
	// Checkpoint 8P-E.17: a prompt sitting in the composer is NOT a delivered
	// fix cycle, and recording one as if it were is the whole of wf-57f90ff2.
	// Every submit-only retry has already been spent by deliverAndConfirm, so
	// this is a durable statement of a condition, not a place to try again.
	if submission == ports.PromptLoadedNotSubmitted {
		return c.recordFixPromptNotSubmitted(ctx, run, fixStep, entry, reviewRun, delivery)
	}
	delivery.Submission = submission
	return c.recordFixDispatchSuccess(ctx, run, workStep, fixStep, entry, reviewRun, cycleNumber, delivery)
}

func (c *Coordinator) recordFixDispatchSuccess(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	workStep, fixStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	reviewRun domain.ReviewRun,
	cycleNumber int,
	delivery promptDeliveryRecord,
) (domain.WorkflowStep, error) {
	now := c.clock()

	attempts, err := c.store.ListWorkflowAttempts(ctx, fixStep.ID)
	if err != nil {
		return fixStep, err
	}
	if int64(len(attempts)) < int64(cycleNumber) {
		// Checkpoint 8L: the fix message is delivered into the SAME live
		// worker session (Send targets reviewRun.SessionID, the worker's
		// session — never a new one), so the fix attempt's harness must
		// reflect whichever harness ExecutionRouter actually selected for
		// that worker, not a hardcoded literal. Before 8L the worker was
		// always Codex so a literal "codex" here happened to be correct;
		// now that the worker can be claude-code, the literal must be
		// replaced with the work step's own last recorded attempt harness
		// (same lookup reviewerHarnessForStep already uses).
		fixHarness := "codex"
		if workAttempts, werr := c.store.ListWorkflowAttempts(ctx, workStep.ID); werr == nil && len(workAttempts) > 0 {
			if h := workAttempts[len(workAttempts)-1].Harness; h != "" {
				fixHarness = h
			}
		}
		attempt, err := c.store.CreateWorkflowAttempt(ctx, "wfa-"+c.newID(), fixStep.ID, fixHarness, "", now)
		if err != nil {
			return fixStep, err
		}
		delivery.FixAttemptID = attempt.ID
	} else if len(attempts) > 0 {
		// Recovery re-entered a cycle whose attempt row already exists. Bind
		// the record to THAT row rather than leaving the field blank, so the
		// delivery and the attempt stay one traceable pair either way.
		delivery.FixAttemptID = attempts[len(attempts)-1].ID
	}

	if entry.Status != domain.WorkflowOutboxAcknowledged {
		if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxAcknowledged, now, ""); err != nil {
			return fixStep, err
		}
	}

	rid := reviewRun.ID
	sid := string(reviewRun.SessionID)
	stepID := fixStep.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		SessionID:         &sid,
		ReviewRunID:       &rid,
		ReviewVerdict:     string(reviewRun.EffectiveVerdict()),
		FingerprintBefore: reviewRun.TargetSHA,
		NextAction:        "fix_dispatched: awaiting a genuinely new workspace fingerprint",
		DurablePhase:      fixDispatchedPhase,
		PayloadVersion:    "v1",
		// RetryState carries the prompt-delivery facts (size, transport,
		// whether a context pack was prepended) so a future delivery problem is
		// diagnosable from the ledger — see promptDeliveryRecord.
		RetryState: delivery.json(),
		CreatedAt:  now,
	}); err != nil {
		return fixStep, err
	}
	return fixStep, nil
}

// recordFixTransportRetry parks a fix cycle whose prompt the transport refused
// outright, in a state AO resumes by itself (Checkpoint 8P-E.13C).
//
// It is deliberately NOT recordFixDispatchFailure with a friendlier label. The
// differences are the whole point:
//
//   - the step stays non-terminal (waiting, not failed), because a failed step
//     can never be re-dispatched and this cycle has not been attempted yet in
//     any sense the agent could observe;
//   - the run is not moved to needs_attention, because nothing here is a
//     decision — the previous behavior asked a human to resolve the words
//     "command too long";
//   - a durable wake is scheduled, so the retry happens headlessly; and
//   - the checkpoint carries the delivery facts, so the retry budget is
//     reconstructible after a restart.
func (c *Coordinator) recordFixTransportRetry(ctx stdctx.Context, run domain.WorkflowRun, fixStep domain.WorkflowStep, entry domain.WorkflowOutboxEntry, delivery promptDeliveryRecord, cause error) (domain.WorkflowStep, error) {
	now := c.clock()
	if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxFailed, now, string(domain.WorkflowErrorPromptDeliveryFailed)); err != nil {
		return fixStep, err
	}
	if fixStep.State == domain.WorkflowStepRunning {
		if _, err := c.store.UpdateWorkflowStepState(ctx, fixStep.ID, domain.WorkflowStepRunning, domain.WorkflowStepWaiting, now); err != nil {
			return fixStep, err
		}
		fixStep.State = domain.WorkflowStepWaiting
	}
	delivery.Reason = cause.Error()
	stepID := fixStep.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction: fmt.Sprintf("prompt_transport_retry: the %d-byte fix prompt was refused before delivery (attempt %d of %d) — AO will re-send it",
			delivery.PromptBytes, delivery.TransportAttempt+1, maxFixTransportRetries),
		DurablePhase:   fixTransportRetryPhase,
		PayloadVersion: "v1",
		RetryState:     delivery.json(),
		CreatedAt:      now,
	}); err != nil {
		return fixStep, err
	}
	wakeStepID := domain.WorkflowStepID(fixStep.ID)
	c.scheduleWake(ctx, run, &wakeStepID, wake.ReasonTransientRetry, "")
	if c.log != nil {
		c.log.Warn("workflow: fix prompt refused before delivery; retrying",
			"step", fixStep.ID, "bytes", delivery.PromptBytes, "transport", delivery.Transport, "attempt", delivery.TransportAttempt+1, "err", cause)
	}
	return fixStep, nil
}

func (c *Coordinator) recordFixDispatchFailure(ctx stdctx.Context, run domain.WorkflowRun, fixStep domain.WorkflowStep, entry domain.WorkflowOutboxEntry, errClass domain.WorkflowErrorClass, cause error) (domain.WorkflowStep, error) {
	now := c.clock()
	if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxFailed, now, string(errClass)); err != nil {
		return fixStep, err
	}
	if fixStep.State == domain.WorkflowStepRunning {
		if _, err := c.store.UpdateWorkflowStepState(ctx, fixStep.ID, domain.WorkflowStepRunning, domain.WorkflowStepFailed, now); err != nil {
			return fixStep, err
		}
		fixStep.State = domain.WorkflowStepFailed
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return fixStep, err
		}
	}
	c.recordAttentionStop(ctx, run, &fixStep.ID, ReasonDispatchFailed,
		fmt.Sprintf("fix dispatch failed (%s): %v", errClass, cause))
	if c.log != nil {
		c.log.Warn("workflow: fix step dispatch failed", "step", fixStep.ID, "err", cause)
	}
	return fixStep, nil
}

// fixPromptNotSubmittedPhase is the durable phase of a prompt that reached the
// agent's composer and did not leave it. It is a canonical attention reason
// (attention.go), because a person can genuinely act on it — and because
// ContinueRun's resume path keys on it to submit what is already there rather
// than send it again.
const fixPromptNotSubmittedPhase = ReasonFixPromptNotSubmitted

// recordFixPromptNotSubmitted parks a fix cycle whose prompt is provably
// sitting unsubmitted in the worker's composer.
//
// The shape is deliberately unlike recordFixDispatchFailure. Nothing failed in
// the transport, the payload IS with the agent, and the step must stay
// non-terminal and re-submittable:
//
//   - the outbox entry is left ACKNOWLEDGED, not failed. The delivery happened;
//     re-minting it would invite a second paste, which is the one thing that
//     must never happen here.
//   - no attempt row is written, so the cycle is not counted as delivered and
//     the fix budget is untouched.
//   - the checkpoint carries the prompt receipt, which is what later lets AO
//     attribute the pending draft to itself instead of to a person.
func (c *Coordinator) recordFixPromptNotSubmitted(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	fixStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	reviewRun domain.ReviewRun,
	delivery promptDeliveryRecord,
) (domain.WorkflowStep, error) {
	now := c.clock()
	if entry.Status != domain.WorkflowOutboxAcknowledged {
		if _, err := c.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxAcknowledged, now, ""); err != nil {
			return fixStep, err
		}
	}
	if fixStep.State == domain.WorkflowStepRunning {
		if _, err := c.store.UpdateWorkflowStepState(ctx, fixStep.ID, domain.WorkflowStepRunning, domain.WorkflowStepWaiting, now); err != nil {
			return fixStep, err
		}
		fixStep.State = domain.WorkflowStepWaiting
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return fixStep, err
		}
	}
	delivery.Submission = ports.PromptLoadedNotSubmitted
	stepID := fixStep.ID
	sid := string(reviewRun.SessionID)
	rid := reviewRun.ID
	detail := fmt.Sprintf(
		"fix cycle %d reached worker session %s but is still sitting unsubmitted in its composer after %d submit attempts — the agent has the text and has not been given the turn",
		delivery.CycleNumber, sid, maxFixSubmitRetries+1)
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		SessionID:         &sid,
		ReviewRunID:       &rid,
		ReviewVerdict:     string(reviewRun.EffectiveVerdict()),
		FingerprintBefore: reviewRun.TargetSHA,
		NextAction:        detail,
		DurablePhase:      fixPromptNotSubmittedPhase,
		PayloadVersion:    "v1",
		RetryState:        delivery.json(),
		CreatedAt:         now,
	}); err != nil {
		return fixStep, err
	}
	c.recordAttentionStopOnce(ctx, run, &fixStep.ID, ReasonFixPromptNotSubmitted, detail)
	if c.log != nil {
		c.log.Warn("workflow: fix prompt is in the agent's composer but was never submitted",
			"run", run.ID, "step", fixStep.ID, "cycle", delivery.CycleNumber, "session", sid)
	}
	return fixStep, nil
}

// markFixAmbiguous is gone: escalating an unresolved delivery now lives in
// fix_delivery_recovery.go's markFixDeliveryUnproven, which is reached only
// after the evidence lookup has come back empty, records WHAT was inconclusive,
// and writes at most one checkpoint per unchanged condition.

func fixPayloadJSON(fixStepID, reviewRunID string, cycleNumber int) string {
	return `{"fixStepId":"` + fixStepID + `","reviewRunId":"` + reviewRunID + `","cycle":` + strconv.Itoa(cycleNumber) + `}`
}
