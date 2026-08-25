package projectmemory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/worktree"
)

// ErrStaleCheck is the sentinel every staleness-check misconfiguration wraps.
// A failure to CHECK is never reported as a clean "still fresh": an item AO
// could not judge keeps whatever verdict it already had.
var ErrStaleCheck = errors.New("projectmemory: staleness check failed")

// CommitResolver is the read-only git surface staleness needs: resolve a
// revision, and ask whether one commit is reachable from another. It is
// satisfied as-is by worktree.Git, which is where AO's allowlisted, read-only
// git access already lives — this package adds no git command of its own.
type CommitResolver interface {
	// ResolveCommit returns the commit SHA rev names in repo.
	ResolveCommit(ctx context.Context, repo, rev string) (string, error)
	// IsAncestor reports whether ancestor is reachable from descendant.
	IsAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error)
}

// FileHasher hashes the file at an absolute path. It returns an error wrapping
// os.ErrNotExist when the file is gone, which is itself a staleness verdict
// rather than a failure.
type FileHasher func(path string) (string, error)

// HashFile is the default FileHasher: the SHA-256 of the file's bytes, hex
// encoded. It is the same hash recorded in Source.FileHash at ingestion, so
// the two are directly comparable.
func HashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // the path is an item's own recorded source file
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verdict is the outcome of checking one item's provenance.
type Verdict struct {
	Stale bool
	// Reason states what stopped holding, and is empty exactly when Stale is
	// false. It is stored on the item, so a reader of stale memory learns why
	// it cannot be trusted rather than only that it cannot.
	Reason string
}

// StaleCheck decides whether an item's provenance still holds in a checkout.
//
// Two things can invalidate a fact, and both are provenance questions rather
// than content questions:
//
//   - The commit it was derived at is no longer reachable from HEAD. The
//     branch was rebased, reset, or abandoned, so the item describes a history
//     this checkout no longer has.
//   - The file it was read from no longer hashes to what it hashed to. The
//     fact was read off a version of the file that is not there any more.
type StaleCheck struct {
	// Repo is the absolute path of the checkout to judge against.
	Repo string
	// Git answers the two reachability questions. Defaults to AO's
	// allowlisted read-only git runner.
	Git CommitResolver
	// Hash hashes a source file. Defaults to HashFile.
	Hash FileHasher
}

// NewGitCheck returns a StaleCheck backed by the git binary, judging against
// the checkout at repo.
func NewGitCheck(repo string) StaleCheck {
	return StaleCheck{Repo: repo, Git: worktree.NewExecGit(""), Hash: HashFile}
}

// evaluator is a StaleCheck with its HEAD resolved once, so a pass over a
// project's whole memory asks git for HEAD a single time.
type evaluator struct {
	check StaleCheck
	head  string
}

func (c StaleCheck) prepare(ctx context.Context) (*evaluator, error) {
	repo := strings.TrimSpace(c.Repo)
	if repo == "" {
		return nil, fmt.Errorf("%w: repository path is required", ErrStaleCheck)
	}
	if !filepath.IsAbs(repo) {
		return nil, fmt.Errorf("%w: repository path %q must be absolute", ErrStaleCheck, repo)
	}
	c.Repo = filepath.Clean(repo)
	if c.Git == nil {
		c.Git = worktree.NewExecGit("")
	}
	if c.Hash == nil {
		c.Hash = HashFile
	}
	head, err := c.Git.ResolveCommit(ctx, c.Repo, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("%w: resolve HEAD in %s: %w", ErrStaleCheck, c.Repo, err)
	}
	return &evaluator{check: c, head: head}, nil
}

// Evaluate judges one item.
func (c StaleCheck) Evaluate(ctx context.Context, item MemoryItem) (Verdict, error) {
	e, err := c.prepare(ctx)
	if err != nil {
		return Verdict{}, err
	}
	return e.evaluate(ctx, item)
}

