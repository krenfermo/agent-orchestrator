package workflow

import (
	stdctx "context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// This file answers one question that a daemon restart used to make
// unanswerable: "was this fix cycle's prompt actually delivered to the worker?"
//
// The incident (wf-6528a538, 2026-08-22): the daemon restarted between the fix
// outbox entry reaching `dispatched` and MessageSender.Send returning. On every
// poll afterwards, dispatchFixStep re-derived the same cycle, found the entry
// already `dispatched`, and — having no fact either way — parked the run in
// needs_attention and wrote another identical `fix_dispatch_ambiguous`
// checkpoint. Every two seconds, forever. An otherwise healthy autonomous fix
// became permanent human intervention purely because AO had restarted, and the
// ledger filled with hundreds of rows describing one unchanged condition.
//
// Two things were missing, and both are here:
//
//  1. NO DURABLE PRE-DELIVERY RECORD. `dispatched` meant "we were about to call
//     Send", which conflates "crashed before Send" with "crashed after Send".
//     recordFixDispatchIntent now writes one checkpoint, strictly before Send,
//     naming the session and carrying a receipt digest of the exact bytes about
//     to be delivered. Its absence is therefore positive proof that Send was
//     never reached — which makes a resend provably safe rather than a guess.
//     The write is fatal on error precisely so that invariant holds: AO never
//     calls Send without having first recorded that it was going to.
//
//  2. NO EVIDENCE LOOKUP. AO already holds durable facts about what the worker
//     session did — the receipt of the last prompt written into it, the
//     reported turn boundaries, the activity clock. classifyFixDelivery reads
//     them and answers with proof where proof exists.
//
// The safety rule throughout: only POSITIVE evidence decides. Missing evidence
// is never read as proof of anything. A signal AO cannot interpret degrades to
// ambiguity — which is escalated once, with the evidence attached, and
// re-evaluated on every later pass so that late-arriving proof still resolves
// it automatically.

// fixDispatchIntentPhase is the durable phase of the pre-delivery record.
//
// It is deliberately NOT a canonical attention reason (attention.go): it names
// something AO is doing, not something it stopped on. A run is never parked on
// it, and ClassifyAttention must never surface it as a stop.
const fixDispatchIntentPhase = "fix_dispatch_intent"

// promptReceiptDigest is the digest of the exact bytes that end up in a
// session's LatestUserPrompt once a message has been written into it.
// Computing it from the outbound prompt at intent time and from the stored
// receipt at recovery time makes "this session received this prompt" an exact
// comparison rather than a fuzzy text match.
//
// The bounding is domain.BoundLatestUserPrompt — the same function
// session_manager applies on the way in, and the reason this comparison can be
// exact at all. Workflow deliberately does not import session_manager (see
// failover.go), so the shared definition lives on the field's own type, where
// both sides can reach it without either depending on the other.
func promptReceiptDigest(prompt string) string {
	sum := sha256.Sum256([]byte(domain.BoundLatestUserPrompt(prompt)))
	return hex.EncodeToString(sum[:])
}

// fixDeliveryVerdict is classifyFixDelivery's whole output vocabulary. There
// are exactly three answers, and the middle one is not a synonym for either
// neighbour.
type fixDeliveryVerdict int

const (
	// fixDeliveryUnproven: AO has no positive evidence either way. The only
	// verdict that may reach a human.
	fixDeliveryUnproven fixDeliveryVerdict = iota
	// fixDeliveryNotSent: AO can PROVE Send was never called, because the
	// pre-delivery record it always writes first is absent.
	fixDeliveryNotSent
	// fixDeliveryDelivered: AO can PROVE the worker received this cycle's
	// prompt, or began/finished the turn it asked for.
	fixDeliveryDelivered
)

// fixDeliveryEvidence is everything classifyFixDelivery looked at, in the form
// it is recorded on the escalation checkpoint. It is the material-change key
// for checkpoint dedup: an unchanged ambiguity produces a byte-identical
// evidence line and therefore no second checkpoint, while any change in the
// session's state produces a different one and a fresh, honest record.
type fixDeliveryEvidence struct {
	CycleNumber      int    `json:"cycleNumber"`
	TransportAttempt int    `json:"transportAttempt,omitempty"`
	SessionID        string `json:"sessionId"`
	// Generation is the fix dispatch generation this evidence is about, when
	// one is recorded. Empty means a generation-less (legacy) delivery. It is
	// part of the dedup key by construction: two different generations of the
	// same cycle are two different unproven conditions and are recorded as two.
	Generation string `json:"generation,omitempty"`
	// IntentRecorded is whether the pre-delivery record exists at all.
	IntentRecorded bool `json:"intentRecorded"`
	// Receipt is what the session says about the last prompt written into it:
	// "match" (this cycle's prompt, proven), "other" (some other prompt), or
	// "none" (nothing recorded).
	Receipt string `json:"receipt"`
	// TurnAfterDispatch is whether a reported turn boundary or live activity
	// postdates the dispatch intent.
	TurnAfterDispatch bool   `json:"turnAfterDispatch"`
	SessionState      string `json:"sessionState"`
	SessionMissing    bool   `json:"sessionMissing,omitempty"`
	SessionTerminated bool   `json:"sessionTerminated,omitempty"`
}

// line renders the evidence as the stable human-readable sentence that becomes
// the escalation checkpoint's next_action. Stable is the operative word: the
// same unchanged condition must render the same bytes on every pass, or the
// dedup this incident needs cannot work.
func (e fixDeliveryEvidence) line() string {
	receipt := map[string]string{
		"match": "the session's prompt receipt matches this cycle",
		"other": "the session's prompt receipt is for a different message",
		"none":  "the session recorded no prompt receipt",
	}[e.Receipt]
	if receipt == "" {
		receipt = "the session's prompt receipt could not be read"
	}
	session := "state " + e.SessionState
	switch {
	case e.SessionMissing:
		session = "the worker session no longer exists"
	case e.SessionTerminated:
		session = "the worker session has been terminated"
	}
	turn := "no turn boundary was reported after the dispatch"
	if e.TurnAfterDispatch {
		turn = "a turn boundary was reported after the dispatch"
	}
	return fmt.Sprintf(
		"cannot prove whether fix cycle %d reached worker session %s: %s, %s, and %s. "+
			"Open that session and check whether it received the reviewer's findings, then continue or cancel this run.",
		e.CycleNumber, e.SessionID, receipt, turn, session)
}

func (e fixDeliveryEvidence) json() string {
	b, err := json.Marshal(e)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// recordFixDispatchIntent writes the durable pre-delivery record.
//
// Ordering is the entire contract, and it is why this returns an error the
// caller must honour instead of being best-effort like most observers here:
//
//	intent written  ->  Send may be called
//	intent missing  ->  Send was never called
//
// A failed intent write means AO does not deliver this pass. Losing a cycle of
// latency is free; losing the ability to tell a crashed-before-send from a
// crashed-after-send is what produced the incident.
func (c *Coordinator) recordFixDispatchIntent(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	fixStep domain.WorkflowStep,
	reviewRun domain.ReviewRun,
	fingerprintBefore string,
	delivery promptDeliveryRecord,
) error {
	stepID := fixStep.ID
	sid := string(reviewRun.SessionID)
	rid := reviewRun.ID
	_, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		SessionID:         &sid,
		ReviewRunID:       &rid,
		ReviewVerdict:     string(reviewRun.EffectiveVerdict()),
		FingerprintBefore: fingerprintBefore,
		NextAction: fmt.Sprintf("fix_dispatch_intent: delivering fix cycle %d (%d bytes, %d findings) to session %s",
			delivery.CycleNumber, delivery.PromptBytes, delivery.Findings.Count, sid),
		DurablePhase:   fixDispatchIntentPhase,
		PayloadVersion: "v1",
		RetryState:     delivery.json(),
		CreatedAt:      c.clock(),
	})
	return err
}

