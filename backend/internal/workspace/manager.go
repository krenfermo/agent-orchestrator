// Package workspace owns the lifecycle of the git worktrees AO creates for a
// plan's tasks: where they live, what branch they are on, what they were cut
// from, and what state they are in, durably, for as long as the plan exists.
//
// It exists because a task worktree has to be attributable. The pre-existing
// per-session worktree (internal/adapters/workspace/gitworktree) answers "does
// this session have a checkout", which is all a single session needs. A plan
// executing many tasks needs answers that question cannot give: which task a
// directory belongs to, which branch the work is ultimately for, and which
// commits -- the base, and each dependency's -- it was built on top of. Those
// are facts about a moment that does not come back, so they are written down
// rather than re-derived later from refs that have since moved.
//
// Two invariants govern everything here:
//
//   - The user's own checkout is never disturbed. The manager creates and
//     removes ITS OWN directories and reads refs; it never checks out, resets,
//     stashes, cleans, or otherwise writes to the primary working tree. The
//     Git interface has no method that could, which is how the invariant is
//     kept rather than merely intended.
//
//   - A direct_branch task gets nothing at all: no directory, no branch, no
//     row, and not one git command. Its work happens in the user's repository
//     on the user's branch, which is the entire point of that mode, and a
//     lifecycle record would be a claim that AO owns something it does not.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Store is the durable persistence the manager needs. *sqlite.Store satisfies
// it; tests use a fake.
type Store interface {
	UpsertTaskWorktree(ctx context.Context, rec domain.TaskWorktreeRecord) error
	GetTaskWorktree(ctx context.Context, taskID string) (domain.TaskWorktreeRecord, bool, error)
}

// FS is the small filesystem surface the manager consults. It only ever asks
// whether a directory is there; it never deletes anything itself (git does).
type FS interface {
	DirExists(path string) (bool, error)
}

var (
	// ErrInvalidRequest means the request could not describe a worktree at
	// all (missing ids, a branch name git would reject, an unusable path).
	ErrInvalidRequest = errors.New("worktree: invalid request")
	// ErrUnsafePath means the computed worktree path escaped the managed root
	// or landed inside the user's repository. Both are refusals rather than
	// corrections: a path that resolved somewhere unexpected means an input
	// was not what it claimed, and creating a worktree anyway is how a tool
	// ends up writing into somebody's source tree.
	ErrUnsafePath = errors.New("worktree: unsafe worktree path")
)

// BranchPrefix is the namespace every AO-created task branch lives under. It
// matches the prefix the per-session worktree adapter already uses, so one
// `git branch --list 'ao/*'` still shows everything AO created.
const BranchPrefix = "ao/"

// idPattern is what an id must look like before it is allowed into a path or
// a branch name. It rules out empty, leading dots, path separators and "..",
// so neither the directory layout nor the ref namespace can be steered by an
// id that came from a plan document.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Options configures a Manager. Root, Git and Store are required.
type Options struct {
	// Root is the directory task worktrees are created under. It must resolve
	// inside the AO data dir -- never anywhere in the user's repository.
	Root  string
	Git   Git
	Store Store
	// FS defaults to the real filesystem.
	FS FS
	// Now defaults to time.Now().UTC(); tests inject a clock.
	Now func() time.Time
}

// Manager creates, records and tears down the AO-owned worktree for a task.
type Manager struct {
	root  string
	git   Git
	store Store
	fs    FS
	now   func() time.Time
}

