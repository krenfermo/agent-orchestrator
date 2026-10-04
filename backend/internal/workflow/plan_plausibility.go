package workflow

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// plan_plausibility.go — F2's semantic floor on a plan AO is about to execute.
//
// NormalizeAndValidatePlan answers "is this plan well formed and safe": known
// dependencies, no cycles, no unsafe verification command, no path outside the
// workspace. Every one of those questions had the answer "yes" for the plan
// that caused F2:
//
//	{"version":"v1","summary":"test","steps":[{"id":"s1","title":"t",
//	  "description":"d","acceptanceCriteria":["a"],
//	  "verify":{"commands":[{"command":"npm","args":["test"]}]}}]}
//
// Schema-valid is not execution-meaningful. A worker handed a task titled "t"
// with acceptance criterion "a" cannot do anything useful with it, and under
// Automatic approval nothing stood between that plan and a dispatched provider
// process.
//
// WHAT THIS IS NOT. It is not a quality bar and it must never become one. It
// does not require a minimum number of steps — a one-step plan is exactly right
// for a one-step objective, and §14's own test matrix demands that stay true.
// It does not require verbosity, does not look for particular words, and does
// not inspect any natural language. Every threshold below is counted in RUNES,
// so a plan written in Spanish, Chinese or Arabic passes or fails identically
// to the same plan in English.
//
// What it rejects is content that carries no information at all: the single
// characters and one-word stubs a model emits when it is filling a schema
// rather than answering. The floors sit far below anything a real planner
// produces (the nine clean observations of the F2 invocation had 28-60 rune
// titles, 200+ rune descriptions and 40-120 rune criteria) and far above the
// failure (1 rune each), so the margin is roughly an order of magnitude on
// both sides.
const (
	// minSummaryRunes rejects "test" (4) and accepts "two steps" (9).
	minSummaryRunes = 6
	// minStepTitleRunes rejects "t" (1) and accepts "Model" (5).
	minStepTitleRunes = 3
	// minStepDescriptionRunes rejects "d" (1) and accepts a terse but real
	// sentence like "Add the user model." (19).
	minStepDescriptionRunes = 12
	// minCriterionRunes rejects "a" (1) and accepts "Model validates names."
	// (22). A criterion is the thing a reviewer judges the work against, so an
	// empty one is worse than none.
	minCriterionRunes = 8
)

// PlanResultPlausibility reports why a structurally valid plan is not a
// plausible answer to objective, or "" when it is.
//
// A non-empty reason means the provider's real result was lost somewhere
// between its invocation and this plan, which is a retryable fact about one
// attempt — NOT a policy violation by the objective. Callers route it to the
// bounded planner retry, not to the terminal invalid-plan path, because the
// thing most likely to recover a lost plan is asking for it again.
func PlanResultPlausibility(plan MasterPlan, objective string) string {
	if runeLen(plan.Summary) < minSummaryRunes {
		return fmt.Sprintf("the plan's summary carries no information (%q); the planner's real result was not what AO received",
			truncateForReason(plan.Summary))
	}
	for i, step := range plan.Steps {
		where := fmt.Sprintf("step %d (%q)", i+1, truncateForReason(step.ID))
		if runeLen(step.Title) < minStepTitleRunes {
			return fmt.Sprintf("%s has a placeholder title (%q)", where, truncateForReason(step.Title))
		}
		if runeLen(step.Description) < minStepDescriptionRunes {
			return fmt.Sprintf("%s has a placeholder description (%q)", where, truncateForReason(step.Description))
		}
		if len(step.AcceptanceCriteria) == 0 {
			return fmt.Sprintf("%s has no acceptance criteria", where)
		}
		for j, criterion := range step.AcceptanceCriteria {
			if runeLen(criterion) < minCriterionRunes {
				return fmt.Sprintf("%s acceptance criterion %d is a placeholder (%q); a reviewer cannot judge work against it",
					where, j+1, truncateForReason(criterion))
			}
		}
	}
	// An objective with real substance decomposed into a plan whose entire
	// content is shorter than the objective's own first line has not been
	// decomposed at all. This is the one whole-plan check, and it is scaled by
	// the objective rather than fixed, so a small objective is never asked for
	// more than it contains.
	if body := planContentRunes(plan); body > 0 {
		if floor := objectiveFloorRunes(objective); body < floor {
			return fmt.Sprintf("the whole plan carries %d characters of content for a %d-character objective; it does not describe the work asked for",
				body, runeLen(objective))
		}
	}
	return ""
}

// objectiveFloorRunes is the least plan content an objective of this size can
// plausibly decompose into: a twentieth of it, and never more than a short
// paragraph however large the objective grows. Deliberately generous — this
// exists to catch a plan that is empty relative to a substantial objective, not
// to insist a plan restate one.
func objectiveFloorRunes(objective string) int {
	floor := runeLen(objective) / 20
	if floor > 120 {
		floor = 120
	}
	return floor
}

// planContentRunes counts the characters a worker and a reviewer actually read:
// titles, descriptions and acceptance criteria. Ids, dependency names and
// verification commands are structure, not content, and a plan padded with them
// would not be any more executable.
func planContentRunes(plan MasterPlan) int {
	total := runeLen(plan.Summary)
	for _, step := range plan.Steps {
		total += runeLen(step.Title) + runeLen(step.Description)
		for _, criterion := range step.AcceptanceCriteria {
			total += runeLen(criterion)
		}
	}
	return total
}

func runeLen(s string) int { return utf8.RuneCountInString(strings.TrimSpace(s)) }

// truncateForReason bounds what a refusal quotes back. A placeholder is short
// by definition, so this only ever trims a value that turned out not to be one.
func truncateForReason(s string) string {
	s = strings.TrimSpace(s)
	const limit = 40
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + "…"
}
