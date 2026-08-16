package codex

import (
	"regexp"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// codexOptionLine matches one entry of Codex's approval/question prompt
// block, e.g. "› 1. Yes" or "  2. No, provide feedback". The cursor glyph
// (›) is Codex-specific and distinct from Claude Code's ❯ marker, so the
// two harnesses' parsers never cross-match each other's fixtures.
var codexOptionLine = regexp.MustCompile(`^(?:›\s*)?(\d+)\.\s+(.+)$`)

// QuestionParser implements ports.QuestionPaneParser for Codex's
// approval/question prompt block: >=2 consecutive numbered option lines
// near the end of the bounded pane window. Never a bare "?" scan.
type QuestionParser struct{}

var _ ports.QuestionPaneParser = QuestionParser{}

// ParseQuestion implements ports.QuestionPaneParser.
func (QuestionParser) ParseQuestion(paneText string) (ports.QuestionCandidate, bool) {
	lines := terminalLines(paneText)
	if len(lines) == 0 {
		return ports.QuestionCandidate{}, false
	}
	const maxWindow = 120
	if len(lines) > maxWindow {
		lines = lines[len(lines)-maxWindow:]
	}

	end := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if codexOptionLine.MatchString(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return ports.QuestionCandidate{}, false
	}
	start := end
	for start > 0 && codexOptionLine.MatchString(lines[start-1]) {
		start--
	}
	if end-start+1 < 2 {
		return ports.QuestionCandidate{}, false
	}

	var choices []domain.QuestionChoice
	for i := start; i <= end; i++ {
		m := codexOptionLine.FindStringSubmatch(lines[i])
		choices = append(choices, domain.QuestionChoice{ID: m[1], Label: strings.TrimSpace(m[2])})
	}

	questionText := ""
	for i := start - 1; i >= 0; i-- {
		if codexOptionLine.MatchString(lines[i]) {
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
