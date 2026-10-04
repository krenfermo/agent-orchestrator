package questions_test

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
)

// autonomy_test.go — P3-C §20/§21.
//
// The property under test is not "AO answers more questions". It is that the
// widening is bounded exactly where the checkpoint says it is: a technical,
// reversible choice may be settled without a person, and a requirement, a
// contract, a destructive operation, a secret or a cost may not — under ANY
// mode.

func TestAskAlwaysIsExactlyThePreP3CBehaviour(t *testing.T) {
	for _, text := range []string{
		"should the helper live in utils.go or in the same file?",
		"which naming convention should I follow here?",
		"should I drop the production table?",
	} {
		base, _ := questions.Classify(text, domain.QuestionCertaintyActual)
		got, _, decision := questions.ClassifyUnderAutonomy(text, domain.QuestionCertaintyActual, domain.QuestionAutonomyAskAlways)
		if got != base {
			t.Errorf("ask_always changed the classification of %q: %q -> %q", text, base, got)
		}
		if decision.AutoDecidable {
			t.Errorf("ask_always auto-decided %q", text)
		}
	}
}

// The zero value is not a mode. A caller that never set one must get
// ask_always, never an accidental widening.
func TestUnsetAutonomyModeDefaultsToAskAlways(t *testing.T) {
	text := "should I extract this into a helper?"
	got, _, decision := questions.ClassifyUnderAutonomy(text, domain.QuestionCertaintyActual, "")
	if decision.AutoDecidable {
		t.Fatalf("an unset autonomy mode auto-decided %q", text)
	}
	if got != domain.QuestionClassificationAmbiguous {
		t.Fatalf("classification = %q, want ambiguous", got)
	}
}

// §20: auto_decide_low_risk settles technical, reversible choices — which is
// the whole reason an autonomous task stops being an interview.
func TestAutoDecideLowRiskSettlesTechnicalChoices(t *testing.T) {
	for _, text := range []string{
		"should I extract this into a helper or inline it?",
		"which file should this live in?",
		"what should I name this interface?",
		"should I add a table-driven unit test for this?",
	} {
		got, reason, decision := questions.ClassifyUnderAutonomy(text, domain.QuestionCertaintyActual,
			domain.QuestionAutonomyAutoDecideLowRisk)
		if !decision.AutoDecidable {
			t.Errorf("auto_decide_low_risk refused the technical question %q: %s", text, decision.Reason)
			continue
		}
		if got != domain.QuestionClassificationAutoResolvable {
			t.Errorf("%q: classification = %q, want auto_resolvable", text, got)
		}
		if reason == "" {
			t.Errorf("%q: an auto-decision was recorded with no justification", text)
		}
	}
}

// §20: and it refuses the classes that are a person's, naming which one.
func TestAutoDecideLowRiskEscalatesTheClassesThatAreAPersons(t *testing.T) {
	for _, tc := range []struct{ text, why string }{
		{"should the user see the archived items by default?", "product behaviour"},
		{"does this change the acceptance criteria for the task?", "requirement/scope"},
		{"is this a breaking change for the public API contract?", "contract"},
		{"this migration cannot be undone — should I run it?", "irreversible"},
		{"should I raise the monthly budget for this provider?", "cost"},
		{"which encryption token should the service use?", "secrets"},
	} {
		got, _, decision := questions.ClassifyUnderAutonomy(tc.text, domain.QuestionCertaintyActual,
			domain.QuestionAutonomyAutoDecideLowRisk)
		if decision.AutoDecidable {
			t.Errorf("auto_decide_low_risk decided a %s question: %q", tc.why, tc.text)
		}
		if got == domain.QuestionClassificationAutoResolvable {
			t.Errorf("%q (%s) was routed to the resolver", tc.text, tc.why)
		}
		if decision.Reason == "" {
			t.Errorf("%q: AO refused without saying why", tc.text)
		}
	}
}

// §20: a question AO cannot recognise as technical is NOT assumed to be one.
// That conservatism is what makes the mode safe to enable.
func TestAutoDecideLowRiskDoesNotAssumeUnrecognisedQuestionsAreTechnical(t *testing.T) {
	text := "how far should this go?"
	_, _, decision := questions.ClassifyUnderAutonomy(text, domain.QuestionCertaintyActual,
		domain.QuestionAutonomyAutoDecideLowRisk)
	if decision.AutoDecidable {
		t.Fatalf("an unrecognised question was assumed technical: %q", text)
	}
}

// §20: full_autonomy takes the functional ambiguity auto_decide_low_risk
// escalates — and still refuses everything destructive.
func TestFullAutonomyDecidesFunctionalButNeverDestructiveAmbiguity(t *testing.T) {
	functional := "should the user see archived items by default?"
	got, _, decision := questions.ClassifyUnderAutonomy(functional, domain.QuestionCertaintyActual,
		domain.QuestionAutonomyFullAutonomy)
	if !decision.AutoDecidable {
		t.Fatalf("full_autonomy refused a non-destructive functional question: %s", decision.Reason)
	}
	if got != domain.QuestionClassificationAutoResolvable {
		t.Fatalf("classification = %q, want auto_resolvable", got)
	}

	for _, text := range []string{
		"this migration cannot be undone — should I run it?",
		"is this a breaking change for the public API contract?",
		"which private key should the service use?",
	} {
		_, _, d := questions.ClassifyUnderAutonomy(text, domain.QuestionCertaintyActual,
			domain.QuestionAutonomyFullAutonomy)
		if d.AutoDecidable {
			t.Errorf("full_autonomy decided a destructive/contract/secret question: %q", text)
		}
	}
}

// The classifier's own refusals run FIRST and no mode can reach past them.
// This is the safety ordering stated as a test rather than as a comment.
func TestNoAutonomyModeReachesPastTheClassifiersRefusals(t *testing.T) {
	for _, mode := range []domain.QuestionAutonomyMode{
		domain.QuestionAutonomyAskAlways,
		domain.QuestionAutonomyAutoDecideLowRisk,
		domain.QuestionAutonomyFullAutonomy,
	} {
		// A sensitive keyword.
		got, _, _ := questions.ClassifyUnderAutonomy("should I delete the production database?",
			domain.QuestionCertaintyActual, mode)
		if got != domain.QuestionClassificationHumanRequired {
			t.Errorf("%s: a sensitive question classified as %q", mode, got)
		}
		// No reconstructable text.
		got, _, _ = questions.ClassifyUnderAutonomy("", domain.QuestionCertaintyUnknown, mode)
		if got != domain.QuestionClassificationHumanRequired {
			t.Errorf("%s: an unreadable question classified as %q", mode, got)
		}
	}
}

// A fact-backed question is answered by the policy resolver exactly as before;
// the autonomy rules are never consulted for one.
func TestFactBackedQuestionsAreUnaffectedByAutonomy(t *testing.T) {
	got, _, decision := questions.ClassifyUnderAutonomy("which branch am I on?",
		domain.QuestionCertaintyActual, domain.QuestionAutonomyFullAutonomy)
	if got != domain.QuestionClassificationPolicyResolvable {
		t.Fatalf("classification = %q, want policy_resolvable", got)
	}
	if decision.AutoDecidable {
		t.Fatal("the autonomy rules were consulted for a fact-backed question")
	}
}
