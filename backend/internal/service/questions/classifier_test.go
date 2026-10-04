package questions

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		certainty domain.QuestionCertainty
		wantClass domain.QuestionClassification
	}{
		{
			name:      "empty text",
			text:      "",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationHumanRequired,
		},
		{
			name:      "unknown certainty",
			text:      "should I push to main?",
			certainty: domain.QuestionCertaintyUnknown,
			wantClass: domain.QuestionClassificationHumanRequired,
		},
		{
			name:      "push to main",
			text:      "Should I push directly to main?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationPolicyResolvable,
		},
		{
			name:      "which branch",
			text:      "Which branch am I on right now?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationPolicyResolvable,
		},
		{
			name:      "should I merge",
			text:      "Should I merge this PR now?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationPolicyResolvable,
		},
		{
			name:      "sensitive keyword deploy",
			text:      "Should I deploy this to production now?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationHumanRequired,
		},
		{
			name:      "sensitive keyword delete",
			text:      "Should I delete the old credentials file?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationHumanRequired,
		},
		{
			name:      "genuinely ambiguous",
			text:      "Should the retry cooldown be 2s or 8s?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationAmbiguous,
		},
		{
			name:      "which existing helper should I use",
			text:      "Which existing helper function should I use to format timestamps?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationAutoResolvable,
		},
		{
			name:      "which of these modules should I use",
			text:      "Which of these modules should I use for retries?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationAutoResolvable,
		},
		{
			name:      "is there already a helper for X",
			text:      "Is there already a helper for parsing this response?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationAutoResolvable,
		},
		{
			name:      "does a function already exist",
			text:      "Does a function already exist for retrying failed requests?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationAutoResolvable,
		},
		{
			name:      "which pattern already implements",
			text:      "Which pattern already implements request retries in this repo?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationAutoResolvable,
		},
		{
			name:      "preference question does not match discovery shape",
			text:      "Which of these two naming schemes do you prefer for the new config field?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationAmbiguous,
		},
		{
			name:      "business choice does not match discovery shape",
			text:      "Should we support both light and dark themes at launch?",
			certainty: domain.QuestionCertaintyInferred,
			wantClass: domain.QuestionClassificationAmbiguous,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotClass, reason := Classify(tc.text, tc.certainty)
			if gotClass != tc.wantClass {
				t.Fatalf("Classify(%q) = %v, want %v (reason=%q)", tc.text, gotClass, tc.wantClass, reason)
			}
			if reason == "" {
				t.Errorf("expected non-empty classification reason")
			}
		})
	}
}

func TestResolveState(t *testing.T) {
	cases := []struct {
		name      string
		class     domain.QuestionClassification
		budget    ClassifyContext
		wantState domain.QuestionState
	}{
		{
			name:      "policy resolvable within budget",
			class:     domain.QuestionClassificationPolicyResolvable,
			budget:    ClassifyContext{PolicyAnsweredCount: 1, MaxAutoAnswered: 5},
			wantState: domain.QuestionStatePending,
		},
		{
			name:      "policy resolvable but budget exhausted",
			class:     domain.QuestionClassificationPolicyResolvable,
			budget:    ClassifyContext{PolicyAnsweredCount: 5, MaxAutoAnswered: 5},
			wantState: domain.QuestionStateHumanRequired,
		},
		{
			name:      "human required stays human required",
			class:     domain.QuestionClassificationHumanRequired,
			budget:    ClassifyContext{MaxAutoAnswered: 5},
			wantState: domain.QuestionStateHumanRequired,
		},
		{
			name:      "ambiguous escalates to human required",
			class:     domain.QuestionClassificationAmbiguous,
			budget:    ClassifyContext{MaxAutoAnswered: 5},
			wantState: domain.QuestionStateHumanRequired,
		},
		{
			name:      "auto resolvable within budget resolves",
			class:     domain.QuestionClassificationAutoResolvable,
			budget:    ClassifyContext{PolicyAnsweredCount: 1, MaxAutoAnswered: 5},
			wantState: domain.QuestionStateResolving,
		},
		{
			name:      "auto resolvable but budget exhausted",
			class:     domain.QuestionClassificationAutoResolvable,
			budget:    ClassifyContext{PolicyAnsweredCount: 5, MaxAutoAnswered: 5},
			wantState: domain.QuestionStateHumanRequired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveState(tc.class, tc.budget)
			if got != tc.wantState {
				t.Fatalf("ResolveState(%v) = %v, want %v", tc.class, got, tc.wantState)
			}
		})
	}
}
