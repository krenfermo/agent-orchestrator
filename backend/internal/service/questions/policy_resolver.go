package questions

import (
	"fmt"
	"strings"
)

// ResolvePolicyAnswer computes the literal answer_text for exactly the
// fact-backed patterns Classify recognizes as policy_resolvable. No LLM
// call, no network call — it only reads already-stored facts (the step's
// checkpoint Branch/WorktreePath). Intentionally tiny in this checkpoint:
// only the 3 patterns Classify matches have an answer here.
//
// Returns ok=false if the caller passed a classification/text combination
// this resolver doesn't have a fact-backed answer for — the caller must
// treat that as a bug (a policy_resolvable question with no computable
// answer should never have been classified that way) rather than silently
// falling back to a guess.
func ResolvePolicyAnswer(questionText string, ctx ClassifyContext) (string, bool) {
	switch {
	case pushToMainPattern.MatchString(questionText):
		branch := ctx.Branch
		if branch == "" {
			branch = "(unknown)"
		}
		return fmt.Sprintf("No — do not push or commit directly to %s. This step's work belongs on branch %q; push there instead.", mainOrMaster(questionText), branch), true

	case whichBranchPattern.MatchString(questionText):
		branch := ctx.Branch
		if branch == "" {
			branch = "(unknown)"
		}
		worktree := ctx.WorktreePath
		if worktree == "" {
			worktree = "(unknown)"
		}
		return fmt.Sprintf("You are on branch %q in worktree %q.", branch, worktree), true

	case shouldMergePattern.MatchString(questionText):
		return "No — do not merge yet. Wait for review/CI to complete and for an explicit go-ahead through the normal workflow steps.", true
	}

	return "", false
}

func mainOrMaster(text string) string {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "master") {
		return "master"
	}
	return "main"
}
