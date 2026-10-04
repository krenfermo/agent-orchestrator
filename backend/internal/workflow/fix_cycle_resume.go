package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// fix_cycle_resume.go — Checkpoint 8P-E.16.
//
// This file exists because of the second half of incident wf-57f90ff2, which is
// not the wrong stop but the fact that nothing could ever undo it.
//
// The run stopped with `fix_no_verifiable_change`: fix step `waiting`, review
// step `waiting` holding a changes_requested verdict, verify `pending`. That
// shape is a closed dead end for every path AO has:
//
//   - dispatchFixStep's cheapest guard is `len(attempts) >= cycleNumber`, and
//     cycle 2 already had its attempt row, so maybeDispatchFix returns
//     unchanged;
//   - observeFixStep only ever runs against a `running` step, and this one is
//     `waiting`, so the observation that could revise the verdict never runs;
//   - dispatchReviewStep will only open cycle N+1 once the fix step's latest
//     checkpoint carries a NEW fingerprint, and stopFix wrote an empty one;
//   - verify is pending and review is not completed, so neither verify branch
//     of the cascade applies.
//
// So POST /continue walked the whole cascade and wrote nothing. Meanwhile
// canContinueRun returned true for the stop (its disposition is not
// Nonrecoverable), which is how the UI came to show an enabled "Reanudar"
// button that was provably incapable of doing anything at all. Twenty-eight
// minutes and several presses after the stop, the run had not written a single
// checkpoint.
//
// resumeUnstartedFixCycle is the missing entry point, and it is deliberately
// the narrowest one that resolves the incident:
//
//   - it lives in ContinueRun and nowhere else, for the same reason
//     resumeStaleVerifyFailure does — THIS call is a person saying "go", and a
//     rule that re-delivers work must never fire off a 2s read poll;
//   - it re-derives the evidence NOW rather than trusting the recorded stop, so
//     a run whose worker has since woken up, or whose worktree has since
//     changed, is left strictly alone;
//   - it re-delivers the SAME cycle's SAME findings to the SAME session. It
//     never opens a new cycle, never spends fix budget, never touches the
//     worktree and never weakens review independence;
//   - it is bounded by maxFixCycleRedeliveries, counted from durable
//     checkpoints, so it survives a restart without resetting and can never
//     become a loop.
//
// Everything it cannot prove, it declines to do. A terminated session, a
// worktree that no longer matches, a worker that did start, a missing dispatch
// record, an exhausted budget: each of those returns the run exactly as it was
// found, still stopped, still explained by the reason a person can read.

// fixCycleRedeliveryPhase is the durable phase of one re-delivery. It is NOT a
// canonical attention reason: it names something AO is doing, not something it
// stopped on, and a run is never parked on it.
const fixCycleRedeliveryPhase = "fix_cycle_redelivery"

// maxFixCycleRedeliveries bounds re-deliveries per fix cycle. A worker that has
// ignored the same findings three times over is not going to start on the
// fourth, and the ordinary human-owned stop is the honest outcome then.
const maxFixCycleRedeliveries = 2

// fixCycleRedeliveryRecord is the durable audit payload of one re-delivery:
// enough to reconstruct the budget and explain the decision from the ledger
// alone, and nothing that is already durable elsewhere.
type fixCycleRedeliveryRecord struct {
	CycleNumber  int    `json:"cycleNumber"`
	Redelivery   int    `json:"redelivery"`
	SessionID    string `json:"sessionId"`
	ClearedStop  string `json:"clearedStop"`
	SilentSince  string `json:"silentSince"`
	Fingerprint  string `json:"fingerprint"`
	ReviewRunID  string `json:"reviewRunId"`
	MaxAllowed   int    `json:"maxAllowed"`
	DispatchedAt string `json:"dispatchedAt"`
}

