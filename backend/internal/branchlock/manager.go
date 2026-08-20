// Package branchlock owns the durable execution lock that makes direct-branch
// mode safe (Checkpoint 8P-E.11).
//
// Isolated-worktree mode never needed one: two workflows physically could not
// touch the same files. Direct branch trades that isolation for working in the
// user's own repository, so "only one modifying workflow may own this
// repository+branch at a time" has to be stated, stored, and enforced. This
// package is where that happens: it resolves a project into the concrete
// repository+branch pairs a run would write, refuses to start over a human's
// uncommitted work, acquires every pair atomically or none of them, and
// reconciles ownership across daemon restarts.
package branchlock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ErrDirtyRepository reports that a direct-branch run refused to start because
// at least one target repository already holds uncommitted changes AO did not
// create. The offending repositories are carried on DirtyRepositoryError so the
// workflow can name them instead of reporting a bare "blocked".
var ErrDirtyRepository = ports.ErrWorkspaceRepositoryDirty

// DirtyRepositoryError names every repository that blocked an acquisition and
// what it is holding.
type DirtyRepositoryError struct {
	Repositories []ports.WorkspacePreflight
}

func (e DirtyRepositoryError) Error() string {
	names := make([]string, 0, len(e.Repositories))
	for _, r := range e.Repositories {
		names = append(names, fmt.Sprintf("%s (%d uncommitted change(s))", r.RepoPath, len(r.Changes)))
	}
	return "branch lock: repository has uncommitted changes: " + strings.Join(names, ", ")
}

func (e DirtyRepositoryError) Unwrap() error { return ErrDirtyRepository }

// Store is the durable persistence surface. It is satisfied by *sqlite.Store.
type Store interface {
	AcquireBranchLock(ctx context.Context, lock domain.BranchLock) (domain.BranchLock, error)
	GetHeldBranchLock(ctx context.Context, lockKey string) (domain.BranchLock, bool, error)
	ListHeldBranchLocks(ctx context.Context) ([]domain.BranchLock, error)
	ListHeldBranchLocksByProject(ctx context.Context, projectID domain.ProjectID) ([]domain.BranchLock, error)
	ListHeldBranchLocksByRun(ctx context.Context, runID string) ([]domain.BranchLock, error)
	ReleaseBranchLock(ctx context.Context, id, reason string, at time.Time) (bool, error)
	ReleaseBranchLocksByRun(ctx context.Context, runID, reason string, at time.Time) (int64, error)
	RenewBranchLock(ctx context.Context, id, runID, stepID, sessionID string, at time.Time) (bool, error)
	AdoptBranchLock(ctx context.Context, id, ownerToken string, at time.Time) (bool, error)
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
	ListWorkspaceRepos(ctx context.Context, projectID string) ([]domain.WorkspaceRepoRecord, error)
	GetWorkflowRun(ctx context.Context, id string) (domain.WorkflowRun, bool, error)
}

// Preflighter is the read-only repository probe. Optional: a nil Preflighter
// disables the dirty-worktree gate, which is only ever correct in tests that
// have no real repositories at all.
type Preflighter interface {
	PreflightRepository(ctx context.Context, repoPath, branch string) (ports.WorkspacePreflight, error)
}

// Deps configures a Manager.
type Deps struct {
	Store Store
	// Preflight probes repositories before a lock is taken.
	Preflight Preflighter
	// OwnerToken identifies this daemon instance. Reconcile uses it to tell a
	// lock this instance owns from one a previous instance left behind.
	OwnerToken string
	NewID      func() string
	Clock      func() time.Time
	Logger     *slog.Logger
}

// Manager resolves, acquires, releases and reconciles branch locks.
type Manager struct {
	store      Store
	preflight  Preflighter
	ownerToken string
	newID      func() string
	clock      func() time.Time
	log        *slog.Logger
}

