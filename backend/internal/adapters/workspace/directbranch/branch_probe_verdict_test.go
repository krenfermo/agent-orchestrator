package directbranch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// branch_probe_verdict_test.go -- gitworktree's counterpart, for the adapter
// that writes into the USER's own repository.
//
// The stakes are higher here than in the worktree adapter, in both directions.
// A spurious "invalid branch name" stops a direct-branch run permanently, and a
// probe whose failure is misread is a probe AO cannot trust to protect a
// checkout a person is working in. Two things are pinned: a killed probe is
// never a verdict, and the cause is never discarded -- the previous form threw
// the underlying error away entirely, so `signal: killed` never reached anyone.

func directBranchKilledError(t *testing.T) error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestDirectBranchSleepHelper")
	cmd.Env = append(os.Environ(), "GO_WANT_DIRECTBRANCH_SLEEP=1")
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

func directBranchExitOne(t *testing.T) error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestDirectBranchExitOneHelper")
	cmd.Env = append(os.Environ(), "GO_WANT_DIRECTBRANCH_EXIT_ONE=1")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected an exit error")
	}
	return err
}

func TestDirectBranchSleepHelper(t *testing.T) {
	if os.Getenv("GO_WANT_DIRECTBRANCH_SLEEP") != "1" {
		return
	}
	select {}
}

func TestDirectBranchExitOneHelper(t *testing.T) {
	if os.Getenv("GO_WANT_DIRECTBRANCH_EXIT_ONE") != "1" {
		return
	}
	os.Exit(1)
}

func directBranchProbeWorkspace(t *testing.T, probeErr error) (*Workspace, string) {
	t.Helper()
	repo := t.TempDir()
	ws, err := New(Options{RepoResolver: staticRepos{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ws.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "rev-parse --git-dir"):
			return []byte(".git\n"), nil
		case strings.Contains(joined, "check-ref-format"):
			return nil, probeErr
		}
		t.Fatalf("no git beyond the probe should run once it failed: %v", args)
		return nil, nil
	}
	return ws, repo
}

func TestDirectBranchDoesNotCallAKilledProbeAnInvalidBranch(t *testing.T) {
	ws, _ := directBranchProbeWorkspace(t, directBranchKilledError(t))
	_, err := ws.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "proj", SessionID: "sess", Branch: "ao/sige-12/root",
	})
	if errors.Is(err, ports.ErrWorkspaceBranchInvalid) {
		t.Fatalf("err = %v, want NOT ports.ErrWorkspaceBranchInvalid: a killed probe rendered no verdict", err)
	}
	if !errors.Is(err, ports.ErrWorkspaceProbeInconclusive) {
		t.Fatalf("err = %v, want ports.ErrWorkspaceProbeInconclusive", err)
	}
	if !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("err = %v, want the underlying cause preserved rather than discarded", err)
	}
}

func TestDirectBranchStillRejectsAnInvalidBranchName(t *testing.T) {
	ws, _ := directBranchProbeWorkspace(t, directBranchExitOne(t))
	_, err := ws.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "proj", SessionID: "sess", Branch: "bad branch!!",
	})
	if !errors.Is(err, ports.ErrWorkspaceBranchInvalid) {
		t.Fatalf("err = %v, want ports.ErrWorkspaceBranchInvalid: git ran and said no", err)
	}
	if !strings.Contains(err.Error(), "bad branch!!") {
		t.Fatalf("err = %v, want the rejected name in the message", err)
	}
}

// Real git, real repository: the validation is unchanged and no alternative
// branch is ever substituted for one git refuses.
func TestDirectBranchValidateAgainstRealGit(t *testing.T) {
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not available: %v", err)
	}
	repo := t.TempDir()
	if out, err := exec.Command(binary, "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	ws, err := New(Options{RepoResolver: staticRepos{"proj": repo}, Binary: binary})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := ws.validateBranchName(context.Background(), repo, "ao/sige-12/root"); err != nil {
		t.Fatalf("validateBranchName(ao/sige-12/root) = %v, want nil: git accepts this name", err)
	}
	if err := ws.validateBranchName(context.Background(), repo, "bad branch!!"); !errors.Is(err, ports.ErrWorkspaceBranchInvalid) {
		t.Fatalf("validateBranchName(bad branch!!) = %v, want ports.ErrWorkspaceBranchInvalid", err)
	}
}
