package claudecode

import (
	"regexp"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var claudeTerminalEscape = regexp.MustCompile(`\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\][^\x07]*(?:\x07|\x1b\\))`)

// claudeOptionLine matches one entry of Claude Code's interactive
// cursor-select list, e.g. "❯ 1. Yes" or "  2. No, and tell Claude what to
// do differently". The cursor glyph is optional since only the highlighted
// row carries it.
var claudeOptionLine = regexp.MustCompile(`^(?:❯\s*)?(\d+)\.\s+(.+)$`)

// QuestionParser implements ports.QuestionPaneParser for Claude Code's
// numbered cursor-select prompt. It looks only for that specific marker
// shape (>=2 consecutive numbered option lines near the end of the bounded
// pane window) — never a bare "?" scan — and returns ok=false on anything
// else, including code containing "?" or a stack trace.
type QuestionParser struct{}

var _ ports.QuestionPaneParser = QuestionParser{}

// ParseQuestion implements ports.QuestionPaneParser.
func (QuestionParser) ParseQuestion(paneText string) (ports.QuestionCandidate, bool) {
	lines := claudeQuestionLines(paneText)
	if len(lines) == 0 {
		return ports.QuestionCandidate{}, false
	}

	// Find the last contiguous run of >=2 numbered option lines.
	end := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if claudeOptionLine.MatchString(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return ports.QuestionCandidate{}, false
	}
	start := end
	for start > 0 && claudeOptionLine.MatchString(lines[start-1]) {
		start--
	}
	if end-start+1 < 2 {
		// A single numbered-looking line is not enough signal; a ternary
		// or an optional-type annotation in a code block can produce one
		// stray match, but never two consecutive ones in this shape.
		return ports.QuestionCandidate{}, false
	}

	var choices []domain.QuestionChoice
	for i := start; i <= end; i++ {
		m := claudeOptionLine.FindStringSubmatch(lines[i])
		choices = append(choices, domain.QuestionChoice{ID: m[1], Label: strings.TrimSpace(m[2])})
	}

	// The question text is the nearest non-empty line above the option
	// block that doesn't itself look like an option row.
	questionText := ""
	for i := start - 1; i >= 0; i-- {
		if claudeOptionLine.MatchString(lines[i]) {
			continue
		}
		questionText = strings.TrimSpace(lines[i])
		break
	}
	if questionText == "" {
		return ports.QuestionCandidate{}, false
	}

	return ports.QuestionCandidate{
		QuestionText:      questionText,
		StructuredChoices: choices,
		Certainty:         domain.QuestionCertaintyInferred,
	}, true
}

func claudeQuestionLines(paneText string) []string {
	plain := claudeTerminalEscape.ReplaceAllString(strings.ReplaceAll(paneText, "\r", "\n"), "")
	raw := strings.Split(plain, "\n")
	lines := raw[:0]
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	// Bound to the most recent window; detection only ever looks at a
	// recent slice of the pane, never the full transcript.
	const maxWindow = 120
	if len(lines) > maxWindow {
		lines = lines[len(lines)-maxWindow:]
	}
	return lines
}