func (e *evaluator) evaluate(ctx context.Context, item MemoryItem) (Verdict, error) {
	if commit := strings.TrimSpace(item.SourceCommit); commit != "" {
		resolved, err := e.check.Git.ResolveCommit(ctx, e.check.Repo, commit)
		if err != nil {
			// A commit the repository cannot name at all is the strongest form
			// of unreachable: it was rewritten away, or it belongs to a
			// history this checkout never had.
			//nolint:nilerr // a commit the repository cannot name is a staleness verdict, not a failure to reach an answer.
			return Verdict{Stale: true, Reason: fmt.Sprintf("source commit %s is not present in %s", shortSHA(commit), e.check.Repo)}, nil
		}
		reachable, err := e.check.Git.IsAncestor(ctx, e.check.Repo, resolved, e.head)
		if err != nil {
			return Verdict{}, fmt.Errorf("%w: ancestry of %s in %s: %w", ErrStaleCheck, shortSHA(commit), e.check.Repo, err)
		}
		if !reachable {
			return Verdict{Stale: true, Reason: fmt.Sprintf("source commit %s is no longer reachable from HEAD %s", shortSHA(commit), shortSHA(e.head))}, nil
		}
	}

	path := strings.TrimSpace(item.Source.Path)
	recorded := strings.TrimSpace(item.Source.FileHash)
	if path == "" || recorded == "" {
		// Nothing file-shaped was recorded, so there is nothing to compare
		// against. That is not evidence of freshness and not evidence of
		// staleness; the commit check above is the whole verdict.
		return Verdict{}, nil
	}
	full := path
	if !filepath.IsAbs(full) {
		full = filepath.Join(e.check.Repo, filepath.FromSlash(path))
	}
	current, err := e.check.Hash(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Verdict{Stale: true, Reason: fmt.Sprintf("source file %s no longer exists", path)}, nil
		}
		return Verdict{}, fmt.Errorf("%w: hash %s: %w", ErrStaleCheck, full, err)
	}
	if current != recorded {
		return Verdict{Stale: true, Reason: fmt.Sprintf("source file %s changed (recorded %s, now %s)", path, shortSHA(recorded), shortSHA(current))}, nil
	}
	return Verdict{}, nil
}

// RefreshResult reports what a staleness pass over a project found.
type RefreshResult struct {
	Checked int
	// MarkedStale counts items that were fresh and now are not.
	MarkedStale int
	// Cleared counts items whose provenance holds again — the file was
	// reverted, the branch that carried the commit was merged back.
	Cleared int
	Items   []MemoryItem
	// Path is the project file written, empty when no verdict changed.
	Path string
}

// RefreshStaleness re-judges every item of a project and persists any verdict
// that changed.
//
// It deliberately does not move UpdatedAt. Staleness is a derived annotation
// about whether a fact still applies, not a change to the fact itself, and
// conflating the two would make a routine staleness sweep look like every item
// had been re-learned.
func (s *Store) RefreshStaleness(ctx context.Context, project string, check StaleCheck) (RefreshResult, error) {
	e, err := check.prepare(ctx)
	if err != nil {
		return RefreshResult{}, err
	}
	file, err := s.load(project)
	if err != nil {
		return RefreshResult{}, err
	}
	result := RefreshResult{Checked: len(file.Items)}
	dirty := false
	for i, item := range file.Items {
		if err := ctx.Err(); err != nil {
			return RefreshResult{}, err
		}
		verdict, err := e.evaluate(ctx, item)
		if err != nil {
			return RefreshResult{}, err
		}
		if verdict.Stale == item.Stale && verdict.Reason == item.StaleReason {
			continue
		}
		if verdict.Stale && !item.Stale {
			result.MarkedStale++
		}
		if !verdict.Stale && item.Stale {
			result.Cleared++
		}
		item.Stale = verdict.Stale
		item.StaleReason = verdict.Reason
		file.Items[i] = item
		dirty = true
	}
	result.Items = file.Items
	if !dirty {
		return result, nil
	}
	path, err := s.save(file, s.now().UTC())
	if err != nil {
		return RefreshResult{}, err
	}
	result.Path = path
	return result, nil
}

func shortSHA(sha string) string {
	trimmed := strings.TrimSpace(sha)
	if len(trimmed) <= 12 {
		return trimmed
	}
	return trimmed[:12]
}
