package worktree

import (
	"context"
	"fmt"
	"strings"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// Git is the entire git surface the lifecycle manager is allowed to have.
//
// It is deliberately this small. The manager's central promise is that it
// never disturbs the user's own checkout, and the cheapest way to keep a
// promise like that is to make the dangerous operations unreachable: there is
// no Checkout, no Reset, no Stash, no Clean and no Fetch here, so no amount of
// future editing inside the manager can reach for one without first widening
// this interface in a diff a reviewer will see.
type Git interface {
	// ResolveCommit returns the commit SHA rev names in repo.
	ResolveCommit(ctx context.Context, repo, rev string) (string, error)
	// BranchExists reports whether refs/heads/<branch> is present in repo.
	BranchExists(ctx context.Context, repo, branch string) (bool, error)
	// AddWorktreeNewBranch creates path as a worktree of repo on a NEW branch
	// rooted at baseSHA.
	AddWorktreeNewBranch(ctx context.Context, repo, path, branch, baseSHA string) error
	// AddWorktreeExistingBranch creates path as a worktree of repo checking
	// out a branch that already exists. This is the recovery form: an AO
	// branch that survived its directory still holds the task's commits, and
	// recreating the branch from base would throw them away.
	AddWorktreeExistingBranch(ctx context.Context, repo, path, branch string) error
	// RemoveWorktree removes a registered worktree. It must NOT force:
	// uncommitted agent work has to make teardown fail rather than vanish.
	RemoveWorktree(ctx context.Context, repo, path string) error
	// Prune drops registrations whose directories are gone. Nothing on disk is
	// deleted by it.
	Prune(ctx context.Context, repo string) error
}

// ErrForbiddenGitCommand is returned when something asks execGit to run a git
// subcommand outside the allowlist below. It cannot be triggered by any code
// in this package today; it exists so that if that ever changes, the failure
// is a loud error at the boundary instead of a quiet mutation of somebody's
// working tree.
var ErrForbiddenGitCommand = fmt.Errorf("worktree: forbidden git subcommand")

// allowedGitSubcommands is every git subcommand this package may run.
//
// `rev-parse` and `show-ref` only read. `worktree` only ever adds a NEW
// directory, removes one AO created, or prunes registrations for directories
// that no longer exist -- none of which touches the primary checkout's HEAD,
// index, or files. Everything that could (checkout, switch, reset, stash,
// clean, restore, merge, rebase, pull) is absent, and absent is the point.
var allowedGitSubcommands = map[string]bool{
	"rev-parse": true,
	"show-ref":  true,
	"worktree":  true,
}

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

func (g execGit) ResolveCommit(ctx context.Context, repo, rev string) (string, error) {
	out, err := g.run(ctx, repo, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("worktree: %q does not resolve to a commit in %s", rev, repo)
	}
	return sha, nil
}

func (g execGit) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	_, err := g.run(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	// show-ref --quiet exits 1 for "no such ref", which is an answer and not a
	// failure. Anything else (a broken repo, a missing binary) is a failure and
	// must not be reported as "the branch is not there".
	if isExitCode(err, 1) {
		return false, nil
	}
	return false, err
}

func (g execGit) AddWorktreeNewBranch(ctx context.Context, repo, path, branch, baseSHA string) error {
	// baseSHA is a resolved commit, never a branch name, so git configures no
	// upstream tracking for the new branch: an AO worktree must never end up
	// pointed at a remote branch it could be pushed to by accident.
	_, err := g.run(ctx, repo, "worktree", "add", "-b", branch, path, baseSHA)
	return err
}

func (g execGit) AddWorktreeExistingBranch(ctx context.Context, repo, path, branch string) error {
	_, err := g.run(ctx, repo, "worktree", "add", path, branch)
	return err
}

func (g execGit) RemoveWorktree(ctx context.Context, repo, path string) error {
	_, err := g.run(ctx, repo, "worktree", "remove", path)
	return err
}

func (g execGit) Prune(ctx context.Context, repo string) error {
	_, err := g.run(ctx, repo, "worktree", "prune")
	return err
}

func (g execGit) run(ctx context.Context, repo string, args ...string) ([]byte, error) {
	if len(args) == 0 || !allowedGitSubcommands[args[0]] {
		sub := ""
		if len(args) > 0 {
			sub = args[0]
		}
		return nil, fmt.Errorf("%w: %q", ErrForbiddenGitCommand, sub)
	}
	full := append([]string{"-C", repo}, args...)
	cmd := aoprocess.CommandContext(ctx, g.binary, full...)
	// git translates its diagnostics; pin them so an operator's LANG cannot
	// change what this package can read back. Same reason the gitworktree
	// adapter pins it.
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "LANGUAGE=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("worktree: %s %s: %w: %s", g.binary, strings.Join(full, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
