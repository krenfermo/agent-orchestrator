package workflow

import (
	stdctx "context"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
)

// dialog_delivery.go — P3-C's last blocker: getting a durable answer into a
// worker that is blocked on an interactive prompt.
//
// THE INCIDENT, precisely. Smoke C proved every link of the autonomous decision
// chain live — a real Claude worker asked a real question, AO captured it,
// classified it low-risk, a Codex resolver answered it from repository
// evidence, and the answer became durable with answer_source=autonomous. Then
// nothing happened, forever: the only way to deliver an answer was
// Runtime.SendMessage, which types text and presses Enter, and sessionguard
// correctly refuses that against a blocked session because the Enter would
// confirm whichever option the cursor sits on. A decision existed, was
// justified and evidenced, and could not reach the agent that was waiting for it.
//
// The mechanism below is the answer, and its shape is dictated by what can go
// wrong. Every step is either a re-observation or a refusal:
//
//	observe -> match -> plan -> navigate -> RE-OBSERVE -> confirm -> verify gone
//
// The re-observation before the confirm is what separates this from the thing
// it replaces. Terminal rendering is asynchronous; keys are accepted long before
// the redraw lands. Confirming without re-reading is pressing Enter and hoping,
// which is exactly the hazard sessionguard exists to prevent — and reproducing
// it inside a new code path would be worse than not having the path at all.
//
// Human and autonomous answers travel this identical path (§7). The ONLY
// difference between them anywhere in AO is the durable answer_source; a "safe
// path for people, blind path for the machine" split would mean the mechanism
// nobody reviewed is the one that runs unattended.

// DialogKeySender is the runtime capability structured delivery needs. It is a
// narrow local interface rather than an import of the tmux adapter, following
// this package's convention, and it is optional: a runtime without it simply
// cannot answer dialogs, which is reported rather than worked around.
type DialogKeySender interface {
	SendKeys(ctx stdctx.Context, handle ports.RuntimeHandle, keys []ports.DialogKey) error
}

// dialogParsers and dialogResponders are the per-harness capability registries.
// A harness absent from either cannot have its prompts answered structurally,
// and callers report that honestly (§15) rather than falling back to typing.
//
// They are declared next to questionHarnessParsers in questions_wiring.go's
// spirit: one place per capability, keyed by harness, with membership as the
// entire definition of "AO can do this for that provider".

// dialogObservationAttempts bounds how many times delivery re-reads the pane
// waiting for a redraw to catch up.
//
// Bounded, and small. The alternative to a bound is a sleep, and the
// alternative to a sleep is a loop that never ends: a provider that has stopped
// redrawing is a provider AO must report on, not one it should wait for
// indefinitely. Five reads at the interval below is well over a second, which
// is an eternity for a terminal repaint and still a blink for a person.
const dialogObservationAttempts = 5

// dialogObservationInterval is the pause between those reads.
const dialogObservationInterval = 250 * time.Millisecond

// DialogDeliveryOutcome is what one delivery attempt actually did.
//
// Delivered is the only field that authorises marking a question consumed, and
// it is set only after AO re-read the pane and found the dialog gone. "The keys
// were written" is deliberately NOT delivery: tmux reports success for keys a
// pane in copy-mode silently swallowed, and a receipt built on the write alone
// would record an answer the agent never saw.
type DialogDeliveryOutcome struct {
	// Attempted reports that a structured response was actually applicable —
	// there was a dialog, and it matched. False means the caller should fall
	// back to its ordinary text path, or report the refusal.
	Attempted bool
	// Delivered reports that AO re-observed the prompt gone afterwards.
	Delivered bool
	// Refusal names why nothing was sent, when nothing was.
	Refusal domain.DialogResponseRefusal
	// SelectedOptionID and SelectedOptionLabel are what the plan aimed at.
	SelectedOptionID    string
	SelectedOptionLabel string
	// Detail is AO's own sentence, for the ledger and the log.
	Detail string
}

