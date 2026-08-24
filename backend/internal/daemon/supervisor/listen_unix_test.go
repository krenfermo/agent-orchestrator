//go:build !windows

package supervisor

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// socketTempDir is t.TempDir() with a bound on how long the path may be.
//
// A Unix socket address is not an ordinary path: it has to fit in
// sockaddr_un.sun_path, which is 104 bytes on macOS and 108 on Linux, and
// neither is negotiable at runtime. Over the limit, bind(2) fails with
// "invalid argument" -- an error that names the path but not the reason.
//
// t.TempDir() builds on TMPDIR, so how much room these tests have is decided by
// whoever set TMPDIR. A default /tmp or /var/folders/... leaves plenty; a
// session that points TMPDIR deep inside its own state directory leaves none,
// and all three tests below fail on an environment variable rather than on
// anything in Listen. The limit is real but it is the TEST's problem: the
// daemon's own socket is a sibling of the run file in ~/.ao, well inside it, so
// making Listen shorten paths would be inventing behavior to satisfy a fixture.
func socketTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// "/supervise.sock" is what Listen appends; 108 is the roomier of the two
	// limits and macOS's 104 is what actually binds here, so budget for 104.
	if len(dir)+len("/supervise.sock")+1 <= 104 {
		return dir
	}
	short, err := os.MkdirTemp("/tmp", "ao-sock")
	if err != nil {
		// No shorter root available. Say why, rather than letting bind(2)
		// report an "invalid argument" that reads like a code defect.
		t.Skipf("TMPDIR path %q leaves no room for a Unix socket address and /tmp is unavailable: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	if len(short)+len("/supervise.sock")+1 > 104 {
		t.Skipf("no temp directory short enough for a Unix socket address (tried %q and %q)", dir, short)
	}
	return short
}

// TestListen_basic verifies that Listen returns a listener whose address is
// <dir(runFilePath)>/supervise.sock, that the socket file exists on disk, and
// that a Dial to that address succeeds.
func TestListen_basic(t *testing.T) {
	t.Parallel()
	dir := socketTempDir(t)
	runFile := filepath.Join(dir, "running.json")

	ln, addr, err := Listen(runFile)
	if err != nil {
		t.Fatalf("Listen: unexpected error: %v", err)
	}
	defer ln.Close()

	wantSock := filepath.Join(dir, "supervise.sock")
	if addr != wantSock {
		t.Errorf("addr = %q, want %q", addr, wantSock)
	}

	// Socket file must exist after Listen.
	if _, err := os.Stat(wantSock); err != nil {
		t.Errorf("socket file missing after Listen: %v", err)
	}

	// Dialing the returned address must succeed.
	conn, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatalf("Dial(%q): %v", addr, err)
	}
	conn.Close()
}

// TestListen_staleSocket verifies that a pre-existing file at the socket path
// does not prevent Listen from succeeding (the stale file is removed first).
func TestListen_staleSocket(t *testing.T) {
	t.Parallel()
	dir := socketTempDir(t)
	runFile := filepath.Join(dir, "running.json")
	sockPath := filepath.Join(dir, "supervise.sock")

	// Pre-create a regular file to simulate a stale socket.
	if err := os.WriteFile(sockPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("pre-create stale file: %v", err)
	}

	ln, _, err := Listen(runFile)
	if err != nil {
		t.Fatalf("Listen with stale socket: unexpected error: %v", err)
	}
	ln.Close()
}

// TestListen_unlinkOnClose verifies that closing the listener removes the
// socket file from the filesystem (Go stdlib default for UnixListener).
func TestListen_unlinkOnClose(t *testing.T) {
	t.Parallel()
	dir := socketTempDir(t)
	runFile := filepath.Join(dir, "running.json")
	sockPath := filepath.Join(dir, "supervise.sock")

	ln, _, err := Listen(runFile)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ln.Close()

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket file still present after Close (err=%v); expected not-exist", err)
	}
}
