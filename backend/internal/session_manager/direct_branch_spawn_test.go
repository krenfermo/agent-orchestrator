package sessionmanager

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/directbranch"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitworktree"
	workspacerouter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/router"
)

// Checkpoint 8P-E.14 end-to-end tests for ordinary task execution.
//
// These deliberately drive the REAL Spawn path over a REAL git repository, the
// REAL workspace router (which picks the real directbranch or gitworktree
// adapter from the project's execution mode), a REAL sqlite store and a REAL
// branchlock.Manager. Only the agent runtime is faked, because launching a real
// agent is not what any of these assert.
//
// The reason for that insistence is requirement-shaped: the incident these
// tests exist for was invisible to every unit-level helper. ProjectSpawnBranch
// already returned "feat/engineering-control-center" correctly, and a test of
// that helper passed throughout. The defect lived in what the whole path did
// together.

const incidentBranch = "feat/engineering-control-center"

// The branch-creation regression, stated exactly as the incident:
// direct-branch project, configured branch feat/engineering-control-center,
// task slug cancel-archive-workflows. AO must select and stay on the
// configured branch and must never produce
// feat/engineering-control-center-cancel-archive-workflows.
func TestDirectBranchTaskUsesConfiguredBranchAndCreatesNoDerivedBranch(t *testing.T) {
	h := newDirectBranchHarness(t, incidentBranch)

	rec, _, _, err := h.manager.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID:   "agent-orchestrator",
		Kind:        domain.KindWorker,
		Harness:     domain.HarnessClaudeCode,
		DisplayName: "cancel-archive-workflows",
		Prompt:      "clean up / cancel stale workflows from the UI",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	if rec.Metadata.Branch != incidentBranch {
		t.Fatalf("session branch = %q, want the configured %q", rec.Metadata.Branch, incidentBranch)
	}
	// The workspace is the registered repository itself, not a worktree.
	if rec.Metadata.WorkspacePath != h.repo {
		t.Fatalf("workspace path = %q, want the registered repo %q", rec.Metadata.WorkspacePath, h.repo)
	}
	if got := h.currentBranch(); got != incidentBranch {
		t.Fatalf("repository is on %q, want %q", got, incidentBranch)
	}

	// The incident branch itself, and any other derived branch, must not exist.
	branches := h.branches()
	derived := incidentBranch + "-cancel-archive-workflows"
	for _, b := range branches {
		if b == derived {
			t.Fatalf("spawn created the incident branch %q", derived)
		}
		if b != incidentBranch && b != "main" {
			t.Fatalf("spawn created an unexpected branch %q (branches: %v)", b, branches)
		}
		if strings.HasPrefix(b, "ao/") {
			t.Fatalf("direct-branch spawn generated an ao/* branch %q", b)
		}
	}

	// And the task owns the branch, under the same lock a workflow would take.
	held := h.heldLocks()
	if len(held) != 1 {
		t.Fatalf("held locks = %d, want the task to own the branch", len(held))
	}
	if held[0].Branch != incidentBranch || held[0].SessionID != string(rec.ID) || held[0].WorkflowRunID != "" {
		t.Fatalf("lock = %#v, want session-owned on the configured branch", held[0])
	}
}

// Isolated-worktree mode must be completely unaffected: it still generates its
// own ao/* branch and its own worktree, and it takes no lock at all.
func TestIsolatedWorktreeTaskStillGetsItsOwnGeneratedBranch(t *testing.T) {
	h := newIsolatedHarness(t)

	rec, _, _, err := h.manager.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: "agent-orchestrator",
		Kind:      domain.KindWorker,
		Harness:   domain.HarnessClaudeCode,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if !strings.HasPrefix(rec.Metadata.Branch, "ao/") {
		t.Fatalf("worktree-mode branch = %q, want a generated ao/* branch", rec.Metadata.Branch)
	}
	if rec.Metadata.WorkspacePath == h.repo {
		t.Fatal("worktree-mode session was placed in the registered repository instead of a worktree")
	}
	if held := h.heldLocks(); len(held) != 0 {
		t.Fatalf("held locks = %d, want none: worktree mode is isolated by construction", len(held))
	}
}

