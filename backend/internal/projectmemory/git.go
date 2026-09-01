package projectmemory

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// git.go — the two questions project memory asks a checkout about itself.
//
// Both are answered defensively. A repository that is not a git checkout, a
// git that is not installed, a detached head, a fresh repository with no
// commits — every one of those is a normal state for a project AO is asked to
// remember, and none of them is a reason to refuse to remember it. So both
// helpers return empty strings rather than errors, and the empty string is
// recorded honestly as "no commit" wherever it is used (see orNone).
//
// This is deliberately NOT a general git wrapper. AO already has those, at the
// workspace adapters and in internal/integration; adding a third would be the
// speculative abstraction AGENTS.md warns about. These two calls exist because
// project memory needs its provenance stamp and nothing else.

// HeadOf reports the commit and branch a checkout is currently at.
//
// A commit AO cannot read is reported as empty rather than guessed at. That
// matters more than it looks: the commit is a memory item's provenance, and a
// fabricated one would make drift detection compare against a history that
// never existed.
func HeadOf(ctx context.Context, repoPath string) (commit, branch string) {
	return gitOutput(ctx, repoPath, "rev-parse", "HEAD"),
		gitOutput(ctx, repoPath, "branch", "--show-current")
}

func gitOutput(ctx context.Context, repoPath string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// SameRepoPath reports whether two paths name the same checkout.
//
// It compares the resolved, symlink-free forms, because that is what the
// repository identity is derived from: two paths that reach one checkout must
// be recognised as one, or the same repository is indexed twice into two
// memories that never see each other. When a path cannot be resolved — it does
// not exist yet, or a parent is a broken link — the cleaned absolute forms are
// compared instead, which is the strictest answer still available.
func SameRepoPath(a, b string) bool {
	ra, aok := resolveForCompare(a)
	rb, bok := resolveForCompare(b)
	if aok && bok {
		return ra == rb
	}
	return cleanAbs(a) == cleanAbs(b)
}

func resolveForCompare(p string) (string, bool) {
	abs, err := filepath.Abs(strings.TrimSpace(p))
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", false
	}
	return resolved, true
}

func cleanAbs(p string) string {
	abs, err := filepath.Abs(strings.TrimSpace(p))
	if err != nil {
		return filepath.Clean(strings.TrimSpace(p))
	}
	return abs
}

// RepoIdentityOf reads a checkout's durable identity (P2-D §9).
//
// ProjectMemoryRepoID hashes the path, which is what a memory row is ADDRESSED
// by and is deliberately not what it is IDENTIFIED by. A path answers neither
// of the two questions integrity needs:
//
//	the same repository, moved to a new path      -> memory should follow it
//	a different repository, at the old path       -> memory must NOT be inherited
//
// A path-derived id gets the first wrong safely (the moved checkout looks
// unfamiliar and is re-indexed) and the second wrong dangerously: the new
// project silently inherits the old project's conventions, decisions and risks
// with nothing to say it happened.
//
// Both git reads follow this file's existing rule — a fact AO cannot read is
// reported as absent, never guessed — and an identity that could not be
// derived at all is the empty string, which matches nothing including another
// empty string. That is what makes an unidentifiable repository fail closed.
func RepoIdentityOf(ctx context.Context, repoPath string) domain.RepoIdentity {
	remote := gitOutput(ctx, repoPath, "remote", "get-url", "origin")
	if remote == "" {
		// No `origin`, but there may be another remote. `remote -v` is the
		// only listing form git offers, and the first URL in it is a stable
		// choice: the list is sorted by remote name.
		if listed := gitOutput(ctx, repoPath, "remote", "-v"); listed != "" {
			for _, line := range strings.Split(listed, "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					remote = fields[1]
					break
				}
			}
		}
	}
	// The root commit of the current history. `--max-parents=0` names every
	// parentless commit; a repository with several (an unrelated history was
	// merged in) yields them in reverse-chronological order, and the LAST is
	// the oldest — the one that does not change when another root is grafted
	// on later.
	root := ""
	if roots := gitOutput(ctx, repoPath, "rev-list", "--max-parents=0", "HEAD"); roots != "" {
		lines := strings.Split(roots, "\n")
		root = strings.TrimSpace(lines[len(lines)-1])
	}
	return domain.NewRepoIdentity(remote, root)
}
