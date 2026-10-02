package codex

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Codex AR1A-FIN-01, end to end through the production reviewer: the real
// Codex agent adapter over a real git workspace yields a read-only reviewer
// command with no writable --add-dir.
func TestTheProductionReviewerCommandCarriesNoWritableRoots(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not on PATH")
	}
	ws := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	got, err := New().ReviewCommand(context.Background(), ports.ReviewInvocation{
		ReviewerID: "review-w1", WorkspacePath: ws, Prompt: "review it", SystemPrompt: "review only",
	})
	if err != nil {
		t.Fatalf("ReviewCommand: %v", err)
	}
	if slices.Contains(got.Argv, "--add-dir") {
		t.Fatalf("read-only reviewer carries a writable --add-dir: %v", got.Argv)
	}
	i := slices.Index(got.Argv, "--sandbox")
	if i < 0 || i+1 >= len(got.Argv) || got.Argv[i+1] != "read-only" {
		t.Fatalf("reviewer is not read-only: %v", got.Argv)
	}
	if slices.Contains(got.Argv, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("reviewer carries the bypass: %v", got.Argv)
	}
}