// New builds a Manager.
func New(deps Deps) *Manager {
	m := &Manager{
		store:      deps.Store,
		preflight:  deps.Preflight,
		ownerToken: deps.OwnerToken,
		newID:      deps.NewID,
		clock:      deps.Clock,
		log:        deps.Logger,
	}
	if m.clock == nil {
		m.clock = time.Now
	}
	if m.newID == nil {
		n := 0
		m.newID = func() string { n++; return fmt.Sprintf("bl-%d", n) }
	}
	if m.ownerToken == "" {
		m.ownerToken = "daemon-" + m.clock().UTC().Format("20060102T150405.000000000")
	}
	return m
}

// Target is one repository+branch pair a direct-branch run will write.
type Target struct {
	// RepoName is domain.RootWorkspaceRepoName for a single-repo project or a
	// workspace root, otherwise the registered child repo name.
	RepoName string
	RepoPath string
	Branch   string
	Key      string
}

// Targets resolves the repository+branch pairs a direct-branch run for this
// project would modify.
//
// For a workspace project every registered repository is its own target with
// its own configured branch — that is what keeps medusa (main) and
// medusa/backend_node (medusa_back_v2) independent instead of collapsing them
// into one lock, and it is also what stops a change in the child from ever
// being staged by the parent. A child still awaiting `git init`
// (GitStatusNeedsInit) is skipped: there is no repository there to lock yet.
//
// A project not in direct-branch mode has no targets at all: worktree-mode runs
// are isolated by construction and must not start serializing on branches.
func (m *Manager) Targets(ctx context.Context, projectID domain.ProjectID) ([]Target, error) {
	project, ok, err := m.store.GetProject(ctx, string(projectID))
	if err != nil {
		return nil, fmt.Errorf("branch lock: load project %s: %w", projectID, err)
	}
	if !ok {
		return nil, fmt.Errorf("branch lock: project %s not found", projectID)
	}
	if domain.ResolveExecutionMode(project.Kind, project.Config) != domain.ExecutionDirectBranch {
		return nil, nil
	}
	rootBranch := strings.TrimSpace(project.Config.WithDefaults().DefaultBranch)
	if rootBranch == "" {
		return nil, fmt.Errorf("branch lock: project %s has no configured branch", projectID)
	}
	targets := []Target{newTarget(domain.RootWorkspaceRepoName, project.Path, rootBranch)}
	if project.Kind.WithDefault() != domain.ProjectKindWorkspace {
		return targets, nil
	}
	repos, err := m.store.ListWorkspaceRepos(ctx, string(projectID))
	if err != nil {
		return nil, fmt.Errorf("branch lock: list workspace repos for %s: %w", projectID, err)
	}
	for _, repo := range repos {
		if repo.GitStatus.WithDefault() == domain.GitStatusNeedsInit {
			continue
		}
		branch := strings.TrimSpace(repo.DefaultBranch)
		if branch == "" {
			return nil, fmt.Errorf("branch lock: workspace repo %q in project %s has no configured branch", repo.Name, projectID)
		}
		targets = append(targets, newTarget(repo.Name, filepath.Join(project.Path, filepath.FromSlash(repo.RelativePath)), branch))
	}
	return targets, nil
}

func newTarget(name, repoPath, branch string) Target {
	return Target{
		RepoName: name,
		RepoPath: repoPath,
		Branch:   branch,
		Key:      domain.BranchLockKey(repoPath, branch),
	}
}

// AcquireRequest is one run's request to become the writer of a project's
// direct-branch targets.
type AcquireRequest struct {
	ProjectID domain.ProjectID
	RunID     string
	StepID    string
	SessionID string
}

