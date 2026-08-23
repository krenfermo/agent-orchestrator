package workspace

import (
	"errors"
	"os/exec"
)

// isExitCode reports whether err (or anything it wraps) is a process exit with
// the given status.
func isExitCode(err error, code int) bool {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode() == code
	}
	return false
}