// deliverDialogResponse answers one blocked worker's prompt with one durable
// answer.
//
// It writes nothing to the database: the caller owns the receipt, because the
// caller is what knows which durable row this answer belongs to. This function
// owns only the interaction and the proof.
func (c *Coordinator) deliverDialogResponse(
	ctx stdctx.Context, sess domain.SessionRecord, resp domain.StructuredProviderResponse,
) DialogDeliveryOutcome {
	parser, hasParser := dialogParserFor(sess.Harness)
	responder, hasResponder := dialogResponderFor(sess.Harness)
	if !hasParser || !hasResponder {
		return DialogDeliveryOutcome{
			Refusal: domain.RefusalProviderUnsupported,
			Detail:  fmt.Sprintf("%s cannot have its prompts answered through AO", sess.Harness),
		}
	}
	keys, ok := c.dialogKeySender()
	if !ok {
		return DialogDeliveryOutcome{
			Refusal: domain.RefusalProviderUnsupported,
			Detail:  "this runtime cannot press keys, so AO cannot answer an interactive prompt in it",
		}
	}
	handle := ports.RuntimeHandle{ID: sess.Metadata.RuntimeHandleID}
	if handle.ID == "" || c.paneReader == nil {
		return DialogDeliveryOutcome{
			Refusal: domain.RefusalProviderUnsupported,
			Detail:  "AO cannot read this session's terminal, so it cannot see what it would be answering",
		}
	}

	// §11: the answer may only reach the session it was computed for.
	if resp.ExpectedSession != "" && resp.ExpectedSession != sess.ID {
		return DialogDeliveryOutcome{Refusal: domain.RefusalSessionMismatch,
			Detail: "this answer was computed for a different session"}
	}
	if resp.ExpectedProvider != "" && resp.ExpectedProvider != sess.Harness {
		return DialogDeliveryOutcome{Refusal: domain.RefusalSessionMismatch,
			Detail: "this answer was computed for a different provider"}
	}

	obs := c.observeDialog(ctx, parser, handle, sess)
	if obs.Inconclusive() {
		// AO could not establish what is on the screen. Nothing is pressed and
		// nothing is concluded: this is the state that must NOT become a
		// delivery receipt (P3-D §3), and reporting it as its own refusal is
		// what keeps it from being folded into the absence below.
		return DialogDeliveryOutcome{Refusal: domain.RefusalDialogUnreadable,
			Detail: orValue(obs.Reason, "AO could not read what the agent is showing")}
	}
	if obs.Absent() {
		// No prompt on screen, established. For a redelivery after a successful
		// one this is the NORMAL outcome and it is what makes exactly-once
		// achievable across a crash: the proof that the answer landed is the
		// absence of the question.
		return DialogDeliveryOutcome{Refusal: domain.RefusalDialogGone,
			Detail: "the agent is no longer showing a prompt"}
	}
	dialog := obs.Dialog
	if refusal := dialog.MatchesExpectation(resp); refusal != domain.RefusalNone {
		return DialogDeliveryOutcome{Refusal: refusal,
			Detail: "the agent is showing a different prompt than the one this answer was computed for"}
	}
	if !responder.SupportsDialogKind(dialog.Kind) {
		return DialogDeliveryOutcome{Refusal: domain.RefusalUnsupportedDialogKind,
			Detail: fmt.Sprintf("AO cannot answer a %q prompt structurally", dialog.Kind)}
	}

	plan, refusal := responder.PlanDialogResponse(dialog, resp)
	if refusal != domain.RefusalNone {
		return DialogDeliveryOutcome{Refusal: refusal, Detail: refusal.Describe()}
	}

	out := DialogDeliveryOutcome{
		Attempted:           true,
		SelectedOptionID:    plan.TargetOptionID,
		SelectedOptionLabel: plan.TargetOptionLabel,
	}
	for _, action := range plan.Actions {
		switch action.Kind {
		case ports.DialogActionKey:
			if err := keys.SendKeys(ctx, handle, []ports.DialogKey{action.Key}); err != nil {
				out.Detail = "AO could not press a key in the agent's terminal: " + err.Error()
				return out
			}
		case ports.DialogActionText:
			// The responder is what decides whether typing is legal, and no
			// select plan emits this. Refusing it here as well is belt and
			// braces on the one mistake this whole file exists to prevent.
			out.Refusal = domain.RefusalUnsupportedDialogKind
			out.Detail = "a structured plan tried to type into a selection prompt"
			return out
		case ports.DialogActionObserve:
			if !c.awaitDialogSelection(ctx, parser, handle, sess, dialog.Fingerprint, action.ExpectOptionID) {
				// The cursor is not where the plan needs it. Stopping here
				// leaves a navigated-but-unconfirmed dialog, which is a state a
				// person or a later pass can still resolve; pressing Enter
				// anyway would confirm an unknown row, which nobody can undo.
				out.Refusal = domain.RefusalDialogChanged
				out.Detail = fmt.Sprintf(
					"AO moved the selection but could not confirm it landed on %q, so it did not press Enter",
					plan.TargetOptionLabel)
				return out
			}
		}
	}

	// The receipt: the prompt has to be GONE. Not "the keys were accepted".
	if !c.awaitDialogGone(ctx, parser, handle, sess, dialog.Fingerprint) {
		out.Detail = "AO answered the prompt but the agent is still showing it; the answer stays pending and is retried"
		return out
	}
	out.Delivered = true
	out.Detail = fmt.Sprintf("selected %q", plan.TargetOptionLabel)
	return out
}

