package claudecode

import (
	"regexp"
	"strconv"
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

	start, options, ok := claudeOptionBlock(lines)
	if !ok {
		return ports.QuestionCandidate{}, false
	}
	choices := make([]domain.QuestionChoice, 0, len(options))
	for _, opt := range options {
		choices = append(choices, domain.QuestionChoice{ID: opt.number, Label: opt.label})
	}

	questionText := claudeQuestionText(lines, start)
	if questionText == "" {
		return ports.QuestionCandidate{}, false
	}

	return ports.QuestionCandidate{
		QuestionText:      questionText,
		StructuredChoices: choices,
		Certainty:         domain.QuestionCertaintyInferred,
	}, true
}

// claudeOption is one parsed entry of the select list.
type claudeOption struct {
	number string
	label  string
	line   int
}

// maxClaudeOptionDescriptionLines bounds how many non-option lines may sit
// between two consecutive options before the run stops being read as one list.
//
// One, deliberately. Claude Code renders at most a single description line
// under each choice, and allowing more would let two numbered lines separated
// by a paragraph of prose be read as a menu -- which is exactly the false
// positive the ">=2 consecutive options" rule was protecting against.
const maxClaudeOptionDescriptionLines = 1

// claudeOptionBlock finds the trailing select list and returns its options in
// display order, plus the index of its first line.
//
// It replaces a strictly-consecutive scan, which current Claude Code builds
// break: each choice is rendered with an indented description line beneath it,
//
//	❯ 1. pathutil.go
//	     Name the new helper file pathutil.go
//	  2. pathhelpers.go
//	     Name the new helper file pathhelpers.go
//
// so no two numbered lines are ever adjacent and the parser saw no list at all.
// A real worker blocked on a real question therefore produced no durable
// question row, and the whole classify/resolve path downstream never ran. Found
// by the P3-C closing smoke against a live Claude Code worker.
//
// The conservatism the old rule provided is preserved by a stronger signal than
// adjacency: the numbers must DESCEND BY EXACTLY ONE as the scan walks upward,
// so the block is 1..N in order. Two unrelated numbered lines in prose or code
// do not form that sequence, and a stray "3." in a stack trace cannot start one.
func claudeOptionBlock(lines []string) (int, []claudeOption, bool) {
	end := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if claudeOptionLine.MatchString(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return 0, nil, false
	}
	last := claudeOptionLine.FindStringSubmatch(lines[end])
	expected, err := strconv.Atoi(last[1])
	if err != nil {
		return 0, nil, false
	}
	reversed := []claudeOption{{number: last[1], label: strings.TrimSpace(last[2]), line: end}}
	start := end
	gap := 0
	for i := end - 1; i >= 0; i-- {
		m := claudeOptionLine.FindStringSubmatch(lines[i])
		if m == nil {
			gap++
			if gap > maxClaudeOptionDescriptionLines {
				break
			}
			continue
		}
		n, cerr := strconv.Atoi(m[1])
		if cerr != nil || n != expected-1 {
			// Not the previous entry of this list. Whatever it is, the list
			// starts below it.
			break
		}
		expected = n
		gap = 0
		start = i
		reversed = append(reversed, claudeOption{number: m[1], label: strings.TrimSpace(m[2]), line: i})
	}
	if len(reversed) < 2 {
		// A single numbered-looking line is not enough signal; a ternary or an
		// optional-type annotation in a code block can produce one stray match,
		// but never an ordered run of two.
		return 0, nil, false
	}
	if expected != 1 {
		// A list that does not begin at 1 is not a Claude select prompt; it is
		// numbered text that happens to end in a numbered line.
		return 0, nil, false
	}
	options := make([]claudeOption, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		options = append(options, reversed[i])
	}
	return start, options, true
}

