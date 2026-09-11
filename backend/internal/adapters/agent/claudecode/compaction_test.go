package claudecode

import "testing"

// compaction_test.go -- the one thing the adapter contributes to Checkpoint
// P7: Claude Code's own compaction vocabulary.
//
// A directive is a command LINE. The single-line guarantee is the load-bearing
// property: tmux delivers a prompt with paste-buffer, which appends to the
// composer rather than clearing it, so a newline inside the directive would
// submit its first line as the command and leave the remainder sitting in the
// draft -- which is precisely the loaded-not-submitted shape the rest of this
// checkpoint is built to avoid.
func TestCompactionDirectiveIsAlwaysOneLine(t *testing.T) {
	p := New()
	cases := []struct {
		name  string
		focus string
		want  string
	}{
		{"no focus", "", "/compact"},
		{"blank focus", "   \t  ", "/compact"},
		{"plain focus", "keep the tests", "/compact keep the tests"},
		{"newlines are flattened", "keep the tests\nand the decisions", "/compact keep the tests and the decisions"},
		{"runs of whitespace collapse", "keep   the\t\ttests\n\n", "/compact keep the tests"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := p.CompactionDirective(tc.focus)
			if !ok {
				t.Fatal("claude-code must always report a compaction vocabulary")
			}
			if got != tc.want {
				t.Fatalf("directive = %q, want %q", got, tc.want)
			}
			for _, r := range got {
				if r == '\n' || r == '\r' {
					t.Fatalf("directive %q contains a line break", got)
				}
			}
		})
	}
}

// The focus is a HINT to the harness's own summarizer, never the thing that
// carries the facts across a compaction -- those travel in AO's
// SessionContextPack. So the directive must never be treated as a place to
// stuff context: it stays a command line whatever it is handed.
func TestCompactionDirectiveStaysACommandLine(t *testing.T) {
	p := New()
	got, ok := p.CompactionDirective("a\nb\nc\nd\ne")
	if !ok {
		t.Fatal("expected a directive")
	}
	if got != "/compact a b c d e" {
		t.Fatalf("directive = %q", got)
	}
}
