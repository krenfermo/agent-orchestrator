package workflow_test

import (
	"strings"
	"testing"

	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func TestReviewPromptIdentifiesWorkspaceFingerprintHonestly(t *testing.T) {
	prompt := workflowcore.BuildReviewPrompt(workflowcore.ReviewPromptInput{Objective: "implement", WorkerSessionID: "session-1", BaseSHA: "0123456789012345678901234567890123456789", HeadSHA: strings.Repeat("a", 64), ReviewRunID: "review-1"})
	if !strings.Contains(prompt, "Reviewed workspace fingerprint: "+strings.Repeat("a", 64)) {
		t.Fatal("missing fingerprint label")
	}
	if !strings.Contains(prompt, "not a Git object id") {
		t.Fatal("missing fingerprint semantics")
	}
	if strings.Contains(prompt, "A commit did land") || strings.Contains(prompt, "Head commit:") {
		t.Fatal("prompt falsely describes fingerprint as commit")
	}
}
