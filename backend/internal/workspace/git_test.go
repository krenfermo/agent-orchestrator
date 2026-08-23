package workspace

import (
	"context"
	"errors"
	"testing"
)

// The allowlist is the mechanism behind "this package cannot touch the user's
// working tree". If a future edit reaches for a mutating subcommand it must
// fail loudly at the boundary rather than quietly rewriting somebody's
// checkout, so the refusal is tested directly.
func TestExecGitRefusesCommandsThatCouldMutateTheWorkingTree(t *testing.T) {
	g, ok := NewExecGit("git").(execGit)
	if !ok {
		t.Fatal("NewExecGit did not return an execGit")
	}
	for _, sub := range []string{"checkout", "switch", "reset", "stash", "clean", "restore", "merge", "rebase", "pull", "commit", ""} {
		var args []string
		if sub != "" {
			args = []string{sub}
		}
		if _, err := g.run(context.Background(), t.TempDir(), args...); !errors.Is(err, ErrForbiddenGitCommand) {
			t.Fatalf("run %q err = %v, want ErrForbiddenGitCommand", sub, err)
		}
	}
}

// The read-only and worktree-scoped subcommands the manager actually needs
// must pass the same guard, or the guard would be protecting nothing by
// blocking everything.
func TestExecGitAllowsTheSubcommandsTheManagerNeeds(t *testing.T) {
	g, ok := NewExecGit("git").(execGit)
	if !ok {
		t.Fatal("NewExecGit did not return an execGit")
	}
	for _, sub := range []string{"rev-parse", "show-ref", "worktree"} {
		// The command itself fails (an empty temp dir is not a repository);
		// what matters is that it was not refused before running.
		if _, err := g.run(context.Background(), t.TempDir(), sub, "--help-me-fail"); errors.Is(err, ErrForbiddenGitCommand) {
			t.Fatalf("run %q was refused by the allowlist", sub)
		}
	}
}
