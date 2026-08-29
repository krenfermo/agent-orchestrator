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
	// Generation is the durable identity of the dispatch that wrote this record
	// — see fix_generation.go. It is minted before the outbox claim, stamped
	// onto the outbox row BY that claim, and written here strictly before Send,
	// so the row and the ledger name the same dispatch and either reconstructs
	// the other. A zero value means a generation-less (legacy) record.
	Generation fixDispatchGeneration `json:"generation,omitempty"`
}

func newPromptDeliveryRecord(prompt string, gen fixDispatchGeneration, contextPack bool) promptDeliveryRecord {
	return promptDeliveryRecord{
		PromptBytes:      len(prompt),
		Transport:        ports.PromptTransportFor(len(prompt)),
		ContextPack:      contextPack,
		CycleNumber:      gen.CycleNumber,
		TransportAttempt: gen.TransportAttempt,
		PromptReceipt:    promptReceiptDigest(prompt),
		Generation:       gen,
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
	if int64(len(attempts)) >= int64(cycleNumber) && !c.fixCycleDispatchIncomplete(ctx, run.ID, fixStep.ID, cycleNumber) {
		// This cycle already has its attempt row: either fully dispatched
		// and progressing, or ambiguous and awaiting attention. Either way,
		// never re-enter dispatch for it.
		//
		// The one exception is crash boundary F, and it is stated as a positive
		// shape rather than as "no dispatch record". The attempt row is created
		// before the fix_dispatched record (that record must name the attempt),
		// so a crash in between leaves a cycle the attempt count calls
		// "dispatched" and that observeFixStep cannot observe at all, because
		// the checkpoint it reads the session and fingerprint from was never
		// written. fixCycleDispatchIncomplete recognises exactly that window —
		// an intent record present, a dispatch record absent — and lets it fall
		// through to the recovery branch, which completes the bookkeeping
		// without sending anything a second time.
		//
		// The shape is deliberately narrow. An attempt row with NO intent record
		// is not this window: the attempt is durable evidence that a delivery
		// completed, so "no intent record" cannot be read as "Send was never
		// reached" there, and falling through would license a resend on state
		// that proves the opposite.
		return fixStep, nil
	}

	now := c.clock()
	transportAttempt := c.fixTransportRetryCount(ctx, run.ID, fixStep.ID, cycleNumber)
	// The identity this pass believes the dispatch to be, derived from what it
	// can read right now and carrying no token yet. A pending entry mints a
	// token from it; a recovered entry compares the generation on disk against
	// it. See fix_generation.go.
	intended := c.intendedFixDispatchGeneration(run, fixStep, reviewRun, cycleNumber, transportAttempt, 0, findings)
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
		gen := intended
		gen.ID = "wfg-" + c.newID()
		return c.dispatchFixFromPending(ctx, run, workStep, fixStep, entry, reviewRun, prompt, findings, gen)
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
		return c.resolveFixDeliveryAfterRestart(ctx, run, workStep, fixStep, entry, reviewRun, prompt, findings, intended)
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
	prompt string,
	findings fixFindingsRef,
	gen fixDispatchGeneration,
) (domain.WorkflowStep, error) {
	now := c.clock()
	// The claim, and the token it is taken under — the same S4/S5 shape worker
	// dispatch uses (dispatch.go), for the same reason.
	//
	// ClaimWorkflowOutboxDispatch replaces the plain status CAS this path used
	// to make. The plain CAS deliberately CLEARS both generation columns, which
	// is right for a transition that ENDS a claim and exactly wrong for one that
	// takes it: it left the row `dispatched` and owned by nobody, so every later
	// transition off it — acknowledge, fail, release — was satisfiable by any
	// pass that happened to re-derive the same cycle.
	claimed, err := c.store.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, gen.ID)
	if err != nil {
		return fixStep, err
	}
	if !claimed {
		// Somebody else took this row between the enqueue that found it pending
		// and this statement. That pass owns the delivery; this one must not
		// send a second copy of the same findings into the worker's composer,
		// and must not report an outcome for a dispatch it does not hold.
		if c.log != nil {
			c.log.Debug("workflow: another pass claimed this fix dispatch first",
				"run", run.ID, "step", fixStep.ID, "cycle", gen.CycleNumber)
		}
		return fixStep, nil
	}
	entry.Status = domain.WorkflowOutboxDispatched
	entry.DispatchGeneration = gen.ID

	return c.deliverFixPrompt(ctx, run, workStep, fixStep, entry, reviewRun, prompt, findings, gen)
}