// observeDialog reads the pane once and reports which of the three things that
// reading established (P3-D §2).
//
// A pane AO could not READ is inconclusive, not empty: "the runtime would not
// answer" and "the screen has no prompt on it" are different facts, and only
// the second may be used as evidence a prompt was answered.
func (c *Coordinator) observeDialog(
	ctx stdctx.Context, parser ports.DialogPaneParser, handle ports.RuntimeHandle, sess domain.SessionRecord,
) domain.DialogObservation {
	paneText, err := c.readVisiblePane(ctx, handle)
	if err != nil {
		return domain.DialogUnreadable("AO could not read the agent's terminal: " + err.Error())
	}
	obs := parser.ObserveDialog(paneText)
	if !obs.Present() {
		return obs
	}
	obs.Dialog.SessionID = sess.ID
	obs.Dialog.Provider = sess.Harness
	// The parser fingerprints without a session (it sees only text); binding the
	// session in here is what makes one prompt's identity distinct from an
	// identical prompt in another worker.
	obs.Dialog.Fingerprint = domain.DialogFingerprint(
		sess.Harness, sess.ID, obs.Dialog.Kind, obs.Dialog.Prompt, obs.Dialog.Options)
	return obs
}

// awaitDialogSelection re-reads until the cursor is on the expected option, or
// the bounded attempts run out.
//
// It also refuses if the DIALOG changed underneath: a prompt that was replaced
// mid-navigation is a different question, and landing a confirm on it would be
// answering something nobody was asked.
func (c *Coordinator) awaitDialogSelection(
	ctx stdctx.Context, parser ports.DialogPaneParser, handle ports.RuntimeHandle,
	sess domain.SessionRecord, fingerprint, expectOptionID string,
) bool {
	for attempt := 0; attempt < dialogObservationAttempts; attempt++ {
		obs := c.observeDialog(ctx, parser, handle, sess)
		switch {
		case obs.Present() && obs.Dialog.Fingerprint == fingerprint:
			if sel, has := obs.Dialog.SelectedOption(); has && sel.ID == expectOptionID {
				return true
			}
		case obs.Present():
			// A different prompt is on screen now. Waiting cannot make it the
			// right one again.
			return false
		case obs.Absent():
			// The prompt AO was navigating is gone. Nothing to confirm, and
			// pressing Enter into whatever replaced it is the exact hazard the
			// re-observation exists to prevent.
			return false
		}
		// Inconclusive falls through to another look: a half-drawn repaint is
		// the ordinary reason a mid-navigation read says nothing, and it
		// resolves on its own within an observation or two.
		if !c.pauseForDialog(ctx) {
			return false
		}
	}
	return false
}

