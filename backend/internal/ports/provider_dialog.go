package ports

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// provider_dialog.go — the three narrow capabilities structured dialog
// answering needs, and the boundary between them.
//
// The boundary is the whole design. Three different things know three different
// facts, and none of them should learn the others':
//
//	DialogPaneParser   what is on the screen        (provider adapter)
//	DialogResponder    how to answer it             (provider adapter)
//	RuntimeKeySender   how to press a key           (runtime adapter)
//
// Workflow core knows only that a dialog exists, which options it offers, and
// which one was chosen. It never names a key. That is not tidiness: an arrow
// sequence hardcoded in core would be a provider's private UI contract written
// somewhere it can never be tested against the real thing, and it would be
// applied to every future provider that never agreed to it.

// ErrDialogResponseUnsupported reports that a provider cannot answer this
// dialog structurally. It is a refusal, not a fault: the caller reports it and
// hands the question to a person.
var ErrDialogResponseUnsupported = errors.New("provider: structured dialog response is not supported")

// DialogPaneParser is a provider's structured reading of its own interactive
// prompt.
//
// It is deliberately separate from QuestionPaneParser rather than a widening of
// it. That one answers "is a question being asked, and what is its text" — the
// input to classification. This one answers "what exactly is on the screen right
// now, including which row the cursor is on" — the input to ANSWERING, which is
// re-read immediately before every delivery and must therefore be cheap, total
// and free of interpretation.
//
// It answers with THREE states, not two (P3-D §2/§11). "Present" and "absent"
// are both claims about the screen; the third, "inconclusive", is the honest
// admission that this reading established neither.
//
// The distinction is load-bearing, and collapsing it is a real incident rather
// than a theoretical one. Absence is EVIDENCE here: it is what proves a prompt
// was answered, and therefore what licenses recording an answer as delivered.
// A two-valued parser has to report every layout it does not understand as an
// absence, so it certifies that the thing it could not read is gone. Claude
// Code 2.1.258's two-column select did exactly that, and a correct autonomous
// decision was filed as delivered to a worker that never saw it.
//
// Present must never be an approximation either: a prompt whose options cannot
// be read exactly is not a dialog to answer, because answering it would mean
// guessing where a keystroke lands. A dialog whose CURSOR alone is unreadable
// is still present -- the responder refuses it on its own terms, which is a
// better answer than pretending the screen is blank.
type DialogPaneParser interface {
	ObserveDialog(paneText string) domain.DialogObservation
}

// DialogActionKind is the closed set of interaction primitives a provider may
// ask a runtime to perform.
//
// Closed, and small, on purpose. Every member is something a terminal can do
// unambiguously to any application; nothing here encodes an application's
// semantics. A provider that needs a primitive not on this list cannot express
// it, which is the correct outcome — a new primitive is a deliberate change to
// this file, reviewed once, rather than an arbitrary string a plugin can smuggle
// into a pane.
type DialogActionKind string

const (
	// DialogActionKey presses one named key (see DialogKey).
	DialogActionKey DialogActionKind = "key"
	// DialogActionText types literal text. Legal only for a free-text dialog;
	// the responder is what refuses to emit it for a select.
	DialogActionText DialogActionKind = "text"
	// DialogActionObserve re-reads the pane and re-checks that the plan is
	// still landing where it was computed to land.
	//
	// It exists as a first-class step rather than as something the caller does
	// around the plan, because WHERE it belongs is a provider fact: only the
	// provider knows that its cursor has finished moving and that the confirm
	// key is the next thing it will interpret. A plan that ends in a confirm
	// without an observation before it is a plan that presses Enter hoping.
	DialogActionObserve DialogActionKind = "observe"
)

// DialogKey is the closed vocabulary of key names a provider may request.
//
// Runtime adapters map these onto their own encoding (tmux key names, ANSI
// escapes). A key outside this set is refused by the runtime rather than passed
// through, so no provider can inject a raw control sequence into a pane.
type DialogKey string

