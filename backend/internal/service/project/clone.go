package project

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// ownerRepoSlugRE matches a bare "owner/repo" GitHub slug.
var ownerRepoSlugRE = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// githubURLRE matches an https://github.com/owner/repo(.git)?(/)? URL and
// captures the owner/repo slug. Only github.com is accepted — this endpoint
// is GitHub-specific, matching the checkpoint's scope.
var githubURLRE = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+?)(?:\.git)?/?$`)

// destinationNameRE constrains a clone destination folder name to a single
// path segment with no traversal potential.
var destinationNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func defaultCloneRunner(ctx context.Context, args ...string) ([]byte, error) {
	return aoprocess.CommandContext(ctx, "gh", args...).CombinedOutput()
}

// normalizeRepoSlug accepts either an "owner/repo" slug or an
// "https://github.com/owner/repo" URL and returns the canonical "owner/repo"
// slug, or an error if the input matches neither shape. Anything else
// (arbitrary text, other hosts, shell metacharacters) is rejected outright —
// the resulting slug is later passed to `gh` as a single structured argv
// element, never through a shell, so this is a format allowlist, not an
// injection defense in itself.
func normalizeRepoSlug(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if ownerRepoSlugRE.MatchString(trimmed) {
		return trimmed, nil
	}
	if m := githubURLRE.FindStringSubmatch(trimmed); len(m) == 3 {
		return m[1] + "/" + m[2], nil
	}
	return "", apierr.Invalid("INVALID_GITHUB_REPO", "Repo must be an \"owner/repo\" slug or an https://github.com/owner/repo URL.", map[string]any{"repo": raw})
}

// CloneFromGitHub clones a GitHub repository into the first configured
// allowed project root and registers it, reusing Add's full validation
// (git-repo / initial-commit checks included). Cloning is only available when
// AllowedRoots is configured: an unconfined server has no safe default
// destination to write into.
func (m *Service) CloneFromGitHub(ctx context.Context, in CloneInput) (Project, error) {
	if len(m.allowedRoots) == 0 {
		return Project{}, apierr.Invalid("NO_ALLOWED_ROOTS_CONFIGURED", "No allowed project roots are configured on this server; cloning is unavailable.", nil)
	}
	slug, err := normalizeRepoSlug(in.Repo)
	if err != nil {
		return Project{}, err
	}

	name := ""
	if in.DestinationName != nil {
		name = strings.TrimSpace(*in.DestinationName)
	}
	if name == "" {
		parts := strings.SplitN(slug, "/", 2)
		name = parts[1]
	}
	if !destinationNameRE.MatchString(name) || name == "." || name == ".." {
		return Project{}, apierr.Invalid("INVALID_DESTINATION_NAME", "Destination folder name must contain only letters, numbers, '.', '_', or '-'.", map[string]any{"destinationName": name})
	}

	// destPath is guaranteed to stay inside root by construction: root comes
	// directly from the trusted allowedRoots config, and name was just
	// validated as a single path segment with no "/" and no ".."/"." — so no
	// separate containment re-check is needed (and none is possible here: the
	// destination does not exist yet, so symlink resolution — the mechanism
	// that defeats escapes elsewhere in this package — cannot run against it).
	root := m.allowedRoots[0]
	destPath := filepath.Join(root, name)
	if _, err := os.Stat(destPath); err == nil {
		return Project{}, apierr.Conflict("DESTINATION_ALREADY_EXISTS", "A folder already exists at the clone destination.", map[string]any{"path": destPath})
	}

	out, err := m.cloneRunner(ctx, "repo", "clone", slug, destPath)
	if err != nil {
		if looksLikeGitHubAuthFailure(out) {
			return Project{}, apierr.Invalid("GITHUB_NOT_AUTHENTICATED", "GitHub CLI is not authenticated. Run `gh auth login` and try again.", nil)
		}
		return Project{}, apierr.Invalid("GITHUB_CLONE_FAILED", "Cloning the repository failed.", map[string]any{"error": strings.TrimSpace(string(out))})
	}

	return m.Add(ctx, AddInput{Path: destPath})
}

func looksLikeGitHubAuthFailure(out []byte) bool {
	lower := strings.ToLower(string(out))
	for _, marker := range []string{"authentication", "gh auth login", "could not read username", "401", "requires authentication"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
