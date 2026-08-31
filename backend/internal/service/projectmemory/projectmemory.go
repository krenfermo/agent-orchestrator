// Package projectmemory is the read/repair service behind AO's project-memory
// API and CLI.
//
// It is a thin resolver over internal/projectmemory: its whole job is to turn
// a project id (which is what an HTTP caller has) into the repository paths
// project memory is keyed by (which is what the memory subsystem needs), and
// to keep the two vocabularies from leaking into each other.
//
// One policy lives here rather than in the memory package, because it is about
// AO's project registry rather than about memory: **a request that names no
// repository operates on the project's own root.** That is the single-repo
// case, which is most of them, and requiring an operator to paste a path for
// it would make `ao memory status` useless.
package projectmemory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	pm "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// ProjectReader is the slice of the project registry this service needs: where
// a project lives, and which repositories it spans.
type ProjectReader interface {
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
	ListWorkspaceRepos(ctx context.Context, projectID string) ([]domain.WorkspaceRepoRecord, error)
}

// Service backs the project-memory HTTP surface.
type Service struct {
	memory   *pm.Service
	projects ProjectReader
}

// New builds the service over the memory subsystem and the project registry.
func New(memory *pm.Service, projects ProjectReader) *Service {
	return &Service{memory: memory, projects: projects}
}

// Compile-time proof that the service satisfies the controller contract.
var _ controllers.ProjectMemoryService = (*Service)(nil)

// Status reports every repository of a project that has memory.
//
// It reports what the memory subsystem has actually registered rather than
// what the project registry contains, and the difference is the useful part:
// a repository that appears in the registry and not here is one AO has never
// indexed, which is exactly what an operator asking "why is there no memory"
// needs to see.
func (s *Service) Status(ctx context.Context, projectID domain.ProjectID) ([]domain.ProjectMemoryStatus, error) {
	return s.memory.StatusAll(ctx, projectID)
}

// Inspect reads stored facts for an operator.
func (s *Service) Inspect(ctx context.Context, req controllers.ProjectMemoryInspectQuery) (controllers.ProjectMemoryInspection, error) {
	repoPath, err := s.resolveRepo(ctx, req.ProjectID, req.RepoPath)
	if err != nil {
		return controllers.ProjectMemoryInspection{}, err
	}
	res, err := s.memory.Inspect(ctx, pm.InspectRequest{
		ProjectID: req.ProjectID, RepoPath: repoPath,
		State: req.State, Type: req.Type, PathPrefix: req.PathPrefix, Limit: req.Limit,
	})
	if err != nil {
		return controllers.ProjectMemoryInspection{}, err
	}
	return controllers.ProjectMemoryInspection{
		RepoID: res.RepoID, Items: res.Items, Total: res.Total, Truncated: res.Truncated,
	}, nil
}

// Rebuild re-derives one repository's memory at its current checkout state.
func (s *Service) Rebuild(ctx context.Context, projectID domain.ProjectID, repoPath string, purge bool) (controllers.ProjectMemoryRebuildOutcome, error) {
	resolved, err := s.resolveRepo(ctx, projectID, repoPath)
	if err != nil {
		return controllers.ProjectMemoryRebuildOutcome{}, err
	}
	commit, branch := pm.HeadOf(ctx, resolved)
	out, err := s.memory.Rebuild(ctx, projectID, resolved, commit, branch, purge)
	if err != nil {
		return controllers.ProjectMemoryRebuildOutcome{}, err
	}
	return controllers.ProjectMemoryRebuildOutcome{
		RepoID: out.RepoID, Generation: out.Generation,
		Skipped: out.Skipped, SkipReason: out.SkipReason,
		FilesIndexed: out.FilesIndexed, FilesSkipped: out.FilesSkipped,
		ItemsWritten: out.ItemsWritten, ItemsReconfirmed: out.ItemsReconfirmed,
		ItemsRetired: out.ItemsRetired, IndexedCommit: out.IndexedCommit,
		Truncated: out.Truncated, TruncatedReason: out.TruncatedReason,
	}, nil
}

// Invalidate retires memory an operator knows, or suspects, is wrong.
//
// With explicit paths it retires exactly what those paths proved. With no
// paths it runs drift detection and applies what that finds — the honest
// answer to "something moved and I cannot tell you what", and a far better one
// than invalidating everything.
func (s *Service) Invalidate(
	ctx context.Context, projectID domain.ProjectID, repoPath string, paths []string, reason string,
) (controllers.ProjectMemoryInvalidateOutcome, error) {
	resolved, err := s.resolveRepo(ctx, projectID, repoPath)
	if err != nil {
		return controllers.ProjectMemoryInvalidateOutcome{}, err
	}
	if len(domain.NormalizeMemorySourcePaths(paths)) > 0 {
		n, err := s.memory.Invalidate(ctx, projectID, resolved, paths, reason)
		if err != nil {
			return controllers.ProjectMemoryInvalidateOutcome{}, err
		}
		return controllers.ProjectMemoryInvalidateOutcome{
			RepoID: domain.ProjectMemoryRepoID(resolved), ItemsInvalidated: n,
		}, nil
	}

	commit, _ := pm.HeadOf(ctx, resolved)
	report, err := s.memory.Verify(ctx, projectID, resolved, commit, true)
	if err != nil {
		return controllers.ProjectMemoryInvalidateOutcome{}, err
	}
	applied := int64(0)
	for _, f := range report.Findings {
		if f.Applied {
			applied++
		}
	}
	return controllers.ProjectMemoryInvalidateOutcome{
		RepoID: report.RepoID, ItemsInvalidated: applied,
		DriftChecked: report.Checked, DriftFound: len(report.Findings),
	}, nil
}

// resolveRepo turns an optional repository path into a concrete one.
//
// An explicit path is honoured as given. An empty path resolves to the
// project's own root — the single-repo case. A path that is not one of the
// project's repositories is refused rather than indexed: project memory is
// scoped by project, and indexing an unrelated checkout under a project's id
// would put facts about one codebase into another's memory.
func (s *Service) resolveRepo(ctx context.Context, projectID domain.ProjectID, repoPath string) (string, error) {
	project, found, err := s.projects.GetProject(ctx, string(projectID))
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("project %s is not registered", projectID)
	}
	requested := strings.TrimSpace(repoPath)
	if requested == "" {
		return project.Path, nil
	}

	allowed := []string{project.Path}
	repos, err := s.projects.ListWorkspaceRepos(ctx, string(projectID))
	if err == nil {
		for _, r := range repos {
			// A workspace child is registered by its path relative to the
			// project root, so the absolute form has to be rebuilt here.
			allowed = append(allowed, filepath.Join(project.Path, r.RelativePath))
		}
	}
	for _, candidate := range allowed {
		if pm.SameRepoPath(candidate, requested) {
			return requested, nil
		}
	}
	return "", fmt.Errorf("%s is not a repository of project %s", requested, projectID)
}
