package gitworktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// workspace_pinned_base_test.go — a base given as an object id is a PIN.
//
// It exists for workflow/repair_artifact.go: a repair checkout is cut from the
// exact commit under review, and it is handed down as an object id rather than
// a branch name precisely because a name can move, be deleted, or be checked
// out elsewhere between the decision and the cut. Every one of those would
// silently produce a worktree of a different tree.
//
// Two properties, and the second is the one that bites. A pin that resolves
// must be used verbatim, and a pin that does NOT resolve must fail — because
// `git rev-parse --verify <40 hex>` answers with the string itself whether or
// not the object exists, so a probe that does not peel to a commit proves
// nothing at all and the candidate list below it would quietly fall back to the
// project's default branch.

func TestCreateCutsFromAPinnedCommitRatherThanTheDefaultBranch(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupOriginClone(t, git, tmp)

	// A commit that exists only on a side branch: the shape of a task's work.
	runGit(t, git, repo, "checkout", "-b", "side")
	if err := os.WriteFile(filepath.Join(repo, "task.txt"), []byte("work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, repo, "add", "task.txt")
	runGit(t, git, repo, "commit", "-m", "task work")
	pinned := gitOutput(t, git, repo, "rev-parse", "--verify", "HEAD")
	runGit(t, git, repo, "checkout", "main")

	ws, err := New(Options{Binary: git, ManagedRoot: filepath.Join(tmp, "managed"),
		RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	info, err := ws.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "proj", SessionID: "sess", Branch: "ao/repair-1", BaseBranch: pinned,
	})
	if err != nil {
		t.Fatalf("create from a pinned commit: %v", err)
	}
	if got := gitOutput(t, git, info.Path, "rev-parse", "--verify", "HEAD"); got != pinned {
		t.Fatalf("worktree HEAD = %q, want the pinned commit %q", got, pinned)
	}
	if _, err := os.Stat(filepath.Join(info.Path, "task.txt")); err != nil {
		t.Fatalf("the pinned commit's work is not in the checkout: %v", err)
	}
}

func TestCreateRefusesAPinnedCommitThatIsNotInTheRepository(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupOriginClone(t, git, tmp)
	ws, err := New(Options{Binary: git, ManagedRoot: filepath.Join(tmp, "managed"),
		RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	absent := strings.Repeat("1", 40)
	_, err = ws.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "proj", SessionID: "sess", Branch: "ao/repair-1", BaseBranch: absent,
	})
	if err == nil {
		t.Fatal("a checkout was created from a commit this repository does not have")
	}
	if !errors.Is(err, ErrBranchNotFetched) {
		t.Fatalf("err = %v, want ErrBranchNotFetched: an unresolvable pin must fail closed, never fall back to main", err)
	}
}

func TestIsObjectIDAcceptsOnlyFullObjectIDs(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want bool
	}{
		{strings.Repeat("a", 40), true},
		{strings.Repeat("F", 64), true},
		// An abbreviation can become ambiguous as a repository grows, so a base
		// that resolves today and refuses tomorrow is never treated as a pin.
		{strings.Repeat("a", 12), false},
		{"main", false},
		{"refs/ao/workflows/wf-1/integration", false},
		{strings.Repeat("z", 40), false},
		{"", false},
	} {
		if got := isObjectID(tt.in); got != tt.want {
			t.Fatalf("isObjectID(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
