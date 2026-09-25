package repoaccess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// eligible.go — which files exist, as far as AO's indexers are concerned.

// ExcludedDirNames are directory names never indexed, at any depth. They are
// the SAFETY and HYGIENE floor shared by every indexer; an indexer may skip
// more for relevance (project memory also skips bin/, obj/ …) but never less.
//
// In a git repository most of these are already absent (untracked or
// ignored), so the list mainly protects the filesystem fallback and tracked
// oddities such as a committed node_modules.
var ExcludedDirNames = []string{
	// version control internals
	".git", ".hg", ".svn", ".bzr", ".jj", ".sl",
	// AO's own state and every coding agent's scratch space. `.claude/worktrees`
	// holds whole COPIES of the repository.
	".ao", ".claude", ".cursor", ".aider", ".codex", ".continue", ".gemini", ".windsurf", ".worktrees",
	// editors
	".idea", ".vscode",
	// dependency trees
	"node_modules", "vendor", "bower_components", ".pnpm-store", "site-packages",
	// build output
	"dist", "build", "out", "target", ".next", ".nuxt", ".svelte-kit", ".turbo", ".parcel-cache",
	// caches and virtualenvs
	"coverage", ".cache", "__pycache__", ".venv", "venv", ".tox", ".mypy_cache", ".pytest_cache",
	".gradle", ".terraform",
}

var excludedDirSet = func() map[string]bool {
	m := make(map[string]bool, len(ExcludedDirNames))
	for _, name := range ExcludedDirNames {
		m[name] = true
	}
	return m
}()

// IsExcludedDir reports whether a directory base name is never indexed.
func IsExcludedDir(name string) bool { return excludedDirSet[name] }

// ListingMode records how a listing was produced, so a consumer can state it.
type ListingMode string

const (
	// ModeGitTracked means the listing is the files `git ls-files` reports for the root.
	ModeGitTracked ListingMode = "git-tracked"
	// ModeFilesystem means the listing came from a filesystem walk, used only when the root is not inside
	// a git work tree.
	ModeFilesystem ListingMode = "filesystem"
)

// Listing is the eligible file set of one root.
type Listing struct {
	Mode ListingMode
	// Paths are root-relative, slash-separated, sorted, and pre-filtered:
	// no excluded directory, no secret path, no escaping path.
	Paths []string
	// SecretTemplates are env templates that exist and were NOT opened. A
	// caller may surface "configuration template exists".
	SecretTemplates []string
	// Skipped counts refusals by reason, for honest coverage reporting.
	Skipped map[string]int
}

func (l *Listing) skip(reason string) {
	if l.Skipped == nil {
		l.Skipped = map[string]int{}
	}
	l.Skipped[reason]++
}

// CheckPath applies the path-only half of the policy (no filesystem access):
// shape, excluded directories, secrets. A nil error means the path may be
// read -- through ReadConfined, which applies the filesystem half.
func CheckPath(rel string) error {
	clean, err := cleanRel(rel)
	if err != nil {
		return err
	}
	segs := strings.Split(clean, "/")
	for _, seg := range segs[:len(segs)-1] {
		if IsExcludedDir(seg) {
			return fmt.Errorf("%w: %s", ErrExcluded, clean)
		}
	}
	if IsSecretPath(clean) {
		return fmt.Errorf("%w: %s", ErrSecretPath, clean)
	}
	return nil
}

// cleanRel validates and normalizes a root-relative path.
func cleanRel(rel string) (string, error) {
	if rel == "" || strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: %q", ErrEscapesRoot, rel)
	}
	slashed := strings.ReplaceAll(rel, "\\", "/")
	if strings.HasPrefix(slashed, "/") || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("%w: %q is absolute", ErrEscapesRoot, rel)
	}
	clean := path.Clean(slashed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q", ErrEscapesRoot, rel)
	}
	return clean, nil
}

// ListEligible returns the files an indexer may consider under root.
//
// Inside a git work tree the candidates are exactly the tracked files: what
// the project IS, not what its directory happens to hold. Untracked agent
// worktrees, ignored build output and nested checkouts (which git records as
// a gitlink, never as their files) are excluded by construction. Outside git
// a walk applies the same exclusions and additionally refuses any directory
// that is itself a checkout (contains a `.git` file or directory).
//
// root must be absolute. The result is sorted, which is what a restartable
// pass's resume cursor relies on.
func ListEligible(ctx context.Context, root string) (Listing, error) {
	if !filepath.IsAbs(root) {
		return Listing{}, fmt.Errorf("repoaccess: root %q must be absolute", root)
	}
	tracked, isGit, err := gitTracked(ctx, root)
	if err != nil {
		return Listing{}, err
	}
	var out Listing
	if isGit {
		out.Mode = ModeGitTracked
		for _, rel := range tracked {
			out.consider(rel)
		}
	} else {
		out.Mode = ModeFilesystem
		if err := walkFilesystem(ctx, root, &out); err != nil {
			return Listing{}, err
		}
	}
	sort.Strings(out.Paths)
	sort.Strings(out.SecretTemplates)
	return out, nil
}

