//go:build !windows

package supervisor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// maxUnixSocketPathBytes: Darwin's sockaddr_un.sun_path is 104 bytes including
// the trailing NUL (Linux allows 108); stay valid on both.
const maxUnixSocketPathBytes = 103

// Listen creates this daemon instance's supervisor Unix socket beside the
// run-file: <dir(runFilePath)>/supervise-<token(instanceID)>.sock.
//
// It never removes a socket it has not proven stale. The only candidate is the
// predecessor recorded in this run-file (prior), and only when (1) prior.Exited:
// the caller holds the run-file lock and that process is not alive -- this is
// the protection; the connection probe below would arm a live supervisor, so it
// never runs without it; (2) the recorded
// address is exactly the name that instance would have used, in this directory;
// (3) the file is a socket, not a symlink or regular file; and (4) nothing
// accepts connections on it, twice. The legacy shared supervise.sock is never
// touched. A too-long path or a missing identity fails closed: no listener.
func Listen(runFilePath, instanceID string, prior *Prior) (net.Listener, string, error) {
	name, err := SocketName(instanceID)
	if err != nil {
		return nil, "", err
	}
	dir, err := filepath.Abs(filepath.Dir(runFilePath))
	if err != nil {
		return nil, "", fmt.Errorf("supervisor: resolve run-file directory: %w", err)
	}
	sockPath := filepath.Join(dir, name)
	if n := len([]byte(sockPath)); n > maxUnixSocketPathBytes {
		return nil, "", fmt.Errorf("supervisor: socket path %q is %d bytes; maximum is %d", sockPath, n, maxUnixSocketPathBytes)
	}
	removeProvenStalePrior(dir, prior)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, "", err
	}
	return ln, sockPath, nil
}

func removeProvenStalePrior(dir string, prior *Prior) {
	if prior == nil || !prior.Exited || prior.Address == "" {
		return
	}
	name, err := SocketName(prior.InstanceID)
	if err != nil {
		return
	}
	expected := filepath.Join(dir, name)
	if filepath.Clean(prior.Address) != expected {
		return
	}
	fi, err := os.Lstat(expected)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return
	}
	for probe := 0; probe < 2; probe++ {
		if !refusesConnections(expected) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Re-check it is still the same socket file before removing it.
	if again, err := os.Lstat(expected); err == nil && os.SameFile(fi, again) {
		_ = os.Remove(expected)
	}
}

// refusesConnections is true only for a definite "nobody listening" answer.
// Any other outcome (accepted, timeout, permission) is not proof of staleness.
func refusesConnections(path string) bool {
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}
