package usage

import (
	"sort"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// turn_class.go -- deriving what a provider call DID from the transcript the
// usage pipeline is already tailing.
//
// The whole derivation reads two fields of a content block: its `type` and,
// when that type is a tool call, the tool's `name`. It never reads `input`,
// never reads message text, and never reads a result. That is not an
// incidental restraint -- it is the property that lets AO answer "how many of
// those 193 calls were coordination" without ever holding a command, a path or
// a prompt.
//
// WHY A SET AND NOT A SWITCH. One billed message arrives as several transcript
// records sharing a message id: on the worked example, 109 of 193 messages had
// their thinking block in the first record and the tool call in a later one.
// Classifying from whichever record the parser happened to emit on would have
// called 56% of that run's calls "message". So the classes a message's blocks
// imply are accumulated as a SET across every record carrying its id -- carried
// in durable parser state so a batch boundary cannot split a message -- and the
// class is derived from the set each time it grows.

// toolTurnClass maps a tool name to what invoking it DOES.
//
// Names are matched exactly and case-sensitively: a harness renaming a tool
// yields an unrecognised name, which contributes no class, which leaves the
// turn classified by whatever else it did (or unclassified). Guessing from a
// substring would be worse -- "BashOutput" contains "Bash" and means the
// opposite thing.
var toolTurnClass = map[string]domain.TurnClass{
	// Mutation.
	"Edit":                        domain.TurnEdit,
	"MultiEdit":                   domain.TurnEdit,
	"Write":                       domain.TurnEdit,
	"NotebookEdit":                domain.TurnEdit,
	"str_replace_editor":          domain.TurnEdit,
	"str_replace_based_edit_tool": domain.TurnEdit,

	// Running something.
	"Bash": domain.TurnCommand,

	// Reading and searching.
	"Read":         domain.TurnRead,
	"Glob":         domain.TurnRead,
	"Grep":         domain.TurnRead,
	"NotebookRead": domain.TurnRead,
	"LS":           domain.TurnRead,
	"WebFetch":     domain.TurnRead,
	"WebSearch":    domain.TurnRead,

	// Checking on work already started. The polling shape: the model paid a
	// whole context re-read to ask whether something it had already launched
	// had finished.
	"BashOutput": domain.TurnWait,
	"KillBash":   domain.TurnWait,
	"KillShell":  domain.TurnWait,

	// Delegation into a separate context.
	"Task":  domain.TurnSubagent,
	"Agent": domain.TurnSubagent,

	// Plan / task state.
	"TodoWrite":     domain.TurnPlan,
	"ExitPlanMode":  domain.TurnPlan,
	"EnterPlanMode": domain.TurnPlan,
}

// turnClassOfTool returns the class a tool name implies, and whether the name
// was recognised at all.
func turnClassOfTool(name string) (domain.TurnClass, bool) {
	c, ok := toolTurnClass[name]
	return c, ok
}

// maxTurnClassesPerMessage bounds the accumulated set. It is the size of the
// closed vocabulary's tool-bearing half; a message cannot legitimately imply
// more classes than there are classes, and the bound exists so a malformed
// transcript cannot grow durable parser state without limit.
const maxTurnClassesPerMessage = 8

// accumulateTurnClasses folds one record's content blocks into the set of
// classes seen so far for its message, returning the new set. The input set is
// not mutated.
//
// A `thinking` block contributes nothing: reasoning accompanies whatever the
// turn did, and letting it vote would make every deliberated tool call
// "mixed". A `text` block contributes nothing either, for the same reason --
// what makes a turn a TurnMessage is the ABSENCE of any tool class, which
// classFromTurnClasses decides once the whole message has been seen.
func accumulateTurnClasses(seen []string, blocks []claudeContentBlock) []string {
	out := seen
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		class, ok := turnClassOfTool(b.Name)
		if !ok {
			continue
		}
		if containsString(out, string(class)) {
			continue
		}
		if len(out) >= maxTurnClassesPerMessage {
			continue
		}
		out = appendUniqueString(out, string(class))
	}
	if len(out) > 1 {
		// Sorted so the same message always produces the same durable state
		// regardless of the order its records arrived in -- a parser state
		// that differed by arrival order would make a re-read produce a
		// different encoding of the same fact.
		sorted := make([]string, len(out))
		copy(sorted, out)
		sort.Strings(sorted)
		out = sorted
	}
	return out
}

// classFromTurnClasses reduces an accumulated set to the message's class.
//
// Empty means the message invoked no tool AO recognises. That is reported as
// TurnMessage rather than TurnUnclassified on purpose: AO did read the
// message's blocks and did find no tool in them, which is a fact about the
// turn and not an absence of information. TurnUnclassified is reserved for
// calls AO never got to look at -- a Codex rollout, a malformed record, a row
// written before migration 0169.
func classFromTurnClasses(seen []string) domain.TurnClass {
	switch len(seen) {
	case 0:
		return domain.TurnMessage
	case 1:
		if c := domain.TurnClass(seen[0]); c.Valid() {
			return c
		}
		return domain.TurnMessage
	default:
		return domain.TurnMixed
	}
}
