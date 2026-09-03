package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// provider_dialog.go — P3-C's structured answer to an interactive provider
// prompt.
//
// THE INCIDENT. A worker blocked on Claude Code's select dialog cannot be
// answered by AO at all. The only delivery mechanism is "paste text, then
// press Enter", and an Enter into a select dialog confirms whichever row the
// cursor happens to be on — which is not the option anybody chose. sessionguard
// refuses that write for exactly that reason (ErrAwaitingDecision), and the
// refusal is correct: it is the last thing standing between AO and answering a
// permission dialog on the user's behalf.
//
// The consequence was that a decision could be taken, recorded, justified and
// evidenced — and still never reach the worker. Smoke C proved every link of
// that chain live and then stopped here, with an autonomous decision durable in
// the database and a worker waiting forever for it.
//
// This file is the vocabulary that makes the answer deliverable. It is
// deliberately PROVIDER-NEUTRAL and contains no keys: what a dialog IS
// (a question with options, one of which is selected) is a fact any provider
// can express, while HOW to select option three is a fact only that provider's
// adapter knows. Keeping the second out of here is what stops workflow core
// from growing a table of arrow-key sequences.

// DialogKind is the closed set of interactive prompts AO can reason about.
//
// Closed for the same reason every other vocabulary in AO is: a dialog AO
// cannot name is a dialog AO must not answer. An unrecognised prompt is not
// coerced into the nearest kind — it is refused, and the person is told.
type DialogKind string

const (
	// DialogSelect is a list of numbered options with exactly one cursor.
	// Answering it means moving the cursor onto a specific option and
	// confirming, never typing.
	DialogSelect DialogKind = "select"
	// DialogConfirm is a two-way yes/no prompt. It is a select with a fixed
	// shape, kept separate because the safe answer for an unknown confirm is
	// always "do not answer it".
	DialogConfirm DialogKind = "confirm"
	// DialogFreeText is a genuine text input: the provider is waiting for a
	// line, and typing it is the correct interaction.
	//
	// It is the ONLY kind for which paste-then-Enter is safe, and naming it
	// explicitly is the point. Before this, every prompt was implicitly treated
	// as free text, which is how a select dialog came to be answered by typing
	// into it.
	DialogFreeText DialogKind = "free_text"
)

// Valid reports whether the kind is one this build understands.
func (k DialogKind) Valid() bool {
	switch k {
	case DialogSelect, DialogConfirm, DialogFreeText:
		return true
	default:
		return false
	}
}

// NeedsStructuredResponse reports whether answering this kind requires the
// structured path rather than a text write.
func (k DialogKind) NeedsStructuredResponse() bool {
	return k == DialogSelect || k == DialogConfirm
}

// DialogOption is one choice a provider offered.
type DialogOption struct {
	// ID is the provider's own identifier for the option — for a numbered list,
	// the number as displayed. It is what a durable answer is matched against.
	ID string
	// Label is the option's visible text.
	Label string
	// Index is the option's zero-based position in the list AS DISPLAYED, which
	// is what cursor movement is counted in. It is deliberately separate from
	// ID: a provider may number its options from one, from zero, or not at all.
	Index int
	// Selected marks the option the cursor is currently on.
	Selected bool
}

// ProviderDialog is one interactive prompt, as observed.
//
// Every field is something the observer PROVED from the pane. In particular
// there is no "assumed cursor": a dialog whose selection could not be observed
// carries Selected on no option at all, and the responder refuses to navigate
// from a position it cannot see. Guessing that the cursor starts at the first
// option is precisely the assumption that would confirm the wrong choice.
type ProviderDialog struct {
	Provider  AgentHarness
	SessionID SessionID
	Kind      DialogKind
	Prompt    string
	Options   []DialogOption
	// AllowFreeText reports that the dialog also accepts typed input (Claude's
	// "Type something." row). It never makes a select dialog safe to type into
	// blindly; it only records that the affordance exists.
	AllowFreeText bool
	DetectedAt    time.Time
	// Fingerprint identifies THIS dialog, so a response computed against it
	// cannot be applied to a different one. See DialogFingerprint.
	Fingerprint string
}