// findFixDispatchIntent returns the pre-delivery record for exactly this
// logical dispatch — this step, this cycle, this transport attempt.
//
// Scoping to the transport attempt matters: a bounded prompt-transport retry
// (recordFixTransportRetry) is a genuinely new delivery with its own outbox
// key, and it must not inherit the previous attempt's intent as proof that it
// itself got as far as Send.
func (c *Coordinator) findFixDispatchIntent(ctx stdctx.Context, runID, stepID string, cycleNumber, transportAttempt int) (domain.WorkflowCheckpoint, promptDeliveryRecord, bool) {
	newest, rec, _, found := c.findFixDispatchIntents(ctx, runID, stepID, cycleNumber, transportAttempt)
	return newest, rec, found
}

// findFixDispatchIntents is the same lookup, also returning EVERY matching
// record rather than only the newest one.
//
// Generation resolution needs all of them. "Which generation owns the delivery
// on disk?" is answerable only if the records agree on one, and a lookup that
// silently kept the newest would answer it by picking a winner — which is the
// heuristic duplicate-suppression requirement 8 rules out. Disagreement is a
// fail-closed condition, so it has to be visible.
func (c *Coordinator) findFixDispatchIntents(ctx stdctx.Context, runID, stepID string, cycleNumber, transportAttempt int) (domain.WorkflowCheckpoint, promptDeliveryRecord, []promptDeliveryRecord, bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// A read failure is not evidence of absence. Returning "found" with a
		// zero record would be a lie; returning "not found" would license a
		// resend on no information at all. The caller treats the third state —
		// see resolveFixDeliveryAfterRestart's readFailed branch.
		return domain.WorkflowCheckpoint{}, promptDeliveryRecord{}, nil, false
	}
	var (
		newest domain.WorkflowCheckpoint
		rec    promptDeliveryRecord
		all    []promptDeliveryRecord
		found  bool
	)
	for _, cp := range cps {
		if cp.DurablePhase != fixDispatchIntentPhase || cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		var candidate promptDeliveryRecord
		if json.Unmarshal([]byte(cp.RetryState), &candidate) != nil {
			continue
		}
		if candidate.CycleNumber != cycleNumber || candidate.TransportAttempt != transportAttempt {
			continue
		}
		all = append(all, candidate)
		if !found || !cp.CreatedAt.Before(newest.CreatedAt) {
			newest, rec, found = cp, candidate, true
		}
	}
	return newest, rec, all, found
}

