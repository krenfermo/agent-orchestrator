//go:build !windows

// Package dockerlock serializes live container tests across test processes.
//
// `go test ./...` runs packages in parallel processes. The skill runner, the
// egress proxy and the skills service each have live tests that start real
// containers and networks on the ONE container runtime of the host, and some of
// them assert on host-wide state ("no container labelled ao.skillrun survived",
// "this network name is free"). Run concurrently, one package's containers
// appear in another's assertions -- the failures Frente 2A observed, which
// were the harness, not the code. Snapshot comparisons only narrowed the race.
//
// Acquire takes an exclusive, host-wide file lock for the calling test and
// releases it when the test ends. It does not skip or silence anything: a test
// that holds the lock and fails, fails.
//
// It is imported only by tests, so it is never linked into a binary.
package dockerlock

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// LockFileName is the lock file, in os.TempDir().
const LockFileName = "ao-docker-live-tests.lock"

// Acquire blocks until this test holds the host-wide live-container lock.
func Acquire(t testing.TB) {
	t.Helper()
	path := filepath.Join(os.TempDir(), LockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("dockerlock: open %s: %v", path, err)
	}
	start := time.Now()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatalf("dockerlock: lock %s: %v", path, err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Logf("dockerlock: waited %s for another package's live container tests", waited.Round(time.Millisecond))
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	})
}