func (l *Listing) consider(rel string) {
	err := CheckPath(rel)
	switch {
	case err == nil:
		clean, _ := cleanRel(rel)
		l.Paths = append(l.Paths, clean)
	case errors.Is(err, ErrSecretPath):
		l.skip("secret")
		if IsSecretTemplate(rel) {
			clean, _ := cleanRel(rel)
			l.SecretTemplates = append(l.SecretTemplates, clean)
		}
	case errors.Is(err, ErrExcluded):
		l.skip("excluded-dir")
	default:
		l.skip("invalid-path")
	}
}

// gitArgs are the hardening flags every git invocation here carries: no
// fsmonitor hook (a repository-configured fsmonitor is arbitrary command
// execution), no repository hooks, no optional locks, no path quoting.
var gitArgs = []string{
	"--no-optional-locks",
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.quotepath=false",
}

// GitCommand builds a hardened, shell-free git invocation in root: no
// fsmonitor hook, no repository hooks, no optional locks, and an environment
// that cannot redirect git to another repository or configuration. Every git
// command AO's indexers run against a project checkout goes through it.
func GitCommand(ctx context.Context, root string, args ...string) *exec.Cmd {
	argv := append(append(append([]string{}, gitArgs...), "-C", root), args...)
	cmd := exec.CommandContext(ctx, "git", argv...) //nolint:gosec // fixed argv, no shell
	cmd.Env = gitEnv()
	return cmd
}

// DirtyTrackedFiles counts tracked files whose working-tree content differs
// from HEAD (staged or unstaged). Untracked files are not counted: they are
// not eligible for indexing at all. ok=false means git could not answer.
func DirtyTrackedFiles(ctx context.Context, root string) (n int, ok bool) {
	out, err := GitCommand(ctx, root, "status", "--porcelain", "--untracked-files=no", "-z").Output()
	if err != nil {
		return 0, false
	}
	entries := bytes.Split(out, []byte{0})
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) <= 3 {
			continue
		}
		n++
		// A rename or copy is "XY new\0old": the next token is the original
		// path of the same change, not another file.
		if entry[0] == 'R' || entry[0] == 'C' || entry[1] == 'R' || entry[1] == 'C' {
			i++
		}
	}
	return n, true
}

// gitTracked lists tracked files relative to root. isGit=false (with no
// error) means root is not inside a git work tree and the caller must fall
// back to the filesystem walk.
func gitTracked(ctx context.Context, root string) (paths []string, isGit bool, err error) {
	out, err := GitCommand(ctx, root, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		return nil, false, nil
	}
	raw, err := GitCommand(ctx, root, "ls-files", "-z", "--cached").Output()
	if err != nil {
		return nil, true, fmt.Errorf("repoaccess: git ls-files in %s: %w", root, err)
	}
	for _, rel := range bytes.Split(raw, []byte{0}) {
		if len(rel) > 0 {
			paths = append(paths, string(rel))
		}
	}
	return paths, true, nil
}

// gitEnv is the process environment minus anything that would redirect git to
// another repository or configuration.
func gitEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GIT_DIR="), strings.HasPrefix(kv, "GIT_WORK_TREE="),
			strings.HasPrefix(kv, "GIT_INDEX_FILE="), strings.HasPrefix(kv, "GIT_CONFIG"),
			strings.HasPrefix(kv, "GIT_EXTERNAL_DIFF="):
			continue
		}
		out = append(out, kv)
	}
	return out
}

// walkFilesystem is the non-git fallback. It never follows symlinks, skips
// excluded directories, and refuses nested checkouts.
func walkFilesystem(ctx context.Context, root string, out *Listing) error {
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if d != nil && d.IsDir() {
				out.skip("unreadable-dir")
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			out.skip("symlink")
			return nil
		}
		if d.IsDir() {
			if abs == root {
				return nil
			}
			if IsExcludedDir(d.Name()) {
				out.skip("excluded-dir")
				return fs.SkipDir
			}
			// A directory with its own .git (a linked worktree's .git FILE or
			// a nested repository's .git DIRECTORY) is a different checkout.
			if _, err := os.Lstat(filepath.Join(abs, ".git")); err == nil {
				out.skip("nested-checkout")
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			out.skip("not-regular")
			return nil
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return err
		}
		out.consider(filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return fmt.Errorf("repoaccess: walk %s: %w", root, err)
	}
	return nil
}
