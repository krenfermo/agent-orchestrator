package wfmemory

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// The adapter is a translation and must stay one. This test exists because the
// failure it guards against is silent: a field the adapter forgets to copy
// leaves the workflow side deriving facts correctly, the memory side storing
// them correctly, and nothing at all arriving — which is exactly the shape the
// gap this file closes had before it was closed.

func TestDerivedKnowledgeSurvivesTheTranslation(t *testing.T) {
	decisions := taskDecisions([]workflowcore.TaskDecisionFact{{
		Statement: "the criterion no longer applies",
		Rationale: "it was committed in 70296042b",
		Topic:     "acceptance-criterion:task-1:abc",
	}})
	if len(decisions) != 1 {
		t.Fatalf("%d decisions, want 1", len(decisions))
	}
	if decisions[0].Statement != "the criterion no longer applies" ||
		decisions[0].Rationale != "it was committed in 70296042b" ||
		decisions[0].Topic != "acceptance-criterion:task-1:abc" {
		t.Fatalf("a decision lost a field in translation: %+v", decisions[0])
	}
	if decisions[0].Scope != "" {
		t.Errorf("the adapter chose a scope (%q); that is normalizeKnowledgeScope's job", decisions[0].Scope)
	}

	risks := taskRisks([]workflowcore.TaskRiskFact{{
		Statement: "a reviewer thread is still unresolved",
		Kind:      domain.KnowledgeKindRisk,
		Topic:     "review-thread:th-1",
		Evidence:  []string{"internal/store/store.go"},
	}})
	if len(risks) != 1 {
		t.Fatalf("%d risks, want 1", len(risks))
	}
	if risks[0].Statement != "a reviewer thread is still unresolved" ||
		risks[0].Kind != domain.KnowledgeKindRisk ||
		risks[0].Topic != "review-thread:th-1" ||
		len(risks[0].Evidence) != 1 {
		t.Fatalf("a risk lost a field in translation: %+v", risks[0])
	}
}

func TestNothingInProducesNothingOut(t *testing.T) {
	if got := taskDecisions(nil); got != nil {
		t.Errorf("taskDecisions(nil) = %+v, want nil", got)
	}
	if got := taskRisks(nil); got != nil {
		t.Errorf("taskRisks(nil) = %+v, want nil", got)
	}
}