// classifyFixDelivery is the single decision function this file exists for. It
// reads only durable facts — the pre-delivery record and the session row — and
// never the agent's transcript text, in keeping with observeWorkStep's and
// observeFixStep's standing rule that AO judges from facts, not from prose.
//
// The rules, in the order they are allowed to fire:
//
//  1. No pre-delivery record => PROVEN not sent. Nothing else can outrank this:
//     the record is written before Send, so its absence means Send never ran.
//  2. The session's prompt receipt digest matches this cycle's prompt =>
//     PROVEN delivered. session_manager writes that receipt from Send's own
//     post-write hook, i.e. strictly after the bytes reached the session.
//  3. A turn boundary or live activity postdates the dispatch => PROVEN the
//     agent began or finished the turn AO asked for. This is requirement 3's
//     "the agent received or began the expected turn".
//  4. Anything else => unproven, with the evidence recorded.
//
// Rules 2 and 3 are corroborating, not competing: either alone is sufficient,
// and both are strictly about events that postdate the intent.
func (c *Coordinator) classifyFixDelivery(
	ctx stdctx.Context,
	sessionID domain.SessionID,
	cycleNumber, transportAttempt int,
	intent domain.WorkflowCheckpoint,
	intentRec promptDeliveryRecord,
	intentFound bool,
	prompt string,
) (fixDeliveryVerdict, fixDeliveryEvidence) {
	evidence := fixDeliveryEvidence{
		CycleNumber:      cycleNumber,
		TransportAttempt: transportAttempt,
		SessionID:        string(sessionID),
		IntentRecorded:   intentFound,
		Receipt:          "none",
		SessionState:     "unknown",
	}
	if !intentFound {
		return fixDeliveryNotSent, evidence
	}

	if c.sessionFacts == nil {
		return fixDeliveryUnproven, evidence
	}
	sess, found, err := c.sessionFacts.GetSession(ctx, sessionID)
	if err != nil {
		// Could not read the one fact source that could prove delivery. Not a
		// verdict — the caller re-reads on the next pass.
		return fixDeliveryUnproven, evidence
	}
	if !found {
		evidence.SessionMissing = true
		return fixDeliveryUnproven, evidence
	}
	evidence.SessionState = string(sess.Activity.State)
	evidence.SessionTerminated = sess.IsTerminated

	// Rule 2. The receipt digest recorded at intent time is authoritative; the
	// live prompt is only a fallback for a record written before this field
	// existed, and is derived exactly the same way.
	want := intentRec.PromptReceipt
	if want == "" {
		want = promptReceiptDigest(prompt)
	}
	switch {
	case sess.Metadata.LatestUserPrompt == "":
		evidence.Receipt = "none"
	case promptReceiptDigest(sess.Metadata.LatestUserPrompt) == want:
		evidence.Receipt = "match"
	default:
		evidence.Receipt = "other"
	}

	// Rule 3. Both clocks are compared against the intent's own timestamp, so
	// activity that predates the dispatch can never be mistaken for a response
	// to it. TurnCompletedAt is cleared when a new turn starts, so a completion
	// stamped after the dispatch can only belong to the turn AO asked for.
	dispatchedAt := intent.CreatedAt
	evidence.TurnAfterDispatch = afterInstant(sess.TurnCompletedAt, dispatchedAt) ||
		(sess.Activity.State == domain.ActivityActive && afterInstant(sess.Activity.LastActivityAt, dispatchedAt))

	if evidence.Receipt == "match" || evidence.TurnAfterDispatch {
		return fixDeliveryDelivered, evidence
	}
	return fixDeliveryUnproven, evidence
}

