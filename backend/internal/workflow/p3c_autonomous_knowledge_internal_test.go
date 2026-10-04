package workflow

import (
	stdctx "context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// p3c_autonomous_knowledge_internal_test.go — P3-C §2/§3/§5/§11.
//
// A decision AO took for itself has to reach Shared Task Knowledge through the
// SAME path every other decision uses, carrying enough to answer the questions
// a later reader will actually have: who decided this, under what policy, on
// what grounds, and against what evidence.

func autonomousQuestion(mode domain.QuestionAutonomyMode) domain.WorkflowQuestion {
	source := domain.AnswerSourceAutonomous
	return domain.WorkflowQuestion{
		ID: "q-auto", Fingerprint: "fp-auto", State: domain.QuestionStateAnswered,
		QuestionText:         "Should the helper live in utils.go or here?",
		AnswerText:           "utils.go",
		AnswerSource:         &source,
		AutonomyMode:         mode,
		ClassificationReason: "auto-decided under autonomy policy " + string(mode) + ": a technical, reversible implementation choice",
	}
}

// §2/§3: the autonomous answer becomes a durable decision, and that decision
// names the policy that authorized AO to take it, the grounds it was classified
// on, and the evidence the resolver read.
func TestAutonomousAnswerBecomesADecisionThatNamesItsPolicy(t *testing.T) {
	c := &Coordinator{questionsStore: fakeKnowledgeQuestions{
		questions: []domain.WorkflowQuestion{autonomousQuestion(domain.QuestionAutonomyAutoDecideLowRisk)},
		resolutions: map[string]domain.WorkflowQuestionResolution{
			"q-auto": {
				Status:             domain.ResolutionStatusComplete,
				ReasonSummary:      "utils.go already holds the sibling helpers",
				EvidenceReferences: []string{"utils.go", "internal/format/path.go"},
			},
		},
	}}

	decisions := c.durableTaskDecisions(stdctx.Background(), knowledgeRun())
	if len(decisions) != 1 {
		t.Fatalf("%d decisions, want 1", len(decisions))
	}
	d := decisions[0]
	if !strings.Contains(d.Statement, "utils.go") {
		t.Errorf("the chosen answer did not reach the decision: %q", d.Statement)
	}
	// The three facts a later reader needs, and every one of them copied from a
	// durable row rather than composed.
	if !strings.Contains(d.Rationale, "decided automatically by AO") {
		t.Errorf("the decision does not say AO took it: %q", d.Rationale)
	}
	if !strings.Contains(d.Rationale, string(domain.QuestionAutonomyAutoDecideLowRisk)) {
		t.Errorf("the decision does not name the autonomy policy: %q", d.Rationale)
	}
	if !strings.Contains(d.Rationale, "utils.go already holds the sibling helpers") {
		t.Errorf("the resolver's own reason was dropped: %q", d.Rationale)
	}
	if len(d.Evidence) != 2 || d.Evidence[0] != "utils.go" {
		t.Errorf("the resolver's evidence did not reach the decision: %+v", d.Evidence)
	}
	if d.Topic != "question:fp-auto" {
		t.Errorf("topic = %q, want the question's durable fingerprint", d.Topic)
	}
}

// §1: and it is never mistaken for a human decision. The rationale of a human
// answer says so, and the rationale of an autonomous one says the opposite —
// the two must not read alike, because every downstream reader treats a human
// answer as authoritative outright.
func TestAutonomousAndHumanDecisionsAreNotInterchangeable(t *testing.T) {
	human := domain.AnswerSourceHuman
	c := &Coordinator{questionsStore: fakeKnowledgeQuestions{
		questions: []domain.WorkflowQuestion{{
			ID: "q-h", Fingerprint: "fp-auto", State: domain.QuestionStateAnswered,
			QuestionText: "Should the helper live in utils.go or here?",
			AnswerText:   "utils.go", AnswerSource: &human,
		}},
	}}
	humanDecisions := c.durableTaskDecisions(stdctx.Background(), knowledgeRun())
	if len(humanDecisions) != 1 {
		t.Fatalf("%d human decisions, want 1", len(humanDecisions))
	}
	if strings.Contains(humanDecisions[0].Rationale, "decided automatically") {
		t.Fatalf("a human decision claims AO took it: %q", humanDecisions[0].Rationale)
	}
	if !strings.Contains(humanDecisions[0].Rationale, "human") {
		t.Fatalf("a human decision does not say a person gave it: %q", humanDecisions[0].Rationale)
	}

	// §5: the SAME question answered by a person produces the SAME topic, which
	// is what lets P2-C's own subject machinery supersede the autonomous
	// decision instead of leaving two competing answers side by side.
	auto := &Coordinator{questionsStore: fakeKnowledgeQuestions{
		questions: []domain.WorkflowQuestion{autonomousQuestion(domain.QuestionAutonomyAutoDecideLowRisk)},
		resolutions: map[string]domain.WorkflowQuestionResolution{
			"q-auto": {Status: domain.ResolutionStatusComplete, ReasonSummary: "sibling helpers live there"},
		},
	}}
	autoDecisions := auto.durableTaskDecisions(stdctx.Background(), knowledgeRun())
	if len(autoDecisions) != 1 {
		t.Fatalf("%d autonomous decisions, want 1", len(autoDecisions))
	}
	if autoDecisions[0].Topic != humanDecisions[0].Topic {
		t.Fatalf("the same question produced two subjects (%q vs %q), so a human answer could never supersede AO's",
			autoDecisions[0].Topic, humanDecisions[0].Topic)
	}
}

// §2: an autonomous answer whose resolution attempt AO cannot verify is not a
// decision. The same fail-closed rule the resolver source already had, because
// the proof belongs to the machinery and not to why the question reached it.
func TestUnverifiableAutonomousAnswerIsNotADecision(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resolution *domain.WorkflowQuestionResolution
	}{
		{"no resolution row at all", nil},
		{"the attempt asked for a human", &domain.WorkflowQuestionResolution{
			Status: domain.ResolutionStatusComplete, RequiresHuman: true}},
		{"the attempt never completed", &domain.WorkflowQuestionResolution{
			Status: domain.ResolutionStatusFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolutions := map[string]domain.WorkflowQuestionResolution{}
			if tc.resolution != nil {
				resolutions["q-auto"] = *tc.resolution
			}
			c := &Coordinator{questionsStore: fakeKnowledgeQuestions{
				questions:   []domain.WorkflowQuestion{autonomousQuestion(domain.QuestionAutonomyFullAutonomy)},
				resolutions: resolutions,
			}}
			if decisions := c.durableTaskDecisions(stdctx.Background(), knowledgeRun()); len(decisions) != 0 {
				t.Fatalf("AO stated %d decisions on an unverifiable authority: %+v", len(decisions), decisions)
			}
		})
	}
}

// §4: deriving the decisions twice — which every read of a finished task does —
// produces the same decisions, not two of them.
func TestAutonomousDecisionDerivationIsStable(t *testing.T) {
	c := &Coordinator{questionsStore: fakeKnowledgeQuestions{
		questions: []domain.WorkflowQuestion{autonomousQuestion(domain.QuestionAutonomyAutoDecideLowRisk)},
		resolutions: map[string]domain.WorkflowQuestionResolution{
			"q-auto": {Status: domain.ResolutionStatusComplete, ReasonSummary: "sibling helpers live there"},
		},
	}}
	first := c.durableTaskDecisions(stdctx.Background(), knowledgeRun())
	second := c.durableTaskDecisions(stdctx.Background(), knowledgeRun())
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("derivations disagree on count: %d vs %d", len(first), len(second))
	}
	if first[0].Statement != second[0].Statement ||
		first[0].Rationale != second[0].Rationale ||
		first[0].Topic != second[0].Topic {
		t.Fatalf("two derivations of one decision differ:\n%+v\n%+v", first[0], second[0])
	}
}
