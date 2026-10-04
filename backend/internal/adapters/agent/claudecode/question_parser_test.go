package claudecode

import "testing"

func TestQuestionParser_ParseQuestion(t *testing.T) {
	cases := []struct {
		name       string
		pane       string
		wantOK     bool
		wantText   string
		wantChoice int
	}{
		{
			name: "real question with choices",
			pane: "Do you want to proceed?\n" +
				"❯ 1. Yes\n" +
				"  2. No, and tell Claude what to do differently\n",
			wantOK:     true,
			wantText:   "Do you want to proceed?",
			wantChoice: 2,
		},
		{
			name:   "normal log line with a question mark",
			pane:   "INFO: did the build finish? checking status...\n",
			wantOK: false,
		},
		{
			name:   "code with optional-type annotation",
			pane:   "func Foo(x *int) {\n  var y string?\n}\n",
			wantOK: false,
		},
		{
			name:   "ternary expression",
			pane:   "const label = isDone ? 'done' : 'pending';\n",
			wantOK: false,
		},
		{
			name: "stack trace",
			pane: "panic: runtime error: index out of range [3] with length 3\n" +
				"goroutine 1 [running]:\n" +
				"main.doWork(...)\n\t/app/main.go:42\n",
			wantOK: false,
		},
		{
			name:   "single numbered line only, not a real prompt block",
			pane:   "See the setup guide.\n1. Install dependencies before continuing\n",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := (QuestionParser{}).ParseQuestion(tc.pane)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got=%+v)", ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			if got.QuestionText != tc.wantText {
				t.Errorf("QuestionText = %q, want %q", got.QuestionText, tc.wantText)
			}
			if len(got.StructuredChoices) != tc.wantChoice {
				t.Errorf("len(StructuredChoices) = %d, want %d", len(got.StructuredChoices), tc.wantChoice)
			}
		})
	}
}

func TestQuestionParser_WaitingBlockedNoParseableBlock(t *testing.T) {
	// A pane that is waiting/blocked per activity state but has no
	// clearly-parseable interactive block must yield ok=false, never
	// invented text.
	pane := "Working...\n\nWaiting for your response.\n"
	_, ok := (QuestionParser{}).ParseQuestion(pane)
	if ok {
		t.Fatalf("expected ok=false for unparseable waiting pane")
	}
}

// TestF7_MultiLineQuestionCapturesTheQuestionNotItsLastBullet is the F7
// regression, built from the pane AO actually captured in the gate.
//
// The model put its evidence inside the AskUserQuestion `question` field, so
// the rendered question is several lines and the last one is a bullet. AO
// stored that bullet, and because the stored text is what the autonomy
// classifier reads, "live migration" was taken as a change to persisted data
// and a reversible technical choice was escalated to a human.
func TestF7_MultiLineQuestionCapturesTheQuestionNotItsLastBullet(t *testing.T) {
	pane := `  ☐ Convention
Which helper convention should the new name-normalization helper under src/helpers/ follow?

Evidence found in the repo:
• src/helpers/stringutil.js — flat module, individually named exports; imported by greeting.js and all 8 feature modules.
• src/helpers/misc_helpers.js — single default-exported object; marked LEGACY; sole consumer is src/legacy/report.js.
• CONTRIBUTING.md states the two conventions are a live migration, that file counts are not evidence, and that I must ask rather than infer.
❯ 1. Flat named exports
     Follow src/helpers/stringutil.js, no default export.
  2. Default-exported object
     Follow src/helpers/misc_helpers.js.
Enter to select · ↑/↓ to navigate · Esc to cancel`

	got, ok := QuestionParser{}.ParseQuestion(pane)
	if !ok {
		t.Fatal("expected a question candidate")
	}
	want := "Which helper convention should the new name-normalization helper under src/helpers/ follow?"
	if got.QuestionText != want {
		t.Errorf("question text = %q,\n want %q", got.QuestionText, want)
	}
	if len(got.StructuredChoices) != 2 {
		t.Fatalf("choices = %d, want 2", len(got.StructuredChoices))
	}
	if got.StructuredChoices[0].Label != "Flat named exports" || got.StructuredChoices[1].Label != "Default-exported object" {
		t.Errorf("choice labels not preserved: %+v", got.StructuredChoices)
	}
}

// A single-line question must keep yielding exactly what it always did.
func TestF7_SingleLineQuestionUnchanged(t *testing.T) {
	pane := `  ☐ Indentation
Should this project use tabs or spaces for indentation?
❯ 1. Spaces
     Indent with spaces.
  2. Tabs
     Indent with tab characters.
Enter to select · ↑/↓ to navigate · Esc to cancel`

	got, ok := QuestionParser{}.ParseQuestion(pane)
	if !ok {
		t.Fatal("expected a question candidate")
	}
	if want := "Should this project use tabs or spaces for indentation?"; got.QuestionText != want {
		t.Errorf("question text = %q, want %q", got.QuestionText, want)
	}
}

// A question with no question mark falls back to the nearest line, which is the
// behaviour that existed before F7.
func TestF7_QuestionWithoutAQuestionMarkFallsBackToNearestLine(t *testing.T) {
	pane := `Pick the option you want AO to take
❯ 1. Retry the build
  2. Stop and report
Enter to select`

	got, ok := QuestionParser{}.ParseQuestion(pane)
	if !ok {
		t.Fatal("expected a question candidate")
	}
	if want := "Pick the option you want AO to take"; got.QuestionText != want {
		t.Errorf("question text = %q, want %q", got.QuestionText, want)
	}
}
