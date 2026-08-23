package worktree

import (
	"errors"
	"fmt"
	"os"
)

// osFS is the real filesystem.
type osFS struct{}

// DirExists reports whether path is an existing directory. A path that is
// there but is not a directory is reported as absent: it is not a worktree,
// and treating it as one would hand a caller a lease to a file.
func (osFS) DirExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		return info.IsDir(), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("worktree: inspect %q: %w", path, err)
}
