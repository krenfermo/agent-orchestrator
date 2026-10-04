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
	//
	// Checkpoint 8K-B budget-sharing decision (flagged for pass 2, not
	// resolved here): CountPolicyAnsweredWorkflowQuestionsByStep's backing
	// query (queries/workflow_questions.sql) filters strictly
	// WHERE answer_source = 'policy'. ResolveState below reuses this same
	// field/budget for the new auto_resolvable case, which means today
	// PolicyAnsweredCount will NOT include any resolver-answered questions
	// (answer_source=resolver) once pass 2 starts emitting that answer
	// source — the two paths share one budget number in name only until the
	// query is widened. Pass 2 must explicitly choose one of: (a) widen the
	// query to WHERE answer_source IN ('policy','resolver') so both auto
	// paths draw from one true shared budget (recommended: they are the
	// same MaxAutoAnsweredQuestionsPerStep loop-safety concern per that
	// field's doc comment), or (b) split into a second counter/field and a
	// separate policy knob. Do not leave this ambiguous when wiring pass 2.
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

// autoResolvableShapePatterns match questions that *read like* an objective,
// discovery/evidence question about the repo — never a preference or
// business/functional choice — narrow and conservative by design (Checkpoint
// 8K-B, pass 1). This is a shape heuristic only: it judges whether the
// question is phrased as "which of these existing things should I use" or
// "does this already exist", not whether an answer actually exists in the
// repo. Verifying an actual answer is pass 2's resolver's job; a question
// that matches here but turns out unanswerable from repo evidence is the
// resolver's problem to fail/escalate, not the classifier's to predict.
//
// Deliberately does NOT match preference/tradeoff questions like "should the
// retry cooldown be 2s or 8s?" — that asks the model to pick a value, not to
// discover an existing fact, so it correctly falls through to ambiguous.
var autoResolvableShapePatterns = []*regexp.Regexp{
	// "which existing helper/function/module/util(ity) should I use" /
	// "which of these should I use" — a discovery question about what
	// already exists in the codebase, not a preference between options AO
	// has to invent.
	regexp.MustCompile(`(?i)\bwhich\s+(of\s+(these|the)\s+)?(existing\s+)?(helper|function|method|module|util(ity)?|component|library|package|class|hook|service)s?\b.*\b(should|do)\s+i\s+use\b`),
	// "is there already a helper/function/... for X" / "does X already
	// exist" — an existence/discovery question, not a design choice.
	regexp.MustCompile(`(?i)\b(is\s+there\s+(already\s+)?an?|does\s+(this|it|.+)\s+already\s+exist)\b.*\b(helper|function|method|module|util(ity)?|component|library|package|class|hook|service)\b`),
	regexp.MustCompile(`(?i)\bdoes\s+(a|an|this|that)\s+(helper|function|method|module|util(ity)?|component|library|package|class|hook|service)\b.*\balready\s+exist\b`),
	// "which file/pattern already implements/handles X" — discovery of
	// existing code shape, not a preference.
	regexp.MustCompile(`(?i)\bwhich\s+(file|pattern|approach)\s+already\s+(implements|handles)\b`),
}

// Classify is the checkpoint's pure, deterministic classification function:
// no I/O, safe to unit test directly. Evaluation order (first match wins):
//
//  1. empty question text or certainty=unknown -> human_required (no text,
//     no auto path, ever, no exceptions).
//  2. a known fact-backed pattern (push/commit-to-main, which
//     branch/worktree, should-I-merge) -> policy_resolvable.
//  3. a sensitive-risk keyword -> human_required.
//  4. (Checkpoint 8K-B) the question's *shape* reads like an objective
//     discovery/evidence question about the repo (which of these existing
//     things should I use, does X already exist) rather than a preference
//     or business/functional choice -> auto_resolvable. This is a shape
//     heuristic only, not an answerability check: Classify never verifies
//     an answer actually exists in the repo, it only judges phrasing. The
//     Decision Resolver that verifies and answers is pass 2; this pass only
//     produces the classification.
//  5. no confident match -> classification=ambiguous, but the *state* this
//     resolves to is always human_required in 8K-A/8K-B-pass-1 (the
//     resolver dispatch that would act on auto_resolvable lands in pass 2);
//     the classification value itself is preserved for observability
//     rather than being silently rewritten.
//
// The MaxAutoAnsweredQuestionsPerStep budget is enforced by the caller
// (detector.go), not here: Classify reports what the question *is*, the
// budget check is what happens to a policy_resolvable (or, once pass 2
// wires it up, auto_resolvable) question's resulting state, which is a
// distinct concern.
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

	for _, p := range autoResolvableShapePatterns {
		if p.MatchString(questionText) {
			return domain.QuestionClassificationAutoResolvable, "discovery-shaped question about existing repo code, eligible for the Decision Resolver (Checkpoint 8K-B)"
		}
	}

	return domain.QuestionClassificationAmbiguous, "no confident classifier match; escalated to human_required (no auto-resolver dispatch wired in this pass)"
}

// ClassifyUnderAutonomy is Classify plus P3-C's autonomy policy.
//
// It calls Classify first and unchanged, so every refusal that file already
// makes -- no text, sensitive keyword, fact-backed pattern -- is made BEFORE
// any autonomy rule is consulted. Only classification=ambiguous is
// reconsidered, and only under a mode that permits it: the run's frozen policy
// decides whether AO settles a technical, reversible choice itself instead of
// parking the run on it (see autonomy.go).
//
// Under ask_always it is exactly Classify, byte for byte, which is what makes
// the default behaviour of every existing run unchanged.
func ClassifyUnderAutonomy(questionText string, certainty domain.QuestionCertainty, mode domain.QuestionAutonomyMode) (domain.QuestionClassification, string, AutonomyDecision) {
	classification, reason := Classify(questionText, certainty)
	if classification != domain.QuestionClassificationAmbiguous {
		return classification, reason, AutonomyDecision{Mode: mode}
	}
	decision := EvaluateAutonomy(questionText, mode)
	if !decision.AutoDecidable {
		// The classification is unchanged and the run still asks. What is added
		// is WHY, which is what §21 requires a question to carry: AO says what
		// it could not determine rather than forwarding the worker's raw prompt.
		return classification, reason + "; not auto-decided because " + decision.Reason, decision
	}
	// Auto-decidable ambiguity becomes the SAME auto_resolvable classification
	// a discovery-shaped question already gets, so it flows through the existing
	// Decision Resolver -- read-only, cross-provider, evidence-bound, budgeted --
	// rather than through a second answering mechanism nobody reviewed.
	return domain.QuestionClassificationAutoResolvable,
		"auto-decided under autonomy policy " + string(decision.Mode) + ": " + decision.Reason,
		decision
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
	case domain.QuestionClassificationAutoResolvable:
		// Same budget check as policy_resolvable, reused rather than
		// rebuilt (see PolicyAnsweredCount's doc comment above for the
		// budget-sharing caveat pass 2 must resolve). Resolving here only
		// sets the *state*; pass 1 wires no dispatch/spawn logic that acts
		// on QuestionStateResolving — that is pass 2's job.
		if budget.MaxAutoAnswered > 0 && budget.PolicyAnsweredCount >= budget.MaxAutoAnswered {
			return domain.QuestionStateHumanRequired
		}
		return domain.QuestionStateResolving
	case domain.QuestionClassificationHumanRequired, domain.QuestionClassificationAmbiguous:
		return domain.QuestionStateHumanRequired
	default:
		return domain.QuestionStateHumanRequired
	}
}
