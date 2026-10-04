package router

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ProjectStore is the project lookup surface needed to choose the workspace
// implementation for a session.
type ProjectStore interface {
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
}

// Deps configures a workspace router.
type Deps struct {
	Git ports.Workspace
	// DirectBranch backs projects configured with
	// domain.ExecutionDirectBranch (Checkpoint 8P-E.11). Optional: when nil,
	// a direct-branch project fails loudly rather than silently falling back
	// to creating a worktree the user explicitly opted out of.
	DirectBranch ports.Workspace
	Scratch      ports.Workspace
	Projects     ProjectStore
}

// Workspace delegates workspace operations to the adapter that matches the
// session's project kind and configured execution mode.
type Workspace struct {
	git          ports.Workspace
	directBranch ports.Workspace
	scratch      ports.Workspace
	projects     ProjectStore
}

var _ ports.Workspace = (*Workspace)(nil)
var _ ports.WorkspaceProject = (*Workspace)(nil)
var _ ports.WorkspaceObserver = (*Workspace)(nil)

// New returns a router over git and scratch workspace implementations.
func New(deps Deps) *Workspace {
	return &Workspace{
		git:          deps.Git,
		directBranch: deps.DirectBranch,
		scratch:      deps.Scratch,
		projects:     deps.Projects,
	}
}

// Create delegates session workspace creation to the project-appropriate
// workspace adapter.
func (w *Workspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	adapter, err := w.adapterForPlacement(ctx, cfg.ProjectID, cfg.Placement)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	return adapter.Create(ctx, cfg)
}

// Restore delegates session workspace restoration to the project-appropriate
// workspace adapter.
func (w *Workspace) Restore(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	adapter, err := w.adapterForPlacement(ctx, cfg.ProjectID, cfg.Placement)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	return adapter.Restore(ctx, cfg)
}

// Destroy delegates normal session workspace cleanup to the
// project-appropriate workspace adapter.
func (w *Workspace) Destroy(ctx context.Context, info ports.WorkspaceInfo) error {
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return err
	}
	return adapter.Destroy(ctx, info)
}

// ForceDestroy delegates forced session workspace cleanup to the
// project-appropriate workspace adapter.
func (w *Workspace) ForceDestroy(ctx context.Context, info ports.WorkspaceInfo) error {
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return err
	}
	return adapter.ForceDestroy(ctx, info)
}

// StashUncommitted delegates preservation of dirty workspace state to the
// project-appropriate workspace adapter.
func (w *Workspace) StashUncommitted(ctx context.Context, info ports.WorkspaceInfo) (string, error) {
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return "", err
	}
	return adapter.StashUncommitted(ctx, info)
}

// ApplyPreserved delegates restored dirty workspace state application to the
// project-appropriate workspace adapter.
func (w *Workspace) ApplyPreserved(ctx context.Context, info ports.WorkspaceInfo, ref string) error {
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return err
	}
	return adapter.ApplyPreserved(ctx, info, ref)
}

// AddExclude delegates local workspace ignore updates to the project-appropriate
// workspace adapter.
func (w *Workspace) AddExclude(ctx context.Context, info ports.WorkspaceInfo, patterns ...string) error {
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return err
	}
	return adapter.AddExclude(ctx, info, patterns...)
}

// MaterializeIntegrationCommit delegates internal integration-commit creation
// to the project-appropriate workspace adapter.
func (w *Workspace) MaterializeIntegrationCommit(ctx context.Context, info ports.WorkspaceInfo, ref, parentSHA, message string, excludePatterns []string) (string, string, bool, error) {
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return "", "", false, err
	}
	return adapter.MaterializeIntegrationCommit(ctx, info, ref, parentSHA, message, excludePatterns)
}

