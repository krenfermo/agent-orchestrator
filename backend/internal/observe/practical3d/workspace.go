package practical3d

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// EmptyWorkspaceManager gives technical positions an empty working copy.
type EmptyWorkspaceManager struct{}

// Prepare creates an empty working copy.
func (EmptyWorkspaceManager) Prepare(_ context.Context, _ Manifest, _ Position, w PositionWorkspace) error {
	return os.Mkdir(w.WorkingCopy, 0o700)
}

// Finalize verifies local teardown.
func (EmptyWorkspaceManager) Finalize(ctx context.Context, _ Manifest, _ Position, w PositionWorkspace) error {
	return VerifyNoPositionProcesses(ctx, w)
}

// GitWorkspaceManager gives each position an independent clone (no shared
// object store, hooks, worktree list or remote) of the frozen fixture commit.
type GitWorkspaceManager struct{ FixtureRepo string }

// Prepare clones the fixture commit into a clean, remote-less working copy.
func (g GitWorkspaceManager) Prepare(ctx context.Context, m Manifest, _ Position, w PositionWorkspace) error {
	repo, err := filepath.Abs(g.FixtureRepo)
	if err != nil {
		return err
	}
	prod, err := productionDataDir()
	if err != nil {
		return err
	}
	if within(repo, prod) || within(resolveOrSelf(repo), resolveOrSelf(prod)) {
		return fmt.Errorf("fixture repo cannot be production AO data")
	}
	if _, err := gitOutput(ctx, repo, "cat-file", "-e", m.FixtureCommit+"^{commit}"); err != nil {
		return fmt.Errorf("fixture commit unavailable: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--quiet", "--no-hardlinks", "--no-checkout", repo, w.WorkingCopy)
	if raw, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("clone fixture: %w: %s", err, strings.TrimSpace(string(raw)))
	}
	for _, args := range [][]string{{"checkout", "--quiet", "--detach", m.FixtureCommit}, {"remote", "remove", "origin"}} {
		if _, err := gitOutput(ctx, w.WorkingCopy, args...); err != nil {
			return err
		}
	}
	head, err := gitOutput(ctx, w.WorkingCopy, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != m.FixtureCommit {
		return fmt.Errorf("working copy HEAD does not match fixture commit")
	}
	status, err := gitOutput(ctx, w.WorkingCopy, "status", "--porcelain=v1", "--untracked-files=all", "--ignored")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(status)) != 0 {
		return fmt.Errorf("new working copy is not clean")
	}
	return nil
}

// Finalize verifies local teardown.
func (g GitWorkspaceManager) Finalize(ctx context.Context, _ Manifest, _ Position, w PositionWorkspace) error {
	return VerifyNoPositionProcesses(ctx, w)
}

// VerifyNoPositionProcesses fails closed if any process other than the
// runner still has its working directory under the position root or names
// that root in its argv (Linux: also in its environment). Position processes
// run with cwd and AO_DATA_DIR/HOME/TMPDIR inside that root, so a survivor
// is a teardown violation.
func VerifyNoPositionProcesses(ctx context.Context, w PositionWorkspace) error {
	if w.Root == "" {
		return nil
	}
	root := resolveOrSelf(w.Root)
	self := os.Getpid()
	if runtime.GOOS == "linux" {
		return verifyProcLinux(root, w.Root, self)
	}
	raw, err := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == strconv.Itoa(self) {
			continue
		}
		if strings.Contains(line, w.Root) || strings.Contains(line, root) {
			return fmt.Errorf("process %s still references the position root", fields[0])
		}
	}
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-w", "-a", "-d", "cwd", "-F", "pn").Output()
	if err != nil && len(out) == 0 {
		return fmt.Errorf("list process working directories: %w", err)
	}
	pid := ""
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid = line[1:]
		case strings.HasPrefix(line, "n") && pid != strconv.Itoa(self):
			if cwd := line[1:]; within(cwd, root) || within(cwd, w.Root) {
				return fmt.Errorf("process %s still has its working directory in the position root", pid)
			}
		}
	}
	return nil
}

func verifyProcLinux(root, raw string, self int) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		dir := "/proc/" + e.Name()
		if cwd, err := os.Readlink(filepath.Join(dir, "cwd")); err == nil && (within(cwd, root) || within(cwd, raw)) {
			return fmt.Errorf("process %d still has its working directory in the position root", pid)
		}
		for _, f := range []string{"cmdline", "environ"} {
			if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil && (bytes.Contains(b, []byte(root)) || bytes.Contains(b, []byte(raw))) {
				return fmt.Errorf("process %d still references the position root", pid)
			}
		}
	}
	return nil
}

func gitOutput(ctx context.Context, repo string, args ...string) ([]byte, error) {
	all := append([]string{"-C", repo}, args...)
	cmd := exec.CommandContext(ctx, "git", all...)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}
