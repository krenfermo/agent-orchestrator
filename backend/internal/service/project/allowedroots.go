package project

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// resolveInputPath resolves a project registration path. When allowed roots
// are configured and the input is relative (and not a "~" home-relative
// path), it is resolved against each configured root in turn, first existing
// directory match wins. Otherwise it falls back to the existing absolute/home
// resolution — unchanged desktop behavior when no roots are configured, and
// still subject to the allowed-roots containment check below either way, so
// an absolute path cannot be used to bypass confinement.
func (m *Service) resolveInputPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed != "" && len(m.allowedRoots) > 0 && !filepath.IsAbs(trimmed) && !strings.HasPrefix(trimmed, "~") {
		for _, root := range m.allowedRoots {
			candidate := filepath.Join(root, trimmed)
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return normalizePath(candidate)
			}
		}
	}
	return normalizePath(raw)
}

// validateWithinAllowedRoots rejects a path that, after symlink resolution,
// does not fall within one of the configured roots. A nil/empty roots list is
// a no-op — the historical desktop trust boundary (the OS file picker)
// applies instead. Both path and each root are passed through comparablePath
// (Clean + best-effort EvalSymlinks) before comparison, which is what defeats
// a symlink planted inside an allowed root pointing outside it.
func validateWithinAllowedRoots(path string, roots []string) error {
	if len(roots) == 0 {
		return nil
	}
	clean := comparablePath(path)
	for _, root := range roots {
		r := comparablePath(root)
		if samePath(clean, r) || isDescendantPath(clean, r) {
			return nil
		}
	}
	return apierr.Invalid("PATH_OUTSIDE_ALLOWED_ROOTS", "Repository path is outside the allowed project roots.", map[string]any{
		"path":         path,
		"allowedRoots": roots,
	})
}

// BrowseEntry is one directory entry returned by ListAllowedRootEntries.
type BrowseEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	IsGitRepo bool   `json:"isGitRepo"`
}

// ListAllowedRootEntries lists the immediate subdirectories of relPath
// resolved against the configured allowed roots (first root under which
// relPath exists as a directory wins). It never recurses and never crosses
// outside the allowed roots: relPath is joined and then re-validated with the
// same containment check Add uses. Dotfiles/directories (.git, .ao, …) are
// hidden from the listing.
func (m *Service) ListAllowedRootEntries(ctx context.Context, relPath string) ([]BrowseEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(m.allowedRoots) == 0 {
		return nil, apierr.Invalid("NO_ALLOWED_ROOTS_CONFIGURED", "No allowed project roots are configured on this server.", nil)
	}
	rel := strings.TrimPrefix(strings.TrimSpace(relPath), "/")

	var target string
	for _, root := range m.allowedRoots {
		candidate := filepath.Join(root, rel)
		if err := validateWithinAllowedRoots(candidate, m.allowedRoots); err != nil {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			target = candidate
			break
		}
	}
	if target == "" {
		return nil, apierr.NotFound("PATH_NOT_FOUND", "Folder not found under any allowed project root.")
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, apierr.Internal("BROWSE_FAILED", "Failed to list folder")
	}
	out := make([]BrowseEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		childPath := filepath.Join(target, e.Name())
		out = append(out, BrowseEntry{
			Name:      e.Name(),
			Path:      childPath,
			IsGitRepo: hasGitMetadata(childPath),
		})
	}
	return out, nil
}
