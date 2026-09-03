package claudecode

import (
	"regexp"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// dialog.go — Claude Code's structured reading of, and answer to, its own
// select prompt.
//
// This is the ONLY place in AO that knows what Claude's cursor glyph looks like
// or how many times to press Down. Both facts are provider UI, they change when
// the provider changes, and the whole point of the port boundary is that they
// change here and nowhere else.

// DialogParser implements ports.DialogPaneParser for Claude Code.
type DialogParser struct{}

var _ ports.DialogPaneParser = DialogParser{}

// ObserveDialog reads the trailing select prompt, including WHICH row the
// cursor is on, and says which of the three things it established.
//
// The cursor is the part QuestionParser does not need and this one cannot do
// without: answering means moving a cursor, and a cursor whose starting
// position is unknown makes every movement a guess. When the glyph is not
// visible the dialog is still PRESENT, with no option marked Selected, and the
// responder refuses it — reporting "AO cannot see which option is highlighted"
// is a far better outcome than pressing Down twice from an imagined position,
// and a far better one than reporting an empty screen (P3-D §6).
//
// The three answers, and what separates them:
//
//   - PRESENT: an option list, a prompt heading above it, and nothing printed
//     after it that would make it scrollback rather than the live screen;
//   - INCONCLUSIVE: the screen is still showing a select prompt's own furniture
//     — its key hints, or a cursor row — and the list itself did not parse.
//     That is a half-drawn repaint, an unknown layout, or a truncated capture,
//     and none of them is an absence;
//   - ABSENT: none of that furniture is on the screen at all. This is what a
//     pane looks like after a prompt is answered — Claude redraws and the
//     prompt is simply gone — and it is the only reading that may be used as
//     evidence a delivery landed.
func (DialogParser) ObserveDialog(paneText string) domain.DialogObservation {
	lines := claudeQuestionLines(paneText)
	// Resolved once, up front, because every failure branch below needs the
	// same question answered: is a prompt still being drawn here at all?
	live := claudeSelectFurniture(lines)
	inconclusive := func(reason string) domain.DialogObservation {
		if !live {
			return domain.NoDialog()
		}
		return domain.DialogUnreadable(reason)
	}
	if len(lines) == 0 {
		// An empty capture proves nothing whatsoever. It is not a screen with
		// no prompt on it; it is no screen.
		return domain.DialogUnreadable("the agent's pane could not be read, or came back empty")
	}
	start, parsed, ok := claudeOptionBlock(lines)
	if !ok {
		return inconclusive("a select prompt's key hints are on screen and its option list did not parse")
	}
	// The block must be part of the CURRENT screen, not something still sitting
	// in scrollback.
	//
	// This matters for exactly one thing, and it is the receipt: after a prompt
	// is answered the agent carries on writing, but a bounded pane capture
	// includes history, so the answered prompt is still findable. A parser that
	// reported it would make "is the dialog gone?" permanently false, and a
	// delivery could never prove it landed. Requiring the options to be near the
	// tail is what distinguishes "being asked now" from "was asked earlier".
	//
	// The allowance is what a real Claude prompt renders after its last option:
	// a separator and a key-hints line, plus slack.
	if last := len(lines) - 1; last-lastOptionLine(parsed) > maxClaudeTrailingLines {
		return inconclusive("an option list is on screen with unexpected output printed after it")
	}
	prompt := ""
	for i := start - 1; i >= 0; i-- {
		if claudeOptionLine.MatchString(lines[i]) {
			continue
		}
		prompt = strings.TrimSpace(lines[i])
		break
	}
	if prompt == "" {
		return inconclusive("an option list is on screen with no readable prompt above it")
	}

	options := make([]domain.DialogOption, 0, len(parsed))
	allowFreeText := false
	for i, p := range parsed {
		label := p.label
		if label == "" {
			return inconclusive("an option on screen has no readable label")
		}
		if isClaudeFreeTextOption(label) {
			allowFreeText = true
		}
		options = append(options, domain.DialogOption{
			ID:       p.number,
			Label:    label,
			Index:    i,
			Selected: claudeCursorLine.MatchString(lines[p.line]),
		})
	}

	kind := domain.DialogSelect
	if isClaudeConfirmShape(options) {
		kind = domain.DialogConfirm
	}
	return domain.DialogSeen(domain.ProviderDialog{
		Provider:      domain.HarnessClaudeCode,
		Kind:          kind,
		Prompt:        prompt,
		Options:       options,
		AllowFreeText: allowFreeText,
		Fingerprint:   domain.DialogFingerprint(domain.HarnessClaudeCode, "", kind, prompt, options),
	})
}

// claudeSelectFurniture reports whether the pane is still showing the parts of
// a select prompt that only exist while one is open.
//
// It is what turns a parse failure into "AO could not read this" rather than
// "there is nothing there", and it has to be specific enough that an answered
// prompt does not keep triggering it forever — which would make a delivery
// impossible to confirm and break P3-C's exactly-once receipt.
//
// Two signals, both live-only. The key-hints footer is drawn by the prompt
// itself and disappears with it. The cursor row `❯ N.` is the highlight glyph,
// which only the open list carries; ordinary transcript text (Claude's own
// `❯ some text` echo) does not match, because the number and period are
// required. Neither survives the redraw that follows an answer, which the
// answered-pane fixture demonstrates.
func claudeSelectFurniture(lines []string) bool {
	for _, line := range lines {
		if claudeCursorLine.MatchString(line) {
			return true
		}
		if claudeSelectHints.MatchString(line) {
			return true
		}
	}
	return false
}

// claudeSelectHints matches the key-hints footer a live select prompt draws.
var claudeSelectHints = regexp.MustCompile(`(?i)(enter to select|esc to cancel|↑/↓ to navigate)`)

// claudeCursorLine matches an option row that carries the selection glyph.
// Only the highlighted row has it, which is what makes it a cursor rather than
// a bullet.
var claudeCursorLine = regexp.MustCompile(`^❯\s*\d+\.\s`)

// isClaudeFreeTextOption recognises the "Type something." affordance Claude
// offers alongside its choices. It records that free text is possible; it never
// makes typing into the dialog the way to choose a listed option.
func isClaudeFreeTextOption(label string) bool {
	l := strings.ToLower(strings.TrimSpace(label))
	return strings.HasPrefix(l, "type something") || strings.HasPrefix(l, "type your own")
}

// isClaudeConfirmShape reports the two-option yes/no prompt.
//
// It is recognised only when BOTH options are unambiguous yes/no words. A
// two-option list of anything else is an ordinary select, and treating it as a
// confirm would attach yes/no semantics to choices that have none.
func isClaudeConfirmShape(options []domain.DialogOption) bool {
	if len(options) != 2 {
		return false
	}
	yes := strings.ToLower(strings.TrimSpace(options[0].Label))
	no := strings.ToLower(strings.TrimSpace(options[1].Label))
	return (yes == "yes" || strings.HasPrefix(yes, "yes,")) &&
		(no == "no" || strings.HasPrefix(no, "no,"))
}

// DialogResponder implements ports.DialogResponder for Claude Code.
type DialogResponder struct{}

var _ ports.DialogResponder = DialogResponder{}

// SupportsDialogKind reports what this adapter can answer.
//
// Free text is deliberately NOT claimed here. A genuine free-text prompt is
// answered by the ordinary message path, which already types and submits
// correctly; routing it through the key vocabulary would be a second way to do
// something that already works.
func (DialogResponder) SupportsDialogKind(kind domain.DialogKind) bool {
	return kind == domain.DialogSelect || kind == domain.DialogConfirm
}

// PlanDialogResponse builds the key sequence that selects exactly one option.
//
// The plan is CURSOR-RELATIVE FROM AN OBSERVED POSITION, and then re-observes
// before confirming. Both halves are deliberate:
//
//   - Relative movement from a position AO has actually read is provable.
//     Pressing the option's digit would be shorter, and this adapter does not do
//     it, because AO has not demonstrated that Claude's list accepts direct
//     numeric selection — and a shortcut that silently does nothing would leave
//     the cursor where it started and confirm the wrong row. If that primitive
//     is ever proven against the real TUI, it belongs here, behind the same
//     plan interface, and nothing outside this file changes.
//   - The Observe step before Enter is what makes the confirm safe rather than
//     hopeful. Terminal rendering is asynchronous: the keys are accepted
//     immediately, the redraw is not. Confirming without re-reading is exactly
//     the "Enter might select a different option" hazard this whole capability
//     exists to remove.
//
// A dialog with no visible cursor is refused rather than navigated from an
// assumed origin.
func (DialogResponder) PlanDialogResponse(
	dialog domain.ProviderDialog, resp domain.StructuredProviderResponse,
) (ports.DialogResponsePlan, domain.DialogResponseRefusal) {
	if !(DialogResponder{}).SupportsDialogKind(dialog.Kind) {
		return ports.DialogResponsePlan{}, domain.RefusalUnsupportedDialogKind
	}
	target, refusal := dialog.MatchOption(resp)
	if refusal != domain.RefusalNone {
		return ports.DialogResponsePlan{}, refusal
	}
	current, ok := dialog.SelectedOption()
	if !ok {
		return ports.DialogResponsePlan{}, domain.RefusalCursorUnknown
	}

	actions := make([]ports.DialogAction, 0, 8)
	step := ports.KeyDown
	distance := target.Index - current.Index
	if distance < 0 {
		step = ports.KeyUp
		distance = -distance
	}
	for i := 0; i < distance; i++ {
		actions = append(actions, ports.DialogAction{Kind: ports.DialogActionKey, Key: step})
	}
	// Re-read before confirming, and say which row the confirm is for. The
	// caller enforces it; naming it here is what makes the expectation part of
	// the plan rather than a convention the caller has to remember.
	actions = append(actions,
		ports.DialogAction{Kind: ports.DialogActionObserve, ExpectOptionID: target.ID},
		ports.DialogAction{Kind: ports.DialogActionKey, Key: ports.KeyEnter},
	)

	return ports.DialogResponsePlan{
		Actions:           actions,
		TargetOptionID:    target.ID,
		TargetOptionLabel: target.Label,
	}, domain.RefusalNone
}

// maxClaudeTrailingLines is how much non-option output may follow the last
// option and still leave the prompt readable as the current one.
//
// Claude renders a separator and a hints line under its list; the slack above
// that absorbs a redraw artefact without absorbing an agent's next paragraph.
const maxClaudeTrailingLines = 6

// lastOptionLine is the line index of the block's final option row.
func lastOptionLine(options []claudeOption) int {
	if len(options) == 0 {
		return 0
	}
	return options[len(options)-1].line
}