// The key vocabulary. Deliberately the minimum a cursor-select prompt needs.
const (
	KeyUp     DialogKey = "up"
	KeyDown   DialogKey = "down"
	KeyEnter  DialogKey = "enter"
	KeyEscape DialogKey = "escape"
	// KeyDigit0..KeyDigit9 are direct numeric selection, for providers that
	// prove their prompt accepts it. They are named individually rather than
	// parameterised so the vocabulary stays a closed set of constants.
	KeyDigit1 DialogKey = "1"
	KeyDigit2 DialogKey = "2"
	KeyDigit3 DialogKey = "3"
	KeyDigit4 DialogKey = "4"
	KeyDigit5 DialogKey = "5"
	KeyDigit6 DialogKey = "6"
	KeyDigit7 DialogKey = "7"
	KeyDigit8 DialogKey = "8"
	KeyDigit9 DialogKey = "9"
)

// Valid reports whether a key is in the vocabulary.
func (k DialogKey) Valid() bool {
	switch k {
	case KeyUp, KeyDown, KeyEnter, KeyEscape,
		KeyDigit1, KeyDigit2, KeyDigit3, KeyDigit4, KeyDigit5,
		KeyDigit6, KeyDigit7, KeyDigit8, KeyDigit9:
		return true
	default:
		return false
	}
}

// DialogAction is one step of a provider's answer plan.
type DialogAction struct {
	Kind DialogActionKind
	Key  DialogKey
	Text string
	// ExpectOptionID is set on an Observe step: the option the cursor must be
	// on for the plan to continue. It is what turns "press Enter and hope" into
	// "press Enter on a row AO has just re-read".
	ExpectOptionID string
}

// DialogResponsePlan is a provider's complete answer to one dialog, expressed
// as primitives and nothing else.
//
// Producing it is PURE: no pane is read, no key is pressed, nothing is written.
// That is what lets the whole plan be asserted in a unit test without a
// terminal, which for a capability whose failure mode is "confirmed the wrong
// option" is not a convenience but the point.
type DialogResponsePlan struct {
	// Actions are performed in order.
	Actions []DialogAction
	// TargetOptionID is the option the plan intends to select, carried so a
	// caller can report and verify the intent independently of the steps.
	TargetOptionID string
	// TargetOptionLabel is that option's text at planning time.
	TargetOptionLabel string
}

// DialogResponder is a provider's ability to answer its own prompts.
//
// PlanDialogResponse is given a freshly observed dialog and the response to
// apply, and returns the primitives that will apply it — or a refusal. It never
// touches a runtime.
type DialogResponder interface {
	// SupportsDialogKind reports whether this provider can answer that kind of
	// prompt structurally. A provider that returns false for everything is
	// simply one AO cannot answer for, and callers report that honestly rather
	// than falling back to typing.
	SupportsDialogKind(kind domain.DialogKind) bool
	// PlanDialogResponse computes the primitives. A non-empty refusal means no
	// plan; the two are never both meaningful.
	PlanDialogResponse(dialog domain.ProviderDialog, resp domain.StructuredProviderResponse) (DialogResponsePlan, domain.DialogResponseRefusal)
}

// RuntimeKeySender is the runtime's ability to press named keys in a pane.
//
// It is separate from AgentMessenger.Send on purpose and permanently. Send
// means "deliver a message to the agent" and carries a trailing Enter, which is
// exactly what must never happen to a select dialog. This means "press these
// keys", nothing more, and it is the only door through which a dialog response
// reaches a terminal.
//
// Implementations MUST reject a key outside the DialogKey vocabulary rather
// than forwarding it.
type RuntimeKeySender interface {
	SendKeys(ctx context.Context, handle RuntimeHandle, keys []DialogKey) error
}

// VisiblePaneReader reads what is CURRENTLY ON SCREEN, with no scrollback.
//
// It exists for one question, and that question is the delivery receipt: "is
// the agent still showing this prompt?". A bounded capture that includes
// history answers a different question — "was this prompt shown recently" —
// and the difference is the whole receipt. An answered prompt stays in
// scrollback forever, so a receipt built on history could never observe the
// dialog gone, and a delivered answer could never be recorded as delivered.
//
// Optional: a runtime without it falls back to the ordinary bounded read, which
// is correct as soon as the agent writes enough to push the prompt out of the
// window, and merely slower to conclude before then.
type VisiblePaneReader interface {
	GetVisibleOutput(ctx context.Context, handle RuntimeHandle, maxLines int) (string, error)
}