// afterInstant is "t happened at or after ref, and t happened at all". A zero
// timestamp means the fact was never recorded, which is not evidence.
func afterInstant(t, ref time.Time) bool {
	return !t.IsZero() && !t.Before(ref)
}

// resolveFixDeliveryAfterRestart is the recovery branch dispatchFixStep takes
// when it finds this cycle's outbox entry already `dispatched` — the window a
// restart can land in. It replaces the old unconditional "park and escalate"
// with the decision table above.
//
// It never calls Send on anything but a PROVEN non-delivery, and it never
// creates a second outbox entry, a second attempt row or a second fix cycle:
// the entry it works with is the one already on disk, under the same
// idempotency key, and the resend is the same logical operation the first
// dispatch was.
func (c *Coordinator) resolveFixDeliveryAfterRestart(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	workStep, fixStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	reviewRun domain.ReviewRun,
	prompt string,
	findings fixFindingsRef,
	intended fixDispatchGeneration,
) (domain.WorkflowStep, error) {
	cycleNumber, transportAttempt := intended.CycleNumber, intended.TransportAttempt
	intent, intentRec, intents, intentFound := c.findFixDispatchIntents(ctx, run.ID, fixStep.ID, cycleNumber, transportAttempt)

	// WHOSE dispatch is this? Answered before anything else, because every
	// branch below either sends or advances the lifecycle, and both are things
	// only the generation that owns the delivery may do. A generation is
	// ADOPTED here, never re-minted: a fresh token would be a second identity
	// for a delivery that already happened.
	gen, disposition, why := c.resolveOwningFixGeneration(entry, intended, intents)
	if disposition == fixGenerationUnprovable {
		return c.markFixGenerationUnprovable(ctx, run, fixStep, intended, why)
	}

	verdict, evidence := c.classifyFixDelivery(ctx, reviewRun.SessionID, cycleNumber, transportAttempt,
		intent, intentRec, intentFound, prompt)
	evidence.Generation = gen.ID

	switch verdict {
	case fixDeliveryNotSent:
		// Provably nothing reached the agent. Deliver once, through the very
		// same path the first attempt would have taken, under the generation
		// that already owns the claim.
		if c.log != nil {
			c.log.Info("workflow: fix prompt was provably never delivered; delivering once",
				"run", run.ID, "step", fixStep.ID, "cycle", cycleNumber, "generation", gen.ID)
		}
		return c.deliverFixPrompt(ctx, run, workStep, fixStep, entry, reviewRun, prompt, findings, gen)

	case fixDeliveryDelivered:
		// The agent has it. Finish the bookkeeping the crash interrupted and
		// hand the cycle to observeFixStep — no second send, ever.
		if c.log != nil {
			c.log.Info("workflow: fix prompt delivery proven after restart; resuming observation",
				"run", run.ID, "step", fixStep.ID, "cycle", cycleNumber, "generation", gen.ID,
				"receipt", evidence.Receipt, "turn", evidence.TurnAfterDispatch)
		}
		return c.adoptDeliveredFix(ctx, run, workStep, fixStep, entry, reviewRun, gen, intentRec, evidence)
	}

	return c.markFixDeliveryUnproven(ctx, run, fixStep, evidence)
}

// markFixGenerationUnprovable is requirement 9's fail-closed outcome: durable
// fix-cycle state that AO cannot map onto exactly one dispatch generation.
//
// It does the opposite of a retry: the step is parked (never failed, so a person
// can still continue the run once they have resolved it), the run is parked with
// a named canonical reason, and the condition is recorded ONCE —
// recordAttentionStopOnce compares against the run's newest checkpoint, so an
// unchanged condition writes nothing on the second and every later pass. No wake
// is scheduled and no budget is spent, because nothing AO can do by itself will
// change the answer: the ledger is what it is.
//
// Fabricating a generation to get past this is the one thing it must never do.
// A guessed identity would let a delivery nobody can account for open an
// attempt and advance a review cycle, which is exactly the class of bug the
// generation exists to prevent.
func (c *Coordinator) markFixGenerationUnprovable(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	fixStep domain.WorkflowStep,
	intended fixDispatchGeneration,
	why string,
) (domain.WorkflowStep, error) {
	now := c.clock()
	if fixStep.State == domain.WorkflowStepRunning || fixStep.State == domain.WorkflowStepReady {
		if _, err := c.store.UpdateWorkflowStepState(ctx, fixStep.ID, fixStep.State, domain.WorkflowStepWaiting, now); err != nil {
			return fixStep, err
		}
		fixStep.State = domain.WorkflowStepWaiting
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return fixStep, err
		}
	}
	detail := why + " AO will not send this fix cycle, open an attempt for it or advance the run on it until that is resolved."
	c.recordAttentionStopOnce(ctx, run, &fixStep.ID, ReasonFixGenerationUnprovable, detail)
	if c.log != nil {
		c.log.Warn("workflow: fix dispatch generation is unprovable; failing closed",
			"run", run.ID, "step", fixStep.ID, "cycle", intended.CycleNumber, "reason", why)
	}
	return fixStep, nil
}

