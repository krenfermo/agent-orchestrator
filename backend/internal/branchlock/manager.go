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
	"sync"
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
	// GetSession resolves the owner of a session-owned lock (Checkpoint
	// 8P-E.14). Reconciliation needs it for exactly the reason it needs
	// GetWorkflowRun: a lock is only legitimate while its owner can still write
	// the branch, and for an ordinary task that fact lives on the session row.
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
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
	// Classifier resolves what a needs_attention lock owner's stop means
	// (Checkpoint 8P-E.13A, retention.go). Optional: with none wired, such an
	// owner always keeps its lock, exactly as before this checkpoint.
	Classifier OwnerClassifier
	NewID      func() string
	Clock      func() time.Time
	Logger     *slog.Logger
}

// Manager resolves, acquires, releases and reconciles branch locks.
type Manager struct {
	store      Store
	preflight  Preflighter
	ownerToken string
	classifier OwnerClassifier
	newID      func() string
	clock      func() time.Time
	log        *slog.Logger

	// lastOwnerMu guards lastOwner.
	lastOwnerMu sync.Mutex
	// lastOwner remembers, per lock key, the owner that most recently held it
	// (Checkpoint 8P-E.14A). It exists for one narrow purpose: a task session's
	// ownership is now turn-scoped, so between turns the branch is unlocked
	// while the session's own uncommitted work is still sitting in the working
	// tree. Without this, the dirty-repository preflight would refuse to give
	// that session its branch back for its own follow-up turn, blaming it for
	// changes it made itself.
	//
	// Deliberately in memory and deliberately not consulted for anyone else: a
	// different owner never inherits the exemption, and a daemon restart forgets
	// it, degrading to the strict "a human must decide about these changes"
	// refusal. Both are the fail-safe direction.
	lastOwner map[string]string
}

// SetClassifier wires the owner classifier after construction.
//
// Late binding is not an aesthetic choice: the classifier is the workflow
// coordinator, and the coordinator needs the branch-lock manager to be built
// first. Rather than split one of them in half, the daemon builds the manager,
// builds the coordinator with it, and hands the classifier back here before the
// first reconcile pass. Calling it is optional; not calling it degrades to the
// conservative "a stopped owner keeps its lock" behavior.
func (m *Manager) SetClassifier(c OwnerClassifier) { m.classifier = c }

// New builds a Manager.
func New(deps Deps) *Manager {
	m := &Manager{
		store:      deps.Store,
		preflight:  deps.Preflight,
		ownerToken: deps.OwnerToken,
		classifier: deps.Classifier,
		newID:      deps.NewID,
		clock:      deps.Clock,
		log:        deps.Logger,
		lastOwner:  map[string]string{},
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

// AcquireRequest is one owner's request to become the writer of a project's
// direct-branch targets.
//
// Exactly one of RunID and SessionID identifies the owner (Checkpoint
// 8P-E.14). An autonomous workflow passes RunID, and SessionID is only the
// current step's worker; an ordinary task passes SessionID alone and owns the
// branch as itself. Both forms contend for the same lock keys, which is the
// whole point: a task and a workflow targeting one repository+branch must
// serialize rather than both start writing it.
type AcquireRequest struct {
	ProjectID domain.ProjectID
	RunID     string
	StepID    string
	SessionID string
	// Kind defaults to direct_branch for backwards compatibility. Isolated
	// task and integration callers supply the exact concrete target below.
	Kind     domain.BranchLockOwnershipKind
	RepoName string
	RepoPath string
	Branch   string
}

// owner returns the identity the acquisition will be recorded under, or "" if
// the request names neither a run nor a session.
func (r AcquireRequest) owner() string {
	return domain.BranchLock{WorkflowRunID: r.RunID, SessionID: r.SessionID}.OwnerKey()
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
	kind := req.Kind.WithDefault()
	var targets []Target
	var err error
	if kind == domain.BranchLockOwnershipDirectBranch {
		targets, err = m.Targets(ctx, req.ProjectID)
	} else {
		if strings.TrimSpace(req.RepoPath) == "" || strings.TrimSpace(req.Branch) == "" {
			return nil, fmt.Errorf("branch lock: %s acquisition requires repo path and branch", kind)
		}
		name := req.RepoName
		if name == "" {
			name = domain.RootWorkspaceRepoName
		}
		targets = []Target{newTarget(name, req.RepoPath, req.Branch)}
	}
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, nil
	}
	// An owner-less acquisition could never be released by run or by session,
	// and would compare equal to no holder, so it would leak the branch
	// permanently. Refuse it here rather than writing an unreleasable row.
	owner := req.owner()
	if owner == "" {
		return nil, fmt.Errorf("branch lock: acquire for project %s names neither a workflow run nor a session", req.ProjectID)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Key < targets[j].Key })

	preflights := map[string]ports.WorkspacePreflight{}
	// Only direct ownership touches the user's primary worktree and therefore
	// needs the dirty-worktree gate. Task workspaces are private; integration
	// ownership protects a ref update rather than editing the primary checkout.
	if kind == domain.BranchLockOwnershipDirectBranch {
		preflights, err = m.preflightAll(ctx, targets, owner)
	}
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
			OwnershipKind:  kind,
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
		m.rememberOwner(target.Key, owner)
		acquired = append(acquired, lock)
	}
	return acquired, nil
}