// A task must not be able to start while a workflow owns the branch, and — the
// property that matters most — it must not route around the contention by
// creating a branch of its own.
func TestDirectBranchTaskIsRefusedWhileAWorkflowOwnsTheBranch(t *testing.T) {
	h := newDirectBranchHarness(t, incidentBranch)
	ctx := context.Background()

	if _, err := h.locks.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: "agent-orchestrator", RunID: "wf-1", StepID: "wfs-1",
	}); err != nil {
		t.Fatalf("workflow acquire: %v", err)
	}

	_, _, _, err := h.manager.Spawn(ctx, ports.SpawnConfig{
		ProjectID:   "agent-orchestrator",
		Kind:        domain.KindWorker,
		Harness:     domain.HarnessClaudeCode,
		DisplayName: "cancel-archive-workflows",
	})
	if !errors.Is(err, ErrBranchBusy) {
		t.Fatalf("spawn err = %v, want ErrBranchBusy", err)
	}
	// The refusal has to name the owner so the operator knows what to do.
	if !strings.Contains(err.Error(), "workflow wf-1") {
		t.Fatalf("error does not identify the holder: %v", err)
	}

	// No derived branch, no branch switch, and still exactly one owner.
	for _, b := range h.branches() {
		if b != incidentBranch && b != "main" {
			t.Fatalf("blocked spawn created branch %q", b)
		}
	}
	held := h.heldLocks()
	if len(held) != 1 || held[0].WorkflowRunID != "wf-1" {
		t.Fatalf("held = %#v, want only the workflow's lock", held)
	}

	// Once the workflow releases, the same task starts normally on the same branch.
	if _, err := h.locks.ReleaseRun(ctx, "wf-1", "run completed"); err != nil {
		t.Fatalf("release run: %v", err)
	}
	rec, _, _, err := h.manager.Spawn(ctx, ports.SpawnConfig{
		ProjectID: "agent-orchestrator", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
	})
	if err != nil {
		t.Fatalf("spawn after release: %v", err)
	}
	if rec.Metadata.Branch != incidentBranch {
		t.Fatalf("branch after release = %q, want %q", rec.Metadata.Branch, incidentBranch)
	}
}

// The inverse ordering: a task owns the branch, and a workflow child spawn for
// a DIFFERENT run must not be able to take it either.
func TestWorkflowCannotTakeTheBranchWhileATaskOwnsIt(t *testing.T) {
	h := newDirectBranchHarness(t, incidentBranch)
	ctx := context.Background()

	rec, _, _, err := h.manager.Spawn(ctx, ports.SpawnConfig{
		ProjectID: "agent-orchestrator", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
	})
	if err != nil {
		t.Fatalf("task spawn: %v", err)
	}

	_, err = h.locks.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: "agent-orchestrator", RunID: "wf-2", StepID: "wfs-1",
	})
	var conflict domain.BranchLockConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("workflow acquire err = %v, want a conflict with the task", err)
	}
	if conflict.Holder.SessionID != string(rec.ID) {
		t.Fatalf("holder = %#v, want the task session", conflict.Holder)
	}
}

// A workflow's own worker session must not contend with the run that spawned
// it: the run already holds the lock, and a child that queued behind its own
// parent would deadlock every autonomous direct-branch run.
func TestWorkflowChildSpawnDoesNotContendWithItsOwnRun(t *testing.T) {
	h := newDirectBranchHarness(t, incidentBranch)
	ctx := context.Background()

	if _, err := h.locks.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: "agent-orchestrator", RunID: "wf-3", StepID: "wfs-1",
	}); err != nil {
		t.Fatalf("workflow acquire: %v", err)
	}
	rec, _, _, err := h.manager.Spawn(ctx, ports.SpawnConfig{
		ProjectID:     "agent-orchestrator",
		Kind:          domain.KindWorker,
		Harness:       domain.HarnessClaudeCode,
		WorkflowRunID: "wf-3",
	})
	if err != nil {
		t.Fatalf("workflow child spawn: %v", err)
	}
	if rec.Metadata.Branch != incidentBranch {
		t.Fatalf("child branch = %q, want %q", rec.Metadata.Branch, incidentBranch)
	}
	held := h.heldLocks()
	if len(held) != 1 || held[0].WorkflowRunID != "wf-3" {
		t.Fatalf("held = %#v, want the run's single lock, not a second session-owned one", held)
	}
}

// A human's uncommitted work is not something waiting fixes, and it is not
// something AO may absorb, stash, or commit. The spawn is refused and the
// working tree is left exactly as it was.
func TestDirectBranchTaskRefusesADirtyRepository(t *testing.T) {
	h := newDirectBranchHarness(t, incidentBranch)
	dirty := filepath.Join(h.repo, "WIP.md")
	if err := os.WriteFile(dirty, []byte("a human's work in progress\n"), 0o600); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	_, _, _, err := h.manager.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: "agent-orchestrator", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
	})
	if !errors.Is(err, ErrBranchDirty) {
		t.Fatalf("spawn err = %v, want ErrBranchDirty", err)
	}
	// The file is still there, untracked and uncommitted: not stashed, not
	// reset, not swept into a commit.
	if _, statErr := os.Stat(dirty); statErr != nil {
		t.Fatalf("AO removed the user's uncommitted file: %v", statErr)
	}
	if out := h.git("status", "--porcelain"); !strings.Contains(out, "WIP.md") {
		t.Fatalf("user's change is no longer uncommitted; status:\n%s", out)
	}
	if h.headSHA() != h.initialHead {
		t.Fatal("AO created a commit while refusing a dirty repository")
	}
	if held := h.heldLocks(); len(held) != 0 {
		t.Fatalf("held = %#v, want no lock left behind by a refused spawn", held)
	}
}

