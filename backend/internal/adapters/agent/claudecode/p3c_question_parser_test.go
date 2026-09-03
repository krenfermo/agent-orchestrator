package claudecode

import "testing"

// p3c_question_parser_test.go — P3-C.
//
// Current Claude Code builds render each choice of a select prompt with an
// indented description line beneath it, so no two numbered lines are ever
// adjacent. The parser required them to be consecutive, so it saw no list at
// all: a real worker blocked on a real question produced no durable question
// row, and the whole classify/resolve/answer path downstream never ran.
//
// The pane below is captured verbatim from the live Claude Code worker the
// P3-C closing smoke blocked, whitespace included, because the whitespace is
// the entire defect.
const liveClaudeSelectPane = `⏺ I need to ask you a question before writing anything.
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

func TestParsesALiveSelectPromptWithDescriptionLines(t *testing.T) {
	q, ok := (QuestionParser{}).ParseQuestion(liveClaudeSelectPane)
	if !ok {
		t.Fatal("the live Claude Code select prompt produced no question")
	}
	if q.QuestionText != "What should the new helper file be named?" {
		t.Fatalf("questionText = %q", q.QuestionText)
	}
	if len(q.StructuredChoices) < 3 {
		t.Fatalf("choices = %+v, want at least the three offered options", q.StructuredChoices)
	}
	if q.StructuredChoices[0].Label != "pathutil.go" || q.StructuredChoices[1].Label != "pathhelpers.go" {
		t.Fatalf("the description lines were parsed as choices: %+v", q.StructuredChoices)
	}
}

// The conservatism the old adjacency rule provided is preserved by a stronger
// signal: the options must form 1..N in order. Numbered prose does not.
func TestNumberedProseIsNotASelectPrompt(t *testing.T) {
	for _, tc := range []struct{ name, pane string }{
		{
			name: "two unrelated numbered lines separated by prose",
			pane: "Here is what I found:\n" +
				"7. the retry budget is spent\n" +
				"and separately, in another file,\n" +
				"12. the cooldown is wrong\n",
		},
		{
			name: "a list that does not begin at one",
			pane: "Remaining steps:\n  3. build\n  4. test\n",
		},
		{
			name: "a single numbered line",
			pane: "Which is it?\n  1. only one option\n",
		},
		{
			name: "options separated by more than one line",
			pane: "Pick one:\n" +
				"  1. first\n" +
				"     a description\n" +
				"     a second description line\n" +
				"  2. second\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := (QuestionParser{}).ParseQuestion(tc.pane); ok {
				t.Fatal("numbered text was read as a question a worker is blocked on")
			}
		})
	}
}
