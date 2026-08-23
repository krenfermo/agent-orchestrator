package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// securePath joins a repository-relative path git reported onto root and
// refuses anything that leaves root.
//
// git only ever names paths inside the worktree, so this can never fire on a
// well-behaved repository. It exists because the alternative -- trusting a
// path read out of a subprocess to stay where it claims -- is how a resolution
// write ends up outside the worktree it was supposed to be confined to, and
// this package writes files only during conflict resolution, where the input
// is exactly such a path.
func securePath(root, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("integration: empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("integration: refusing absolute path %q", rel)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full := filepath.Join(absRoot, filepath.FromSlash(rel))
	if full != absRoot && !strings.HasPrefix(full, absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("integration: path %q escapes %s", rel, root)
	}
	return full, nil
}

// writeFile replaces path's contents, keeping the mode it already had. A
// conflicted file always exists (it is a file both sides edited), so the
// fallback mode is only reached if something removed it underneath us.
func writeFile(path string, content []byte) error {
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, content, mode)
}