// ReacquireForSession takes the locks a task session needs for a new turn
// (Checkpoint 8P-E.14A).
//
// It differs from Acquire in the two ways a turn start differs from a task
// start:
//
//   - It is a no-op when the session already holds every target. A turn start
//     is reported by a hook on every prompt, and a task that never released
//     (because its previous turn is still open, or because the project only has
//     one repository and nothing happened in between) must not pay a preflight
//     for each one.
//   - It is a no-op when a workflow run owns the targets and this session is
//     that run's current worker. The run owns the branch on the worker's behalf;
//     the worker asking for it in its own name would contend with its own run
//     and log a conflict on every prompt it submits.
//
// Everything else — the dirty preflight, the conflict error, the all-or-nothing
// acquisition — is Acquire's, unchanged.
func (m *Manager) ReacquireForSession(ctx context.Context, projectID domain.ProjectID, sessionID string) ([]domain.BranchLock, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	targets, err := m.Targets(ctx, projectID)
	if err != nil || len(targets) == 0 {
		return nil, err
	}
	owner := domain.BranchLock{SessionID: sessionID}.OwnerKey()
	alreadyOurs := 0
	for _, target := range targets {
		holder, found, herr := m.store.GetHeldBranchLock(ctx, target.Key)
		if herr != nil {
			return nil, herr
		}
		if !found {
			continue
		}
		if holder.OwnerKey() == owner {
			alreadyOurs++
			continue
		}
		if !holder.SessionOwned() && holder.SessionID == sessionID {
			return nil, nil
		}
	}
	if alreadyOurs == len(targets) {
		return nil, nil
	}
	return m.Acquire(ctx, AcquireRequest{ProjectID: projectID, SessionID: sessionID})
}

// rememberOwner records who last held a lock key, for the turn-boundary dirty
// exemption in preflightAll.
func (m *Manager) rememberOwner(lockKey, owner string) {
	if lockKey == "" || owner == "" {
		return
	}
	m.lastOwnerMu.Lock()
	m.lastOwner[lockKey] = owner
	m.lastOwnerMu.Unlock()
}

// heldLastBy reports whether owner is the most recent holder this daemon
// instance recorded for lockKey.
func (m *Manager) heldLastBy(lockKey, owner string) bool {
	if lockKey == "" || owner == "" {
		return false
	}
	m.lastOwnerMu.Lock()
	defer m.lastOwnerMu.Unlock()
	return m.lastOwner[lockKey] == owner
}

// preflightAll probes every target and refuses the whole acquisition if any
// repository holds pre-existing uncommitted work. A repository already locked
// by this same owner is exempt: its "dirty" state is that owner's own
// in-progress work, not a human's, and re-entering must not block on the
// changes it itself produced.
//
// The same exemption extends across a turn boundary to an owner that held this
// exact lock key most recently and gave it back (see Manager.lastOwner): a task
// session's ownership is turn-scoped now, so its own uncommitted work outlives
// its lock, and refusing it the branch for its own follow-up turn would blame
// it for its own changes. Only the immediately previous owner qualifies, so a
// second task still meets the full refusal.
func (m *Manager) preflightAll(ctx context.Context, targets []Target, owner string) (map[string]ports.WorkspacePreflight, error) {
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
		if found && owner != "" && holder.OwnerKey() == owner {
			continue
		}
		if !found && m.heldLastBy(target.Key, owner) {
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

// HeldBySession returns the locks one ordinary task session owns in its own
// right (Checkpoint 8P-E.14). Locks a workflow holds while this session happens
// to be its current worker are deliberately excluded: they belong to the run,
// and the run outlives the session.
//
// It filters the full held set in Go rather than adding a session-scoped query.
// The held set is bounded by the number of repositories currently being
// written, which is a handful even on a busy install, so an index and a
// migration would buy nothing over one scan.
func (m *Manager) HeldBySession(ctx context.Context, sessionID string) ([]domain.BranchLock, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	locks, err := m.store.ListHeldBranchLocks(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.BranchLock
	for _, lock := range locks {
		if lock.SessionOwned() && lock.SessionID == sessionID {
			out = append(out, lock)
		}
	}
	return out, nil
}

// ReleaseSession frees every lock a task session owns and reports how many it
// freed. It is the session-owned counterpart of ReleaseRun and is called from
// the single termination choke point, so a task that ends any way at all --
// finished, killed, failed, reaped -- never leaves its branch occupied.
func (m *Manager) ReleaseSession(ctx context.Context, sessionID, reason string) (int64, error) {
	locks, err := m.HeldBySession(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	now := m.clock()
	var released int64
	for _, lock := range locks {
		ok, rerr := m.store.ReleaseBranchLock(ctx, lock.ID, reason, now)
		if rerr != nil {
			return released, rerr
		}
		if ok {
			released++
		}
	}
	return released, nil
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
// belong to a run that can still write this branch?", and it is answered from
// durable workflow state, never from a timestamp heuristic. The policy itself
// lives in decideRetention (retention.go); this loop only applies it:
//
//   - The run no longer exists, or is terminal → stale, released. Nothing will
//     ever resume it, so holding the branch hostage would be a permanent leak.
//   - The run is live → legitimate. Adopted if the lock carries a previous
//     instance's owner token, kept otherwise. It is emphatically not released:
//     the recovered run is about to resume writing that branch, and freeing it
//     would let a second run start writing it too.
//   - The run is stopped in needs_attention → Checkpoint 8P-E.13A's addition.
//     Kept when AO will resume it by itself or when it left uncommitted work
//     the branch has to protect; released when it is neither, because a
//     permanently stopped run must not deadlock a branch forever.
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
		retention, cerr := m.classifyLock(ctx, lock)
		if cerr != nil {
			return out, cerr
		}
		if err := m.apply(ctx, lock, retention, now); err != nil {
			return out, err
		}
		switch retention.Decision {
		case RetentionRelease:
			out.Released++
			if m.log != nil {
				m.log.Info("branchlock: released stale lock", "lock", lock.ID, "run", lock.WorkflowRunID, "branch", lock.Branch, "reason", retention.Reason)
			}
		case RetentionAdopt:
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