// New validates the options and returns a Manager.
func New(opts Options) (*Manager, error) {
	if strings.TrimSpace(opts.Root) == "" {
		return nil, errors.New("worktree: Root is required")
	}
	if opts.Git == nil {
		return nil, errors.New("worktree: Git is required")
	}
	if opts.Store == nil {
		return nil, errors.New("worktree: Store is required")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("worktree: root: %w", err)
	}
	fs := opts.FS
	if fs == nil {
		fs = osFS{}
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Manager{root: filepath.Clean(root), git: opts.Git, store: opts.Store, fs: fs, now: now}, nil
}

// Request is everything the manager needs to place one task's work.
type Request struct {
	ProjectID     domain.ProjectID
	WorkflowRunID string
	TaskID        string
	// RepoPath is the user's own repository. It is read from and registered
	// against; it is never checked out, reset, or written to.
	RepoPath string
	// TargetBranch is where this task's work is ultimately meant to land, and
	// (unless BaseRef says otherwise) what the worktree is cut from.
	TargetBranch string
	// BaseRef overrides what the worktree is cut from. Empty means
	// TargetBranch, which is the normal case.
	BaseRef string
	// Mode is the execution mode RESOLVED FOR THIS TASK -- the plan's per-task
	// selection where it made one, the project's setting otherwise. Callers
	// get it from domain.ResolveTaskExecutionMode; passing the project's raw
	// mode would ignore the downgrade the planner recorded.
	Mode domain.ExecutionMode
	// DependencyTaskIDs are the tasks this one builds on. Each is resolved to
	// the commit its work sat at when this worktree was cut, and that pairing
	// is what gets persisted.
	DependencyTaskIDs []string
}

// Lease is the answer to "where does this task's work happen".
type Lease struct {
	// Mode is the resolved execution mode the answer came from.
	Mode domain.ExecutionMode
	// Isolated reports whether AO created a worktree for this task. False for
	// direct_branch, where Record is the zero value and Path is the user's own
	// repository.
	Isolated bool
	// Path is the directory the task's agent should work in.
	Path string
	// Record is the durable worktree record. Zero when Isolated is false.
	Record domain.TaskWorktreeRecord
}

// Ensure places one task's work, creating and recording an AO-owned worktree
// when the task's mode calls for one.
//
// It is idempotent: a task whose worktree already exists on disk gets the same
// lease back without git being asked to do anything. A task whose record
// exists but whose directory is gone (the shape a manual `rm -rf` leaves) is
// recovered onto its EXISTING branch rather than a fresh one -- the branch
// still holds whatever the task committed, and re-cutting it from base would
// silently discard that work.
//
// The record is written BEFORE git is asked to create anything, in state
// creating. A crash in between then leaves a row that says exactly what was
// being attempted, which is the difference between a directory AO can clean up
// and a directory nobody can explain.
func (m *Manager) Ensure(ctx context.Context, req Request) (Lease, error) {
	mode := req.Mode.WithDefault()
	if err := validateRequest(req); err != nil {
		return Lease{}, err
	}
	// Direct branch: the work happens in the user's repository, on the user's
	// branch. Nothing is created, nothing is recorded, and -- importantly for
	// a mode whose whole promise is "AO does not touch my checkout" -- not one
	// git command is run.
	if mode.DirectBranch() {
		return Lease{Mode: mode, Isolated: false, Path: req.RepoPath}, nil
	}

	path, err := m.pathFor(req)
	if err != nil {
		return Lease{}, err
	}
	branch := BranchFor(req.WorkflowRunID, req.TaskID)

	existing, found, err := m.store.GetTaskWorktree(ctx, req.TaskID)
	if err != nil {
		return Lease{}, fmt.Errorf("worktree: read record for task %s: %w", req.TaskID, err)
	}
	if found && existing.State == domain.TaskWorktreeActive {
		present, err := m.fs.DirExists(existing.Path)
		if err != nil {
			return Lease{}, err
		}
		if present {
			return Lease{Mode: mode, Isolated: true, Path: existing.Path, Record: existing}, nil
		}
		// The registration outlived the directory. Drop the registration so
		// the add below can re-materialise the path; prune deletes nothing on
		// disk, so no work can be lost by it.
		if err := m.git.Prune(ctx, req.RepoPath); err != nil {
			return Lease{}, err
		}
	}

	baseRef := firstNonEmpty(req.BaseRef, req.TargetBranch)
	baseSHA, err := m.resolveBase(ctx, req.RepoPath, baseRef)
	if err != nil {
		return Lease{}, err
	}
	deps, err := m.resolveDependencies(ctx, req, baseSHA)
	if err != nil {
		return Lease{}, err
	}

	now := m.now()
	rec := domain.TaskWorktreeRecord{
		WorkflowRunID: req.WorkflowRunID,
		TaskID:        req.TaskID,
		ProjectID:     req.ProjectID,
		RepoPath:      req.RepoPath,
		Path:          path,
		Branch:        branch,
		TargetBranch:  req.TargetBranch,
		BaseSHA:       baseSHA,
		Dependencies:  deps,
		ExecutionMode: mode,
		State:         domain.TaskWorktreeCreating,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if found {
		rec.CreatedAt = existing.CreatedAt
	}
	if err := m.store.UpsertTaskWorktree(ctx, rec); err != nil {
		return Lease{}, fmt.Errorf("worktree: record task %s: %w", req.TaskID, err)
	}

	if err := m.materialise(ctx, req.RepoPath, path, branch, baseSHA); err != nil {
		return Lease{}, m.markFailed(ctx, rec, err)
	}

	rec.State = domain.TaskWorktreeActive
	rec.UpdatedAt = m.now()
	if err := m.store.UpsertTaskWorktree(ctx, rec); err != nil {
		return Lease{}, fmt.Errorf("worktree: record task %s active: %w", req.TaskID, err)
	}
	return Lease{Mode: mode, Isolated: true, Path: path, Record: rec}, nil
}

// materialise runs the one git command that creates the directory, choosing
// the form that preserves work: a branch that already exists is checked out as
// it stands, never recreated from base.
func (m *Manager) materialise(ctx context.Context, repo, path, branch, baseSHA string) error {
	exists, err := m.git.BranchExists(ctx, repo, branch)
	if err != nil {
		return err
	}
	if exists {
		return m.git.AddWorktreeExistingBranch(ctx, repo, path, branch)
	}
	return m.git.AddWorktreeNewBranch(ctx, repo, path, branch, baseSHA)
}

// Release tears down a task's worktree directory and marks the record
// released. It reports false when the task has no record at all, which is the
// normal answer for a direct_branch task and for a task whose worktree was
// never created.
//
// The BRANCH is deliberately left behind. Releasing a worktree removes a
// checkout, never commits: the task's work lives on the ao/* branch and an
// integration step still has to read it. The released record keeps naming that
// branch, so the work stays findable after the directory is gone.
//
// Teardown does not force. A worktree holding uncommitted changes makes git
// refuse, that refusal is returned, and the record is marked failed rather
// than released -- deleting an agent's in-progress work to tidy up a directory
// is not a trade this manager is allowed to make.
func (m *Manager) Release(ctx context.Context, taskID string) (domain.TaskWorktreeRecord, bool, error) {
	rec, found, err := m.store.GetTaskWorktree(ctx, taskID)
	if err != nil {
		return domain.TaskWorktreeRecord{}, false, fmt.Errorf("worktree: read record for task %s: %w", taskID, err)
	}
	if !found {
		return domain.TaskWorktreeRecord{}, false, nil
	}
	if rec.State == domain.TaskWorktreeReleased {
		return rec, true, nil
	}
	if err := m.git.RemoveWorktree(ctx, rec.RepoPath, rec.Path); err != nil {
		return domain.TaskWorktreeRecord{}, false, m.markFailed(ctx, rec, err)
	}
	now := m.now()
	rec.State = domain.TaskWorktreeReleased
	rec.Detail = ""
	rec.UpdatedAt = now
	rec.ReleasedAt = &now
	if err := m.store.UpsertTaskWorktree(ctx, rec); err != nil {
		return domain.TaskWorktreeRecord{}, false, fmt.Errorf("worktree: record task %s released: %w", taskID, err)
	}
	return rec, true, nil
}

// markFailed persists the failure alongside the record and returns the
// original error. A persistence failure on top of it is joined rather than
// substituted: the git failure is the one the caller has to act on, and the
// fact that it could not even be written down is extra, not a replacement.
func (m *Manager) markFailed(ctx context.Context, rec domain.TaskWorktreeRecord, cause error) error {
	rec.State = domain.TaskWorktreeFailed
	rec.Detail = cause.Error()
	rec.UpdatedAt = m.now()
	if err := m.store.UpsertTaskWorktree(ctx, rec); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// resolveBase turns the requested base into a commit SHA, trying the local
// branch, then the origin tracking branch, then the ref as given. Pinning a
// SHA up front is what makes BaseSHA an honest record: the branch may move
// between now and whenever anyone reads the record back.
func (m *Manager) resolveBase(ctx context.Context, repo, ref string) (string, error) {
	candidates := []string{ref}
	if !strings.Contains(ref, "/") {
		candidates = []string{"refs/heads/" + ref, "refs/remotes/origin/" + ref, ref}
	}
	var lastErr error
	for _, candidate := range candidates {
		sha, err := m.git.ResolveCommit(ctx, repo, candidate)
		if err == nil {
			return sha, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("worktree: resolve base %q in %s: %w", ref, repo, lastErr)
}

// resolveDependencies pins the commit each dependency task's work sat at.
//
// A dependency that has its own AO worktree is pinned to the head of ITS
// branch, which is where its commits are. A dependency with no worktree ran in
// direct-branch mode (or has not been given a worktree yet), so its work is
// already on the target branch and the base commit is where it sits. Either
// way the pairing is recorded rather than left to be re-derived: the branch
// will move, and afterwards nobody can say what this task was built on.
func (m *Manager) resolveDependencies(ctx context.Context, req Request, baseSHA string) ([]domain.TaskWorktreeDependency, error) {
	out := make([]domain.TaskWorktreeDependency, 0, len(req.DependencyTaskIDs))
	seen := map[string]bool{}
	for _, depID := range req.DependencyTaskIDs {
		depID = strings.TrimSpace(depID)
		if depID == "" || depID == req.TaskID || seen[depID] {
			continue
		}
		seen[depID] = true
		dep := domain.TaskWorktreeDependency{TaskID: depID, SHA: baseSHA}
		rec, found, err := m.store.GetTaskWorktree(ctx, depID)
		if err != nil {
			return nil, fmt.Errorf("worktree: read dependency record %s: %w", depID, err)
		}
		if found && rec.Branch != "" {
			// A dependency branch that has since been deleted is not a reason
			// to fail this task: the base commit remains a true statement of
			// what this worktree was cut from, and the pairing still records
			// that the dependency existed.
			if sha, err := m.git.ResolveCommit(ctx, req.RepoPath, "refs/heads/"+rec.Branch); err == nil {
				dep.SHA = sha
			}
		}
		out = append(out, dep)
	}
	sortDependencies(out)
	return out, nil
}

// pathFor computes and guards the worktree directory for a task. The layout is
// <root>/<project>/<run>/<task>: a directory a human can read, and one where
// every segment is an id already validated to contain no separators.
func (m *Manager) pathFor(req Request) (string, error) {
	path := filepath.Join(m.root, string(req.ProjectID), req.WorkflowRunID, req.TaskID)
	clean := filepath.Clean(path)
	if !withinRoot(m.root, clean) {
		return "", fmt.Errorf("%w: %q escapes managed root %q", ErrUnsafePath, clean, m.root)
	}
	repo := filepath.Clean(req.RepoPath)
	// A worktree inside the user's repository would put AO-owned files in the
	// tree the user works in -- the exact outcome isolation exists to avoid.
	if clean == repo || withinRoot(repo, clean) {
		return "", fmt.Errorf("%w: %q is inside the primary repository %q", ErrUnsafePath, clean, repo)
	}
	return clean, nil
}

// BranchFor is the AO-owned branch name for a task. It is derived rather than
// stored-only so cleanup can name what it is looking for, and it is namespaced
// per run so two plans running the same task id never share a branch.
func BranchFor(runID, taskID string) string {
	return BranchPrefix + runID + "/" + taskID
}

func validateRequest(req Request) error {
	if strings.TrimSpace(req.RepoPath) == "" {
		return fmt.Errorf("%w: RepoPath is required", ErrInvalidRequest)
	}
	if !req.Mode.IsKnown() {
		return fmt.Errorf("%w: unknown execution mode %q", ErrInvalidRequest, req.Mode)
	}
	if req.Mode.WithDefault().DirectBranch() {
		// Direct branch creates no directory and no branch, so the id and
		// branch rules below have nothing to protect.
		return nil
	}
	if strings.TrimSpace(string(req.ProjectID)) == "" {
		return fmt.Errorf("%w: ProjectID is required", ErrInvalidRequest)
	}
	for name, id := range map[string]string{
		"ProjectID":     string(req.ProjectID),
		"WorkflowRunID": req.WorkflowRunID,
		"TaskID":        req.TaskID,
	} {
		if !idPattern.MatchString(id) {
			return fmt.Errorf("%w: %s %q is not usable in a path or a branch name", ErrInvalidRequest, name, id)
		}
	}
	if strings.TrimSpace(req.TargetBranch) == "" {
		return fmt.Errorf("%w: TargetBranch is required", ErrInvalidRequest)
	}
	if strings.HasPrefix(strings.TrimSpace(req.TargetBranch), BranchPrefix) {
		// An ao/* target would make the throwaway branch the destination, so
		// "where does this land" and "where is it scratch-written" collapse
		// into one answer and integration has nowhere to go.
		return fmt.Errorf("%w: TargetBranch %q is an AO-owned branch", ErrInvalidRequest, req.TargetBranch)
	}
	return nil
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func sortDependencies(deps []domain.TaskWorktreeDependency) {
	sort.Slice(deps, func(i, j int) bool { return deps[i].TaskID < deps[j].TaskID })
}
