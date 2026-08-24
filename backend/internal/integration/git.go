package integration

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// Git is the entire git surface the Integration Coordinator is allowed to
// have. It is deliberately split in two halves, and the split is the safety
// property rather than a stylistic choice:
//
//   - The read methods and CompareAndSetBranch take a REPOSITORY path. None of
//     them writes a file: reads are reads, and CompareAndSetBranch moves a ref
//     with git update-ref, which touches .git and never the working tree.
//
//   - Every method that can put a file on disk -- the three replay operations,
//     their continue/abort, the resolution write, the two checkouts -- takes a
//     WORKTREE path. The coordinator refuses a request whose worktree is the
//     repository itself (see Request.validate), so no mutation in this package
//     can reach the checkout a human is working in.
//
// The exclusion that makes CompareAndSetBranch safe is the branch lock, not
// this interface: a target_integration lock and a direct_branch lock share one
// lock key, so nothing else can be writing the target branch while an
// integration moves it.
type Git interface {
	// ResolveCommit returns the commit SHA rev names in dir.
	ResolveCommit(ctx context.Context, dir, rev string) (string, error)
	// ResolveCommitIfExists is ResolveCommit for a ref that legitimately may
	// not exist yet: the FIRST integration onto a master run's AO-owned ref
	// happens when nothing has ever been integrated, so there is no commit to
	// read and that is not an error. exists=false means "the ref is absent",
	// which is a different fact from "the repository could not be read", and
	// only the first is safe to treat as an empty target.
	ResolveCommitIfExists(ctx context.Context, dir, rev string) (sha string, exists bool, err error)
	// IsAncestor reports whether ancestor is reachable from descendant. It is
	// how "does the target still contain everything the source was built on"
	// is answered, and therefore how fast-forward applicability is decided.
	IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error)
	// MergeBase returns the best common ancestor of a and b, or "" when they
	// share no history at all. The empty answer is an answer rather than an
	// error: two branches with no common ancestor cannot be replayed onto each
	// other by any of the strategies here, and that is a situation for a person
	// rather than a broken repository.
	MergeBase(ctx context.Context, dir, a, b string) (string, error)
	// HasMergeCommits reports whether the range base..head contains a merge.
	// A range that does is one git rebase would silently flatten, which is
	// what disqualifies rebase as a strategy for it.
	HasMergeCommits(ctx context.Context, dir, base, head string) (bool, error)
	// PatchIdentity returns a stable identity for the change from..to: the
	// lines it adds and removes, in the files it touches, with everything that
	// merely describes WHERE they sit removed.
	//
	// It is how "is what this task contributes still the same change" is
	// answered after a replay, which is what a carried-over review approval
	// stands or falls on. Like every other read here it takes a repository
	// path: a worktree shares the repository's objects, so a replayed commit
	// is readable from either, and reading from the repository keeps the
	// question read-only.
	PatchIdentity(ctx context.Context, dir, from, to string) (string, error)

	// Rebase replays the worktree's checked-out branch onto onto. It returns
	// ErrReplayConflict when it stopped on a conflict, leaving the rebase in
	// progress so the conflict can be inspected, resolved and continued.
	Rebase(ctx context.Context, worktree, onto string) error
	// CherryPick STAGES head's cumulative change relative to the worktree's
	// current HEAD, without committing, with the same conflict contract as
	// Rebase. It is a squashed replay rather than a per-commit one because the
	// case it exists for is a history git cannot replay commit by commit -- one
	// containing a merge, which `git cherry-pick` refuses outright and `git
	// rebase` silently flattens. Staging the net change instead keeps every
	// line of the task's work and says plainly, in one commit, that the shape
	// of its history did not survive.
	CherryPick(ctx context.Context, worktree, head string) error
	// Merge creates a merge commit of head into the worktree's current HEAD,
	// with the same conflict contract as Rebase.
	Merge(ctx context.Context, worktree, head, message string) error
	// Commit commits whatever is staged, and is how a CherryPick is finished.
	Commit(ctx context.Context, worktree, message string) error
	// ContinueReplay resumes op after its conflicts have been staged.
	ContinueReplay(ctx context.Context, worktree string, op ReplayOp) error
	// AbortReplay returns the worktree to the state it was in before op
	// started. Callers treat it as best-effort: it is only ever reached on a
	// path where the integration's decision has already been made, and an
	// operation that had already finished has nothing left to abort.
	AbortReplay(ctx context.Context, worktree string, op ReplayOp) error

	// UnmergedPaths lists the paths git currently reports as conflicted.
	UnmergedPaths(ctx context.Context, worktree string) ([]string, error)
	// StageBlob returns the content of one merge stage of path: 1 is the
	// common ancestor, 2 and 3 are the two sides. The bool is false when that
	// stage does not exist, which is itself an answer -- a missing ancestor
	// means both sides created the file, and there is nothing to append to.
	StageBlob(ctx context.Context, worktree, path string, stage int) ([]byte, bool, error)
	// WriteResolution writes content at path and stages it as resolved.
	WriteResolution(ctx context.Context, worktree, path string, content []byte) error

	// CheckoutDetached puts the worktree on a detached HEAD at rev. Only ever
	// called on AO's own task worktree; see the interface comment.
	CheckoutDetached(ctx context.Context, worktree, rev string) error
	// CheckoutBranch puts the worktree back on branch, which is how a
	// cherry-pick or merge staging area is undone after the ref has moved.
	CheckoutBranch(ctx context.Context, worktree, branch string) error

	// CompareAndSetRef moves ref from expected to next, and fails if the ref is
	// not at expected. This is the atomic step: the whole integration lands in
	// one ref update or not at all, and a target that moved between the read
	// and the write loses rather than being overwritten.
	//
	// It takes a full ref name rather than a branch because not every
	// integration target is a branch: a master run accumulates its tasks on an
	// AO-owned ref under refs/ao/, and that ref is contended by exactly the
	// same tasks, for exactly the same reason, as a branch would be.
	CompareAndSetRef(ctx context.Context, repo, ref, next, expected string) error
}

