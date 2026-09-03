package questions

import (
	"regexp"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// autonomy.go — P3-C §20/§21: the risk rules that decide whether an AMBIGUOUS
// question may be settled by AO instead of by a person.
//
// Everything here runs strictly AFTER Classify has already refused the classes
// that are never auto-decidable. That ordering is the safety property and it is
// not negotiable: the sensitive-keyword refusal, the "no text, no auto path"
// refusal and every fact-backed pattern are evaluated first, so no autonomy
// mode below can reach a destructive, credential-touching or security-adjacent
// question. What these rules govern is only the ambiguous middle — the
// questions AO today parks on and that a person would answer with a shrug.
//
// The two axes, from the checkpoint:
//
//	auto_decide_low_risk  AO settles TECHNICAL, REVERSIBLE ambiguity itself.
//	                      A functional/contract/irreversible/cost question is
//	                      still a person's.
//	full_autonomy         AO also settles functional ambiguity that is not
//	                      destructive, choosing the reasonable option and
//	                      recording what it chose.
//
// A question that is NOT auto-decidable under the run's mode keeps exactly the
// classification it had before this file existed, so ask_always is bit-for-bit
// the pre-P3-C behaviour.

// escalationPattern is one class of question the checkpoint names as always a
// person's, with the reason AO reports when it refuses.
type escalationPattern struct {
	re     *regexp.Regexp
	reason string
}

// functionalEscalations are the questions that change WHAT the product does,
// rather than how it is built. auto_decide_low_risk always escalates them;
// full_autonomy may decide them, because deciding them is what that mode is.
var functionalEscalations = []escalationPattern{
	{regexp.MustCompile(`(?i)\b(requirement|acceptance criteri|scope|spec(ification)?)\b`),
		"the answer would change a functional requirement or the agreed scope"},
	{regexp.MustCompile(`(?i)\b(should\s+(the\s+)?(user|customer|product|feature)|what\s+should\s+(the\s+)?(user|customer)\b)`),
		"the answer decides product behaviour a person owns"},
	{regexp.MustCompile(`(?i)\b(business|pricing|billing|invoice|legal|compliance|licen[sc]e)\b`),
		"the answer is a business or compliance decision"},
	{regexp.MustCompile(`(?i)\b(behaviou?rs?|features?)\s+(do\s+you\s+want|should\s+i\s+(build|implement))\b`),
		"the options are equivalent in effort but produce a different product"},
}

// alwaysEscalations are questions no autonomy mode decides. They overlap
// deliberately with the classifier's sensitive keywords: this list catches the
// same intent phrased without one of those exact words, and a rule stated twice
// in two places that BOTH refuse is not a duplication worth removing.
var alwaysEscalations = []escalationPattern{
	{regexp.MustCompile(`(?i)\b(irreversib\w*|cannot\s+be\s+undone|permanently|destructive|data\s+loss|wipe|truncate|purge)\b`),
		"the answer is irreversible or destructive"},
	{regexp.MustCompile(`(?i)\b(breaking\s+change|backwards[- ]incompatible|incompatible\s+(api|contract|schema)|api\s+contract)\b`),
		"the answer would break a published contract or API"},
	{regexp.MustCompile(`(?i)\b(auth(entication|orization)?|token|password|private\s+key|encryption|permission\s+model)\b`),
		"the answer touches authentication, secrets or the permission model"},
	{regexp.MustCompile(`(?i)\b(migration|migrate\s+(the\s+)?(data|database)|schema\s+change|drop\s+column)\b`),
		"the answer changes persisted data or its schema"},
	{regexp.MustCompile(`(?i)\b(cost|budget|spend|quota|rate\s+limit\s+increase)\b`),
		"the answer has a significant cost or quota implication"},
}

// technicalMarkers are the shapes that make a question recognisably about HOW
// the code is written rather than what it does: naming, placement, structure,
// formatting, a local implementation choice. A question that matches none of
// them is not assumed technical — it is escalated — because the failure mode
// that matters here is deciding something nobody delegated.
var technicalMarkers = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(name|naming|rename|call\s+(it|this))\b`),
	regexp.MustCompile(`(?i)\b(where\s+should\s+(i|this|it)\s+(put|live|go)|which\s+(file|package|module|directory|folder))\b`),
	regexp.MustCompile(`(?i)\b(extract|inline|refactor|helper|abstraction|interface|struct|type\s+alias)\b`),
	regexp.MustCompile(`(?i)\b(format|style|lint|comment|doc\s?comment|import\s+order)\b`),
	regexp.MustCompile(`(?i)\b(unit\s+test|test\s+(file|case|name)|table[- ]driven|fixture)\b`),
	regexp.MustCompile(`(?i)\b(which\s+(existing\s+)?(pattern|approach|helper|util(ity)?|library)\b.*\b(use|follow)|follow\s+the\s+existing)\b`),
	regexp.MustCompile(`(?i)\bshould\s+i\s+(add|split|combine|reuse|extract)\b`),
}

// AutonomyDecision is the complete answer for one ambiguous question: whether
// AO may settle it, and — either way — the sentence explaining that choice.
//
// The reason is not decoration. §21 requires that when AO DOES ask, it says
// what it could not determine and why it matters; and §22 requires that when it
// does NOT ask, the recorded decision says why AO was allowed to take it. One
// value carries both, so the two can never drift apart.
type AutonomyDecision struct {
	// AutoDecidable reports that AO may resolve this question itself.
	AutoDecidable bool
	// Reason is AO's own sentence, always populated.
	Reason string
	// Mode is the frozen policy the decision was taken under, recorded so an
	// auto-decision stays explainable after the rules change.
	Mode domain.QuestionAutonomyMode
}

// EvaluateAutonomy decides whether one AMBIGUOUS question may be settled by AO.
//
// It is called only for classification=ambiguous. Every other classification is
// already a definite answer — fact-backed, sensitive, or resolver-shaped — and
// running these rules over one would be a second opinion about a question that
// does not have one.
func EvaluateAutonomy(questionText string, mode domain.QuestionAutonomyMode) AutonomyDecision {
	if !mode.Valid() {
		mode = domain.QuestionAutonomyAskAlways
	}
	out := AutonomyDecision{Mode: mode}
	if !mode.AllowsAutoDecision() {
		out.Reason = "this run's autonomy policy is ask_always, so every ambiguous question goes to a person"
		return out
	}
	text := strings.TrimSpace(questionText)
	if text == "" {
		out.Reason = "there is no question text to reason about"
		return out
	}
	for _, esc := range alwaysEscalations {
		if esc.re.MatchString(text) {
			out.Reason = esc.reason
			return out
		}
	}
	if !mode.AllowsFunctionalDecision() {
		for _, esc := range functionalEscalations {
			if esc.re.MatchString(text) {
				out.Reason = esc.reason
				return out
			}
		}
		if !matchesAny(text, technicalMarkers) {
			// The conservative default, and the reason auto_decide_low_risk is
			// safe to turn on: a question AO cannot recognise as technical is
			// not assumed to be one.
			out.Reason = "AO could not recognise this as a technical, reversible choice, so it is not one AO decides under auto_decide_low_risk"
			return out
		}
		out.AutoDecidable = true
		out.Reason = "a technical, reversible implementation choice, which auto_decide_low_risk authorizes AO to settle from repository evidence"
		return out
	}
	out.AutoDecidable = true
	out.Reason = "full_autonomy authorizes AO to choose the reasonable option for any non-destructive ambiguity and record what it chose"
	return out
}

func matchesAny(text string, patterns []*regexp.Regexp) bool {
	for _, p := range patterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}
