package workflow

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func fixtureFacts() domain.TaskCheckpointSummary {
	return domain.TaskCheckpointSummary{
		Objective:            "ship the thing",
		Task:                 "task 2",
		AcceptanceCriteria:   []string{"no regressions"},
		RelevantFiles:        []string{"calc.py"},
		FilesChanged:         []string{"calc.py"},
		Decisions:            []string{"use python not go -> approved (policy)"},
		Tests:                []string{"python3 -c ... == OK"},
		LatestReviewFindings: "looks good",
		ActiveErrors:         nil,
		CurrentFingerprint:   "abc123",
		NextAction:           "verify",
	}
}

// Test #9: identical facts always hash identically (idempotency).
func TestSessionContextPack_DeterministicHash(t *testing.T) {
	p1 := BuildSessionContextPack(domain.WorkflowRoleWorker, fixtureFacts())
	p2 := BuildSessionContextPack(domain.WorkflowRoleWorker, fixtureFacts())
	if p1.ContentHash() != p2.ContentHash() {
		t.Fatalf("hashes differ for identical facts: %q vs %q", p1.ContentHash(), p2.ContentHash())
	}
	changed := fixtureFacts()
	changed.CurrentFingerprint = "different"
	p3 := BuildSessionContextPack(domain.WorkflowRoleWorker, changed)
	if p1.ContentHash() == p3.ContentHash() {
		t.Fatalf("hash unchanged despite a real fact change")
	}
}

// Test #10: question decisions are included (via Decisions, populated from
// 8K facts by BuildTaskCheckpointSummary — see task_checkpoint_summary.go).
func TestSessionContextPack_IncludesQuestionDecisions(t *testing.T) {
	source := domain.AnswerSourcePolicy
	q := domain.WorkflowQuestion{QuestionText: "Which linter config?", AnswerText: "strict", AnswerSource: &source}
	summary := BuildTaskCheckpointSummary(TaskCheckpointSummaryInput{
		Detail:    RunDetail{Run: domain.WorkflowRun{Objective: "x"}},
		Questions: []domain.WorkflowQuestion{q},
	})
	if len(summary.Decisions) != 1 || !strings.Contains(summary.Decisions[0], "Which linter config?") || !strings.Contains(summary.Decisions[0], "strict") {
		t.Fatalf("decisions = %v, want the question/answer included", summary.Decisions)
	}
	pack := BuildSessionContextPack(domain.WorkflowRoleWorker, summary)
	rendered := RenderContextPackForRole(pack)
	if !strings.Contains(rendered, "Which linter config?") {
		t.Fatalf("rendered pack missing question decision:\n%s", rendered)
	}
}

// Test #11: review findings are included for fix_worker rendering.
func TestSessionContextPack_IncludesReviewFindingsForFixWorker(t *testing.T) {
	facts := fixtureFacts()
	pack := BuildSessionContextPack(domain.WorkflowRoleFixWorker, facts)
	rendered := RenderContextPackForRole(pack)
	if !strings.Contains(rendered, "looks good") {
		t.Fatalf("rendered fix_worker pack missing review findings:\n%s", rendered)
	}
}

// Test #8: the pack/rendered text never contains transcript-shaped content
// or secrets — it is built exclusively from TaskCheckpointSummary's typed
// fields, so this test asserts the render only ever contains labeled fact
// lines, never anything resembling an env var or API key pattern.
func TestSessionContextPack_ExcludesTranscriptAndSecrets(t *testing.T) {
	facts := fixtureFacts()
	for _, role := range []domain.WorkflowRole{domain.WorkflowRoleWorker, domain.WorkflowRoleFixWorker, domain.WorkflowRoleReviewer, domain.WorkflowRoleDecisionResolver} {
		rendered := RenderContextPackForRole(BuildSessionContextPack(role, facts))
		for _, forbidden := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "sk-", "chain-of-thought", "<thinking>"} {
			if strings.Contains(rendered, forbidden) {
				t.Fatalf("role %q rendered pack unexpectedly contains %q:\n%s", role, forbidden, rendered)
			}
		}
	}
}

// Role-specific trimming: reviewer never sees a "Relevant files"/"Decisions"
// worker-oriented section, decision_resolver never sees "Files changed".
func TestSessionContextPack_RoleSpecificTrimming(t *testing.T) {
	facts := fixtureFacts()
	reviewer := RenderContextPackForRole(BuildSessionContextPack(domain.WorkflowRoleReviewer, facts))
	if strings.Contains(reviewer, "Relevant files") {
		t.Fatalf("reviewer pack should not include worker-only 'Relevant files' section:\n%s", reviewer)
	}
	resolver := RenderContextPackForRole(BuildSessionContextPack(domain.WorkflowRoleDecisionResolver, facts))
	if strings.Contains(resolver, "Files changed") {
		t.Fatalf("decision_resolver pack should not include 'Files changed' section:\n%s", resolver)
	}
}
