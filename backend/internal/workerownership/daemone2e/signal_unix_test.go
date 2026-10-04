//go:build !windows

package daemone2e

import (
	"os"
	"syscall"
)

// syscallZero is signal 0: an existence check that delivers nothing.
func syscallZero() os.Signal { return syscall.Signal(0) }