func claudeQuestionLines(paneText string) []string {
	plain := claudeTerminalEscape.ReplaceAllString(strings.ReplaceAll(paneText, "\r", "\n"), "")
	raw := strings.Split(plain, "\n")
	// The side panel comes off BEFORE anything is trimmed, because trimming is
	// what destroys the only evidence that there is one: a column. See
	// stripClaudeSidePanel.
	raw = stripClaudeSidePanel(raw)
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

// ---- the side panel -----------------------------------------------------------

// claudeSidePanelEdge is the set of glyphs that can draw the LEFT edge of a
// box Claude renders beside its option list. `─` is deliberately absent: it
// draws horizontal rules that span the whole terminal, and treating one as a
// column boundary would truncate the pane at an arbitrary point.
var claudeSidePanelEdge = map[rune]bool{'┌': true, '│': true, '└': true, '├': true}

// minClaudeSidePanelRows is how many consecutive rows must share one edge
// column before AO will believe there is a panel there.
//
// Two, which is the smallest box that can exist, and consecutive, which is what
// separates a drawn box from two unrelated glyphs that happen to land on the
// same column in different paragraphs.
const minClaudeSidePanelRows = 2

// stripClaudeSidePanel removes the right-hand preview column from the rows it
// actually occupies.
//
// Claude Code 2.1.258 renders a select prompt as TWO columns: the numbered
// options on the left, and a code/preview box drawn to their right, sharing the
// same physical lines. Every consumer here reads a pane line-by-line, so
// without this the right column is part of the left column's text — and both
// halves of P3-D smoke B follow directly from that:
//
//   - the option labels came out as "String concatenation         ┌────────┐",
//     which is what reached the classifier, the resolver and the durable
//     question row; and
//   - the box's own continuation rows counted as output printed AFTER the last
//     option, which pushed the prompt past ParseDialog's freshness window and
//     made the parser report NO DIALOG while the dialog was on the screen. That
//     answer became `dialog_gone`, and `dialog_gone` is read as a delivery
//     receipt. A real, correct, autonomous decision was recorded as handed over
//     to a worker that never saw it.
//
// The strip is scoped to the panel's OWN rows rather than applied to the whole
// pane, and that is not a detail: truncating every line at the panel's column
// would cut the prompt line itself ("How should `farewell(name)` build its
// return value?" is longer than the column) and AO would answer a question it
// had misread. Only rows that demonstrably carry the edge glyph, plus the rows
// enclosed between the top and bottom of that same run, are cut.
//
// Conservative in the direction that matters: an unrecognised layout leaves the
// pane exactly as it was, which the observation model then reports as
// inconclusive rather than as an absence.
func stripClaudeSidePanel(raw []string) []string {
	col, from, to, ok := claudeSidePanelBounds(raw)
	if !ok {
		return raw
	}
	out := make([]string, len(raw))
	copy(out, raw)
	for i := from; i <= to; i++ {
		runes := []rune(out[i])
		if len(runes) > col {
			out[i] = string(runes[:col])
		}
	}
	return out
}

// claudeSidePanelBounds finds the longest consecutive run of rows sharing one
// edge column, and returns that column and the run's bounds.
//
// The "at least one row has content to the left" requirement is what makes it a
// side panel rather than a full-width box: a box drawn on its own, with nothing
// beside it, is ordinary agent output and cutting it would delete real text.
func claudeSidePanelBounds(raw []string) (col, from, to int, ok bool) {
	bestLen := 0
	runStart, runCol, runLen := -1, -1, 0
	flush := func(end int) {
		if runLen >= minClaudeSidePanelRows && runLen > bestLen &&
			claudeRowsHaveTextLeftOf(raw, runStart, end, runCol) {
			col, from, to, bestLen, ok = runCol, runStart, end, runLen, true
		}
		runStart, runCol, runLen = -1, -1, 0
	}
	for i, line := range raw {
		c, has := claudeEdgeColumn(line)
		switch {
		case !has:
			flush(i - 1)
		case c == runCol:
			runLen++
		default:
			flush(i - 1)
			runStart, runCol, runLen = i, c, 1
		}
	}
	flush(len(raw) - 1)
	return col, from, to, ok
}

// claudeEdgeColumn returns the rune column of the first panel edge glyph on a
// line, if the line has one at a column other than the first.
//
// Column 0 is excluded because a panel that starts at the left margin has
// nothing to its left to preserve, so it is not a side panel — and cutting
// there would empty the line entirely.
func claudeEdgeColumn(line string) (int, bool) {
	for i, r := range []rune(line) {
		if claudeSidePanelEdge[r] {
			if i == 0 {
				return 0, false
			}
			return i, true
		}
	}
	return 0, false
}

// claudeRowsHaveTextLeftOf reports whether any row in the run carries real text
// to the left of the edge column.
func claudeRowsHaveTextLeftOf(raw []string, from, to, col int) bool {
	for i := from; i <= to && i < len(raw); i++ {
		runes := []rune(raw[i])
		if len(runes) > col && strings.TrimSpace(string(runes[:col])) != "" {
			return true
		}
	}
	return false
}

// maxQuestionLookbackLines bounds how far above the option block the question
// is looked for. Claude Code renders a question above its choices, wrapped to
// the pane width and sometimes followed by supporting lines; a couple of dozen
// lines covers that generously while keeping the scan from wandering into
// whatever tool output happens to precede the prompt.
const maxQuestionLookbackLines = 24

// claudeQuestionText picks the question out of the lines above the option block.
//
// F7: this used to take the NEAREST non-option line, which is right only when
// the question is one line. Claude Code passes the whole `question` field
// through, and a model that supplies its evidence inside that field renders as
//
//	Which helper convention should the new helper under src/helpers/ follow?
//
//	Evidence found in the repo:
//	• src/helpers/stringutil.js — flat module, individually named exports...
//	• CONTRIBUTING.md states the two conventions are a live migration, that
//	  file counts are not evidence, and that I must ask rather than infer.
//	❯ 1. Flat named exports
//	  2. Default-exported object
//
// so the stored question became that last bullet. The consequences are not
// cosmetic: the captured text is what the autonomy classifier reads, what the
// resolver is asked to answer, and what a person is shown. In the gate it made
// the classifier read "live migration" as a change to persisted data and
// escalate a reversible technical choice to a human.
//
// So the nearest line ending in '?' within a bounded look-back wins, and the
// old nearest-line rule remains the fallback for a question that genuinely has
// no question mark. Only the SELECTION is changed here -- nothing is
// reformatted, joined or truncated, so a one-line question yields exactly the
// text it always did.
func claudeQuestionText(lines []string, start int) string {
	fallback := ""
	for i := start - 1; i >= 0 && i >= start-maxQuestionLookbackLines; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || claudeOptionLine.MatchString(lines[i]) {
			continue
		}
		if fallback == "" {
			fallback = line
		}
		if strings.HasSuffix(line, "?") {
			return line
		}
	}
	return fallback
}
