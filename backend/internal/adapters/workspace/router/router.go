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
	adapter, err := w.adapterForProject(ctx, cfg.ProjectID)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	return adapter.Create(ctx, cfg)
}

// Restore delegates session workspace restoration to the project-appropriate
// workspace adapter.
func (w *Workspace) Restore(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	adapter, err := w.adapterForProject(ctx, cfg.ProjectID)
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
func (w *Workspace) ObserveWorkspace(ctx context.Context, info ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return ports.WorkspaceObservation{}, err
	}
	observer, ok := adapter.(ports.WorkspaceObserver)
	if !ok {
		return ports.WorkspaceObservation{}, errors.New("workspace router: selected workspace does not support observation")
	}
	return observer.ObserveWorkspace(ctx, info)
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

// CommitAll delegates the autonomous local commit to the project-appropriate
// adapter. Adapters that cannot commit (the worktree adapter, whose sessions
// commit through the agent on their own branch) report a clear unsupported
// error rather than silently doing nothing.
func (w *Workspace) CommitAll(ctx context.Context, info ports.WorkspaceInfo, message string) (string, bool, error) {
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return "", false, err
	}
	committer, ok := adapter.(ports.WorkspaceCommitter)
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
