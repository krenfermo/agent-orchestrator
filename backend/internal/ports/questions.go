package ports

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// QuestionCandidate is what a harness-specific QuestionPaneParser extracts
// from a bounded pane-text window when it recognizes that harness's
// interactive-prompt marker. It is conservative by construction: a parser
// that isn't confident returns ok=false rather than guessing.
type QuestionCandidate struct {
	QuestionText      string
	StructuredChoices []domain.QuestionChoice
	Certainty         domain.QuestionCertainty
}

// QuestionPaneParser looks for one harness's known interactive-prompt
// markers (e.g. Claude Code's cursor-select list, Codex's approval/question
// block) within a bounded recent pane-text window and, only on a confident
// match, returns the reconstructed question text and any structured
// choices. It must never scan for a bare "?" or otherwise invent text: no
// confident marker match means ok=false, full stop.
type QuestionPaneParser interface {
	ParseQuestion(paneText string) (QuestionCandidate, bool)
}
