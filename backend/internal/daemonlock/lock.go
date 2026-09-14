// Package daemonlock gives an AO daemon exclusive ownership of its data dir and
// its run-file for its whole lifetime (P9).
//
// A check-then-write guard ("is a daemon serving the run-file? no -> start")
// cannot stop two daemons that start at the same moment: both pass the check
// before either writes. An exclusive, non-blocking OS lock held from startup to
// exit can. The lock is released by the kernel when the process exits for any
// reason, so a crashed daemon never leaves it held.
package daemonlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// ErrHeld reports that another process holds the lock.
var ErrHeld = errors.New("daemonlock: held by another process")

// Lock is one held exclusive lock.
type Lock struct{ f *os.File }

// Acquire takes an exclusive, non-blocking lock on path, creating it if needed.
// It returns ErrHeld when another process holds it.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("daemonlock: create dir for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemonlock: open %s: %w", path, err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	// Diagnostics only: which process holds it. Never read as a decision.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{f: f}, nil
}

// Release gives the lock up. Safe on a nil Lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
