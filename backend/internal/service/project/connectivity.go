package project

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// connectionProbeTimeout bounds the non-destructive "git ls-remote" probe so
// a hung SSH/HTTPS handshake (bad host, firewalled network) cannot block the
// request indefinitely.
const connectionProbeTimeout = 15 * time.Second

// RepoTransportHTTPS and RepoTransportSSH classify a remote URL's transport
// for display; RepoTransportUnknown covers anything else (e.g. no remote, or
// a local filesystem path).
const (
	RepoTransportHTTPS   = "https"
	RepoTransportSSH     = "ssh"
	RepoTransportUnknown = "unknown"
)

// classifyTransport reports whether remote is reached over SSH or HTTPS,
// covering the three shapes git accepts: scp-like ("git@host:org/repo"),
// explicit ssh:// URLs, and SSH host aliases from ~/.ssh/config (e.g.
// "github-nuevo:org/repo", used exactly this way by the real MEDUSA
// backend_node remote) that carry no "://" or user@ prefix at all.
func classifyTransport(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return RepoTransportUnknown
	}
	switch {
	case strings.HasPrefix(remote, "https://"), strings.HasPrefix(remote, "http://"):
		return RepoTransportHTTPS
	case strings.HasPrefix(remote, "ssh://"):
		return RepoTransportSSH
	case strings.HasPrefix(remote, "git@"):
		return RepoTransportSSH
	case strings.HasPrefix(remote, "/"), strings.HasPrefix(remote, "file://"):
		return RepoTransportUnknown
	}
	// scp-like shorthand "host-alias:path" (no scheme, no "@") — the form
	// `git remote add origin github-nuevo:org/repo.git` produces, resolving
	// "github-nuevo" through the user's ~/.ssh/config Host alias. A bare
	// "host:path" with no slash before the colon is this shape; a Windows
	// drive path ("C:\...") never appears in a git remote URL, so no
	// disambiguation against that is needed here.
	if idx := strings.Index(remote, ":"); idx > 0 && !strings.Contains(remote[:idx], "/") {
		return RepoTransportSSH
	}
	return RepoTransportUnknown
}

// credentialInURLPattern matches userinfo embedded in an HTTPS remote
// (https://user:token@host/... or https://token@host/...) so it can be
// stripped before the URL is ever returned to a client.
var credentialInURLPattern = regexp.MustCompile(`^(https?://)[^/@]+@`)

// sanitizeRemoteURL strips any embedded credentials (a PAT or user:token
// pair git allows inline in an HTTPS remote) from remote before it is
// surfaced in an API response. SSH remotes never carry inline credentials —
// access is via the resolved host's keys — so they pass through unchanged.
func sanitizeRemoteURL(remote string) string {
	return credentialInURLPattern.ReplaceAllString(strings.TrimSpace(remote), "$1")
}

// ConnectionTestResult is the outcome of a single non-destructive
// connectivity probe against a repository's existing `origin` remote.
type ConnectionTestResult struct {
	Status      string `json:"status" enum:"ok,error"`
	Message     string `json:"message,omitempty"`
	LatencyMS   int64  `json:"latencyMs,omitempty"`
	RequiresSSH bool   `json:"-"`
}

