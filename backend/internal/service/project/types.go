package project

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// Summary is the row shape returned by GET /api/v1/projects.
type Summary struct {
	ID                domain.ProjectID    `json:"id"`
	Name              string              `json:"name"`
	Path              string              `json:"path"`
	Kind              domain.ProjectKind  `json:"kind" enum:"single_repo,workspace,scratch"`
	SessionPrefix     string              `json:"sessionPrefix"`
	OrchestratorAgent domain.AgentHarness `json:"orchestratorAgent,omitempty"`
	ResolveError      string              `json:"resolveError,omitempty"`
	// Repo is the project's `origin` remote URL, empty when the repo has none.
	Repo string `json:"repo,omitempty"`
	// DefaultBranch is the base branch new session worktrees are created from.
	DefaultBranch string `json:"defaultBranch,omitempty"`
	// Valid is a cheap (stat-only, no shell-out) check that the registered
	// path still exists and still looks like a Git repository. false surfaces
	// a project whose folder moved or was deleted since registration.
	Valid bool `json:"valid"`
}

// Project is the full read-model returned by GET /api/v1/projects/{id}.
type Project struct {
	ID             domain.ProjectID      `json:"id"`
	Name           string                `json:"name"`
	Kind           domain.ProjectKind    `json:"kind" enum:"single_repo,workspace,scratch"`
	Path           string                `json:"path"`
	Repo           string                `json:"repo"`
	// Transport is the root repo's remote transport, classified from Repo:
	// "https", "ssh", or "unknown" (no remote, or a local path). Empty when
	// Repo is empty.
	Transport      string                `json:"transport,omitempty" enum:"https,ssh,unknown"`
	DefaultBranch  string                `json:"defaultBranch"`
	Agent          string                `json:"agent,omitempty"`
	Config         *domain.ProjectConfig `json:"config,omitempty"`
	WorkspaceRepos []WorkspaceRepo       `json:"workspaceRepos,omitempty"`
}

// Degraded is returned in place of Project when project config failed to load.
type Degraded struct {
	ID           domain.ProjectID   `json:"id"`
	Name         string             `json:"name"`
	Kind         domain.ProjectKind `json:"kind" enum:"single_repo,workspace,scratch"`
	Path         string             `json:"path"`
	ResolveError string             `json:"resolveError"`
}

// WorkspaceRepo is the project-detail read shape for a registered child repo.
type WorkspaceRepo struct {
	Name         string `json:"name"`
	RelativePath string `json:"relativePath"`
	Repo         string `json:"repo"`
	// Transport is Repo's remote transport, classified the same way as
	// Project.Transport.
	Transport    string `json:"transport,omitempty" enum:"https,ssh,unknown"`
	GitStatus    string `json:"gitStatus,omitempty"`
	// DefaultBranch is this repo's own base branch — the branch autonomous
	// worker worktrees for this repo are created from. Detected from the
	// child repo's checked-out branch at registration time; independent of
	// the project-level DefaultBranch fallback used by single-repo projects
	// and by any child that has no branch of its own (GitStatusNeedsInit).
	DefaultBranch string `json:"defaultBranch,omitempty"`
}
