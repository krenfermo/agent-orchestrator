package workflow_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// p1d_real_git_placement_aba_test.go — P1-D §M: the worktree name/path ABA,
// against a REAL git repository.
//
// The failure this exists to rule out is specific and is not hypothetical on a
// long-lived daemon: AO creates a worktree at some path on some ao/* branch,
// that generation ends and the checkout is removed, and a LATER generation of
// the same task is cut at an equivalent path and branch name. Everything a
// stale holder remembers — the path, the branch, even the directory's
// existence — is true again, about somebody else's checkout.
//
// Identity is therefore never the path. It is the durable placement record and
// its generation, and this test proves that against git rather than against a
// fake: a real worktree is added, removed, and recreated at the same path and
// branch name, and the stale generation is refused every authority in turn.

func requireGitBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	return path
}

func runGit(t *testing.T, binary, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRealRepo builds a real repository with one commit on main.
func newRealRepo(t *testing.T, binary string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, binary, dir, "init", "--initial-branch=main")
	runGit(t, binary, dir, "config", "user.email", "ao@example.test")
	runGit(t, binary, dir, "config", "user.name", "AO Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, binary, dir, "add", ".")
	runGit(t, binary, dir, "commit", "-m", "seed")
	return dir
}

// §M: a real worktree at path X is removed, an equivalent one is created at the
// same path and branch name, and the stale generation gains no authority over
// it — it cannot adopt, integrate, lock, GC, or mutate the replacement.
func TestRealGitWorktreePathReuseDoesNotGiveTheStaleGenerationAuthority(t *testing.T) {
	binary := requireGitBinary(t)
	f := newPlacementFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	repo := newRealRepo(t, binary)
	worktreePath := filepath.Join(t.TempDir(), "ao-task-checkout")
	const branch = "ao/reused-branch-name"

	// ---- generation 1: a real worktree at path X on branch B ---------------
	first, _, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, binary, repo, "worktree", "add", "-b", branch, worktreePath, "main")
	firstSHA := runGit(t, binary, worktreePath, "rev-parse", "HEAD")
	if ok, err := f.store.RecordExecutionPlacementPreparation(ctx, f.run.ID, "", "",
		first.PlacementGeneration, firstSHA, worktreePath, "wt-gen-1", now); err != nil || !ok {
		t.Fatalf("record generation 1's checkout: ok=%v err=%v", ok, err)
	}

	// ---- generation 1 ends: the checkout and the branch really go away -----
	runGit(t, binary, repo, "worktree", "remove", "--force", worktreePath)
	runGit(t, binary, repo, "branch", "-D", branch)
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("the generation 1 checkout is still on disk: %v", err)
	}

	// ---- generation 2: the SAME path and the SAME branch name --------------
	second, err := f.coord.ReplaceExecutionPlacement(ctx, f.run, f.step, "the checkout was recreated")
	if err != nil {
		t.Fatalf("replace placement: %v", err)
	}
	runGit(t, binary, repo, "worktree", "add", "-b", branch, worktreePath, "main")
	secondSHA := runGit(t, binary, worktreePath, "rev-parse", "HEAD")
	if ok, err := f.store.RecordExecutionPlacementPreparation(ctx, f.run.ID, "", "",
		second.PlacementGeneration, secondSHA, worktreePath, "wt-gen-2", now); err != nil || !ok {
		t.Fatalf("record generation 2's checkout: ok=%v err=%v", ok, err)
	}
	// The ABA is real: the same path, the same branch name, a live checkout.
	if got := runGit(t, binary, worktreePath, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Fatalf("the replacement checkout is on %q, want the reused name %q", got, branch)
	}

	// ---- the stale generation is refused every authority -------------------

	// ADOPT: it is not current, so nothing hands it the placement.
	if f.coord.PlacementIsCurrentForTest(ctx, f.run, first.PlacementGeneration) {
		t.Fatal("the stale generation reports as current after its checkout was recreated")
	}
	if _, err := f.coord.RequireCurrentPlacementForTest(ctx, f.run, first.PlacementGeneration); err == nil {
		t.Fatal("a stale placement generation was granted authority over a reused path")
	}

	// MUTATE / LAUNCH: every state transition CASes on the generation, so a
	// stale holder cannot move the replacement into a state that would let it
	// be written.
	if ok, err := f.store.TransitionExecutionPlacement(ctx, f.run.ID, "", "",
		first.PlacementGeneration, domain.PlacementSelected, domain.PlacementActive, "", "stale holder", now); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("a stale placement generation activated itself over a reused checkout")
	}

	// INTEGRATE: it cannot record a landing for work it does not own.
	if ok, err := f.store.MarkExecutionPlacementIntegrated(ctx, f.run.ID, "", "",
		first.PlacementGeneration, secondSHA, now); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("a stale placement generation recorded an integration for the replacement's commit")
	}

	// GC: retirement is generation-conditioned, so a sweep on the stale
	// holder's behalf removes nothing.
	if retired, err := f.store.RetireSupersededExecutionPlacements(ctx, f.run.ID, "", "",
		first.PlacementGeneration, "stale sweep", now); err != nil {
		t.Fatal(err)
	} else if retired != 0 {
		t.Fatalf("a stale generation retired %d placements", retired)
	}

	// LOCK: a direct-branch lock is keyed on (repo, branch) and is not what
	// protects an isolated placement — the checkout is. What must hold is that
	// the live record still describes the REPLACEMENT, so anything resolving
	// "which checkout is this task's" gets generation 2.
	live := f.live(t)
	if live.PlacementGeneration != second.PlacementGeneration {
		t.Fatalf("the live placement is generation %d, want the replacement %d",
			live.PlacementGeneration, second.PlacementGeneration)
	}
	if live.WorktreeRecordID != "wt-gen-2" {
		t.Fatalf("the live placement names worktree record %q; a reused path must not resolve to the old identity", live.WorktreeRecordID)
	}

	// And the replacement's own authority is intact throughout: it can still do
	// everything the stale one was refused.
	if _, err := f.coord.RequireCurrentPlacementForTest(ctx, f.run, second.PlacementGeneration); err != nil {
		t.Fatalf("the replacement lost its own authority: %v", err)
	}
	if ok, err := f.store.MarkExecutionPlacementIntegrated(ctx, f.run.ID, "", "",
		second.PlacementGeneration, secondSHA, now); err != nil || !ok {
		t.Fatalf("the current placement could not record its own integration: ok=%v err=%v", ok, err)
	}

	// The checkout itself is untouched by any of this: AO refused authority, it
	// did not go and delete somebody's directory to make a point.
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("the replacement checkout was disturbed: %v", err)
	}
}