// ReplayOp names the in-progress git operation a continue or abort refers to.
// git spells each one differently (`rebase --continue`, `cherry-pick
// --continue`, but `commit --no-edit` for a merge), so the distinction has to
// survive as far as the runner.
type ReplayOp string

// The three replay operations. ReplayCherryPick is a squashed replay rather
// than git's per-commit cherry-pick; see Git.CherryPick for why.
const (
	ReplayRebase     ReplayOp = "rebase"
	ReplayCherryPick ReplayOp = "cherry-pick"
	ReplayMerge      ReplayOp = "merge"
)

// ErrReplayConflict reports that a replay stopped with conflicts staged in the
// index. It is not a failure of the operation: it is the operation asking
// whether the conflict can be resolved.
var ErrReplayConflict = errors.New("integration: replay stopped on conflict")

// execGit is the real Git, running the git binary.
type execGit struct{ binary string }

// NewExecGit returns a Git backed by the git binary at path (defaulting to
// "git" on PATH).
func NewExecGit(binary string) Git {
	if strings.TrimSpace(binary) == "" {
		binary = "git"
	}
	return execGit{binary: binary}
}

func (g execGit) ResolveCommit(ctx context.Context, dir, rev string) (string, error) {
	out, err := g.run(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("integration: %q does not resolve to a commit in %s", rev, dir)
	}
	return sha, nil
}

