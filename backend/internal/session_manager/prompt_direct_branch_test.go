package sessionmanager

import (
	"strings"
	"testing"
)

// Checkpoint 8P-E.14 regression tests for the actual cause of the
// "feat/engineering-control-center-cancel-archive-workflows" incident.
//
// AO's workspace routing was already correct: the task ran in the registered
// repository, on the configured branch, because the project was in
// direct-branch mode. The derived branch was then created by the agent, on
// AO's own instruction — the worker system prompt said "Work on a feature
// branch, not the default branch" and the project context directly below it
// said "Default branch: feat/engineering-control-center".
//
// These tests assert the prompt no longer says that in direct-branch mode, and
// still does say it in isolated-worktree mode.

func directBranchWorkerPrompt() string {
	return buildSystemPromptText(systemPromptConfig{
		Role: sessionPromptRoleWorker,
		Project: promptProject{
			ID:            "agent-orchestrator",
			Name:          "agent-orchestrator",
			Repo:          "git@github.com:aoagents/agent-orchestrator.git",
			DefaultBranch: "feat/engineering-control-center",
			Path:          "/repos/agent-orchestrator",
			DirectBranch:  true,
		},
	})
}

// The exact instruction that produced the incident must be gone.
func TestDirectBranchWorkerPromptNeverAsksForAFeatureBranch(t *testing.T) {
	got := directBranchWorkerPrompt()
	for _, banned := range []string{
		"Work on a feature branch, not the default branch.",
		"create independent PR branches",
		"create each source branch as a child of this session branch",
	} {
		if strings.Contains(got, banned) {
			t.Fatalf("direct-branch worker prompt still contains worktree-mode branch guidance %q:\n%s", banned, got)
		}
	}
}

// And the replacement has to be an explicit prohibition naming the branch, not
// merely the absence of the old rule.
func TestDirectBranchWorkerPromptPinsTheConfiguredBranch(t *testing.T) {
	got := directBranchWorkerPrompt()
	for _, want := range []string{
		"Work directly on `feat/engineering-control-center`",
		"Do NOT create a branch",
		"git checkout -b",
		"Working branch (direct-branch execution; do not create or switch branches): feat/engineering-control-center",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("direct-branch worker prompt missing %q:\n%s", want, got)
		}
	}
	// "Default branch:" is what made the configured branch read as a base to
	// branch off. It must not appear in this mode at all.
	if strings.Contains(got, "Default branch:") {
		t.Fatalf("direct-branch worker prompt still labels the working branch as the default branch:\n%s", got)
	}
}

// The session-namespace PR convention only makes sense when AO generated an
// ao/<session>/root branch. Following it in direct-branch mode is precisely how
// a derived branch gets created.
func TestDirectBranchWorkerPromptOmitsTheSessionNamespacePRConvention(t *testing.T) {
	got := directBranchWorkerPrompt()
	if strings.Contains(got, "Pull Requests for This Session") {
		t.Fatalf("direct-branch worker prompt still carries the session-branch PR namespace convention:\n%s", got)
	}
	// The container-label section follows it and must survive its removal.
	if !strings.Contains(got, "Docker Containers Started By This Session") {
		t.Fatalf("direct-branch worker prompt dropped the container-label section:\n%s", got)
	}
}

// Isolated-worktree mode is untouched: the feature-branch convention is correct
// there and must keep working exactly as before.
func TestIsolatedWorktreeWorkerPromptKeepsFeatureBranchGuidance(t *testing.T) {
	got := buildSystemPromptText(systemPromptConfig{
		Role: sessionPromptRoleWorker,
		Project: promptProject{
			ID:            "agent-orchestrator",
			Repo:          "git@github.com:aoagents/agent-orchestrator.git",
			DefaultBranch: "main",
			Path:          "/repos/agent-orchestrator",
		},
	})
	if !strings.Contains(got, "Work on a feature branch, not the default branch.") {
		t.Fatalf("worktree-mode worker prompt lost its feature-branch rule:\n%s", got)
	}
	if !strings.Contains(got, "Pull Requests for This Session") {
		t.Fatalf("worktree-mode worker prompt lost the session-branch PR convention:\n%s", got)
	}
	if !strings.Contains(got, "Default branch: main") {
		t.Fatalf("worktree-mode project context changed:\n%s", got)
	}
	if strings.Contains(got, "Do NOT create a branch") {
		t.Fatalf("worktree-mode prompt wrongly picked up the direct-branch prohibition:\n%s", got)
	}
}