// fixCycleDispatchIncomplete recognises crash boundary F for one cycle: AO
// wrote the pre-delivery record (so Send was reached) and never wrote the
// fix_dispatched record that completes it.
//
// Both halves are required. A cycle with neither record has nothing to complete;
// a cycle with both is finished. Only "intent yes, dispatched no" is the
// interrupted window, and saying so positively is what keeps a ledger that has
// lost its dispatch rows for some other reason from being read as one AO may
// deliver into again.
func (c *Coordinator) fixCycleDispatchIncomplete(ctx stdctx.Context, runID, stepID string, cycleNumber int) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// A read failure must never look like an interrupted dispatch: that
		// would re-enter dispatch on no information at all.
		return false
	}
	intent := false
	for _, cp := range cps {
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
			continue
		}
		switch cp.DurablePhase {
		case fixDispatchedPhase:
			if fixCycleNumberOf(cp) == cycleNumber {
				return false
			}
		case fixDispatchIntentPhase:
			if fixCycleNumberOf(cp) == cycleNumber {
				intent = true
			}
		}
	}
	return intent
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
	prompt string,
	findings fixFindingsRef,
	gen fixDispatchGeneration,
) (domain.WorkflowStep, error) {
	cycleNumber, transportAttempt := gen.CycleNumber, gen.TransportAttempt
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
		return c.refuseFixDelivery(ctx, fixStep, entry, gen, refusal)
	}
	// The generation half of the same gate. fixAuthorityRefusal asks whether the
	// CURRENT review authorizes a fix cycle at all; this asks whether the cycle
	// in hand is still the cycle that review authorized — the exact question a
	// superseded review generation, a re-aimed session or a changed findings
	// payload each answers "no" to while the first gate says yes. See
	// fixGenerationStaleRefusal.
	if refusal := c.fixGenerationStaleRefusal(ctx, gen, reviewRun, findings); refusal != "" {
		if c.log != nil {
			c.log.Info("workflow: refusing a stale fix dispatch generation",
				"run", run.ID, "step", fixStep.ID, "generation", gen.ID, "cycle", cycleNumber, "reason", refusal)
		}
		return c.refuseFixDelivery(ctx, fixStep, entry, gen, refusal)
	}

	fixStep, err := c.runFixStep(ctx, fixStep)
	if err != nil {
		return fixStep, err
	}
	// Checkpoint 8M §12/§27: apply the session lifecycle decision (and
	// persist it) right here — the single outbox-idempotency-guarded point
	// reached exactly once per real cycle dispatch, never once per poll.
	prompt, contextPack := c.applyFixLifecycleDecision(ctx, run, fixStep, reviewRun, cycleNumber, prompt)
	delivery := newPromptDeliveryRecord(prompt, gen, contextPack)
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
			return c.recordFixTransportRetry(ctx, run, fixStep, entry, gen, delivery, err)
		}
		return c.recordFixDispatchFailure(ctx, run, fixStep, entry, gen, domain.WorkflowErrorPromptDeliveryFailed, err)
	}
	// Checkpoint 8P-E.17: a prompt sitting in the composer is NOT a delivered
	// fix cycle, and recording one as if it were is the whole of wf-57f90ff2.
	// Every submit-only retry has already been spent by deliverAndConfirm, so
	// this is a durable statement of a condition, not a place to try again.
	if submission == ports.PromptLoadedNotSubmitted {
		return c.recordFixPromptNotSubmitted(ctx, run, fixStep, entry, reviewRun, gen, delivery)
	}
	delivery.Submission = submission
	return c.recordFixDispatchSuccess(ctx, run, workStep, fixStep, entry, reviewRun, gen, delivery)
}

// refuseFixDelivery is what a refused delivery leaves behind: nothing, plus the
// claim handed back.
//
// Refusing is inert by design — no prompt, no attempt, no checkpoint, no state
// change — but the outbox row has already been claimed by this pass, and a
// claim nobody is going to use is a row the dispatch that IS current cannot
// take. Releasing it is compare-and-swapped on this generation's own token, so
// a pass that has since lost the claim releases nothing rather than handing a
// live delivery's row to somebody else.
func (c *Coordinator) refuseFixDelivery(
	ctx stdctx.Context,
	fixStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	gen fixDispatchGeneration,
	reason string,
) (domain.WorkflowStep, error) {
	if entry.Status != domain.WorkflowOutboxDispatched {
		return fixStep, nil
	}
	released, err := c.store.ReleaseDispatchedWorkflowOutboxGeneration(ctx, entry.ID, "", gen.ID)
	if err != nil {
		return fixStep, err
	}
	if !released && c.log != nil {
		c.log.Debug("workflow: refused fix delivery no longer holds its outbox claim",
			"step", fixStep.ID, "generation", gen.ID, "reason", reason)
	}
	return fixStep, nil
}