// Termination is the single release point. Whatever ends a task, the branch
// must come free — otherwise the next task or workflow queues behind a session
// that no longer exists.
func TestTerminatingADirectBranchTaskReleasesItsLock(t *testing.T) {
	h := newDirectBranchHarness(t, incidentBranch)
	ctx := context.Background()

	rec, _, _, err := h.manager.Spawn(ctx, ports.SpawnConfig{
		ProjectID: "agent-orchestrator", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if len(h.heldLocks()) != 1 {
		t.Fatal("spawn did not take the branch lock")
	}

	if err := h.lcm.MarkTerminated(ctx, rec.ID); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if held := h.heldLocks(); len(held) != 0 {
		t.Fatalf("held = %#v, want the lock released on termination", held)
	}

	// And the branch is genuinely available again.
	if _, err := h.locks.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: "agent-orchestrator", RunID: "wf-after", StepID: "wfs-1",
	}); err != nil {
		t.Fatalf("acquire after task termination: %v", err)
	}
}

// A spawn that fails after taking the lock must not leave the branch occupied
// by a session that never started. An unknown harness fails late enough to
// exercise the rollback path.
func TestFailedDirectBranchSpawnDoesNotLeakTheLock(t *testing.T) {
	h := newDirectBranchHarness(t, "does/not/exist")

	_, _, _, err := h.manager.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: "agent-orchestrator", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
	})
	if err == nil {
		t.Fatal("spawn onto a nonexistent branch succeeded")
	}
	if held := h.heldLocks(); len(held) != 0 {
		t.Fatalf("held = %#v, want the lock released after the failed spawn", held)
	}
}

// ---- harness ----

type directBranchHarness struct {
	t           *testing.T
	repo        string
	store       *sqlite.Store
	locks       *branchlock.Manager
	manager     *Manager
	lcm         *storeLCM
	initialHead string
}

func newDirectBranchHarness(t *testing.T, branch string) *directBranchHarness {
	return newHarness(t, branch, domain.ExecutionDirectBranch)
}

func newIsolatedHarness(t *testing.T) *directBranchHarness {
	return newHarness(t, "main", domain.ExecutionIsolatedWorktree)
}

func newHarness(t *testing.T, branch string, mode domain.ExecutionMode) *directBranchHarness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the direct-branch spawn tests")
	}
	dataDir := t.TempDir()
	repo := initHarnessRepo(t, branch)
	store := sqlitetest.MustOpenAt(t, dataDir)

	if err := store.UpsertProject(context.Background(), domain.ProjectRecord{
		ID:           "agent-orchestrator",
		Path:         repo,
		RegisteredAt: time.Now().UTC(),
		Config: domain.ProjectConfig{
			DefaultBranch: branch,
			ExecutionMode: mode,
			SessionPrefix: "ao",
			Worker:        domain.RoleOverride{Harness: domain.HarnessClaudeCode},
			Orchestrator:  domain.RoleOverride{Harness: domain.HarnessClaudeCode},
		},
	}); err != nil {
		t.Fatalf("register project: %v", err)
	}

	resolver := repoResolver{path: repo}
	direct, err := directbranch.New(directbranch.Options{RepoResolver: resolver})
	if err != nil {
		t.Fatalf("directbranch adapter: %v", err)
	}
	worktree, err := gitworktree.New(gitworktree.Options{
		ManagedRoot:   filepath.Join(dataDir, "worktrees"),
		RepoResolver:  resolver,
		DefaultBranch: branch,
	})
	if err != nil {
		t.Fatalf("gitworktree adapter: %v", err)
	}
	router := workspacerouter.New(workspacerouter.Deps{
		Git:          worktree,
		DirectBranch: direct,
		Projects:     store,
	})

	locks := branchlock.New(branchlock.Deps{
		Store:      store,
		Preflight:  router,
		OwnerToken: "test-daemon",
	})

	lcm := &storeLCM{store: store}
	mgr := New(Deps{
		Runtime:   &fakeRuntime{},
		Agents:    fakeAgents{},
		Workspace: router,
		Store:     store,
		Messenger: &fakeMessenger{},
		Lifecycle: lcm,
		DataDir:   dataDir,
		LookPath:  func(string) (string, error) { return "/bin/true", nil },
	})
	mgr.SetBranchLocks(sessionBranchLockAdapter{mgr: locks})
	lcm.locks = locks

	h := &directBranchHarness{t: t, repo: repo, store: store, locks: locks, manager: mgr, lcm: lcm}
	h.initialHead = h.headSHA()
	return h
}

func initHarnessRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "ao@example.com")
	run("config", "user.name", "AO Tests")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "initial")
	run("branch", "-M", "main")
	// Resolve symlinks so the path matches what the adapters canonicalize to.
	// On macOS t.TempDir() hands back /var/... while /var is a symlink to
	// /private/var, and the workspace adapters return the resolved form.
	// The configured branch has to already exist: direct-branch mode never
	// invents one (BRANCH FIDELITY). "does/not/exist" is used by the failure
	// test precisely because it is absent.
	if branch != "main" && branch != "does/not/exist" {
		run("branch", branch)
	}
	// Resolve symlinks so the path matches what the adapters canonicalize to:
	// on macOS t.TempDir() hands back /var/... while /var symlinks to
	// /private/var, and the workspace adapters return the resolved form.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir
}

func (h *directBranchHarness) git(args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", h.repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (h *directBranchHarness) currentBranch() string {
	return strings.TrimSpace(h.git("branch", "--show-current"))
}

func (h *directBranchHarness) headSHA() string {
	return strings.TrimSpace(h.git("rev-parse", "HEAD"))
}

func (h *directBranchHarness) branches() []string {
	var out []string
	for _, line := range strings.Split(h.git("for-each-ref", "--format=%(refname:short)", "refs/heads"), "\n") {
		if b := strings.TrimSpace(line); b != "" {
			out = append(out, b)
		}
	}
	return out
}

func (h *directBranchHarness) heldLocks() []domain.BranchLock {
	h.t.Helper()
	held, err := h.store.ListHeldBranchLocks(context.Background())
	if err != nil {
		h.t.Fatalf("list held locks: %v", err)
	}
	return held
}

type repoResolver struct{ path string }

func (r repoResolver) RepoPath(domain.ProjectID) (string, error) { return r.path, nil }

// sessionBranchLockAdapter mirrors the daemon's own wiring adapter so the test
// exercises the same translation production uses.
type sessionBranchLockAdapter struct{ mgr *branchlock.Manager }

func (s sessionBranchLockAdapter) AcquireForSession(ctx context.Context, projectID domain.ProjectID, sessionID domain.SessionID) ([]domain.BranchLock, error) {
	return s.mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: projectID, SessionID: string(sessionID)})
}

func (s sessionBranchLockAdapter) ReleaseSession(ctx context.Context, sessionID, reason string) (int64, error) {
	return s.mgr.ReleaseSession(ctx, sessionID, reason)
}

// storeLCM is a minimal lifecycle recorder over the real store. It reproduces
// the one behavior these tests depend on: MarkTerminated is the choke point
// that releases a task's branch locks.
type storeLCM struct {
	store *sqlite.Store
	locks *branchlock.Manager
}

func (l *storeLCM) PrepareLaunch(domain.SessionID, string) error { return nil }
func (l *storeLCM) CancelLaunch(domain.SessionID, string)        {}
func (l *storeLCM) ReleaseLaunch(domain.SessionID, string)       {}

func (l *storeLCM) MarkSpawned(ctx context.Context, id domain.SessionID, metadata domain.SessionMetadata) error {
	rec, ok, err := l.store.GetSession(ctx, id)
	if err != nil || !ok {
		return err
	}
	rec.Metadata = metadata
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now().UTC()}
	return l.store.UpdateSession(ctx, rec)
}

func (l *storeLCM) CommitControllerEpoch(context.Context, domain.SessionID, domain.SessionMode, domain.SessionMode, string, bool) (bool, error) {
	return false, nil
}

func (l *storeLCM) ConfirmAgentSwitchSourceStopped(context.Context, domain.AgentSwitchSourceStopConfirmation) (bool, error) {
	return false, nil
}

func (l *storeLCM) ActivateAgentSwitchTarget(context.Context, domain.AgentSwitchTargetActivation) (bool, error) {
	return false, nil
}

func (l *storeLCM) MarkTerminated(ctx context.Context, id domain.SessionID) error {
	if l.locks != nil {
		if _, err := l.locks.ReleaseSession(ctx, string(id), "task session terminated"); err != nil {
			return err
		}
	}
	rec, ok, err := l.store.GetSession(ctx, id)
	if err != nil || !ok {
		return err
	}
	rec.IsTerminated = true
	rec.Activity = domain.Activity{State: domain.ActivityExited, LastActivityAt: time.Now().UTC()}
	return l.store.UpdateSession(ctx, rec)
}
