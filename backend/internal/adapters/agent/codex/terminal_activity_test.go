package codex

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestDetectTerminalActivity(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   domain.ActivityState
		ok     bool
	}{
		{
			name:   "idle composer",
			output: "\x1b[1m›\x1b[0m \x1b[2mWrite tests for @filename\x1b[0m\n\ngpt-5.6-sol low · ~/project\n",
			want:   domain.ActivityIdle,
			ok:     true,
		},
		{
			// Codex keeps "esc to interrupt" on screen for exactly as long as a
			// turn is in flight, so the composer being visible underneath it
			// means nothing. This is the reading that clears a latched
			// waiting_input on a worker that is still working (wf-57f90ff2).
			name:   "working composer",
			output: "• Working (2m 10s • esc to interrupt)\n› Add tests\n\ngpt-5.6-sol low · ~/project\n",
			want:   domain.ActivityActive,
			ok:     true,
		},
		{
			name:   "approval picker",
			output: "› 1. Approve once\n  2. Deny\nPress enter to confirm or esc to go back\n",
		},
		{
			name:   "assistant text",
			output: "The symbol is:\n›\nnot a composer footer\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := (&Plugin{}).DetectTerminalActivity(tt.output)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("DetectTerminalActivity() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// ContinuouslyDetectTerminalActivity must stay on: it is the only thing that
// makes the observer look at a session already latched in waiting_input, which
// is the state Codex's hook set can never leave on its own.
func TestCodexOptsIntoContinuousTerminalDetection(t *testing.T) {
	if !(&Plugin{}).ContinuouslyDetectTerminalActivity() {
		t.Fatal("Codex must opt into continuous terminal activity detection")
	}
}
