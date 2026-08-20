package project

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

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
	ID   domain.ProjectID   `json:"id"`
	Name string             `json:"name"`
	Kind domain.ProjectKind `json:"kind" enum:"single_repo,workspace,scratch"`
	Path string             `json:"path"`
	Repo string             `json:"repo"`
	// Transport is the root repo's remote transport, classified from Repo:
	// "https", "ssh", or "unknown" (no remote, or a local path). Empty when
	// Repo is empty.
	Transport      string                `json:"transport,omitempty" enum:"https,ssh,unknown"`
	DefaultBranch  string                `json:"defaultBranch"`
	Agent          string                `json:"agent,omitempty"`
	Config         *domain.ProjectConfig `json:"config,omitempty"`
	WorkspaceRepos []WorkspaceRepo       `json:"workspaceRepos,omitempty"`
	// ExecutionMode is the project's effective execution mode (Checkpoint
	// 8P-E.11), already resolved against project kind -- a scratch project
	// always reports isolated_worktree regardless of what is configured, so
	// the UI never has to re-apply that rule.
	ExecutionMode domain.ExecutionMode `json:"executionMode" enum:"isolated_worktree,direct_branch"`
	// Repositories describes every real Git repository this project executes
	// in, with the branch each one is configured for and, in direct-branch
	// mode, which workflow currently occupies it. Populated for direct-branch
	// projects; empty in isolated-worktree mode, where sessions get their own
	// worktrees and nothing is ever occupied.
	Repositories []RepositoryExecution `json:"repositories,omitempty"`
}

// RepositoryExecution is one repository's direct-branch execution status.
type RepositoryExecution struct {
	// Name is domain.RootWorkspaceRepoName for a single-repo project or a
	// workspace root, otherwise the registered child repo name.
	Name string `json:"name"`
	// RelativePath is empty for the root and the registered path for a child.
	RelativePath string `json:"relativePath,omitempty"`
	Path         string `json:"path"`
	// Branch is the branch this repository is configured to work on -- the
	// authoritative one, never a detected fallback.
	Branch        string               `json:"branch"`
	ExecutionMode domain.ExecutionMode `json:"executionMode" enum:"isolated_worktree,direct_branch"`
	// Lock names the workflow currently holding this repository+branch, or
	// nil when it is free.
	Lock *RepositoryLock `json:"lock,omitempty"`
}

// RepositoryLock is the occupancy of one repository+branch pair.
type RepositoryLock struct {
	WorkflowRunID string    `json:"workflowRunId"`
	SessionID     string    `json:"sessionId,omitempty"`
	AcquiredAt    time.Time `json:"acquiredAt"`
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
	Transport string `json:"transport,omitempty" enum:"https,ssh,unknown"`
	GitStatus string `json:"gitStatus,omitempty"`
	// DefaultBranch is this repo's own base branch — the branch autonomous
	// worker worktrees for this repo are created from. Detected from the
	// child repo's checked-out branch at registration time; independent of
	// the project-level DefaultBranch fallback used by single-repo projects
	// and by any child that has no branch of its own (GitStatusNeedsInit).
	DefaultBranch string `json:"defaultBranch,omitempty"`
}
