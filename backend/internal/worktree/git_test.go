package worktree

import (
	"context"
	"errors"
	"strings"
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
	for _, sub := range []string{"rev-parse", "show-ref", "worktree", "merge-base"} {
		// The command itself fails (an empty temp dir is not a repository);
		// what matters is that it was not refused before running.
		if _, err := g.run(context.Background(), t.TempDir(), sub, "--help-me-fail"); errors.Is(err, ErrForbiddenGitCommand) {
			t.Fatalf("run %q was refused by the allowlist", sub)
		}
	}
}

// update-ref is on the allowlist because DeleteBranch needs exactly one form of
// it, and being on the allowlist is deliberately not enough: it is the only
// command here that writes a ref, so the runner pins it to the
// compare-and-delete and refuses every other shape.
//
// The two refusals that matter are the last two. `update-ref -d <ref>` with no
// old value deletes a branch whatever it points at, which would let a proof
// made a moment earlier authorize deleting commits made since; and
// `update-ref <ref> <sha>` writes a ref outright, which this package must never
// do at all.
func TestExecGitNarrowsUpdateRefToCompareAndDelete(t *testing.T) {
	g, ok := NewExecGit("git").(execGit)
	if !ok {
		t.Fatal("NewExecGit did not return an execGit")
	}
	refused := [][]string{
		{"update-ref"},
		{"update-ref", "-d", "refs/heads/ao/task-1"},
		{"update-ref", "refs/heads/main", "deadbeef"},
		{"update-ref", "--stdin"},
		{"update-ref", "-d", "refs/heads/ao/task-1", ""},
		{"update-ref", "-d", "refs/heads/ao/task-1", "sha", "extra"},
	}
	for _, args := range refused {
		if _, err := g.run(context.Background(), t.TempDir(), args...); !errors.Is(err, ErrForbiddenGitCommand) {
			t.Fatalf("run %v err = %v, want ErrForbiddenGitCommand", args, err)
		}
	}
	// The one permitted shape must get past the guard (and then fail on its
	// own, because an empty temp dir is not a repository).
	if _, err := g.run(context.Background(), t.TempDir(), "update-ref", "-d", "refs/heads/ao/task-1", "deadbeef"); errors.Is(err, ErrForbiddenGitCommand) {
		t.Fatal("the compare-and-delete form was refused by the guard")
	}
}

// DeleteBranch without an expected commit is git's "delete whatever is there",
// which is the one thing this package may never do. It is refused before the
// binary is reached, so a caller that lost track of the SHA cannot fall back to
// an unguarded delete by accident.
func TestDeleteBranchRefusesWithoutAnExpectedCommit(t *testing.T) {
	err := NewExecGit("git").DeleteBranch(context.Background(), t.TempDir(), "ao/task-1", "   ")
	if err == nil {
		t.Fatal("an unguarded delete was accepted")
	}
	if !strings.Contains(err.Error(), "expected to be at") {
		t.Fatalf("err = %v, want it to name the missing expected commit", err)
	}
}
