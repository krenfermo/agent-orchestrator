package claudecode

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// p3c_dialog_test.go — the plan is asserted WITHOUT a terminal.
//
// That separation is the point rather than a convenience. This capability's
// failure mode is "confirmed an option nobody chose", and a plan that can only
// be checked by running a real TUI is one nobody checks. Every keystroke below
// is derived from the real pane the incident produced.

// The pane is the live capture from the P3-C smoke, whitespace included,
// because the whitespace is what broke the parser the first time.
const livePane = `⏺ I need to ask you a question before writing anything.
────────────────────────────────────────────────────────────
 ☐ File name
What should the new helper file be named?
❯ 1. pathutil.go
     Name the new helper file pathutil.go
  2. pathhelpers.go
     Name the new helper file pathhelpers.go
  3. Type something.
────────────────────────────────────────────────────────────
  4. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel`

func parseLive(t *testing.T) domain.ProviderDialog {
	t.Helper()
	obs := (DialogParser{}).ObserveDialog(livePane)
	if !obs.Present() {
		t.Fatalf("the live Claude select prompt produced no dialog: %s/%s", obs.State, obs.Reason)
	}
	return obs.Dialog
}

func TestParsesTheLiveDialogIncludingItsCursor(t *testing.T) {
	d := parseLive(t)
	if d.Kind != domain.DialogSelect {
		t.Fatalf("kind = %q, want select", d.Kind)
	}
	if d.Prompt != "What should the new helper file be named?" {
		t.Fatalf("prompt = %q", d.Prompt)
	}
	if len(d.Options) != 4 {
		t.Fatalf("%d options, want 4: %+v", len(d.Options), d.Options)
	}
	sel, ok := d.SelectedOption()
	if !ok {
		t.Fatal("the cursor was not observed, so no navigation could ever be proven")
	}
	if sel.ID != "1" || sel.Index != 0 {
		t.Fatalf("cursor on %+v, want option 1 at index 0", sel)
	}
	if !d.AllowFreeText {
		t.Fatal(`the "Type something." affordance was not recorded`)
	}
	if d.Fingerprint == "" {
		t.Fatal("the dialog has no identity, so no response could be bound to it")
	}
}

// The headline: choosing option 2 moves the cursor exactly one row and
// re-reads before confirming.
func TestPlanSelectsTheIntendedOptionAndVerifiesBeforeConfirming(t *testing.T) {
	d := parseLive(t)
	plan, refusal := (DialogResponder{}).PlanDialogResponse(d, domain.StructuredProviderResponse{
		DialogFingerprint: d.Fingerprint, Kind: domain.DialogSelect,
		OptionID: "2", OptionLabel: "pathhelpers.go",
	})
	if refusal != domain.RefusalNone {
		t.Fatalf("refusal = %q, want a plan", refusal)
	}
	if plan.TargetOptionID != "2" || plan.TargetOptionLabel != "pathhelpers.go" {
		t.Fatalf("plan targets %+v", plan)
	}
	want := []ports.DialogAction{
		{Kind: ports.DialogActionKey, Key: ports.KeyDown},
		{Kind: ports.DialogActionObserve, ExpectOptionID: "2"},
		{Kind: ports.DialogActionKey, Key: ports.KeyEnter},
	}
	if len(plan.Actions) != len(want) {
		t.Fatalf("plan = %+v, want %+v", plan.Actions, want)
	}
	for i := range want {
		if plan.Actions[i] != want[i] {
			t.Fatalf("step %d = %+v, want %+v", i, plan.Actions[i], want[i])
		}
	}
}

// Selecting the row the cursor is already on presses no movement key at all,
// and still re-reads before confirming.
func TestPlanForTheAlreadySelectedOptionMovesNothing(t *testing.T) {
	d := parseLive(t)
	plan, refusal := (DialogResponder{}).PlanDialogResponse(d, domain.StructuredProviderResponse{
		OptionID: "1", OptionLabel: "pathutil.go",
	})
	if refusal != domain.RefusalNone {
		t.Fatalf("refusal = %q", refusal)
	}
	if len(plan.Actions) != 2 ||
		plan.Actions[0].Kind != ports.DialogActionObserve ||
		plan.Actions[1].Key != ports.KeyEnter {
		t.Fatalf("plan = %+v, want observe then Enter", plan.Actions)
	}
}

// Moving upward is the same rule in the other direction.
func TestPlanMovesUpwardWhenTheTargetIsAboveTheCursor(t *testing.T) {
	d := parseLive(t)
	for i := range d.Options {
		d.Options[i].Selected = d.Options[i].ID == "3"
	}
	plan, refusal := (DialogResponder{}).PlanDialogResponse(d, domain.StructuredProviderResponse{
		OptionID: "1", OptionLabel: "pathutil.go",
	})
	if refusal != domain.RefusalNone {
		t.Fatalf("refusal = %q", refusal)
	}
	ups := 0
	for _, a := range plan.Actions {
		if a.Key == ports.KeyUp {
			ups++
		}
		if a.Key == ports.KeyDown {
			t.Fatalf("the plan moved down to reach a row above the cursor: %+v", plan.Actions)
		}
	}
	if ups != 2 {
		t.Fatalf("%d Up presses, want 2", ups)
	}
}