// Acquire takes every lock the project's direct-branch targets require, or none
// of them.
//
// Three properties matter here and each is deliberate:
//
//   - Preflight runs first, across all targets, and a dirty repository aborts
//     before anything is written. Discovering a human's uncommitted work after
//     having already locked two of three repositories would leave the project
//     half-owned by a run that cannot proceed.
//   - Targets are locked in a stable key order. Two runs contending for the
//     same overlapping set therefore always contend in the same order, so they
//     cannot deadlock by each holding what the other needs next.
//   - Any conflict rolls back the locks this call already took. A partially
//     acquired run is never left holding a branch it will not use.
//
// A nil error means the run owns every returned lock. A
// domain.BranchLockConflictError means another run owns at least one target and
// this one must wait; a DirtyRepositoryError means a human must act first.
// Neither is a workflow failure.
func (m *Manager) Acquire(ctx context.Context, req AcquireRequest) ([]domain.BranchLock, error) {
	targets, err := m.Targets(ctx, req.ProjectID)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, nil
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Key < targets[j].Key })

	preflights, err := m.preflightAll(ctx, targets, req.RunID)
	if err != nil {
		return nil, err
	}

	now := m.clock()
	acquired := make([]domain.BranchLock, 0, len(targets))
	for _, target := range targets {
		lock, err := m.store.AcquireBranchLock(ctx, domain.BranchLock{
			ID:             "blk-" + m.newID(),
			LockKey:        target.Key,
			ProjectID:      req.ProjectID,
			RepoPath:       target.RepoPath,
			RepoName:       target.RepoName,
			Branch:         target.Branch,
			WorkflowRunID:  req.RunID,
			WorkflowStepID: req.StepID,
			SessionID:      req.SessionID,
			OwnerToken:     m.ownerToken,
			BaseSHA:        preflights[target.Key].HeadSHA,
			AcquiredAt:     now,
		})
		if err != nil {
			m.rollback(ctx, acquired, "rolled back: could not acquire every target lock")
			return nil, err
		}
		acquired = append(acquired, lock)
	}
	return acquired, nil
}

// preflightAll probes every target and refuses the whole acquisition if any
// repository holds pre-existing uncommitted work. A repository already locked
// by this same run is exempt: its "dirty" state is this run's own in-progress
// work, not a human's, and re-entering a run must not block on the changes it
// itself produced.
func (m *Manager) preflightAll(ctx context.Context, targets []Target, runID string) (map[string]ports.WorkspacePreflight, error) {
	out := make(map[string]ports.WorkspacePreflight, len(targets))
	if m.preflight == nil {
		return out, nil
	}
	var dirty []ports.WorkspacePreflight
	for _, target := range targets {
		pre, err := m.preflight.PreflightRepository(ctx, target.RepoPath, target.Branch)
		if err != nil {
			return nil, fmt.Errorf("branch lock: preflight %s: %w", target.RepoPath, err)
		}
		out[target.Key] = pre
		if !pre.Dirty {
			continue
		}
		holder, found, herr := m.store.GetHeldBranchLock(ctx, target.Key)
		if herr != nil {
			return nil, herr
		}
		if found && holder.WorkflowRunID == runID {
			continue
		}
		dirty = append(dirty, pre)
	}
	if len(dirty) > 0 {
		return nil, DirtyRepositoryError{Repositories: dirty}
	}
	return out, nil
}

func (m *Manager) rollback(ctx context.Context, locks []domain.BranchLock, reason string) {
	now := m.clock()
	for _, lock := range locks {
		if _, err := m.store.ReleaseBranchLock(ctx, lock.ID, reason, now); err != nil && m.log != nil {
			m.log.Warn("branchlock: rollback release failed", "lock", lock.ID, "err", err)
		}
	}
}

// Holder returns the run currently occupying a repository+branch pair.
func (m *Manager) Holder(ctx context.Context, repoPath, branch string) (domain.BranchLock, bool, error) {
	return m.store.GetHeldBranchLock(ctx, domain.BranchLockKey(repoPath, branch))
}

// HeldByProject returns every lock currently occupying one project's
// repositories, for Project Settings and board occupancy.
func (m *Manager) HeldByProject(ctx context.Context, projectID domain.ProjectID) ([]domain.BranchLock, error) {
	return m.store.ListHeldBranchLocksByProject(ctx, projectID)
}

// HeldByRun returns the locks one run currently owns.
func (m *Manager) HeldByRun(ctx context.Context, runID string) ([]domain.BranchLock, error) {
	return m.store.ListHeldBranchLocksByRun(ctx, runID)
}