func (r fixCycleRedeliveryRecord) json() string {
	b, err := json.Marshal(r)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// resumeFixCycleStops are the two durable stops this rule may reopen. The
// legacy one is listed first because it is what the incident's rows actually
// carry: fix_cycle_not_started did not exist when they were written, and a run
// already stranded on disk must be recoverable without rewriting history.
var resumeFixCycleStops = map[string]bool{
	ReasonFixNoVerifiableChange: true,
	ReasonFixCycleNotStarted:    true,
	ReasonFixPromptNotSubmitted: true,
}

// submitPendingFixPrompt is the resume path for a cycle whose prompt is already
// in the worker's composer — Checkpoint 8P-E.17.
//
// It answers the one question that decides between the two safe actions:
// is there pending input, and can AO attribute it to itself?
//
//   - composer empty          -> nothing pending; the caller re-delivers.
//   - composer holds a draft, and AO's own durable record says it wrote this
//     cycle's prompt into THIS session -> the draft is AO's. Submit it. Nothing
//     is pasted, nothing is deleted, and the prompt cannot be duplicated.
//   - composer holds a draft AO cannot attribute -> refuse outright. It may be
//     a person's unsent message, and neither submitting nor clearing someone
//     else's draft is AO's to do.
//   - composer unreadable     -> refuse. Missing evidence is not evidence.
//
// Attribution is deliberately conservative: it rests on AO having recorded a
// delivery of this cycle to this exact session (the fix_dispatched /
// fix_dispatch_intent / fix_cycle_redelivery checkpoint the caller already
// resolved), not on matching the composer's rendered text, which no harness
// reproduces byte-for-byte once it has collapsed a large paste into a summary
// line like "[Pasted Content 15360 chars]".
//
// Returns (handled, error): handled means the pending draft was dealt with and
// the caller must not deliver anything.
func (c *Coordinator) submitPendingFixPrompt(ctx stdctx.Context, sessionID string, attributable bool) (bool, ports.PromptSubmission, error) {
	reporter, ok := c.messageSender.(SubmissionReportingSender)
	if !ok {
		// No composer visibility at all: behave exactly as before this existed.
		return false, ports.PromptSubmissionUnset, nil
	}
	state := reporter.ComposerState(ctx, domain.SessionID(sessionID))
	switch state {
	case ports.PromptSubmitted:
		// Composer is empty; a fresh delivery is the right thing.
		return false, state, nil
	case ports.PromptLoadedNotSubmitted:
		if !attributable {
			// Someone else's draft, or one AO cannot claim. Leave it alone.
			return true, state, nil
		}
		submitted, err := reporter.SubmitPending(ctx, domain.SessionID(sessionID))
		return true, submitted, err
	default:
		// Ambiguous: AO cannot see whether a draft is pending, so it must not
		// paste over one.
		return true, state, nil
	}
}

// resumeUnstartedFixCycle re-delivers a fix cycle whose worker provably never
// started it. It returns the (possibly updated) run and whether it acted.
//
// A no-op for every run that does not carry the exact durable shape below, and
// the caller's behaviour is then byte-for-byte what it always was.
func (c *Coordinator) resumeUnstartedFixCycle(ctx stdctx.Context, run domain.WorkflowRun) (domain.WorkflowRun, bool, error) {
	if run.State != domain.WorkflowRunNeedsAttention {
		return run, false, nil
	}
	if c.sessionFacts == nil || c.workspaceFacts == nil || c.messageSender == nil || c.reviewRuns == nil {
		return run, false, nil
	}

	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return run, false, err
	}
	var workStep, reviewStep, fixStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		case domain.WorkflowStepReview:
			reviewStep = &steps[i]
		case domain.WorkflowStepFix:
			fixStep = &steps[i]
		}
	}
	if workStep == nil || reviewStep == nil || fixStep == nil {
		return run, false, nil
	}
	// Only the parked shape. A fix step that is running is already being
	// observed, and one that is terminal is not this rule's business.
	if fixStep.State != domain.WorkflowStepWaiting {
		return run, false, nil
	}

	stop, _, ok := c.stopReason(ctx, run)
	if !ok || !resumeFixCycleStops[stop] {
		return run, false, nil
	}

	dispatch, hasDispatch, err := c.latestFixDispatchCheckpoint(ctx, run.ID, fixStep.ID)
	if err != nil {
		return run, false, err
	}
	// No dispatch record naming a session is no evidence at all, and this rule
	// only ever acts on evidence.
	if !hasDispatch || dispatch.SessionID == nil || *dispatch.SessionID == "" {
		return run, false, nil
	}
	sessionID := *dispatch.SessionID
	cycleNumber := fixCycleNumberOf(dispatch)
	if cycleNumber <= 0 {
		return run, false, nil
	}

	// ---- Re-derive the evidence NOW. The recorded stop is a claim about the
	// past; every condition below has to still be true for a re-delivery to be
	// the right thing to do.
	sess, found, err := c.sessionFacts.GetSession(ctx, domain.SessionID(sessionID))
	if err != nil {
		return run, false, err
	}
	if !found || sess.IsTerminated || sess.Activity.State == domain.ActivityExited {
		// Nothing to re-deliver to. The stop stands, and it is a person's call.
		return run, false, nil
	}
	if fixCycleStarted(sess, dispatch.CreatedAt) {
		// The worker did start this cycle after all — either the stop was
		// recorded by an older daemon without this rule, or the worker woke up
		// afterwards. Re-sending would duplicate findings it is already acting
		// on. Leave it; ordinary observation owns it from here.
		return run, false, nil
	}
	obs, obsOK := c.observeFixWorkspace(ctx, sess)
	if !obsOK {
		// Could not read the workspace, so cannot prove nothing was produced.
		return run, false, nil
	}
	if fp := WorkspaceFingerprint(obs); fp != dispatch.FingerprintBefore {
		// Something IS there. Re-delivering findings over unreviewed work would
		// be a guess about whose work it is; this rule refuses to make it.
		return run, false, nil
	}

	// ---- The findings to re-deliver. They must be the same cycle's, from the
	// same review, to the same session.
	if reviewStep.ReviewRunID == nil {
		return run, false, nil
	}
	reviewRun, found, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
	if err != nil {
		return run, false, err
	}
	if !found || reviewRun.EffectiveVerdict() != domain.VerdictChangesRequested {
		// The reviewer is no longer asking for changes, so there is nothing to
		// re-deliver and this is not the stop it looked like.
		return run, false, nil
	}
	if string(reviewRun.SessionID) != sessionID {
		// The dispatch record and the review disagree about which session owns
		// this work. Two sources of truth in conflict is exactly when AO must
		// not act.
		return run, false, nil
	}

	// Checkpoint 8P-E.17: before considering a re-delivery at all, deal with a
	// prompt that is already in the composer. AO's dispatch record for this
	// cycle names this session, which is what makes a pending draft here
	// attributable to AO — so the correct action is to submit it, never to
	// paste a second copy on top of it.
	handled, submission, serr := c.submitPendingFixPrompt(ctx, sessionID, true)
	if serr != nil {
		return run, false, serr
	}
	if handled {
		if submission != ports.PromptSubmitted {
			// Could not submit (or could not see) the pending draft. The stop
			// stands, unchanged, rather than being papered over with a resend.
			if c.log != nil {
				c.log.Info("workflow: fix prompt still pending in the composer; not re-sending it",
					"run", run.ID, "session", sessionID, "cycle", cycleNumber, "submission", submission)
			}
			return run, false, nil
		}
		return c.adoptSubmittedPendingFixPrompt(ctx, run, *workStep, *fixStep, dispatch, reviewRun, cycleNumber, sessionID, stop)
	}

	redeliveries := c.fixCycleRedeliveryCount(ctx, run.ID, fixStep.ID, cycleNumber)
	if redeliveries >= maxFixCycleRedeliveries {
		return run, false, nil
	}

	artifact, err := c.planArtifactForRun(ctx, run)
	if err != nil {
		return run, false, err
	}
	findings := reviewFindingsRef(reviewRun)
	prompt := BuildFixPrompt(FixPromptInput{
		Objective:          run.Objective,
		AcceptanceCriteria: artifact.AcceptanceCriteria,
		EffectiveSpec:      RenderEffectiveSpecification(c.effectiveTaskSpecification(ctx, run, artifact.AcceptanceCriteria)),
		ReviewRunID:        reviewRun.ID,
		Findings:           findings.Body,
		CycleNumber:        cycleNumber,
	})

	// ---- Claim the delivery identity BEFORE anything is sent. The key is
	// specific to this re-delivery, so it is a genuinely new command rather
	// than a re-run of one that already happened — and if a concurrent caller
	// or an earlier crashed pass already claimed it, this pass sends nothing.
	transportAttempt := c.fixTransportRetryCount(ctx, run.ID, fixStep.ID, cycleNumber)
	now := c.clock()
	// A re-delivery is its own dispatch generation, distinguished from the
	// original by the Redelivery ordinal — the same ordinal that already makes
	// its outbox key its own command. Without it the two would share a binding
	// and a recovery could adopt one for the other.
	gen := c.newFixDispatchGeneration(run, *fixStep, reviewRun, cycleNumber, transportAttempt, redeliveries+1, findings)
	entry, _, err := c.store.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID:             "wfo-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &fixStep.ID,
		IdempotencyKey: fixCycleRedeliveryIdempotencyKey(fixStep.ID, cycleNumber, transportAttempt, redeliveries+1),
		CommandType:    domain.WorkflowOutboxSendMessage,
		Payload:        fixPayloadJSON(fixStep.ID, reviewRun.ID, cycleNumber),
		CreatedAt:      now,
	})
	if err != nil {
		return run, false, err
	}
	if entry.Status != domain.WorkflowOutboxPending {
		// This exact re-delivery already got at least as far as Send on an
		// earlier pass. Never call Send again on it — MessageSender.Send has no
		// idempotency key of its own, which is the standing reason every
		// dispatch path here treats a non-pending entry as "hands off".
		return run, false, nil
	}

	// The durable record, written strictly before the send and fatal if it
	// fails: the re-delivery budget is counted from these rows, so a send AO
	// could not first write down is a send AO does not make. It also carries
	// the session and the fingerprint, so if the daemon dies immediately after
	// this write, observeFixStep still finds a dispatch checkpoint it can
	// observe against rather than a step stuck running with nothing to compare.
	stepID := fixStep.ID
	sid := sessionID
	rid := reviewRun.ID
	record := fixCycleRedeliveryRecord{
		CycleNumber:  cycleNumber,
		Redelivery:   redeliveries + 1,
		SessionID:    sessionID,
		ClearedStop:  stop,
		SilentSince:  dispatch.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Fingerprint:  dispatch.FingerprintBefore,
		ReviewRunID:  reviewRun.ID,
		MaxAllowed:   maxFixCycleRedeliveries,
		DispatchedAt: now.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		SessionID:         &sid,
		ReviewRunID:       &rid,
		FingerprintBefore: dispatch.FingerprintBefore,
		NextAction: fmt.Sprintf(
			"fix_cycle_redelivery: worker session %s never started fix cycle %d; re-delivering the same findings (%d of %d)",
			sessionID, cycleNumber, redeliveries+1, maxFixCycleRedeliveries),
		DurablePhase:   fixCycleRedeliveryPhase,
		PayloadVersion: "v1",
		RetryState:     record.json(),
		CreatedAt:      now,
	}); err != nil {
		return run, false, err
	}

	// Release the stop before delivering, so the cascade that follows this call
	// sees a live run rather than the parked snapshot the stop left behind.
	run = c.unparkRun(ctx, run, stop,
		fmt.Sprintf("worker session %s never started fix cycle %d and the workspace is unchanged, so the same findings are being re-delivered",
			sessionID, cycleNumber))

	// The claim, under this re-delivery's own token. A losing claim sends
	// nothing at all: the run has already been unparked above, which is
	// harmless and idempotent, and the pass that holds the claim owns the send.
	claimed, err := c.store.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, gen.ID)
	if err != nil {
		return run, false, err
	}
	if !claimed {
		return run, false, nil
	}
	entry.Status = domain.WorkflowOutboxDispatched
	entry.DispatchGeneration = gen.ID

	if c.log != nil {
		c.log.Info("workflow: re-delivering a fix cycle the worker never started",
			"run", run.ID, "step", fixStep.ID, "cycle", cycleNumber,
			"session", sessionID, "redelivery", redeliveries+1, "clearedStop", stop)
	}

	// deliverFixPrompt owns everything from here: it moves the step
	// waiting->running, writes the pre-delivery intent, sends, and records the
	// fix_dispatched checkpoint the observation path reads. Reusing it is the
	// point — a re-delivered cycle and a first-pass cycle must leave IDENTICAL
	// durable state, and it creates no second attempt row because
	// recordFixDispatchSuccess only creates one when len(attempts) < cycleNumber.
	if _, err := c.deliverFixPrompt(ctx, run, *workStep, *fixStep, entry, reviewRun, prompt, findings, gen); err != nil {
		return run, true, err
	}
	if refreshed, ok, rerr := c.store.GetWorkflowRun(ctx, run.ID); rerr == nil && ok {
		run = refreshed
	}
	return run, true, nil
}