// adoptDeliveredFix completes a dispatch whose delivery AO has since proven,
// without re-sending it: the step is put back into the running state the
// observation path expects, the attempt row and acknowledgement the crash
// interrupted are written by the ordinary success path, and a run parked on the
// earlier ambiguity is released.
//
// Reusing recordFixDispatchSuccess rather than open-coding the writes is the
// point: a recovered dispatch and a first-pass dispatch must leave IDENTICAL
// durable state, or every downstream reader has to learn about a second shape.
func (c *Coordinator) adoptDeliveredFix(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	workStep, fixStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	reviewRun domain.ReviewRun,
	gen fixDispatchGeneration,
	intentRec promptDeliveryRecord,
	evidence fixDeliveryEvidence,
) (domain.WorkflowStep, error) {
	fixStep, err := c.runFixStep(ctx, fixStep)
	if err != nil {
		return fixStep, err
	}
	// Release the stop BEFORE the success bookkeeping, not after: the stop
	// reason is resolved from the run's newest checkpoint, and
	// recordFixDispatchSuccess is about to write one. Clearing afterwards would
	// look up a reason that had just been superseded and leave the run parked
	// on a question this call had already answered.
	c.clearAmbiguousFixStop(ctx, run)
	intentRec.Reason = "delivery proven after restart (" + evidence.Receipt + ")"
	intentRec.CycleNumber = gen.CycleNumber
	return c.recordFixDispatchSuccess(ctx, run, workStep, fixStep, entry, reviewRun, gen, intentRec)
}

// clearAmbiguousFixStop releases a run parked on an unproven fix delivery that
// AO has since proven. It is the same narrow, evidence-first shape as
// clearIntegrationStop and reconcileMirroredChildStop: called only from a site
// that has just proven the condition gone, and touching exactly one reason —
// a run stopped for anything else is left alone.
//
// Without it, proving delivery on a later pass would leave the run stopped on a
// question that had been answered, which is the same class of bug as the
// mirrored child stop that outlived its child.
func (c *Coordinator) clearAmbiguousFixStop(ctx stdctx.Context, run domain.WorkflowRun) {
	if run.State != domain.WorkflowRunNeedsAttention {
		return
	}
	reason, _, ok := c.stopReason(ctx, run)
	if !ok || reason != ReasonFixDispatchAmbiguous {
		return
	}
	c.unparkRun(ctx, run, reason, "the fix prompt's delivery was proven from durable session evidence")
}

// markFixDeliveryUnproven is the only path in this file that may reach a human,
// and it is reached only after every automatic avenue has been tried and come
// back empty — which is what requirement 6 means by "needs_attention must mean
// AO has exhausted safe automatic evidence".
//
// It escalates ONCE. recordAttentionStopOnce compares the stop it is about to
// write against the run's newest checkpoint, so an unchanged ambiguity — the
// same cycle, the same session, the same receipt and turn evidence — writes
// nothing at all on the second and every subsequent pass. That is the whole
// cure for the ~2s checkpoint spam: the condition, not the poll, decides
// whether there is anything new to record. Any material change in the evidence
// renders a different sentence and is therefore recorded as the new fact it is.
func (c *Coordinator) markFixDeliveryUnproven(ctx stdctx.Context, run domain.WorkflowRun, fixStep domain.WorkflowStep, evidence fixDeliveryEvidence) (domain.WorkflowStep, error) {
	now := c.clock()
	if fixStep.State == domain.WorkflowStepRunning || fixStep.State == domain.WorkflowStepReady {
		if _, err := c.store.UpdateWorkflowStepState(ctx, fixStep.ID, fixStep.State, domain.WorkflowStepWaiting, now); err != nil {
			return fixStep, err
		}
		fixStep.State = domain.WorkflowStepWaiting
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return fixStep, err
		}
	}
	c.recordAttentionStopOnce(ctx, run, &fixStep.ID, ReasonFixDispatchAmbiguous, evidence.line())
	return fixStep, nil
}
