// Package codexsandbox computes the explicit extra writable roots a Codex
// session running in the workspace-write sandbox needs, and nothing more.
//
// AR-1a (D-SEC-4, Codex AR1A-INT-01): Codex sessions no longer default to a
// full sandbox bypass. Inside workspace-write the session's own worktree is
// writable, but two things a working session legitimately writes are not:
//
//   - the git metadata of its repositories. An AO worktree's index, objects and
//     refs live in the repository's common git directory, outside the
//     worktree; and Codex protects a `.git` inside a writable root as
//     read-only. Without an explicit root, `git add` / `git commit` fail.
//   - the additional workspace roots of a workspace project (its child
//     repositories), which the session manager passes explicitly.
//
// WritableRoots returns exactly those, validated fail-closed: every additional
// directory must be an existing directory inside the workspace (so nothing is
// widened beyond the workspace), every git directory comes from git itself for
// one of those validated directories, and no root may be the filesystem root,
// the home directory, or an ancestor of AO's own state.
package codexsandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrInvalidRoot is a workspace or additional directory AO refuses to hand a
// sandboxed Codex session as writable.
var ErrInvalidRoot = errors.New("codexsandbox: invalid writable root")

// GitCommonDirFunc returns the absolute common git directory of dir, and
// whether dir is inside a git repository at all.
type GitCommonDirFunc func(ctx context.Context, dir string) (string, bool, error)

// Resolver computes writable roots. The zero value uses git on PATH and the
// process's home directory.
type Resolver struct {
	GitCommonDir GitCommonDirFunc
	// Home and AODataDir override the protected locations (tests).
	Home      string
	AODataDir string
}

// WritableRoots is Resolver{}.WritableRoots.
func WritableRoots(ctx context.Context, workspace string, additional []string) ([]string, error) {
	return Resolver{}.WritableRoots(ctx, workspace, additional)
}

// WritableRoots returns the validated, deduplicated extra writable roots for a
// workspace-write Codex session over workspace: first the additional
// directories (all inside workspace), then the common git directory of the
// workspace and of each additional directory that is a git repository.
// An empty workspace yields no roots. Any invalid input refuses the whole set.
func (r Resolver) WritableRoots(ctx context.Context, workspace string, additional []string) ([]string, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return nil, nil
	}
	ws, err := resolveDir(workspace)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace %q: %v", ErrInvalidRoot, workspace, err)
	}
	protected, err := r.protected()
	if err != nil {
		return nil, err
	}

	var roots []string
	seen := map[string]bool{}
	add := func(p string) error {
		if seen[p] {
			return nil
		}
		if err := checkNotProtected(p, protected); err != nil {
			return err
		}
		seen[p] = true
		roots = append(roots, p)
		return nil
	}

	repos := []string{ws}
	for _, raw := range additional {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil, fmt.Errorf("%w: empty additional directory", ErrInvalidRoot)
		}
		if !filepath.IsAbs(raw) {
			return nil, fmt.Errorf("%w: additional directory %q is not absolute", ErrInvalidRoot, raw)
		}
		dir, err := resolveDir(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: additional directory %q: %v", ErrInvalidRoot, raw, err)
		}
		if !within(dir, ws) {
			return nil, fmt.Errorf("%w: additional directory %q is outside the workspace %q", ErrInvalidRoot, raw, ws)
		}
		if dir == ws {
			continue
		}
		if seen[dir] {
			continue
		}
		if err := add(dir); err != nil {
			return nil, err
		}
		repos = append(repos, dir)
	}

	gitDir := r.GitCommonDir
	if gitDir == nil {
		gitDir = gitCommonDir
	}
	for _, repo := range repos {
		common, ok, err := gitDir(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("%w: git directory of %q: %v", ErrInvalidRoot, repo, err)
		}
		if !ok {
			continue
		}
		resolved, err := resolveDir(common)
		if err != nil {
			return nil, fmt.Errorf("%w: git directory %q of %q: %v", ErrInvalidRoot, common, repo, err)
		}
		if err := add(resolved); err != nil {
			return nil, err
		}
	}
	return roots, nil
}

// protected returns the directories no root may equal or contain.
func (r Resolver) protected() ([]string, error) {
	home := r.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("%w: home directory: %v", ErrInvalidRoot, err)
		}
		home = h
	}
	home = resolveLoose(home)
	out := []string{home, filepath.Join(home, ".ao")}
	data := r.AODataDir
	if data == "" {
		data = os.Getenv("AO_DATA_DIR")
	}
	if data != "" {
		out = append(out, resolveLoose(data))
	}
	return out, nil
}

// checkNotProtected refuses the filesystem root and any root that is a
// protected directory or an ancestor of one. A root strictly inside a
// protected directory is fine as long as it is not an ancestor of another:
// AO's own worktrees live under ~/.ao, and their git directories are the
// user's repositories elsewhere.
func checkNotProtected(root string, protected []string) error {
	if root == string(filepath.Separator) || filepath.Dir(root) == root {
		return fmt.Errorf("%w: %q is the filesystem root", ErrInvalidRoot, root)
	}
	for _, p := range protected {
		if within(p, root) {
			return fmt.Errorf("%w: %q would expose %q", ErrInvalidRoot, root, p)
		}
	}
	return nil
}

// within reports whether path is base or inside it.
func within(path, base string) bool {
	if path == base {
		return true
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func resolveDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("not a directory")
	}
	return filepath.Clean(resolved), nil
}

func resolveLoose(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(abs)
}

// gitCommonDir asks git for dir's common git directory. A directory that is not
// in a git repository (no .git entry and git refuses it) is reported as such;
// a directory that looks like a repository but cannot be queried is an error,
// never "not a repository" (fail closed).
func gitCommonDir(ctx context.Context, dir string) (string, bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	out, err := cmd.Output()
	if err != nil {
		if _, statErr := os.Lstat(filepath.Join(dir, ".git")); statErr == nil {
			return "", false, fmt.Errorf("git rev-parse: %w", err)
		}
		return "", false, nil
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return "", false, errors.New("git rev-parse returned no common directory")
	}
	return common, true, nil
}
