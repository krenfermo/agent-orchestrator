package gitworktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// branch_probe_verdict_test.go -- the SIGE incident, pinned.
//
//	git check-ref-format --branch ao/sige-12/root   ->   signal: killed
//
// AO answered INVALID_BRANCH. `ao/sige-12/root` is a name git accepts without
// complaint, so that verdict was invented out of a process that never rendered
// one, and it was permanent: an invalid branch name is not something a retry
// fixes, so the launch was dead.
//
// These tests hold the line in BOTH directions. A killed, cancelled, or
// unstartable probe must never be reported as an invalid name; and a probe that
// really did run and reject the name must still be reported as invalid. Neither
// half weakens git's authority over what a legal ref is -- git remains the only
// thing that decides, and no alternative branch is ever substituted.

// killedError returns a real *exec.ExitError from a process terminated by a
// signal -- the exact shape exec.CommandContext produces when it kills a child
// whose context was cancelled, and the exact shape the SIGE incident logged.
func killedError(t *testing.T) error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestGitWorktreeSleepHelper")
	cmd.Env = append(os.Environ(), "GO_WANT_GITWORKTREE_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("wait = %v, want an *exec.ExitError", err)
	}
	if exit.ProcessState.Exited() {
		t.Fatalf("helper exited normally; this test needs a signalled process")
	}
	return err
}

func TestGitWorktreeSleepHelper(t *testing.T) {
	if os.Getenv("GO_WANT_GITWORKTREE_SLEEP") != "1" {
		return
	}
	select {}
}

// A killed check-ref-format is not a verdict about the name. The valid name
// from the incident is used deliberately: the bug was that AO called it
// invalid.
func TestCreateDoesNotCallAKilledProbeAnInvalidBranch(t *testing.T) {
	ws, err := New(Options{ManagedRoot: t.TempDir(), RepoResolver: StaticRepoResolver{"proj": t.TempDir()}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	killed := killedError(t)
	ws.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "check-ref-format") {
			return nil, commandError{args: args, err: killed}
		}
		t.Fatalf("no git beyond check-ref-format should run once the probe failed: %v", args)
		return nil, nil
	}
	_, err = ws.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "proj", SessionID: "sess", Branch: "ao/sige-12/root",
	})
	if errors.Is(err, ports.ErrWorkspaceBranchInvalid) {
		t.Fatalf("err = %v, want NOT ports.ErrWorkspaceBranchInvalid: a killed probe rendered no verdict", err)
	}
	if !errors.Is(err, ports.ErrWorkspaceProbeInconclusive) {
		t.Fatalf("err = %v, want ports.ErrWorkspaceProbeInconclusive", err)
	}
	// The cause has to survive: "signal: killed" is what an operator needs in
	// order to look for a cancellation, a deadline or an OOM kill.
	if !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("err = %v, want the underlying cause preserved", err)
	}
	if !strings.Contains(err.Error(), "ao/sige-12/root") {
		t.Fatalf("err = %v, want the branch that was being checked named", err)
	}
}

// A cancelled context is the other half of the same incident: the process may
// even have exited, but AO stopped waiting for it, so whatever came back is not
// an answer AO asked for.
func TestCreateDoesNotCallACancelledProbeAnInvalidBranch(t *testing.T) {
	ws, err := New(Options{ManagedRoot: t.TempDir(), RepoResolver: StaticRepoResolver{"proj": t.TempDir()}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	rejected := exitStatusOne(t)
	ctx, cancel := context.WithCancel(context.Background())
	ws.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "check-ref-format") {
			cancel()
			return nil, commandError{args: args, err: rejected}
		}
		return nil, nil
	}
	_, err = ws.Create(ctx, ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess", Branch: "ao/sige-12/root"})
	if errors.Is(err, ports.ErrWorkspaceBranchInvalid) {
		t.Fatalf("err = %v, want NOT ports.ErrWorkspaceBranchInvalid under a cancelled context", err)
	}
	if !errors.Is(err, ports.ErrWorkspaceProbeInconclusive) {
		t.Fatalf("err = %v, want ports.ErrWorkspaceProbeInconclusive", err)
	}
}

// A probe that could not START -- git missing from PATH, not executable -- is
// not a statement about the name either.
func TestCreateDoesNotCallAnUnstartableProbeAnInvalidBranch(t *testing.T) {
	ws, err := New(Options{ManagedRoot: t.TempDir(), RepoResolver: StaticRepoResolver{"proj": t.TempDir()}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ws.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "check-ref-format") {
			return nil, commandError{args: args, err: exec.ErrNotFound}
		}
		return nil, nil
	}
	_, err = ws.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess", Branch: "ao/sige-12/root"})
	if errors.Is(err, ports.ErrWorkspaceBranchInvalid) {
		t.Fatalf("err = %v, want NOT ports.ErrWorkspaceBranchInvalid when git never ran", err)
	}
	if !errors.Is(err, ports.ErrWorkspaceProbeInconclusive) {
		t.Fatalf("err = %v, want ports.ErrWorkspaceProbeInconclusive", err)
	}
}

// The validation itself is untouched: real git, real repository, the exact name
// from the incident, and the exact name git really does reject.
func TestValidateBranchAgainstRealGit(t *testing.T) {
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not available: %v", err)
	}
	repo := t.TempDir()
	if out, err := exec.Command(binary, "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	ws, err := New(Options{ManagedRoot: t.TempDir(), RepoResolver: StaticRepoResolver{"proj": repo}, Binary: binary})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := ws.validateBranch(context.Background(), repo, "ao/sige-12/root"); err != nil {
		t.Fatalf("validateBranch(ao/sige-12/root) = %v, want nil: git accepts this name", err)
	}
	err = ws.validateBranch(context.Background(), repo, "bad branch!!")
	if !errors.Is(err, ports.ErrWorkspaceBranchInvalid) {
		t.Fatalf("validateBranch(bad branch!!) = %v, want ports.ErrWorkspaceBranchInvalid", err)
	}
}
