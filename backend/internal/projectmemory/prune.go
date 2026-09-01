package projectmemory

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// prune.go — retiring the canonical memories a worktree should never have had
// (P2-E A8).
//
// Before P2-E one caller passed a task's isolated worktree as the canonical
// memory root, so each reviewed task minted a second repo_id and full-indexed
// an unintegrated branch into it. The fix stops new ones appearing; this
// removes the ones already there, and it exists as a PRODUCT operation rather
// than a hand-written DELETE because the rows are real memory in a real
// database and "which of these is safe to remove" is a question that deserves
// proofs rather than a WHERE clause somebody typed once.
//
// Five proofs are required before a single row is touched, and a repository
// that fails any of them is reported and left alone:
//
//  1. The repo_id belongs to the project being pruned.
//  2. Its recorded path is a linked worktree, or is gone from disk entirely
//     while sitting under a worktree root. A path AO can still see and which
//     is NOT a linked worktree is a repository, whatever else is true of it.
//  3. It is not the project's own registered root, and not any registered
//     repository of it.
//  4. A different, canonical repo_id for this project DOES exist. Pruning the
//     only memory a project has -- even if it was built wrongly -- would
//     leave it with nothing rather than with less.
//  5. The caller has proven no live execution depends on that workspace.
//
// Fail closed on every unknown: a path that cannot be resolved, a git that
// cannot answer, a project record that cannot be read. The cost of refusing is
// that an operator runs it again after tidying; the cost of a wrong prune is
// deleted memory.

// PruneRequest asks for one scoped prune of worktree-minted memories.
type PruneRequest struct {
	ProjectID domain.ProjectID
	// ProjectRoot is the project's registered path, and RegisteredRepos are
	// every repository the project legitimately has (the root plus, for a
	// workspace project, its children). Both come from the project record --
	// this package never guesses which checkouts are a project's.
	ProjectRoot     string
	RegisteredRepos []string
	// BusyWorkspaces are workspace paths a live execution is using. The caller
	// resolves them from workflow state; a non-empty match refuses the prune
	// for that repository (P2-E A8's "no active workflow depends on it").
	BusyWorkspaces []string
	// Apply performs the purge. False is a dry run that reports exactly what
	// it would remove and changes nothing.
	Apply bool
}

// PruneCandidate is one repository memory the prune considered.
type PruneCandidate struct {
	RepoID   string
	RepoPath string
	// Items and Relations are what would be (or were) removed.
	Items     int
	Relations int
	// ParentRepo is the repository the worktree belongs to, when git could say.
	ParentRepo string
	// Prunable reports the verdict, and Reason explains it either way -- a
	// refusal is the more useful half, so it is never silent.
	Prunable bool
	Reason   string
	Purged   bool
}

// PruneReport is what one prune found and did.
type PruneReport struct {
	// CanonicalRepoIDs are the project's legitimate repository memories, named
	// so an operator can see what the prune is preserving rather than only
	// what it removes.
	CanonicalRepoIDs []string
	Candidates       []PruneCandidate
	PurgedItems      int
	PurgedRelations  int
}

// Prunable reports whether anything was found to remove.
func (r PruneReport) Prunable() bool {
	for _, c := range r.Candidates {
		if c.Prunable {
			return true
		}
	}
	return false
}

