//go:build !windows

package daemonlock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func lockFile(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrHeld
		}
		return fmt.Errorf("daemonlock: flock %s: %w", f.Name(), err)
	}
	return nil
}