// TestRepoConnection runs a read-only `git ls-remote origin HEAD` against a
// project's root repo (repoName == "") or one of its registered workspace
// child repos, using that repository's own on-disk Git configuration —
// whatever credential helper, SSH key, or ~/.ssh/config Host alias it already
// resolves to. It never assumes the desktop's globally authenticated `gh`
// account: git itself picks the transport and credentials per remote, exactly
// as an interactive `git fetch` in that repo would.
//
// The probe never pushes, never writes to the repo, and never touches Git
// credential storage or the remote configuration — ls-remote only queries the
// remote's advertised refs.
func (m *Service) TestRepoConnection(ctx context.Context, id domain.ProjectID, repoName string) (ConnectionTestResult, error) {
	row, ok, err := m.store.GetProject(ctx, string(id))
	if err != nil {
		return ConnectionTestResult{}, apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load project")
	}
	if !ok || !row.ArchivedAt.IsZero() {
		return ConnectionTestResult{}, apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
	}

	repoPath := row.Path
	remote := row.RepoOriginURL
	if repoName != "" {
		if row.Kind.WithDefault() != domain.ProjectKindWorkspace {
			return ConnectionTestResult{}, apierr.Invalid("NOT_A_WORKSPACE_PROJECT", "repo is only valid for workspace projects", nil)
		}
		repos, err := m.store.ListWorkspaceRepos(ctx, row.ID)
		if err != nil {
			return ConnectionTestResult{}, apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load workspace repositories")
		}
		found := false
		for _, r := range repos {
			if r.Name == repoName {
				repoPath = joinRepoPath(row.Path, r.RelativePath)
				remote = r.RepoOriginURL
				found = true
				break
			}
		}
		if !found {
			return ConnectionTestResult{}, apierr.NotFound("WORKSPACE_REPO_NOT_FOUND", "Unknown workspace repository")
		}
	}
	if strings.TrimSpace(remote) == "" {
		return ConnectionTestResult{Status: "error", Message: "No origin remote is configured for this repository."}, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, connectionProbeTimeout)
	defer cancel()

	start := time.Now()
	cmd := aoprocess.CommandContext(probeCtx, "git", "-C", repoPath, "ls-remote", "--exit-code", "origin", "HEAD")
	// GIT_TERMINAL_PROMPT=0 makes an HTTPS remote lacking cached credentials
	// fail immediately with a clear error instead of hanging on a password
	// prompt with no TTY attached. SSH already fails fast without a TTY (no
	// passphrase/host-key prompt is possible), so no SSH-specific env is
	// injected here — doing so would risk overriding a repo's own
	// core.sshCommand (exactly the per-remote config, e.g. an identity file
	// tied to a ~/.ssh/config Host alias, this probe must respect).
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, runErr := cmd.CombinedOutput()
	latency := time.Since(start).Milliseconds()

	if runErr != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = runErr.Error()
		}
		return ConnectionTestResult{Status: "error", Message: msg, LatencyMS: latency}, nil
	}
	return ConnectionTestResult{Status: "ok", LatencyMS: latency}, nil
}

func joinRepoPath(root, relative string) string {
	if relative == "" || relative == "." {
		return root
	}
	return root + string('/') + relative
}

// RefreshWorkspaceRepos re-detects a workspace project's direct child
// repositories from disk (same discovery Add uses) and replaces the stored
// registry with the fresh result: repos that no longer exist or are no
// longer git repos drop out, ready/needs_init status and each repo's own
// DefaultBranch are recomputed from the repo's actual current state. It only
// touches the project row and its workspace_repos rows — never sessions —
// so it is safe to run against a project with live or historical sessions.
//
// This exists because a repo's own DefaultBranch is captured once at
// registration time; if the child is later checked out on a different branch
// (as happened with the real MEDUSA workspace's backend_node), the stored
// value silently goes stale until something re-detects it.
func (m *Service) RefreshWorkspaceRepos(ctx context.Context, id domain.ProjectID) (Project, error) {
	if err := validateProjectID(id); err != nil {
		return Project{}, err
	}
	row, ok, err := m.store.GetProject(ctx, string(id))
	if err != nil {
		return Project{}, apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load project")
	}
	if !ok || !row.ArchivedAt.IsZero() {
		return Project{}, apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
	}
	if row.Kind.WithDefault() != domain.ProjectKindWorkspace {
		return Project{}, apierr.Invalid("NOT_A_WORKSPACE_PROJECT", "Only workspace projects have repositories to refresh", nil)
	}
	repos, _, err := detectWorkspaceChildren(ctx, row.Path, domain.ProjectID(row.ID), time.Now())
	if err != nil {
		return Project{}, err
	}
	if err := m.store.UpsertWorkspaceProject(ctx, row, repos); err != nil {
		return Project{}, apierr.Internal("PROJECT_SAVE_FAILED", "Failed to save refreshed workspace repositories")
	}
	p := m.projectFromRow(row)
	p.WorkspaceRepos = workspaceReposFromRecords(repos)
	return p, nil
}