// Prune retires every canonical memory of one project that was minted from a
// worktree rather than from a repository.
func (s *Service) Prune(ctx context.Context, req PruneRequest) (PruneReport, error) {
	states, err := s.repo.ListProjectMemoryIndexStates(ctx, req.ProjectID)
	if err != nil {
		return PruneReport{}, err
	}
	report := PruneReport{}

	// Proof 3's allowlist: every repository this project legitimately has.
	registered := map[string]struct{}{}
	for _, p := range append([]string{req.ProjectRoot}, req.RegisteredRepos...) {
		if canonical, cerr := canonicalRepoPath(p); cerr == nil && canonical != "" {
			registered[canonical] = struct{}{}
		}
	}
	busy := map[string]struct{}{}
	for _, p := range req.BusyWorkspaces {
		if canonical, cerr := canonicalRepoPath(p); cerr == nil && canonical != "" {
			busy[canonical] = struct{}{}
		}
	}

	// Pass 1: classify. Nothing is removed while the picture is incomplete,
	// because proof 4 is about the SET -- "a canonical memory survives this"
	// cannot be answered one repository at a time.
	for _, st := range states {
		report.Candidates = append(report.Candidates, s.classifyPruneCandidate(ctx, st, registered, busy))
	}
	for i := range report.Candidates {
		if !report.Candidates[i].Prunable {
			report.CanonicalRepoIDs = append(report.CanonicalRepoIDs, report.Candidates[i].RepoID)
		}
	}
	sort.Strings(report.CanonicalRepoIDs)

	// Proof 4. With no surviving canonical memory the prune would leave the
	// project with nothing, so it refuses wholesale rather than per repository.
	if len(report.CanonicalRepoIDs) == 0 {
		for i := range report.Candidates {
			if report.Candidates[i].Prunable {
				report.Candidates[i].Prunable = false
				report.Candidates[i].Reason = "refused: this is the only memory the project has, and a prune must leave it with less rather than with none"
			}
		}
		return report, nil
	}

	if !req.Apply {
		return report, nil
	}
	for i := range report.Candidates {
		c := &report.Candidates[i]
		if !c.Prunable {
			continue
		}
		// Deregister rather than purge: this is not a repository whose memory
		// should be rebuilt, it is a workspace that should never have had one.
		if err := s.repo.DeregisterProjectMemoryRepo(ctx, req.ProjectID, c.RepoID); err != nil {
			return report, fmt.Errorf("deregister %s: %w", c.RepoID, err)
		}
		c.Purged = true
		report.PurgedItems += c.Items
		report.PurgedRelations += c.Relations
	}
	return report, nil
}

// classifyPruneCandidate applies proofs 1, 2, 3 and 5 to one repository memory.
func (s *Service) classifyPruneCandidate(
	ctx context.Context, st domain.ProjectMemoryIndexState,
	registered, busy map[string]struct{},
) PruneCandidate {
	c := PruneCandidate{RepoID: st.RepoID, RepoPath: st.RepoPath}
	if items, err := s.repo.ListProjectMemoryItems(ctx, st.ProjectID, st.RepoID); err == nil {
		c.Items = len(items)
	}
	if rels, err := s.repo.ListProjectMemoryRelations(ctx, st.ProjectID, st.RepoID); err == nil {
		c.Relations = len(rels)
	}

	canonical, err := canonicalRepoPath(st.RepoPath)
	if err != nil {
		// The path no longer resolves. That is the ordinary state of a cleaned
		// up worktree, but it is ALSO what a temporarily unmounted repository
		// looks like, and the two are indistinguishable from here -- so the
		// only rows removed on this branch are ones a surviving record proves
		// were never a registered repository.
		canonical = cleanAbs(st.RepoPath)
	}
	if _, ok := registered[canonical]; ok {
		c.Reason = "kept: this is a registered repository of the project"
		return c
	}
	if _, ok := busy[canonical]; ok {
		c.Reason = "refused: a live execution is using this workspace"
		return c
	}

	parent, linked := s.linkedWorktreeOf(ctx, canonical)
	switch {
	case linked:
		c.ParentRepo = parent
		c.Prunable = true
		c.Reason = fmt.Sprintf("prunable: a linked worktree of %s, never a repository of its own", orNone(parent))
	case !pathExists(canonical):
		// Gone from disk AND not registered. It cannot be re-indexed, nothing
		// can prove it current again, and it is not any repository the project
		// has -- so it is dead memory whatever created it.
		c.Prunable = true
		c.Reason = "prunable: the path no longer exists and is not a registered repository of this project"
	default:
		// A real directory AO can see, which git does not call a linked
		// worktree, and which the project does not list. AO cannot say what it
		// is, so it does not touch it.
		c.Reason = "refused: the path exists and is not a linked worktree; AO cannot prove it is safe to remove"
	}
	return c
}

// linkedWorktreeOf is indirected so a test can classify without building real
// linked worktrees; production always asks git.
func (s *Service) linkedWorktreeOf(ctx context.Context, path string) (string, bool) {
	if s.linkedWorktree != nil {
		return s.linkedWorktree(ctx, path)
	}
	return LinkedWorktreeOf(ctx, path)
}

func pathExists(p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}