// DialogObservationState is what ONE reading of a pane established.
//
// Three values, not two, and the third is the point (P3-D §2). A parser that
// can only say "dialog" or "no dialog" has to report every layout it does not
// understand as an absence — and absence is evidence: it is what proves a
// prompt was answered, and therefore what licenses recording an answer as
// delivered. So a parser that cannot read the screen ends up certifying that
// the thing it could not read is gone.
//
// That is not hypothetical. Claude Code 2.1.258 renders its select prompt in
// two columns; AO's parser did not understand the layout, reported no dialog
// while the dialog was on the screen, and a correct autonomous decision was
// recorded as handed to a worker that never received it (P3-D smoke B).
type DialogObservationState string

const (
	// DialogPresent means a dialog was recognised well enough to act on.
	DialogPresent DialogObservationState = "present"
	// DialogAbsent means the observation SUCCEEDED and showed no dialog. It is
	// the only state that may be read as evidence a prompt was answered.
	DialogAbsent DialogObservationState = "absent"
	// DialogInconclusive means AO could not establish either: an unknown
	// layout, a half-drawn repaint, a truncated capture, a prompt whose options
	// did not parse. It is never an absence, never a delivery receipt, and
	// never a licence to press a key.
	DialogInconclusive DialogObservationState = "inconclusive"
)

// DialogObservation is one reading of one pane.
type DialogObservation struct {
	State DialogObservationState
	// Dialog is populated only for DialogPresent.
	Dialog ProviderDialog
	// Reason says what AO could not establish, for the ledger and for the
	// sentence a person reads. Empty for a clean present/absent.
	Reason string
}

// Present reports a dialog recognised well enough to act on. Named so callers
// branch on a question rather than on a string comparison.
func (o DialogObservation) Present() bool { return o.State == DialogPresent }

// Absent reports an observation that succeeded and showed no dialog. It is the
// only reading that may be used as evidence a prompt was answered.
func (o DialogObservation) Absent() bool { return o.State == DialogAbsent }

// Inconclusive reports an observation that established neither. It is never an
// absence, never a delivery receipt, and never a licence to press a key.
func (o DialogObservation) Inconclusive() bool { return o.State == DialogInconclusive }

// DialogSeen is the constructor for a recognised dialog.
func DialogSeen(d ProviderDialog) DialogObservation {
	return DialogObservation{State: DialogPresent, Dialog: d}
}

// NoDialog is the constructor for a proven absence.
func NoDialog() DialogObservation { return DialogObservation{State: DialogAbsent} }

// DialogUnreadable is the constructor for an observation AO may not act on.
func DialogUnreadable(reason string) DialogObservation {
	return DialogObservation{State: DialogInconclusive, Reason: reason}
}

// SelectedOption returns the option the cursor is on, if the observation
// proved one.
func (d ProviderDialog) SelectedOption() (DialogOption, bool) {
	for _, o := range d.Options {
		if o.Selected {
			return o, true
		}
	}
	return DialogOption{}, false
}

// OptionByID finds an option by the provider's own identifier.
func (d ProviderDialog) OptionByID(id string) (DialogOption, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return DialogOption{}, false
	}
	for _, o := range d.Options {
		if o.ID == id {
			return o, true
		}
	}
	return DialogOption{}, false
}

// OptionByLabel finds an option by its visible text, compared loosely enough to
// survive re-rendering (case and surrounding whitespace) and no more.
//
// It is the FALLBACK match and never the primary one: a label is presentation
// and can be re-worded by a provider update, while the option id is the thing
// the provider itself uses. See MatchOption.
func (d ProviderDialog) OptionByLabel(label string) (DialogOption, bool) {
	want := normalizeDialogText(label)
	if want == "" {
		return DialogOption{}, false
	}
	var found DialogOption
	matches := 0
	for _, o := range d.Options {
		if normalizeDialogText(o.Label) == want {
			found = o
			matches++
		}
	}
	if matches != 1 {
		// Zero is no match; more than one is an ambiguous match, and answering
		// an ambiguous match is choosing for the user.
		return DialogOption{}, false
	}
	return found, true
}

