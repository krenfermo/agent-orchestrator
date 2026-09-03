package domain

import (
	"strings"
	"time"
)

// workflow_autonomy.go — P3-C §20: how much of a Task's own ambiguity AO is
// allowed to settle for itself.
//
// The problem this names. A Task started for unattended execution stops the
// moment its worker asks anything the classifier cannot map onto a fact-backed
// pattern: questions.Classify returns `ambiguous`, ResolveState turns that into
// state=human_required, and the run parks. That is correct and conservative for
// a question about deleting a production table. It is absurd for "should this
// helper live in utils or in the same file", which is technical, reversible,
// and something AO can decide from the repository as well as anybody. An
// autonomous Task that has to be interviewed about every such choice is not
// autonomous, and the accumulated interruptions are the single biggest reason a
// long task never finishes unattended.
//
// The policy below is the knob that separates the two cases. It NEVER widens
// what AO may decide about the classes that are dangerous by construction: the
// sensitive-keyword refusal in the classifier runs before this policy is
// consulted, so no mode here can auto-decide a destructive, security-adjacent
// or credential-touching question. What it widens is the ambiguous middle.

// QuestionAutonomyMode is the frozen answer to "may AO decide this itself?".
type QuestionAutonomyMode string

const (
	// QuestionAutonomyAskAlways is the pre-P3-C behaviour, preserved exactly:
	// every question the classifier cannot answer from a fact-backed pattern
	// goes to a person. It stays the default for a run whose owner never chose
	// otherwise, because widening autonomy is a decision somebody should make.
	QuestionAutonomyAskAlways QuestionAutonomyMode = "ask_always"
	// QuestionAutonomyAutoDecideLowRisk lets AO settle technical, reversible,
	// low-risk ambiguity by itself — through the read-only Decision Resolver,
	// from repository evidence — and record the decision. It still asks a
	// person when the question changes a functional requirement, is
	// destructive, touches security or secrets, breaks an API contract, is
	// irreversible, carries significant cost or scope, or is a genuine choice
	// between equivalent options that produce different products.
	QuestionAutonomyAutoDecideLowRisk QuestionAutonomyMode = "auto_decide_low_risk"
	// QuestionAutonomyFullAutonomy additionally lets AO choose a reasonable
	// option for functional ambiguity that is not destructive, and record what
	// it chose. It never reaches the classes the classifier refuses outright.
	QuestionAutonomyFullAutonomy QuestionAutonomyMode = "full_autonomy"
)

// Valid reports whether m is a recognised autonomy mode.
func (m QuestionAutonomyMode) Valid() bool {
	switch m {
	case QuestionAutonomyAskAlways, QuestionAutonomyAutoDecideLowRisk, QuestionAutonomyFullAutonomy:
		return true
	default:
		return false
	}
}

// AllowsAutoDecision reports whether this mode permits AO to settle an
// ambiguous question without asking. False for ask_always, which is the whole
// of that mode.
func (m QuestionAutonomyMode) AllowsAutoDecision() bool {
	return m == QuestionAutonomyAutoDecideLowRisk || m == QuestionAutonomyFullAutonomy
}

// AllowsFunctionalDecision reports whether this mode permits AO to settle
// ambiguity that changes what the product DOES, as opposed to how it is built.
// Only full_autonomy does.
func (m QuestionAutonomyMode) AllowsFunctionalDecision() bool {
	return m == QuestionAutonomyFullAutonomy
}

// NormalizeQuestionAutonomyMode trims and lower-cases a caller-supplied mode,
// leaving anything unrecognised unchanged so the caller can reject it rather
// than silently running under a policy nobody chose. Mirrors
// NormalizeRepairMode exactly.
func NormalizeQuestionAutonomyMode(raw string) QuestionAutonomyMode {
	return QuestionAutonomyMode(strings.ToLower(strings.TrimSpace(raw)))
}

// QuestionAutonomyPolicyVersion versions the modes and the risk rules that act
// on them, so a recorded auto-decision stays explainable after they change.
const QuestionAutonomyPolicyVersion = "v1"

// QuestionAutonomySnapshot is the frozen autonomy policy embedded in a run's
// WorkflowPolicy, alongside Repair/Routing/Wake/Execution/Strategy. Frozen for
// the same reason those are: a later Settings change must not alter what an
// in-flight run is allowed to decide on its own, and a restart must not change
// the answer.
type QuestionAutonomySnapshot struct {
	Version string               `json:"version,omitempty"`
	Mode    QuestionAutonomyMode `json:"mode,omitempty"`
	At      time.Time            `json:"at,omitempty"`
}

// Recorded reports whether this snapshot is a real frozen decision rather than
// the zero value a run created before P3-C carries.
func (s QuestionAutonomySnapshot) Recorded() bool { return s.Mode.Valid() && s.Version != "" }

// DefaultQuestionAutonomyPolicy is what a run freezes when nobody said
// otherwise: ask_always. A run created before anybody could choose an autonomy
// mode never opted into having its ambiguity settled for it.
func DefaultQuestionAutonomyPolicy(now time.Time) QuestionAutonomySnapshot {
	return QuestionAutonomySnapshot{
		Version: QuestionAutonomyPolicyVersion,
		Mode:    QuestionAutonomyAskAlways,
		At:      now,
	}
}
