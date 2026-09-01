package domain

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// workflow_objective.go — how long a Task's specification may be (P2-E B3).
//
// Until P2-E there was no limit ANYWHERE: the DB says only `length(objective) >
// 0`, the DTO is a bare string, and the create path trims and stores it. The
// thing that actually stopped people pasting a specification was the UI, which
// used a single-line <input> — so the constraint was not a policy anybody
// chose, it was a widget, and the failure mode was silent: a browser drops the
// newlines when you paste multi-line text into a single-line input, so a
// carefully structured brief arrived as one run-on paragraph and nothing said
// so.
//
// Both halves of that are wrong, and this file fixes the second. A Task is one
// task, but "one task" can legitimately need several thousand words of
// specification — scope, constraints, acceptance criteria, a test matrix, an
// explicit NOT-doing list — and needing that should not force the objective
// through a planner by picking Autonomous or Master.
//
// The number is anchored on an existing AO contract rather than invented:
// internal/adapters/agent/kimchi's systemPromptMaxBytes is 128 KiB, which is
// already what AO considers a prompt-sized payload. A Task specification is
// the same kind of thing, so it gets the same ceiling.
//
// BYTES, not runes, and stated as such everywhere it surfaces. A limit counted
// in characters means something different for a Spanish specification than for
// an English one, and the resources it actually protects — the request body,
// the column, the provider payload — are all counted in bytes.

// MaxWorkflowObjectiveBytes bounds a workflow objective / Task specification.
//
// It exists to stop abuse and unbounded rows, not to shape how anybody writes.
// It is deliberately far above any real specification: 128 KiB is roughly
// twenty thousand words.
const MaxWorkflowObjectiveBytes = 128 * 1024

// ErrObjectiveTooLong is the sentinel a caller can test for without matching
// on message text.
var ErrObjectiveTooLong = errors.New("workflow objective exceeds the maximum length")

// ErrObjectiveEmpty is the other half: a run has to be about something.
var ErrObjectiveEmpty = errors.New("workflow objective is empty")

// ValidateWorkflowObjective checks a Task specification and returns it
// unchanged when it fits.
//
// It NEVER truncates. Silently shortening somebody's specification would mean
// the agent works from a brief the author never wrote and cannot see, which is
// strictly worse than refusing — so an over-long objective is an error that
// names both sizes, and the author decides what to cut.
//
// Only surrounding whitespace is trimmed, exactly as the create path already
// did. Interior newlines, blank lines and markdown are content and survive
// untouched.
func ValidateWorkflowObjective(objective string) (string, error) {
	trimmed := strings.TrimSpace(objective)
	if trimmed == "" {
		return "", ErrObjectiveEmpty
	}
	if n := len(trimmed); n > MaxWorkflowObjectiveBytes {
		return "", fmt.Errorf("%w: %d bytes, maximum %d",
			ErrObjectiveTooLong, n, MaxWorkflowObjectiveBytes)
	}
	if !utf8.ValidString(trimmed) {
		return "", errors.New("workflow objective is not valid UTF-8")
	}
	return trimmed, nil
}

// ObjectiveTooLongMessage renders the human-facing refusal, in the words the
// UI and the CLI both show.
//
// It reports the size in bytes because that is what the limit is in, and says
// so, rather than leaving somebody to wonder why a 40,000-character brief was
// refused at a "128 KiB" limit.
func ObjectiveTooLongMessage(objective string) string {
	return fmt.Sprintf(
		"La instrucción de la tarea supera el máximo permitido: %d bytes UTF-8, máximo %d (%d KiB).",
		len(strings.TrimSpace(objective)), MaxWorkflowObjectiveBytes, MaxWorkflowObjectiveBytes/1024)
}