func (g execGit) ResolveCommitIfExists(ctx context.Context, dir, rev string) (string, bool, error) {
	// `rev-parse --verify --quiet` is precisely this question: it exits
	// non-zero with no output when the ref is absent, and only prints on
	// success. Distinguishing that from a genuine failure is why this cannot
	// simply swallow ResolveCommit's error.
	out, err := g.run(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	sha := strings.TrimSpace(string(out))
	if err != nil {
		if sha == "" && isMissingRefExit(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if sha == "" {
		return "", false, nil
	}
	return sha, true, nil
}

// isMissingRefExit reports whether git's failure was the plain "no such ref"
// exit rather than a broken repository. An unreadable repository must never be
// mistaken for an empty target: the first would integrate onto nothing, the
// second would create a ref in a repository AO cannot actually use.
func isMissingRefExit(err error) bool {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode() == 1
	}
	return false
}

func (g execGit) IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	_, err := g.run(ctx, dir, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	// Exit 1 is "no", which is an answer. Anything else is a broken repository
	// or a bad revision, and reporting that as "not an ancestor" would make the
	// coordinator choose a rewriting strategy on the strength of an error.
	if isExitCode(err, 1) {
		return false, nil
	}
	return false, err
}

func (g execGit) MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	out, err := g.run(ctx, dir, "merge-base", a, b)
	if err != nil {
		// Exit 1 is "these commits have no common ancestor", which is an
		// answer. Anything else is a real failure and must not be reported as
		// unrelated histories.
		if isExitCode(err, 1) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (g execGit) HasMergeCommits(ctx context.Context, dir, base, head string) (bool, error) {
	out, err := g.run(ctx, dir, "rev-list", "--merges", "--max-count=1", base+".."+head)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func (g execGit) PatchIdentity(ctx context.Context, dir, from, to string) (string, error) {
	// -U0 asks git for the change with no surrounding context, which is the
	// whole point: context is the code AROUND this task's change, and after a
	// replay that code belongs to the dependency that landed first -- reviewed
	// on its own way in, and not this task's contribution to re-review.
	// --no-ext-diff so a user's configured external differ cannot change what
	// AO hashes, and --find-renames so a rename is one identity rather than a
	// delete plus an add that happens to look different after a replay.
	out, err := g.run(ctx, dir, "diff", "--no-color", "--no-ext-diff", "--find-renames", "-U0", from, to)
	if err != nil {
		return "", err
	}
	return patchIdentity(out), nil
}

func (g execGit) Rebase(ctx context.Context, worktree, onto string) error {
	return g.replay(ctx, worktree, "rebase", onto)
}

func (g execGit) CherryPick(ctx context.Context, worktree, head string) error {
	// --squash performs the three-way merge and stages the result without
	// creating a commit or recording head as a parent, which is exactly "apply
	// this branch's changes here" with no claim about history.
	return g.replay(ctx, worktree, "merge", "--squash", head)
}

func (g execGit) Merge(ctx context.Context, worktree, head, message string) error {
	return g.replay(ctx, worktree, "merge", "--no-ff", "--no-edit", "-m", message, head)
}

func (g execGit) Commit(ctx context.Context, worktree, message string) error {
	_, err := g.run(ctx, worktree, "commit", "-m", message)
	return err
}

func (g execGit) ContinueReplay(ctx context.Context, worktree string, op ReplayOp) error {
	switch op {
	case ReplayMerge:
		// A conflicted merge has no --continue; committing the resolved index
		// is what finishes it.
		return g.replay(ctx, worktree, "commit", "--no-edit")
	case ReplayCherryPick:
		// A squashed replay is a single step: once its conflicts are staged
		// there is nothing left to resume, and the caller's Commit finishes it.
		return nil
	case ReplayRebase:
		return g.replay(ctx, worktree, "rebase", "--continue")
	default:
		return fmt.Errorf("integration: unknown replay op %q", op)
	}
}

func (g execGit) AbortReplay(ctx context.Context, worktree string, op ReplayOp) error {
	switch op {
	case ReplayCherryPick:
		// A squashed replay leaves its state in the index and the working tree
		// rather than in a sequencer directory, so there is no --abort for it.
		// This resets AO's OWN task worktree, never the user's checkout.
		_, err := g.run(ctx, worktree, "reset", "--hard")
		return err
	case ReplayRebase, ReplayMerge:
		// git exits non-zero when there is no operation in progress. That is
		// already the state the abort was trying to reach, and the sole caller
		// treats the result as best-effort, so the error is returned as-is
		// rather than being told apart from a real failure by parsing prose.
		_, err := g.run(ctx, worktree, string(op), "--abort")
		return err
	default:
		return fmt.Errorf("integration: unknown replay op %q", op)
	}
}

// replay runs one git command whose failure has to be split into "stopped on a
// conflict" and "genuinely failed". Everything that leaves unmerged paths in
// the index is the former, whatever git's exit code was.
func (g execGit) replay(ctx context.Context, worktree string, args ...string) error {
	_, err := g.run(ctx, worktree, args...)
	if err == nil {
		return nil
	}
	paths, perr := g.UnmergedPaths(ctx, worktree)
	if perr == nil && len(paths) > 0 {
		return fmt.Errorf("%w: %s", ErrReplayConflict, strings.Join(paths, ", "))
	}
	return err
}

func (g execGit) UnmergedPaths(ctx context.Context, worktree string) ([]string, error) {
	out, err := g.run(ctx, worktree, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

func (g execGit) StageBlob(ctx context.Context, worktree, path string, stage int) ([]byte, bool, error) {
	out, err := g.run(ctx, worktree, "show", fmt.Sprintf(":%d:%s", stage, path))
	if err != nil {
		// A stage that is not in the index is an absence, not a breakage: it
		// means that side added or deleted the file rather than editing it.
		if isExitCode(err, 128) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return out, true, nil
}

func (g execGit) WriteResolution(ctx context.Context, worktree, path string, content []byte) error {
	// hash-object + update-index writes the resolved blob without going
	// through the filesystem twice, and without ever needing a shell redirect.
	full, err := securePath(worktree, path)
	if err != nil {
		return err
	}
	if err := writeFile(full, content); err != nil {
		return err
	}
	_, err = g.run(ctx, worktree, "add", "--", path)
	return err
}

func (g execGit) CheckoutDetached(ctx context.Context, worktree, rev string) error {
	_, err := g.run(ctx, worktree, "checkout", "--detach", rev)
	return err
}

func (g execGit) CheckoutBranch(ctx context.Context, worktree, branch string) error {
	_, err := g.run(ctx, worktree, "checkout", branch)
	return err
}

func (g execGit) CompareAndSetRef(ctx context.Context, repo, ref, next, expected string) error {
	// An empty expected value is git's own spelling of "this ref must not
	// exist yet", which is what the first integration onto a fresh AO ref
	// needs, and is still a compare-and-set rather than a blind write.
	_, err := g.run(ctx, repo, "update-ref", ref, next, expected)
	return err
}

func (g execGit) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := aoprocess.CommandContext(ctx, g.binary, full...)
	// git translates its diagnostics; pin them so an operator's LANG cannot
	// change what this package can read back, the same way internal/worktree
	// and the gitworktree adapter already do.
	//
	// The two editor variables matter just as much: `rebase --continue` and
	// `commit` open an editor for the commit message by default, and this
	// process has no terminal to open one on. Pointing both at `true` makes
	// every message the one git already prepared, deterministically, instead of
	// leaving the behavior to whatever EDITOR the daemon inherited.
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "LANGUAGE=", "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true")
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		return out, fmt.Errorf("integration: %s %s: %w: %s", g.binary, strings.Join(full, " "), err, stderr)
	}
	return out, nil
}

// isExitCode reports whether err (or anything it wraps) is a process exit with
// the given status.
func isExitCode(err error, code int) bool {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode() == code
	}
	return false
}