// ObserveWorkspace delegates the read-only handoff snapshot to the selected
// project adapter. Adapters that cannot observe state return a clear error
// instead of fabricating repository facts.
//
// P3-A: an observation is a READ of a path the caller already holds, and it is
// the one workspace operation whose answer does not depend on which adapter
// runs it -- both implementations shell out to the same `git -C <path>`
// sequence and return the same struct. Routing it by the PROJECT's execution
// mode is therefore not merely imprecise, it is the bug: a direct-branch run in
// a project configured for isolated worktrees is observed by the worktree
// adapter, whose managed-root guard refuses the user's own repository, and the
// work step watching for that worker's change never sees it. The refusal is
// correct for every DESTRUCTIVE operation and only for those.
//
// So when the routed adapter answers "not my department"
// (ports.ErrWorkspaceNotManaged) the other repository observer is asked about
// the same path. This is not a guess between two possible answers: the path is
// the caller's, not the router's, so the fallback cannot read a different
// repository than the one it was asked about, and an adapter that genuinely
// cannot observe still returns its own error rather than an invented fact.
func (w *Workspace) ObserveWorkspace(ctx context.Context, info ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return ports.WorkspaceObservation{}, err
	}
	observer, ok := adapter.(ports.WorkspaceObserver)
	if !ok {
		return ports.WorkspaceObservation{}, errors.New("workspace router: selected workspace does not support observation")
	}
	obs, err := observer.ObserveWorkspace(ctx, info)
	if err == nil || !errors.Is(err, ports.ErrWorkspaceNotManaged) {
		return obs, err
	}
	fallback, ok := w.unmanagedPathObserver(adapter)
	if !ok {
		return obs, err
	}
	return fallback.ObserveWorkspace(ctx, info)
}

// unmanagedPathObserver is the observer to ask about a path the routed adapter
// does not manage. Today that is the direct-branch adapter, which is precisely
// the one with no managed-root constraint: it observes whatever repository it
// is pointed at, which is what a direct-branch checkout is. It is never the
// scratch adapter (a scratch project has no repository to fall back to) and
// never the adapter that already refused.
func (w *Workspace) unmanagedPathObserver(refused ports.Workspace) (ports.WorkspaceObserver, bool) {
	if w == nil || w.directBranch == nil || w.directBranch == refused {
		return nil, false
	}
	observer, ok := w.directBranch.(ports.WorkspaceObserver)
	return observer, ok
}

// CreateWorkspaceProject delegates root-as-repo workspace project creation to
// the project-appropriate adapter.
func (w *Workspace) CreateWorkspaceProject(ctx context.Context, cfg ports.WorkspaceProjectConfig) (ports.WorkspaceProjectInfo, error) {
	project, err := w.workspaceProjectAdapter(ctx, cfg.ProjectID)
	if err != nil {
		return ports.WorkspaceProjectInfo{}, err
	}
	return project.CreateWorkspaceProject(ctx, cfg)
}

// DestroyWorkspaceProject delegates root-as-repo workspace project cleanup to
// the project-appropriate adapter.
func (w *Workspace) DestroyWorkspaceProject(ctx context.Context, info ports.WorkspaceProjectInfo) error {
	project, err := w.workspaceProjectAdapter(ctx, info.Root.ProjectID)
	if err != nil {
		return err
	}
	return project.DestroyWorkspaceProject(ctx, info)
}

// CommitAll delegates the autonomous local commit to the DIRECT-BRANCH adapter,
// keyed by the repository the caller named rather than by the project's mode.
//
// It is the same argument PreflightRepository makes immediately below, and
// P3-C §28 is why it is now made here too: a commit through this path only ever
// targets a direct-branch repository, because both callers have already proven
// it. autonomousLocalCommit gates on the RUN's frozen placement, and
// commitAuthorityFor refuses an isolated placement outright with its own
// message. Asking the PROJECT again after that could only produce a different
// answer than the placement gave -- and it did: a run with an explicit
// direct-branch placement inside an isolated-default project routed to the
// worktree adapter, which is not a committer, so the commit-and-continue flow
// answered 500 for the one stop it exists to clear. Found by the P3-C closing
// smoke.
//
// F5 adds a third caller with the same shape: commitIsolatedWorktree names an
// AO-owned WORKTREE rather than a direct-branch checkout. That is still keyed by
// the path the caller named and still lands through the direct-branch adapter's
// `git -C <path>` commit, which is correct for any git working directory -- the
// adapter is the commit implementation here, not a statement about the project's
// execution mode. What the callers keep proving is that the path is theirs to
// write: a branch lock for direct-branch, the run's own frozen isolated
// placement for a worktree.
//
// A deployment with no direct-branch adapter still reports a clear unsupported
// error rather than silently doing nothing.
func (w *Workspace) CommitAll(ctx context.Context, info ports.WorkspaceInfo, message string) (string, bool, error) {
	if w == nil || w.directBranch == nil {
		return "", false, errors.New("workspace router: direct-branch workspace is not configured")
	}
	committer, ok := w.directBranch.(ports.WorkspaceCommitter)
	if !ok {
		return "", false, fmt.Errorf("workspace router: autonomous commit: %w", ports.ErrWorkspaceOperationUnsupported)
	}
	return committer.CommitAll(ctx, info, message)
}

