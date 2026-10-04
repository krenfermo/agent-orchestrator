package codex

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
			name: "real approval question with choices",
			pane: "Allow Codex to run `rm -rf tmp`?\n" +
				"› 1. Yes\n" +
				"  2. Yes, and don't ask again\n" +
				"  3. No, provide feedback\n",
			wantOK:     true,
			wantText:   "Allow Codex to run `rm -rf tmp`?",
			wantChoice: 3,
		},
		{
			name:   "normal log line with a question mark",
			pane:   "esc to interrupt · did the tests pass?\n",
			wantOK: false,
		},
		{
			name:   "code with ternary",
			pane:   "result := ok ? \"pass\" : \"fail\"\n",
			wantOK: false,
		},
		{
			name: "stack trace",
			pane: "Traceback (most recent call last):\n" +
				"  File \"main.py\", line 10, in <module>\n" +
				"KeyError: 'missing'\n",
			wantOK: false,
		},
		{
			name:   "waiting pane with no parseable block",
			pane:   "› Codex is thinking...\n\nesc to interrupt\n",
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