// Renew refreshes the heartbeat and step/session scope of every lock a run
// holds. Best-effort: a failure to renew never fails the caller, because a
// heartbeat is diagnostic — ownership itself is decided by state, not by
// freshness.
func (m *Manager) Renew(ctx context.Context, runID, stepID, sessionID string) {
	locks, err := m.store.ListHeldBranchLocksByRun(ctx, runID)
	if err != nil {
		if m.log != nil {
			m.log.Warn("branchlock: renew lookup failed", "run", runID, "err", err)
		}
		return
	}
	now := m.clock()
	for _, lock := range locks {
		if _, err := m.store.RenewBranchLock(ctx, lock.ID, runID, stepID, sessionID, now); err != nil && m.log != nil {
			m.log.Warn("branchlock: renew failed", "lock", lock.ID, "err", err)
		}
	}
}

// ReleaseRun frees every lock a run holds. It is called on completion,
// failure, and cancellation alike: whichever way a run ends, it must not leave
// a branch occupied.
func (m *Manager) ReleaseRun(ctx context.Context, runID, reason string) (int64, error) {
	return m.store.ReleaseBranchLocksByRun(ctx, runID, reason, m.clock())
}

// ReconcileResult reports what a restart reconciliation decided.
type ReconcileResult struct {
	Adopted  int
	Released int
	Kept     int
}

// Reconcile makes branch-lock ownership decidable after a daemon restart.
//
// The question a crashed daemon leaves behind is "does this held lock still
// belong to a live run?", and it is answered from durable workflow state, never
// from a timestamp heuristic:
//
//   - The run no longer exists, or is terminal → the lock is stale and is
//     released. Nothing will ever resume it, so holding the branch hostage
//     would be a permanent leak.
//   - The run is still live and the lock carries a previous instance's owner
//     token → the lock is legitimate and is adopted by this instance. It is
//     emphatically not released: the recovered run is about to resume writing
//     that branch, and freeing it would let a second run start writing it too.
//   - The run is live and this instance already owns the lock → kept untouched.
//
// Because the held-lock uniqueness constraint lives in the database, no outcome
// here can produce two owners of the same pair even if reconciliation raced
// with a fresh acquisition.
func (m *Manager) Reconcile(ctx context.Context) (ReconcileResult, error) {
	locks, err := m.store.ListHeldBranchLocks(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}
	now := m.clock()
	var out ReconcileResult
	for _, lock := range locks {
		run, ok, err := m.store.GetWorkflowRun(ctx, lock.WorkflowRunID)
		if err != nil {
			return out, fmt.Errorf("branch lock: reconcile run %s: %w", lock.WorkflowRunID, err)
		}
		switch {
		case !ok:
			if _, rerr := m.store.ReleaseBranchLock(ctx, lock.ID, "stale: workflow run no longer exists", now); rerr != nil {
				return out, rerr
			}
			out.Released++
		case run.State.Terminal():
			if _, rerr := m.store.ReleaseBranchLock(ctx, lock.ID, "stale: workflow run is "+string(run.State), now); rerr != nil {
				return out, rerr
			}
			out.Released++
		case lock.OwnerToken != m.ownerToken:
			if _, aerr := m.store.AdoptBranchLock(ctx, lock.ID, m.ownerToken, now); aerr != nil {
				return out, aerr
			}
			out.Adopted++
		default:
			out.Kept++
		}
	}
	if m.log != nil && (out.Adopted > 0 || out.Released > 0) {
		m.log.Info("branchlock: reconciled", "adopted", out.Adopted, "released", out.Released, "kept", out.Kept)
	}
	return out, nil
}

// IsConflict reports whether err means "another run owns the branch".
func IsConflict(err error) bool { return errors.Is(err, domain.ErrBranchLockHeld) }

// IsDirty reports whether err means "a human's uncommitted work is in the way".
func IsDirty(err error) bool { return errors.Is(err, ErrDirtyRepository) }

// Conflict extracts the conflicting holder from err, if it carries one.
func Conflict(err error) (domain.BranchLock, bool) {
	var conflict domain.BranchLockConflictError
	if errors.As(err, &conflict) {
		return conflict.Holder, true
	}
	return domain.BranchLock{}, false
}

// Dirty extracts the blocking repositories from err, if it carries any.
func Dirty(err error) ([]ports.WorkspacePreflight, bool) {
	var dirty DirtyRepositoryError
	if errors.As(err, &dirty) {
		return dirty.Repositories, true
	}
	return nil, false
}