// PreflightRepository delegates the read-only direct-branch safety probe. It is
// keyed by repository path rather than by project, so it takes the direct-branch
// adapter directly: the probe only makes sense for that mode.
func (w *Workspace) PreflightRepository(ctx context.Context, repoPath, branch string) (ports.WorkspacePreflight, error) {
	if w == nil || w.directBranch == nil {
		return ports.WorkspacePreflight{}, errors.New("workspace router: direct-branch workspace is not configured")
	}
	preflighter, ok := w.directBranch.(ports.WorkspacePreflighter)
	if !ok {
		return ports.WorkspacePreflight{}, fmt.Errorf("workspace router: preflight: %w", ports.ErrWorkspaceOperationUnsupported)
	}
	return preflighter.PreflightRepository(ctx, repoPath, branch)
}

func (w *Workspace) workspaceProjectAdapter(ctx context.Context, projectID domain.ProjectID) (ports.WorkspaceProject, error) {
	adapter, err := w.adapterForProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if projectAdapter, ok := adapter.(ports.WorkspaceProject); ok {
		return projectAdapter, nil
	}
	return w.gitWorkspaceProject()
}

// adapterForPlacement chooses the workspace implementation for one obligation,
// preferring the run's FROZEN placement over the project's current execution
// mode.
//
// The precedence is the point. Project configuration is mutable and answers
// "how does this project usually work"; a frozen placement is immutable for the
// life of an obligation and answers "where was THIS work decided to happen".
// When they disagree, the second one is the only answer that cannot change
// under a run that has already started — and honouring the first is what turned
// an explicit "current branch" into a worktree the user never asked for.
//
// A placement AO cannot read is never coerced to a default: it falls through to
// the project, which is the pre-P3-A behaviour, rather than guessing.
func (w *Workspace) adapterForPlacement(ctx context.Context, projectID domain.ProjectID, placement domain.ExecutionPlacementType) (ports.Workspace, error) {
	if w == nil {
		return nil, errors.New("workspace router: nil router")
	}
	if !placement.IsKnown() {
		return w.adapterForProject(ctx, projectID)
	}
	// A scratch project has no repository to work a branch in, so its own
	// adapter still wins: a placement cannot conjure a checkout that does not
	// exist.
	if w.projects != nil && projectID != "" {
		if project, ok, err := w.projects.GetProject(ctx, string(projectID)); err == nil && ok &&
			project.Kind.WithDefault() == domain.ProjectKindScratch {
			return w.adapterForProject(ctx, projectID)
		}
	}
	if placement == domain.PlacementDirectBranch {
		if w.directBranch == nil {
			// Exactly the 8P-E.11 refusal, for exactly the same reason: an
			// unconfigured direct-branch adapter is an error, never a fallback
			// to the worktree the caller opted out of.
			return nil, errors.New("workspace router: direct-branch workspace is not configured")
		}
		return w.directBranch, nil
	}
	if w.git == nil {
		return nil, errors.New("workspace router: git workspace is not configured")
	}
	return w.git, nil
}

func (w *Workspace) adapterForProject(ctx context.Context, projectID domain.ProjectID) (ports.Workspace, error) {
	if w == nil {
		return nil, errors.New("workspace router: nil router")
	}
	if w.projects != nil && projectID != "" {
		project, ok, err := w.projects.GetProject(ctx, string(projectID))
		if err != nil {
			return nil, fmt.Errorf("workspace router: project %q: %w", projectID, err)
		}
		if ok && project.Kind.WithDefault() == domain.ProjectKindScratch {
			if w.scratch == nil {
				return nil, errors.New("workspace router: scratch workspace is not configured")
			}
			return w.scratch, nil
		}
		// Checkpoint 8P-E.11: a project that opted into direct-branch
		// execution must never silently get a worktree instead. An
		// unconfigured direct-branch adapter is an error, not a fallback.
		if ok && domain.ResolveExecutionMode(project.Kind, project.Config) == domain.ExecutionDirectBranch {
			if w.directBranch == nil {
				return nil, errors.New("workspace router: direct-branch workspace is not configured")
			}
			return w.directBranch, nil
		}
	}
	if w.git == nil {
		return nil, errors.New("workspace router: git workspace is not configured")
	}
	return w.git, nil
}

func (w *Workspace) gitWorkspaceProject() (ports.WorkspaceProject, error) {
	if w == nil || w.git == nil {
		return nil, errors.New("workspace router: git workspace is not configured")
	}
	gitProject, ok := w.git.(ports.WorkspaceProject)
	if !ok {
		return nil, errors.New("workspace router: git workspace does not support workspace projects")
	}
	return gitProject, nil
}
