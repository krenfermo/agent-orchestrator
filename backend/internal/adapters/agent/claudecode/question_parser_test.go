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