func (c *Coordinator) recordFixDispatchSuccess(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	workStep, fixStep domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
	reviewRun domain.ReviewRun,
	gen fixDispatchGeneration,
	delivery promptDeliveryRecord,
) (domain.WorkflowStep, error) {
	now := c.clock()
	cycleNumber := gen.CycleNumber

	// THE OWNERSHIP GATE, and it comes first on purpose.
	//
	// Everything below this line mutates the fix lifecycle: it opens the
	// attempt row a fix cycle is counted by, and it writes the fix_dispatched
	// fingerprint checkpoint that observeFixStep observes against and that
	// dispatchReviewStep later reads as its licence to open the next review. A
	// generation that no longer owns this delivery must be able to do none of
	// it — which is requirement 3's "a stale generation must not open an
	// attempt, advance fix state, or trigger review", enforced at the one place
	// all three are written.
	//
	// The gate is the acknowledge CAS itself: `dispatched -> acknowledged` for
	// the EXACT token that holds the claim. Zero rows matched means this pass
	// does not own the delivery, and the honest response is to change nothing.
	// An entry already `acknowledged` is crash boundary D — the ack landed and
	// the daemon died before the attempt row — and is the one case that
	// legitimately proceeds without a CAS of its own, because the recovery path
	// that got here has already proven, from the pre-delivery record, that this
	// generation is the one that acknowledged it.
	if entry.Status != domain.WorkflowOutboxAcknowledged {
		acked, err := c.store.AcknowledgeWorkflowOutboxDispatch(ctx, entry.ID, entry.Status, now, gen.ID)
		if err != nil {
			return fixStep, err
		}
		if !acked {
			if c.log != nil {
				c.log.Info("workflow: fix dispatch generation no longer owns its outbox entry; recording nothing",
					"run", run.ID, "step", fixStep.ID, "generation", gen.ID, "cycle", cycleNumber, "status", entry.Status)
			}
			return fixStep, nil
		}
		entry.Status = domain.WorkflowOutboxAcknowledged
	}

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
	// The attempt is bound to the generation on BOTH records, so recovery can
	// answer "which attempt did this generation open?" from either side.
	delivery.Generation = gen
	delivery.Generation.FixAttemptID = delivery.FixAttemptID

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
func (c *Coordinator) recordFixTransportRetry(ctx stdctx.Context, run domain.WorkflowRun, fixStep domain.WorkflowStep, entry domain.WorkflowOutboxEntry, gen fixDispatchGeneration, delivery promptDeliveryRecord, cause error) (domain.WorkflowStep, error) {
	now := c.clock()
	// Ownership-conditioned, like every other transition off `dispatched`: a
	// generation that has lost its claim must not stamp its own transport
	// failure onto a live delivery, nor park a step somebody else is driving.
	failed, err := c.store.FailWorkflowOutboxWithGeneration(
		ctx, entry.ID, entry.Status, now, string(domain.WorkflowErrorPromptDeliveryFailed), gen.ID, gen.ID)
	if err != nil {
		return fixStep, err
	}
	if !failed {
		if c.log != nil {
			c.log.Info("workflow: stale fix generation cannot record a transport retry",
				"step", fixStep.ID, "generation", gen.ID, "cycle", gen.CycleNumber)
		}
		return fixStep, nil
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

func (c *Coordinator) recordFixDispatchFailure(ctx stdctx.Context, run domain.WorkflowRun, fixStep domain.WorkflowStep, entry domain.WorkflowOutboxEntry, gen fixDispatchGeneration, errClass domain.WorkflowErrorClass, cause error) (domain.WorkflowStep, error) {
	now := c.clock()
	failed, err := c.store.FailWorkflowOutboxWithGeneration(
		ctx, entry.ID, entry.Status, now, string(errClass), gen.ID, gen.ID)
	if err != nil {
		return fixStep, err
	}
	if !failed {
		// The claim moved on. Failing the step and parking the run for a
		// dispatch this pass no longer owns would stop a delivery that is, as
		// far as the ledger is concerned, still live.
		if c.log != nil {
			c.log.Info("workflow: stale fix generation cannot record a dispatch failure",
				"step", fixStep.ID, "generation", gen.ID, "cycle", gen.CycleNumber, "err", cause)
		}
		return fixStep, nil
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
	gen fixDispatchGeneration,
	delivery promptDeliveryRecord,
) (domain.WorkflowStep, error) {
	now := c.clock()
	if entry.Status != domain.WorkflowOutboxAcknowledged {
		acked, err := c.store.AcknowledgeWorkflowOutboxDispatch(ctx, entry.ID, entry.Status, now, gen.ID)
		if err != nil {
			return fixStep, err
		}
		if !acked {
			// Not this pass's delivery to park. Whoever owns the claim owns the
			// outcome, and this generation records nothing.
			if c.log != nil {
				c.log.Info("workflow: stale fix generation cannot record an unsubmitted prompt",
					"step", fixStep.ID, "generation", gen.ID, "cycle", gen.CycleNumber)
			}
			return fixStep, nil
		}
		entry.Status = domain.WorkflowOutboxAcknowledged
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