// adoptSubmittedPendingFixPrompt completes a cycle whose already-loaded prompt
// AO has just submitted.
//
// The cycle is now genuinely with the agent, so the durable state must be the
// state a first-pass delivery leaves: the step running and observable, the run
// released from its stop. It writes no new outbox entry, no new attempt row and
// no second dispatch identity — this is the SAME delivery finally being given
// its turn, not a new one.
func (c *Coordinator) adoptSubmittedPendingFixPrompt(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	workStep, fixStep domain.WorkflowStep,
	dispatch domain.WorkflowCheckpoint,
	reviewRun domain.ReviewRun,
	cycleNumber int,
	sessionID, stop string,
) (domain.WorkflowRun, bool, error) {
	fixStep, err := c.runFixStep(ctx, fixStep)
	if err != nil {
		return run, false, err
	}
	// Release the stop BEFORE the success bookkeeping, for the same reason
	// adoptDeliveredFix does: the stop is resolved from the run's newest
	// checkpoint, and recordFixDispatchSuccess is about to write one.
	run = c.unparkRun(ctx, run, stop,
		fmt.Sprintf("fix cycle %d was already loaded in session %s and AO submitted it without re-sending the prompt",
			cycleNumber, sessionID))

	// The cycle's own outbox entry, under its canonical key — Enqueue returns
	// the row already on disk rather than minting a second delivery identity.
	transportAttempt := c.fixTransportRetryCount(ctx, run.ID, fixStep.ID, cycleNumber)
	entry, _, err := c.store.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID:             "wfo-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &fixStep.ID,
		IdempotencyKey: fixStepOutboxIdempotencyKey(fixStep.ID, cycleNumber, transportAttempt),
		CommandType:    domain.WorkflowOutboxSendMessage,
		Payload:        fixPayloadJSON(fixStep.ID, reviewRun.ID, cycleNumber),
		CreatedAt:      c.clock(),
	})
	if err != nil {
		return run, false, err
	}

	// Finish the dispatch through the ordinary success path rather than
	// open-coding it. This is what makes an adopted cycle indistinguishable
	// from a first-pass one — including the attempt row, whose absence would
	// otherwise send the very next cascade pass back into dispatchFixStep to
	// re-derive and re-escalate a cycle that is now genuinely with the agent.
	delivery := promptDeliveryRecordFromJSON(dispatch.RetryState)
	delivery.CycleNumber = cycleNumber
	delivery.Submission = ports.PromptSubmitted
	delivery.Reason = "submitted a prompt that was already loaded in the composer; nothing was re-sent"
	// The generation is ADOPTED from the dispatch record this submit is
	// completing — this is the SAME delivery finally being given its turn, so
	// minting a new identity for it would be a lie about how many deliveries
	// there were. A record written before generations existed adopts as legacy
	// (empty token), which is exactly what the ownership CAS needs for a row
	// claimed before the column existed.
	gen := delivery.Generation
	gen.CycleNumber = cycleNumber
	if _, err := c.recordFixDispatchSuccess(ctx, run, workStep, fixStep, entry, reviewRun, gen, delivery); err != nil {
		return run, false, err
	}
	if c.log != nil {
		c.log.Info("workflow: submitted a fix prompt that was already loaded in the composer",
			"run", run.ID, "step", fixStep.ID, "cycle", cycleNumber, "session", sessionID, "clearedStop", stop)
	}
	if refreshed, ok, rerr := c.store.GetWorkflowRun(ctx, run.ID); rerr == nil && ok {
		run = refreshed
	}
	return run, true, nil
}

