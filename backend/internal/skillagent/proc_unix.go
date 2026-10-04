//go:build unix

package skillagent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func supportedOS() bool { return true }

// runGroup runs cmd in its own process group, records its pid, and kills the
// whole group when ctx ends -- the CLI and anything it started (its bundled
// search binary) go together, so a cancelled run leaves no child reading the
// staged copy after AO has decided the run is over.
func runGroup(ctx context.Context, cmd *exec.Cmd, pidFile string) ([]byte, error) {
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o600)
	defer func() { _ = os.Remove(pidFile) }()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.Bytes(), err
	case <-ctx.Done():
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-done
		}
		return out.Bytes(), ctx.Err()
	}
}

// reapPID kills the process group a dead daemon's agent left behind. It acts
// only when the recorded process is still alive AND its command line names
// this run's staging directory, so a recycled pid belonging to something else
// is never signalled.
func reapPID(pidFile, stagingDir string) bool {
	b, err := os.ReadFile(pidFile) //nolint:gosec // AO's own pid file under its staging root.
	if err != nil {
		return false
	}
	defer func() { _ = os.Remove(pidFile) }()
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return false
	}
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if !processMatches(pid, stagingDir) {
		return false
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return true
}

// processMatches reports whether pid's working directory is the run's
// staging directory. On Linux /proc/<pid>/cwd answers directly; on macOS lsof
// does. Both report the resolved path, so the staging path is resolved too.
func processMatches(pid int, stagingDir string) bool {
	want := map[string]bool{stagingDir: true}
	if resolved, err := filepath.EvalSymlinks(stagingDir); err == nil {
		want[resolved] = true
	}
	if cwd, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd"); err == nil {
		return want[cwd]
	}
	out, err := exec.Command("/usr/sbin/lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output() //nolint:gosec // fixed argv, numeric pid.
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") && want[strings.TrimPrefix(line, "n")] {
			return true
		}
	}
	return false
}
