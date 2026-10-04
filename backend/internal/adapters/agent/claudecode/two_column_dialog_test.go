package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

// P3-D: the two-column select that made AO report "no prompt on screen" while a
// real Claude Code worker sat on one.
//
// The fixture is a verbatim capture from claude-code 2.1.258 during the P3-D
// smoke B run: a 220-column terminal, the option list in a left column, and a
// code-preview box drawn to its right starting at column 34. Nothing about it is
// simplified — the whole failure lives in the columns, so a tidied-up version
// would test a layout that never occurred.
func loadPane(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

const twoColumnFixture = "select_two_column_2_1_258.txt"

// The dialog is on the screen. Anything other than "present" is a lie, and
// "absent" is the specific lie that consumed an answer nobody received.
func TestTwoColumnSelectIsSeen(t *testing.T) {
	obs := (DialogParser{}).ObserveDialog(loadPane(t, twoColumnFixture))
	if !obs.Present() {
		t.Fatalf("the parser reported %q while a real select prompt was on the screen (%s)",
			obs.State, obs.Reason)
	}
	dialog := obs.Dialog
	if dialog.Prompt != "How should `farewell(name)` build its return value?" {
		t.Fatalf("prompt = %q", dialog.Prompt)
	}
	if len(dialog.Options) != 2 {
		t.Fatalf("options = %d, want 2: %+v", len(dialog.Options), dialog.Options)
	}
	// The labels are the whole point: the preview column must not be in them.
	if got := dialog.Options[0].Label; got != "String concatenation" {
		t.Fatalf("option 1 label = %q, want %q", got, "String concatenation")
	}
	if got := dialog.Options[1].Label; got != "Template literal" {
		t.Fatalf("option 2 label = %q, want %q", got, "Template literal")
	}
	if !dialog.Options[0].Selected || dialog.Options[1].Selected {
		t.Fatalf("cursor is on the wrong row: %+v", dialog.Options)
	}
}

// The same layout, read by the question parser that captures the durable row.
// It produced the contaminated labels that reached the resolver.
func TestTwoColumnSelectCapturesCleanChoices(t *testing.T) {
	got, ok := (QuestionParser{}).ParseQuestion(loadPane(t, twoColumnFixture))
	if !ok {
		t.Fatal("the question parser saw no question in a real select prompt")
	}
	if len(got.StructuredChoices) != 2 {
		t.Fatalf("choices = %+v", got.StructuredChoices)
	}
	for i, want := range []string{"String concatenation", "Template literal"} {
		if got.StructuredChoices[i].Label != want {
			t.Fatalf("choice %d label = %q, want %q", i, got.StructuredChoices[i].Label, want)
		}
	}
}

// ---- the three observation states -------------------------------------------

// A prompt that was genuinely answered: the TUI redraws and nothing of it is
// left. This must stay ABSENT — it is the receipt P3-C's exactly-once delivery
// stands on, and fixing the false "gone" must not cost the true one.
func TestAnAnsweredPromptIsAProvenAbsence(t *testing.T) {
	obs := (DialogParser{}).ObserveDialog(loadPane(t, "select_answered_and_gone_2_1_258.txt"))
	if !obs.Absent() {
		t.Fatalf("state = %q (%s), want absent: an answered prompt must be provable as gone",
			obs.State, obs.Reason)
	}
}

// A pane caught mid-repaint: the heading and prompt are drawn, the option list
// is not finished, the preview box is half there. AO has established nothing.
func TestAPartialRepaintIsInconclusiveNotGone(t *testing.T) {
	obs := (DialogParser{}).ObserveDialog(loadPane(t, "select_partial_repaint_2_1_258.txt"))
	if !obs.Inconclusive() {
		t.Fatalf("state = %q, want inconclusive: a half-drawn prompt is not an absence", obs.State)
	}
	if obs.Reason == "" {
		t.Fatal("an inconclusive observation must say what could not be established")
	}
}

// The same prompt on a narrow terminal, where Claude has no room for the
// preview column and renders one. The fix must not depend on the two-column
// layout being there.
func TestTheSamePromptIsReadOnANarrowTerminal(t *testing.T) {
	obs := (DialogParser{}).ObserveDialog(loadPane(t, "select_single_column_narrow.txt"))
	if !obs.Present() {
		t.Fatalf("state = %q (%s), want present", obs.State, obs.Reason)
	}
	if got := obs.Dialog.Options[0].Label; got != "String concatenation" {
		t.Fatalf("option 1 label = %q", got)
	}
	if got := obs.Dialog.Options[1].Label; got != "Template literal" {
		t.Fatalf("option 2 label = %q", got)
	}
}

// An empty capture is not an empty screen.
func TestAnEmptyCaptureIsInconclusive(t *testing.T) {
	if obs := (DialogParser{}).ObserveDialog(""); !obs.Inconclusive() {
		t.Fatalf("state = %q, want inconclusive for a capture that returned nothing", obs.State)
	}
}

// Ordinary agent output, with no prompt furniture anywhere, is a real absence.
// Without this the parser would report "unreadable" for every quiet screen and
// no delivery could ever be confirmed.
func TestOrdinaryOutputIsAbsent(t *testing.T) {
	pane := "⏺ Done. I changed greet.js and ran the tests.\n\n" +
		"  What changed: greet.js — added farewell(name).\n"
	if obs := (DialogParser{}).ObserveDialog(pane); !obs.Absent() {
		t.Fatalf("state = %q (%s), want absent", obs.State, obs.Reason)
	}
}

// A DIFFERENT prompt is present, not absent. The caller compares fingerprints;
// what the parser owes it is the truth that something is being asked.
func TestADifferentPromptIsStillPresent(t *testing.T) {
	pane := "Which file should I edit?\n❯ 1. greet.js\n  2. verify.js\n\nEnter to select · Esc to cancel\n"
	obs := (DialogParser{}).ObserveDialog(pane)
	if !obs.Present() {
		t.Fatalf("state = %q, want present", obs.State)
	}
	if obs.Dialog.Prompt != "Which file should I edit?" {
		t.Fatalf("prompt = %q", obs.Dialog.Prompt)
	}
}

// The cursor is the one thing whose absence does NOT downgrade the reading:
// present-with-unknown-cursor is a fact the responder refuses on its own terms,
// and reporting an empty screen instead would be a worse answer (P3-D §6).
func TestAnUnreadableCursorLeavesTheDialogPresent(t *testing.T) {
	pane := "How should farewell build its value?\n  1. String concatenation\n  2. Template literal\n\n" +
		"Enter to select · Esc to cancel\n"
	obs := (DialogParser{}).ObserveDialog(pane)
	if !obs.Present() {
		t.Fatalf("state = %q, want present", obs.State)
	}
	if _, has := obs.Dialog.SelectedOption(); has {
		t.Fatal("a cursor was invented for a prompt that shows none")
	}
}