// DialogFingerprint is the stable identity of one observed dialog.
//
// It covers the provider, the session, the kind, the prompt and every option's
// id and label — but NOT which option is selected, and not the timestamp. That
// split is deliberate and is what makes the fingerprint useful: moving the
// cursor while answering must not change the dialog's identity, or AO could
// never verify mid-navigation that it is still answering the same question.
func DialogFingerprint(provider AgentHarness, session SessionID, kind DialogKind, prompt string, options []DialogOption) string {
	var b strings.Builder
	b.WriteString(string(provider))
	b.WriteByte('\x00')
	b.WriteString(string(session))
	b.WriteByte('\x00')
	b.WriteString(string(kind))
	b.WriteByte('\x00')
	b.WriteString(normalizeDialogText(prompt))
	for _, o := range options {
		b.WriteByte('\x00')
		b.WriteString(o.ID)
		b.WriteByte('\x1f')
		b.WriteString(normalizeDialogText(o.Label))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "dlg_" + hex.EncodeToString(sum[:])[:24]
}

// normalizeDialogText collapses the presentation differences a terminal
// re-render can introduce (leading/trailing space, interior runs of whitespace,
// case) and nothing else. It never strips punctuation or words: two options
// that differ by a word are two different options.
func normalizeDialogText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// StructuredProviderResponse is one answer, addressed to one observed dialog.
//
// It is what a durable decision becomes when it is about to be delivered, and
// every field on it is a PRECONDITION rather than a description: the responder
// re-observes the dialog and refuses unless all of them still hold. A response
// that cannot state which dialog it answers cannot be delivered at all.
type StructuredProviderResponse struct {
	// DialogFingerprint is the dialog this response was computed against.
	DialogFingerprint string
	Kind              DialogKind
	// OptionID is the provider's identifier for the chosen option. It is the
	// primary match.
	OptionID string
	// OptionLabel is the chosen option's text at the time of the decision, used
	// only when OptionID cannot be matched. See MatchOption.
	OptionLabel string
	// Text is the answer for a free-text dialog.
	Text string
	// ExpectedProvider and ExpectedSession are the identity this response may
	// be delivered to, and to nothing else.
	ExpectedProvider AgentHarness
	ExpectedSession  SessionID
	// ExpectedGeneration fences the response to the dispatch that produced the
	// question. Zero means the caller had no generation to bind to, which is
	// the pre-existing shape and is not treated as a match-anything wildcard —
	// it simply is not compared.
	ExpectedGeneration int64
	// ExpectedPrompt is the question this answer was decided about, as it was
	// durably recorded. It is the primary staleness check: a prompt on screen
	// that asks something else is a different question, and answering it with
	// this decision is the exact failure §5 forbids.
	ExpectedPrompt string
}

// MatchesExpectation reports whether an observed dialog is still the one a
// response was computed for.
//
// It compares the PROMPT rather than a stored fingerprint because the durable
// answer predates any dialog observation: what AO recorded is the question it
// decided about, and "is the agent still asking that?" is the question worth
// asking. A response carrying no expectation at all is not blocked — that is
// the pre-P3-C shape — but every response this package builds carries one.
func (d ProviderDialog) MatchesExpectation(resp StructuredProviderResponse) DialogResponseRefusal {
	if resp.DialogFingerprint != "" && resp.DialogFingerprint != d.Fingerprint {
		return RefusalDialogChanged
	}
	if want := normalizeDialogText(resp.ExpectedPrompt); want != "" &&
		want != normalizeDialogText(d.Prompt) {
		return RefusalDialogChanged
	}
	return RefusalNone
}

// DialogResponseRefusal is the closed vocabulary of "AO will not deliver this".
//
// Every member is a refusal a person can act on, and none of them is an error
// in the ordinary sense: they are all correct outcomes of asking whether a
// specific answer still applies to a specific prompt.
type DialogResponseRefusal string

const (
	// RefusalNone means the response may be delivered.
	RefusalNone DialogResponseRefusal = ""
	// RefusalDialogGone means the prompt is no longer on screen. It is the
	// normal outcome of a redelivery after a successful one, which is what
	// makes exactly-once achievable across a crash.
	RefusalDialogGone DialogResponseRefusal = "dialog_gone"
	// RefusalDialogChanged means a dialog is present and is not the one this
	// response answers. Answering it would apply an old decision to a new
	// question.
	RefusalDialogChanged DialogResponseRefusal = "dialog_changed"
	// RefusalOptionUnmatched means the chosen option is not among the options
	// now offered, or matches more than one of them.
	RefusalOptionUnmatched DialogResponseRefusal = "option_unmatched"
	// RefusalCursorUnknown means the dialog's current selection could not be
	// observed, so no navigation can be proven to land anywhere.
	RefusalCursorUnknown DialogResponseRefusal = "cursor_unknown"
	// RefusalUnsupportedDialogKind means the provider cannot answer this kind
	// of prompt structurally.
	RefusalUnsupportedDialogKind DialogResponseRefusal = "unsupported_dialog_kind"
	// RefusalProviderUnsupported means the provider declares no structured
	// dialog capability at all.
	RefusalProviderUnsupported DialogResponseRefusal = "provider_unsupported"
	// RefusalSessionMismatch means the response names a different session,
	// provider or generation than the one in front of AO.
	RefusalSessionMismatch DialogResponseRefusal = "session_mismatch"
	// RefusalDialogUnreadable means AO could not establish what is on the
	// screen: an unknown layout, a half-drawn repaint, a truncated capture.
	//
	// It is deliberately NOT RefusalDialogGone, and the whole of P3-D §3 is in
	// that distinction. `dialog_gone` is EVIDENCE — it is what proves an answer
	// was consumed, and it licenses recording a question as delivered. This one
	// is the absence of evidence, and it licenses nothing: no key is pressed, no
	// delivery is recorded, and the answer stays owed until a later observation
	// can say something.
	RefusalDialogUnreadable DialogResponseRefusal = "dialog_unreadable"
)

// Describe renders one refusal as the sentence a person should read.
func (r DialogResponseRefusal) Describe() string {
	switch r {
	case RefusalDialogGone:
		return "the agent is no longer showing this question, so there is nothing to answer"
	case RefusalDialogUnreadable:
		return "AO could not read what the agent is showing, so it did not act on it"
	case RefusalDialogChanged:
		return "the agent is showing a different question now; AO will not answer it with an older decision"
	case RefusalOptionUnmatched:
		return "the chosen option is not among the ones the agent is currently offering"
	case RefusalCursorUnknown:
		return "AO cannot see which option the agent has highlighted, so it cannot prove where a selection would land"
	case RefusalUnsupportedDialogKind:
		return "AO does not know how to answer this kind of prompt structurally"
	case RefusalProviderUnsupported:
		return "this provider does not support answering its prompts through AO"
	case RefusalSessionMismatch:
		return "this answer belongs to a different session, provider or attempt"
	default:
		return ""
	}
}

// MatchOption resolves a response onto one option of a freshly observed dialog.
//
// Order matters and is the §6 requirement: the provider's own option identity
// first, the label only as a fallback, and an unmatchable or ambiguous answer
// refused outright. Matching on label alone would let a provider re-wording its
// options silently redirect a decision to a different one.
func (d ProviderDialog) MatchOption(resp StructuredProviderResponse) (DialogOption, DialogResponseRefusal) {
	if opt, ok := d.OptionByID(resp.OptionID); ok {
		if label := strings.TrimSpace(resp.OptionLabel); label != "" &&
			normalizeDialogText(opt.Label) != normalizeDialogText(label) {
			// The id still exists but now names something else. That is a
			// re-ordered or re-used list, not a match: the decision was about
			// the words, and the words moved.
			return DialogOption{}, RefusalOptionUnmatched
		}
		return opt, RefusalNone
	}
	if opt, ok := d.OptionByLabel(resp.OptionLabel); ok {
		return opt, RefusalNone
	}
	return DialogOption{}, RefusalOptionUnmatched
}

// OptionIndexString renders an option's displayed index, for adapters that
// address options positionally.
func OptionIndexString(o DialogOption) string { return strconv.Itoa(o.Index) }
