package codex

import (
	"regexp"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

var codexTerminalEscape = regexp.MustCompile(`\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\][^\x07]*(?:\x07|\x1b\\))`)

// ContinuouslyDetectTerminalActivity opts Codex into terminal reconciliation on
// every observer tick rather than only when a stale `active` reading needs
// refreshing.
//
// Codex needs it because of its own hook set. codexManagedHooks installs
// SessionStart, UserPromptSubmit, PermissionRequest and Stop — and nothing
// else. There is no permission-resolved event and no pre/post-tool-use pair, so
// a PermissionRequest-derived waiting_input has no hook that can clear it
// before the turn's Stop. It is latched for the rest of the turn while the
// agent goes on running commands (incident wf-57f90ff2: a Codex worker
// executing tests read as "awaiting input" for the whole turn, and AO stopped
// the run on it).
//
// The pane is the only signal that can resolve that latch, and Codex publishes
// an unambiguous one — see DetectTerminalActivity. Without continuous
// detection the observer never looks: its non-continuous branch only re-reads
// sessions already in `active`, which is exactly the state a latched session is
// not in.
func (p *Plugin) ContinuouslyDetectTerminalActivity() bool { return true }

// DetectTerminalActivity reads the two states Codex's TUI states outright:
// active while a turn is in flight, idle when the composer and footer are
// visible. Anything else is unknown, never a guess.
//
// "esc to interrupt" is Codex's own in-flight marker: it is on screen for
// exactly as long as the agent is working and gone the moment it is not. This
// used to answer "unknown" for it — discarding the one authoritative liveness
// fact the pane carries — which is what let a stale waiting_input survive an
// entire working turn.
func (p *Plugin) DetectTerminalActivity(output string) (domain.ActivityState, bool) {
	lines := terminalLines(output)
	if len(lines) < 2 {
		return "", false
	}
	start := len(lines) - 12
	if start < 0 {
		start = 0
	}
	for _, line := range lines[start:] {
		if strings.Contains(strings.ToLower(line), "esc to interrupt") {
			return domain.ActivityActive, true
		}
	}
	for i := len(lines) - 2; i >= start; i-- {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "›") {
			continue
		}
		if strings.Contains(lines[i+1], " · ") {
			return domain.ActivityIdle, true
		}
	}
	return "", false
}

func terminalLines(output string) []string {
	plain := codexTerminalEscape.ReplaceAllString(strings.ReplaceAll(output, "\r", "\n"), "")
	raw := strings.Split(plain, "\n")
	lines := raw[:0]
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
