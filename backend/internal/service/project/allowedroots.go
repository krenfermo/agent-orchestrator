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

// effectiveBrowseRoots returns the roots ListAllowedRootEntries confines
// browsing to: the configured AllowedRoots, or -- when none are configured
// -- the OS user's own home directory (Checkpoint 8P-E.4). This is still a
// bounded root, not unrestricted browsing: it matches the same trust level
// the desktop app already assumes for this local user (the historical "the
// OS file picker is the boundary" behavior already lets that user reach
// their whole home directory). A deployment that wants a tighter boundary
// (or a different implicit root) sets AO_PROJECT_ROOTS explicitly, which
// always wins over this fallback.
func (m *Service) effectiveBrowseRoots() ([]string, error) {
	if len(m.allowedRoots) > 0 {
		return m.allowedRoots, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return nil, apierr.Invalid("NO_ALLOWED_ROOTS_CONFIGURED", "No allowed project roots are configured on this server and the home directory could not be resolved.", nil)
	}
	return []string{home}, nil
}

// ListAllowedRootEntries lists directory entries for the web folder-browser
// UX (Checkpoint 8P-E.4): both Project and Workspace import drive this
// instead of requiring a typed absolute path. path is either empty (top
// level) or an absolute directory previously returned as some entry's Path
// -- never a caller-supplied relative fragment, so there is no ambiguity
// about which configured root it is relative to.
//
// Top level (path=""): a single effective root (configured or the home-
// directory fallback) skips straight to listing that root's own children,
// since there is nothing to choose between; multiple configured roots are
// listed as the top-level entries themselves so the caller can descend into
// whichever one it wants (the "Allowed locations" list).
//
// Non-empty path: must be absolute and, after symlink resolution, must fall
// within one of the effective roots (validateWithinAllowedRoots) -- the same
// containment check Add uses, so "../" traversal and a symlink planted
// inside an allowed root pointing outside it are both rejected identically.
func (m *Service) ListAllowedRootEntries(ctx context.Context, path string) (BrowseResult, error) {
	if err := ctx.Err(); err != nil {
		return BrowseResult{}, err
	}
	roots, err := m.effectiveBrowseRoots()
	if err != nil {
		return BrowseResult{}, err
	}

	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		if len(roots) == 1 {
			entries, err := listDirEntries(roots[0])
			if err != nil {
				return BrowseResult{}, err
			}
			return BrowseResult{Path: roots[0], Entries: entries}, nil
		}
		return BrowseResult{Path: "", Entries: rootsAsBrowseEntries(roots)}, nil
	}

	if !filepath.IsAbs(trimmed) {
		return BrowseResult{}, apierr.Invalid("PATH_NOT_ABSOLUTE", "Browse path must be an absolute path returned by a previous browse call.", map[string]any{"path": path})
	}
	clean := filepath.Clean(trimmed)
	if err := validateWithinAllowedRoots(clean, roots); err != nil {
		return BrowseResult{}, err
	}
	info, err := os.Stat(clean)
	if err != nil || !info.IsDir() {
		return BrowseResult{}, apierr.NotFound("PATH_NOT_FOUND", "Folder not found under any allowed project root.")
	}
	entries, err := listDirEntries(clean)
	if err != nil {
		return BrowseResult{}, err
	}
	return BrowseResult{Path: clean, Entries: entries}, nil
}

// rootsAsBrowseEntries presents each configured allowed root as a
// top-level, navigable entry (Checkpoint 8P-E.4's "Allowed locations" list),
// used only when more than one root is configured.
func rootsAsBrowseEntries(roots []string) []BrowseEntry {
	out := make([]BrowseEntry, 0, len(roots))
	for _, root := range roots {
		out = append(out, BrowseEntry{Name: root, Path: root, IsGitRepo: hasGitMetadata(root)})
	}
	return out
}

// listDirEntries lists dir's immediate subdirectories, hiding dotfiles/
// dot-directories (.git, .ao, …) from the listing. A child that is itself a
// symlink is silently excluded rather than followed: os.ReadDir reports a
// directory entry's own type (Lstat-like), not its symlink target, so
// IsDir() is already false for a symlinked directory and it never reaches
// the output below -- no separate symlink handling is needed here (path
// itself, the directory actually being read, is what
// validateWithinAllowedRoots's symlink resolution guards). Only directory
// names and a git-repo flag are returned -- never file contents, never
// dotfiles -- and this function performs no writes.
func listDirEntries(dir string) ([]BrowseEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, apierr.Internal("BROWSE_FAILED", "Failed to list folder")
	}
	out := make([]BrowseEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		childPath := filepath.Join(dir, e.Name())
		out = append(out, BrowseEntry{
			Name:      e.Name(),
			Path:      childPath,
			IsGitRepo: hasGitMetadata(childPath),
		})
	}
	return out, nil
}