// A dialog whose cursor AO cannot see is refused, never navigated from an
// assumed origin. This is the single most important refusal in the file: a
// wrong origin confirms a wrong option, silently.
func TestADialogWithNoVisibleCursorIsRefused(t *testing.T) {
	d := parseLive(t)
	for i := range d.Options {
		d.Options[i].Selected = false
	}
	_, refusal := (DialogResponder{}).PlanDialogResponse(d, domain.StructuredProviderResponse{
		OptionID: "2", OptionLabel: "pathhelpers.go",
	})
	if refusal != domain.RefusalCursorUnknown {
		t.Fatalf("refusal = %q, want cursor_unknown", refusal)
	}
}

// §6: the option must be matched unambiguously, and the id is only a match
// while it still names the same words.
func TestOptionMatchingFailsClosed(t *testing.T) {
	d := parseLive(t)
	for _, tc := range []struct {
		name string
		resp domain.StructuredProviderResponse
	}{
		{"an id that is not offered", domain.StructuredProviderResponse{OptionID: "9", OptionLabel: "nothing"}},
		{"a label that is not offered", domain.StructuredProviderResponse{OptionLabel: "somethingelse.go"}},
		{"an id whose words changed under it", domain.StructuredProviderResponse{OptionID: "2", OptionLabel: "a different option entirely"}},
		{"no identity at all", domain.StructuredProviderResponse{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, refusal := (DialogResponder{}).PlanDialogResponse(d, tc.resp); refusal != domain.RefusalOptionUnmatched {
				t.Fatalf("refusal = %q, want option_unmatched", refusal)
			}
		})
	}
}

// The label is a fallback, and only when it is unambiguous.
func TestLabelMatchesWhenTheIdIsGone(t *testing.T) {
	d := parseLive(t)
	plan, refusal := (DialogResponder{}).PlanDialogResponse(d, domain.StructuredProviderResponse{
		OptionID: "", OptionLabel: "  PathHelpers.go ",
	})
	if refusal != domain.RefusalNone {
		t.Fatalf("refusal = %q, want the label fallback to match", refusal)
	}
	if plan.TargetOptionID != "2" {
		t.Fatalf("matched %q, want option 2", plan.TargetOptionID)
	}
}

// A yes/no prompt is recognised as a confirm, and answered by the same
// mechanism rather than by typing "yes".
func TestConfirmDialogIsRecognisedAndAnswerable(t *testing.T) {
	pane := "Do you want to proceed?\n❯ 1. Yes\n  2. No, and tell Claude what to do differently\n"
	obs := (DialogParser{}).ObserveDialog(pane)
	if !obs.Present() {
		t.Fatalf("the confirm prompt produced no dialog: %s/%s", obs.State, obs.Reason)
	}
	d := obs.Dialog
	if d.Kind != domain.DialogConfirm {
		t.Fatalf("kind = %q, want confirm", d.Kind)
	}
	plan, refusal := (DialogResponder{}).PlanDialogResponse(d, domain.StructuredProviderResponse{
		OptionID: "2", OptionLabel: "No, and tell Claude what to do differently",
	})
	if refusal != domain.RefusalNone {
		t.Fatalf("refusal = %q", refusal)
	}
	if plan.TargetOptionID != "2" {
		t.Fatalf("plan targets %q", plan.TargetOptionID)
	}
}

// The plan never types into a select. Typing is what the whole capability
// exists to stop.
func TestAPlanForASelectNeverTypesText(t *testing.T) {
	d := parseLive(t)
	plan, _ := (DialogResponder{}).PlanDialogResponse(d, domain.StructuredProviderResponse{
		OptionID: "2", OptionLabel: "pathhelpers.go",
	})
	for _, a := range plan.Actions {
		if a.Kind == ports.DialogActionText {
			t.Fatalf("the plan types into a select dialog: %+v", plan.Actions)
		}
		if a.Kind == ports.DialogActionKey && !a.Key.Valid() {
			t.Fatalf("the plan requests a key outside the vocabulary: %q", a.Key)
		}
	}
}

// Free text is not claimed by this adapter: the ordinary message path already
// types and submits correctly, and a second way to do it would be a second way
// to get it wrong.
func TestFreeTextIsNotClaimedByTheStructuredResponder(t *testing.T) {
	if (DialogResponder{}).SupportsDialogKind(domain.DialogFreeText) {
		t.Fatal("the structured responder claims free text")
	}
}