// awaitDialogGone re-reads until the prompt is no longer there.
func (c *Coordinator) awaitDialogGone(
	ctx stdctx.Context, parser ports.DialogPaneParser, handle ports.RuntimeHandle,
	sess domain.SessionRecord, fingerprint string,
) bool {
	for attempt := 0; attempt < dialogObservationAttempts; attempt++ {
		obs := c.observeDialog(ctx, parser, handle, sess)
		if obs.Absent() || (obs.Present() && obs.Dialog.Fingerprint != fingerprint) {
			// Either nothing is being asked, or a NEW question is — and both
			// mean this one was answered and the agent moved on.
			//
			// Inconclusive is deliberately not in this condition (P3-D §3). It
			// used to be, through `!ok`, and that is precisely how a parser
			// that could not read a two-column select certified the prompt it
			// could not read as gone. An observation that established nothing
			// gets another look, and if the attempts run out the receipt is
			// simply not granted.
			return true
		}
		if !c.pauseForDialog(ctx) {
			return false
		}
	}
	return false
}

// pauseForDialog waits one observation interval, honouring cancellation.
// It reports false when the caller's context ended, so every wait is bounded by
// both a count and the caller's own deadline.
func (c *Coordinator) pauseForDialog(ctx stdctx.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(dialogObservationInterval):
		return true
	}
}

// dialogResponseFor turns one durable answered question into the structured
// response that answers its prompt.
//
// The durable row is the whole source. AnswerReference carries the option's own
// identifier when the answer named one (a human choosing a structured choice, a
// policy answer), and AnswerText carries the label — which is what a resolver's
// free-form answer produces. Both are passed through so MatchOption can prefer
// the identity and fall back to the words (§6).
func dialogResponseFor(q domain.WorkflowQuestion, sess domain.SessionRecord) domain.StructuredProviderResponse {
	return domain.StructuredProviderResponse{
		Kind:             domain.DialogSelect,
		OptionID:         strings.TrimSpace(q.AnswerReference),
		OptionLabel:      strings.TrimSpace(q.AnswerText),
		ExpectedProvider: sess.Harness,
		ExpectedSession:  sess.ID,
		// The QUESTION's own text is what binds this answer to the prompt it
		// answers (§5). A dialog fingerprint would be the obvious choice and is
		// the wrong one here: the question row predates any dialog observation,
		// so the only thing durably true about the prompt AO decided on is what
		// it asked. Comparing that to what is on screen now is what stops an
		// old decision from landing on a new question.
		ExpectedPrompt: strings.TrimSpace(q.QuestionText),
	}
}

// dialogKeySender resolves the runtime's key capability, when one is wired.
func (c *Coordinator) dialogKeySender() (DialogKeySender, bool) {
	if c.dialogKeys == nil {
		return nil, false
	}
	return c.dialogKeys, true
}

// readVisiblePane reads what the agent is showing RIGHT NOW.
//
// It prefers the runtime's visible-screen capability and falls back to the
// ordinary bounded read. The preference is what makes the delivery receipt
// possible: an answered prompt stays in scrollback for the life of the pane, so
// a history-inclusive read can never observe it gone, and an answer that landed
// could never be recorded as landed. The fallback is still correct -- it simply
// waits until the agent's own output has pushed the prompt out of the window.
func (c *Coordinator) readVisiblePane(ctx stdctx.Context, handle ports.RuntimeHandle) (string, error) {
	if visible, ok := c.paneReader.(ports.VisiblePaneReader); ok {
		return visible.GetVisibleOutput(ctx, handle, questions.PaneCaptureRangeLines)
	}
	return c.paneReader.GetOutput(ctx, handle, questions.PaneCaptureRangeLines)
}