// fixCycleRedeliveryIdempotencyKey extends the ordinary fix-cycle key with the
// re-delivery ordinal, so each re-delivery is its own command with its own
// outbox row rather than a second run of one that already completed.
func fixCycleRedeliveryIdempotencyKey(stepID string, cycleNumber, transportAttempt, redelivery int) string {
	return fixStepOutboxIdempotencyKey(stepID, cycleNumber, transportAttempt) +
		":redeliver" + strconv.Itoa(redelivery)
}

// fixCycleRedeliveryCount folds the ledger into "how many times has this exact
// cycle already been re-delivered". Durable by construction, so a restart
// mid-recovery cannot reset the budget.
func (c *Coordinator) fixCycleRedeliveryCount(ctx stdctx.Context, runID, stepID string, cycleNumber int) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// A read failure must never look like unused budget.
		return maxFixCycleRedeliveries
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase != fixCycleRedeliveryPhase || cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		var rec fixCycleRedeliveryRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil || rec.CycleNumber != cycleNumber {
			continue
		}
		n++
	}
	return n
}

// latestFixDispatchCheckpoint returns the newest checkpoint that represents a
// delivery of some fix cycle to a session — the ordinary dispatch, the
// pre-delivery intent a crash may have left as the newest record, or a previous
// re-delivery. All three carry the session and the fingerprint the cycle was
// dispatched against, which is exactly what this rule needs and what the stop's
// own checkpoint does not have.
func (c *Coordinator) latestFixDispatchCheckpoint(ctx stdctx.Context, runID, stepID string) (domain.WorkflowCheckpoint, bool, error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return domain.WorkflowCheckpoint{}, false, err
	}
	var newest domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		switch cp.DurablePhase {
		case fixDispatchedPhase, fixDispatchIntentPhase, fixCycleRedeliveryPhase:
		default:
			continue
		}
		if !found || !cp.CreatedAt.Before(newest.CreatedAt) {
			newest, found = cp, true
		}
	}
	return newest, found, nil
}
