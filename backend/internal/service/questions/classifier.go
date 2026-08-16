// Package questions implements Checkpoint 8K-A: durable capture,
// deterministic classification, and policy-only resolution of questions a
// worker harness asks while paused on the user. No second-LLM resolver, no
// cross-harness answering — see classifier.go's doc comment for the exact
// classification order this checkpoint implements.
package questions

import (
	"regexp"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ClassifyContext is the read-only, already-persisted context the
// classifier and policy resolver need. Every field here is a fact AO
// already stores; the classifier makes no I/O call of its own.
type ClassifyContext struct {
	Branch       string
	WorktreePath string
	// PolicyAnsweredCount is how many prior questions on this step were
	// already answered by the policy resolver (answer_source=policy). Used
	// to enforce WorkflowPolicy.MaxAutoAnsweredQuestionsPerStep.
	PolicyAnsweredCount int
	MaxAutoAnswered     int
}

// sensitiveKeywords are risk markers that force human_required regardless
// of any fact-backed match, per the checkpoint's conservative bias: an
// operational/destructive/security-adjacent question is never
// auto-resolved even if it superficially resembles a known pattern.
var sensitiveKeywords = []string{
	"deploy", "production", "delete", "drop", "credential", "secret",
	"force-push", "force push", "security",
}

// pushToMainPattern matches "should I push/commit directly to main/master"
// style questions — a fact-backed pattern AO can answer from the step's
// checkpoint Branch field (it already knows what branch the step is on).
var pushToMainPattern = regexp.MustCompile(`(?i)\b(push|commit)\b.*\b(directly\s+to\s+|to\s+)?(main|master)\b`)

// whichBranchPattern matches "which branch/worktree am I on" style
// questions — answerable purely from WorkflowCheckpoint.Branch/WorktreePath.
var whichBranchPattern = regexp.MustCompile(`(?i)\bwhich\s+(branch|worktree)\b|\bwhat\s+branch\b|\bam\s+i\s+on\b`)

// shouldMergePattern matches "should I merge" style questions — answerable
// from the step's stored review/merge policy fields.
var shouldMergePattern = regexp.MustCompile(`(?i)\bshould\s+i\s+merge\b|\bmerge\s+this\s+(pr|pull request|branch)\b`)

// Classify is the checkpoint's pure, deterministic classification function:
// no I/O, safe to unit test directly. Evaluation order (first match wins):
//
//  1. empty question text or certainty=unknown -> human_required (no text,
//     no auto path, ever, no exceptions).
//  2. a known fact-backed pattern (push/commit-to-main, which
//     branch/worktree, should-I-merge) -> policy_resolvable.
//  3. a sensitive-risk keyword -> human_required.
//  4. no confident match -> classification=ambiguous, but the *state* this
//     resolves to is always human_required in 8K-A (no auto-resolver to
//     escalate to yet); the classification value itself is preserved for
//     observability rather than being silently rewritten.
//
// The MaxAutoAnsweredQuestionsPerStep budget is enforced by the caller
// (detector.go), not here: Classify reports what the question *is*, the
// budget check is what happens to a policy_resolvable question's resulting
// state, which is a distinct concern.
func Classify(questionText string, certainty domain.QuestionCertainty) (domain.QuestionClassification, string) {
	if strings.TrimSpace(questionText) == "" || certainty == domain.QuestionCertaintyUnknown {
		return domain.QuestionClassificationHumanRequired, "question text could not be reconstructed reliably"
	}

	if pushToMainPattern.MatchString(questionText) {
		return domain.QuestionClassificationPolicyResolvable, "fact-backed: push/commit-directly-to-main question, answerable from the step's checkpoint branch"
	}
	if whichBranchPattern.MatchString(questionText) {
		return domain.QuestionClassificationPolicyResolvable, "fact-backed: which branch/worktree question, answerable from the step's checkpoint branch/worktree path"
	}
	if shouldMergePattern.MatchString(questionText) {
		return domain.QuestionClassificationPolicyResolvable, "fact-backed: should-I-merge question, answerable from stored review/merge policy"
	}

	lower := strings.ToLower(questionText)
	for _, kw := range sensitiveKeywords {
		if strings.Contains(lower, kw) {
			return domain.QuestionClassificationHumanRequired, "sensitive-risk keyword detected: " + kw
		}
	}

	return domain.QuestionClassificationAmbiguous, "no confident classifier match; escalated to human_required (no auto-resolver in this checkpoint)"
}

// ResolveState maps a classification (plus the budget check) to the
// question's initial persisted state. classification=ambiguous always
// forces state=human_required even though the classification value itself
// stays ambiguous (see Classify's doc comment).
func ResolveState(classification domain.QuestionClassification, budget ClassifyContext) domain.QuestionState {
	switch classification {
	case domain.QuestionClassificationPolicyResolvable:
		if budget.MaxAutoAnswered > 0 && budget.PolicyAnsweredCount >= budget.MaxAutoAnswered {
			return domain.QuestionStateHumanRequired
		}
		return domain.QuestionStatePending
	case domain.QuestionClassificationHumanRequired, domain.QuestionClassificationAmbiguous:
		return domain.QuestionStateHumanRequired
	default: // auto_resolvable is not emitted by 8K-A's classifier
		return domain.QuestionStateHumanRequired
	}
}
