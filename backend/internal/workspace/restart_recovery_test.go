package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/worktree"
)

// These run against the real git binary, for the same reason the rest of this
// package's integration tests do: every property being asserted -- whether a
// worktree registration survives its directory, whether git refuses to remove a
// dirty tree, whether a ref delete is really a compare-and-delete -- is a
// property of git, and a fake would only prove that this file agrees with
// itself.
//
// The scenario in all of them is a daemon that died. What is being tested is
// never "does the happy path work" but "given the exact durable state a crash
// at THIS point leaves behind, does the next boot reach a consistent one
// without duplicating a commit or destroying work".

// restartFixture is one project mid-plan: the user's repository, an AO-owned
// task worktree on an ao/* branch with the agent's commits on it, and the
// durable record of both.
type restartFixture struct {
	t      *testing.T
	binary string
	repo   string
	store  *fakeStore
	mgr    *Manager
	root   string
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	binary := requireGit(t)
	root := filepath.Join(t.TempDir(), "ao-worktrees")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	git(t, binary, repo, "init", "--initial-branch=main")
	git(t, binary, repo, "config", "user.email", "ao@example.com")
	git(t, binary, repo, "config", "user.name", "Ao Agents")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	git(t, binary, repo, "add", "README.md")
	git(t, binary, repo, "commit", "-m", "seed")

	store := newFakeStore()
	mgr, err := New(Options{
		Root:  root,
		Git:   worktree.NewExecGit(binary),
		Store: store,
		Now:   func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return &restartFixture{t: t, binary: binary, repo: repo, store: store, mgr: mgr, root: root}
}

// startTask is everything that happens before the agent types: the worktree is
// created and recorded active.
func (f *restartFixture) startTask(taskID string) domain.TaskWorktreeRecord {
	f.t.Helper()
	lease, err := f.mgr.Ensure(context.Background(), Request{
		ProjectID: "proj", WorkflowRunID: "wf-1", TaskID: taskID,
		RepoPath: f.repo, TargetBranch: "main", Mode: domain.ExecutionIsolatedWorktree,
	})
	if err != nil {
		f.t.Fatalf("ensure %s: %v", taskID, err)
	}
	return lease.Record
}

// agentCommits is the work itself, on the task's own ao/* branch.
func (f *restartFixture) agentCommits(rec domain.TaskWorktreeRecord, name, content string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(rec.Path, name), []byte(content), 0o600); err != nil {
		f.t.Fatalf("write %s: %v", name, err)
	}
	git(f.t, f.binary, rec.Path, "add", name)
	git(f.t, f.binary, rec.Path, "commit", "-m", "task work")
	return git(f.t, f.binary, rec.Path, "rev-parse", "HEAD")
}

// integrate is the Integration Coordinator's half, reduced to its outcome: the
// task's commits are now reachable from the target branch. It is done with a
// plain merge in the repository because what these tests are about is what
// happens AFTER that, not how it happened.
func (f *restartFixture) integrate(rec domain.TaskWorktreeRecord) string {
	f.t.Helper()
	git(f.t, f.binary, f.repo, "merge", "--ff-only", rec.Branch)
	return git(f.t, f.binary, f.repo, "rev-parse", "HEAD")
}

func (f *restartFixture) branchExists(branch string) bool {
	f.t.Helper()
	present, err := worktree.NewExecGit(f.binary).BranchExists(context.Background(), f.repo, branch)
	if err != nil {
		f.t.Fatalf("branch exists %s: %v", branch, err)
	}
	return present
}

func (f *restartFixture) dirExists(path string) bool {
	f.t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func (f *restartFixture) record(taskID string) domain.TaskWorktreeRecord {
	f.t.Helper()
	rec, ok, err := f.store.GetTaskWorktree(context.Background(), taskID)
	if err != nil || !ok {
		f.t.Fatalf("record %s: ok=%v err=%v", taskID, ok, err)
	}
	return rec
}

func (f *restartFixture) reconcile() ReconcileReport {
	f.t.Helper()
	report, err := f.mgr.Reconcile(context.Background())
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
	return report
}

func actionFor(report ReconcileReport, taskID string) ReconcileEntry {
	for _, e := range report.Entries {
		if e.TaskID == taskID {
			return e
		}
	}
	return ReconcileEntry{}
}

// Restart during work.
//
// The worker was mid-task when the daemon died: the worktree exists, the branch
// holds commits, and the record says active. The boot pass must recognise all
// three as agreeing and do nothing at all -- in particular it must not create a
// second worktree, and the agent's commits must still be there afterwards.
func TestRestartDuringWorkAdoptsTheWorktreeAndKeepsTheCommits(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)
	rec := f.startTask("t1")
	head := f.agentCommits(rec, "feature.txt", "half-finished work\n")

	report := f.reconcile()

	if got := actionFor(report, "t1").Action; got != ReconcileAdopted {
		t.Fatalf("action = %q, want adopted", got)
	}
	if !f.dirExists(rec.Path) {
		t.Fatal("the worktree the agent was working in is gone")
	}
	if got := git(t, f.binary, rec.Path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("worktree HEAD = %s, want the agent's commit %s", got, head)
	}
	if got := f.record("t1").State; got != domain.TaskWorktreeActive {
		t.Fatalf("state = %q, want active", got)
	}
	// The strongest form of "did not recreate": a fresh Ensure for the same
	// task returns the same directory rather than cutting another one, and the
	// commits are still on it.
	lease, err := f.mgr.Ensure(context.Background(), Request{
		ProjectID: "proj", WorkflowRunID: "wf-1", TaskID: "t1",
		RepoPath: f.repo, TargetBranch: "main", Mode: domain.ExecutionIsolatedWorktree,
	})
	if err != nil {
		t.Fatalf("ensure after reconcile: %v", err)
	}
	if lease.Path != rec.Path {
		t.Fatalf("ensure returned %s, want the existing worktree %s", lease.Path, rec.Path)
	}
	if got := git(t, f.binary, lease.Path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD after re-ensure = %s, want %s", got, head)
	}
}

// Restart mid-worktree-creation, on the far side of the git call.
//
// The record is written before `git worktree add` runs, so the crash window
// leaves state=creating with a directory that DOES exist. The repair is
// entirely in the record: the row is promoted to active and git is not asked to
// create anything, because there is nothing left to create.
func TestRestartMidWorktreeCreationAdoptsTheDirectoryThatWasCreated(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)
	rec := f.startTask("t1")
	head := f.agentCommits(rec, "feature.txt", "work\n")

	// Rewind the record to the state the crash would have left: the git call
	// landed, the "now it is active" write did not.
	rec.State = domain.TaskWorktreeCreating
	if err := f.store.UpsertTaskWorktree(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	report := f.reconcile()

	if got := actionFor(report, "t1").Action; got != ReconcileRecovered {
		t.Fatalf("action = %q, want recovered", got)
	}
	if got := f.record("t1").State; got != domain.TaskWorktreeActive {
		t.Fatalf("state = %q, want active", got)
	}
	if got := git(t, f.binary, rec.Path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("the recovered worktree lost its commit: HEAD = %s, want %s", got, head)
	}
}

// Restart mid-worktree-creation, on the near side of the git call -- or any
// later moment where the directory has since been removed by hand.
//
// The registration is pruned so the path can be re-materialised, and nothing
// else happens. Specifically the BRANCH is untouched: it still holds whatever
// the agent committed, and the next Ensure checks it out rather than cutting a
// new one from base.
func TestRestartWithAMissingDirectoryPrunesAndKeepsTheBranchesCommits(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)
	rec := f.startTask("t1")
	head := f.agentCommits(rec, "feature.txt", "work that must survive\n")
	if err := os.RemoveAll(rec.Path); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}

	report := f.reconcile()

	if got := actionFor(report, "t1").Action; got != ReconcilePruned {
		t.Fatalf("action = %q, want pruned", got)
	}
	if !f.branchExists(rec.Branch) {
		t.Fatal("the task branch was deleted along with its directory")
	}
	lease, err := f.mgr.Ensure(context.Background(), Request{
		ProjectID: "proj", WorkflowRunID: "wf-1", TaskID: "t1",
		RepoPath: f.repo, TargetBranch: "main", Mode: domain.ExecutionIsolatedWorktree,
	})
	if err != nil {
		t.Fatalf("ensure after prune: %v", err)
	}
	if got := git(t, f.binary, lease.Path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("the re-materialised worktree is at %s, want the agent's commit %s", got, head)
	}
}

// Restart after integration, before cleanup -- the window the `integrated`
// state exists for.
//
// The work is on the target and the worktree and branch are still there, which
// is byte-for-byte what an un-integrated task looks like. The record is the
// only thing that tells them apart, and the boot pass must finish the cleanup
// rather than doing anything that could produce a second integration.
func TestRestartAfterIntegrationBeforeCleanupFinishesTheCleanup(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)
	rec := f.startTask("t1")
	f.agentCommits(rec, "feature.txt", "landed work\n")
	integrated := f.integrate(rec)

	// The crash point: the integration is durably recorded, nothing has been
	// torn down.
	if _, err := f.mgr.MarkIntegrated(context.Background(), "t1", integrated); err != nil {
		t.Fatalf("mark integrated: %v", err)
	}

	report := f.reconcile()

	if got := actionFor(report, "t1").Action; got != ReconcileCleanedUp {
		t.Fatalf("action = %q (%s), want cleaned_up", got, actionFor(report, "t1").Detail)
	}
	if f.dirExists(rec.Path) {
		t.Fatalf("the worktree %s outlived its integration", rec.Path)
	}
	if f.branchExists(rec.Branch) {
		t.Fatalf("the temporary branch %s outlived its integration", rec.Branch)
	}
	final := f.record("t1")
	if final.State != domain.TaskWorktreeReleased || !final.BranchDeleted {
		t.Fatalf("record = %+v, want released with the branch deleted", final)
	}
	if final.IntegratedSHA != integrated {
		t.Fatalf("integrated sha = %q, want %q", final.IntegratedSHA, integrated)
	}
	// The work itself is exactly where the integration left it.
	if got := git(t, f.binary, f.repo, "rev-parse", "main"); got != integrated {
		t.Fatalf("main = %s, want %s", got, integrated)
	}
}

// Restart INSIDE the cleanup, between the two halves.
//
// The record is written released before the branch is deleted, so this is the
// state a crash there leaves: no directory, a branch still full of commits, and
// a row that says so. The next pass has to finish the branch half rather than
// reading "released" as "done".
func TestRestartInsideCleanupFinishesTheBranchDeletion(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)
	rec := f.startTask("t1")
	f.agentCommits(rec, "feature.txt", "landed work\n")
	integrated := f.integrate(rec)
	if _, err := f.mgr.MarkIntegrated(context.Background(), "t1", integrated); err != nil {
		t.Fatalf("mark integrated: %v", err)
	}
	// Half a cleanup: the checkout is gone, the record says released, the
	// branch is untouched.
	rec = f.record("t1")
	rec.State = domain.TaskWorktreeReleased
	rec.BranchDeleted = false
	if err := f.store.UpsertTaskWorktree(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if err := f.mgr.git.RemoveWorktree(context.Background(), f.repo, rec.Path); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	if !f.branchExists(rec.Branch) {
		t.Fatal("precondition: the branch should still be there")
	}

	report := f.reconcile()

	if got := actionFor(report, "t1").Action; got != ReconcileCleanedUp {
		t.Fatalf("action = %q (%s), want cleaned_up", got, actionFor(report, "t1").Detail)
	}
	if f.branchExists(rec.Branch) {
		t.Fatalf("branch %s survived the second half of the cleanup", rec.Branch)
	}
	if got := f.record("t1"); !got.BranchDeleted {
		t.Fatalf("record = %+v, want the branch recorded as deleted", got)
	}
}

// Running the boot pass twice must do the second half of nothing twice.
//
// A restart loop, or simply a daemon that boots more than once, calls this
// repeatedly over the same records. Every rule's postcondition has to be a
// state the same rule leaves alone, which is what makes "reconcile again" free
// rather than dangerous.
func TestReconcileTwiceChangesNothingTheSecondTime(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)

	working := f.startTask("t1")
	f.agentCommits(working, "a.txt", "still working\n")

	landed := f.startTask("t2")
	f.agentCommits(landed, "b.txt", "landed\n")
	integrated := f.integrate(landed)
	if _, err := f.mgr.MarkIntegrated(context.Background(), "t2", integrated); err != nil {
		t.Fatal(err)
	}

	first := f.reconcile()
	if got := actionFor(first, "t2").Action; got != ReconcileCleanedUp {
		t.Fatalf("first pass action for t2 = %q, want cleaned_up", got)
	}
	afterFirst := f.record("t2")
	mainAfterFirst := git(t, f.binary, f.repo, "rev-parse", "main")

	second := f.reconcile()

	// t2 is fully cleaned up, so the second pass has nothing to say about it at
	// all: a released row with its branch deleted is not even listed.
	if entry := actionFor(second, "t2"); entry.Action != "" {
		t.Fatalf("second pass acted on an already-cleaned task: %+v", entry)
	}
	if got := f.record("t2"); got.State != afterFirst.State || got.BranchDeleted != afterFirst.BranchDeleted {
		t.Fatalf("record changed on the second pass: %+v -> %+v", afterFirst, got)
	}
	if got := git(t, f.binary, f.repo, "rev-parse", "main"); got != mainAfterFirst {
		t.Fatalf("the target moved on a second reconcile: %s -> %s", mainAfterFirst, got)
	}
	// And the task that was still working is adopted both times.
	if got := actionFor(second, "t1").Action; got != ReconcileAdopted {
		t.Fatalf("second pass action for t1 = %q, want adopted", got)
	}
	if !f.dirExists(working.Path) {
		t.Fatal("a second reconcile removed the worktree of a task that is still working")
	}
}

// Cleanup is authorized by a recorded integration and by nothing else.
//
// A caller that believes a task is finished when its record does not say so is
// a caller about to delete the only copy of somebody's commits, so this is a
// refusal rather than a no-op -- and nothing at all is touched on the way to it.
func TestCleanupRefusesATaskThatHasNotIntegrated(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)
	rec := f.startTask("t1")
	f.agentCommits(rec, "feature.txt", "work that never landed\n")

	_, err := f.mgr.Cleanup(context.Background(), "t1")
	if !errors.Is(err, ErrNotIntegrated) {
		t.Fatalf("err = %v, want ErrNotIntegrated", err)
	}
	if !f.dirExists(rec.Path) || !f.branchExists(rec.Branch) {
		t.Fatal("a refused cleanup removed something anyway")
	}
}

// The guarantee that outranks tidiness: a branch is deleted only when every
// commit on it is already reachable from where the work landed.
//
// Here the agent committed again after the integration -- a fix, a stray
// commit, anything -- so the branch tip is no longer contained in the recorded
// commit. The checkout is still torn down (it is reproducible), the branch is
// kept, and the reason names what is being protected.
func TestCleanupKeepsABranchHoldingCommitsThatNeverLanded(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)
	rec := f.startTask("t1")
	f.agentCommits(rec, "feature.txt", "landed work\n")
	integrated := f.integrate(rec)
	if _, err := f.mgr.MarkIntegrated(context.Background(), "t1", integrated); err != nil {
		t.Fatal(err)
	}
	// Work that exists only on the ao/* branch.
	stray := f.agentCommits(rec, "unlanded.txt", "never integrated\n")

	result, err := f.mgr.Cleanup(context.Background(), "t1")
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if result.BranchDeleted {
		t.Fatal("a branch holding unintegrated commits was deleted")
	}
	if result.BranchKept == "" {
		t.Fatal("the branch was kept without saying why")
	}
	if !strings.Contains(result.BranchKept, "exists nowhere else") {
		t.Fatalf("BranchKept = %q, want it to name the work it is protecting", result.BranchKept)
	}
	if !f.branchExists(rec.Branch) {
		t.Fatal("the branch is gone despite the refusal")
	}
	if got := git(t, f.binary, f.repo, "rev-parse", "refs/heads/"+rec.Branch); got != stray {
		t.Fatalf("branch = %s, want the stray commit %s", got, stray)
	}
	// The record is honest about it: released, but not claiming the branch is
	// gone -- which is exactly what keeps the next reconcile pass looking.
	final := f.record("t1")
	if final.State != domain.TaskWorktreeReleased || final.BranchDeleted {
		t.Fatalf("record = %+v, want released with the branch NOT recorded as deleted", final)
	}
	if got := actionFor(f.reconcile(), "t1").Action; got != ReconcileKept {
		t.Fatalf("reconcile action = %q, want kept", got)
	}
}

// A worktree with uncommitted changes makes git refuse to remove it, and that
// refusal is passed through rather than forced past.
//
// The task's committed work integrated; whatever is dirty in the directory did
// not, and deleting it to tidy up is not a trade this manager may make. The
// record stays at `integrated` so the obligation to finish survives and the
// next pass retries.
func TestCleanupDoesNotForceRemoveADirtyWorktree(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)
	rec := f.startTask("t1")
	f.agentCommits(rec, "feature.txt", "landed work\n")
	integrated := f.integrate(rec)
	if _, err := f.mgr.MarkIntegrated(context.Background(), "t1", integrated); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec.Path, "feature.txt"), []byte("uncommitted edit\n"), 0o600); err != nil {
		t.Fatalf("dirty the worktree: %v", err)
	}

	if _, err := f.mgr.Cleanup(context.Background(), "t1"); err == nil {
		t.Fatal("a dirty worktree was removed without complaint")
	}
	if !f.dirExists(rec.Path) {
		t.Fatal("the dirty worktree was removed anyway")
	}
	if got := f.record("t1").State; got != domain.TaskWorktreeIntegrated {
		t.Fatalf("state = %q, want it left at integrated so a later pass retries", got)
	}
	if got := actionFor(f.reconcile(), "t1").Action; got != ReconcileBlocked {
		t.Fatalf("reconcile action = %q, want blocked", got)
	}
	// And once the obstruction is gone, the same pass finishes the job.
	git(t, f.binary, rec.Path, "checkout", "--", "feature.txt")
	if got := actionFor(f.reconcile(), "t1").Action; got != ReconcileCleanedUp {
		t.Fatalf("reconcile action after the tree was cleaned = %q, want cleaned_up", got)
	}
	if f.dirExists(rec.Path) || f.branchExists(rec.Branch) {
		t.Fatal("cleanup did not finish once it could")
	}
}

// A cancelled or failed task's leftovers are kept, durably and on purpose, and
// no number of reconcile passes changes that.
func TestPreservedWorkSurvivesEveryReconcilePass(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)
	rec := f.startTask("t1")
	head := f.agentCommits(rec, "feature.txt", "work nobody integrated\n")

	preserved, found, err := f.mgr.Preserve(context.Background(), "t1", "the task was cancelled")
	if err != nil || !found {
		t.Fatalf("preserve: found=%v err=%v", found, err)
	}
	if preserved.State != domain.TaskWorktreePreserved {
		t.Fatalf("state = %q, want preserved", preserved.State)
	}
	if preserved.Detail != "the task was cancelled" {
		t.Fatalf("detail = %q, want the reason it is being kept", preserved.Detail)
	}

	for pass := 0; pass < 3; pass++ {
		report := f.reconcile()
		// A preserved record is not even listed as unfinished, so no pass has
		// anything to decide about it.
		if entry := actionFor(report, "t1"); entry.Action != "" {
			t.Fatalf("pass %d acted on preserved work: %+v", pass, entry)
		}
	}
	if !f.dirExists(rec.Path) || !f.branchExists(rec.Branch) {
		t.Fatal("preserved work was cleaned up")
	}
	if got := git(t, f.binary, rec.Path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("preserved HEAD = %s, want %s", got, head)
	}
	// Cleanup is refused for it too, which is the difference between "skipped"
	// and "preserved".
	if _, err := f.mgr.Cleanup(context.Background(), "t1"); !errors.Is(err, ErrNotIntegrated) {
		t.Fatalf("cleanup of preserved work: err = %v, want ErrNotIntegrated", err)
	}
}

// MarkIntegrated is idempotent at the same commit and an error at a different
// one. Overwriting would let a second, wrong integration authorize deleting the
// first one's evidence.
func TestMarkIntegratedIsIdempotentAndRefusesADifferentCommit(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)
	rec := f.startTask("t1")
	f.agentCommits(rec, "feature.txt", "landed\n")
	integrated := f.integrate(rec)

	if _, err := f.mgr.MarkIntegrated(context.Background(), "t1", integrated); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.MarkIntegrated(context.Background(), "t1", integrated); err != nil {
		t.Fatalf("re-marking the same commit: %v", err)
	}
	if _, err := f.mgr.MarkIntegrated(context.Background(), "t1", "0000000000000000000000000000000000000000"); err == nil {
		t.Fatal("a second, different integrated commit overwrote the first")
	}
	if got := f.record("t1").IntegratedSHA; got != integrated {
		t.Fatalf("integrated sha = %s, want the original %s", got, integrated)
	}

	// And once cleaned up, marking again does not re-open the cleanup.
	if _, err := f.mgr.Cleanup(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.MarkIntegrated(context.Background(), "t1", integrated); err != nil {
		t.Fatal(err)
	}
	if got := f.record("t1").State; got != domain.TaskWorktreeReleased {
		t.Fatalf("state = %q, want it to stay released", got)
	}
}

// A task with no worktree record at all -- a direct_branch task -- is a normal
// answer, not a failure, for the two calls a caller may reach with either.
func TestDirectBranchTasksHaveNothingToCleanUpOrPreserve(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)

	if _, err := f.mgr.MarkIntegrated(context.Background(), "direct", "abc123"); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("MarkIntegrated err = %v, want ErrNoRecord", err)
	}
	if _, found, err := f.mgr.Preserve(context.Background(), "direct", "cancelled"); err != nil || found {
		t.Fatalf("Preserve = found %v, err %v; want not-found and no error", found, err)
	}
	if _, err := f.mgr.Cleanup(context.Background(), "direct"); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("Cleanup err = %v, want ErrNoRecord", err)
	}
}

// A record written before the integrated commit was recorded at all -- the
// shape every pre-cleanup row on an existing install has -- is closed out as
// far as it honestly can be.
//
// The checkout is torn down, because a checkout is reproducible. The branch is
// kept and the reason says why: nothing here can prove what is on it, and "I
// cannot prove it is safe" has to be treated exactly as "it is not safe" or the
// proof is decorative.
func TestALegacyRecordWithNoIntegratedCommitKeepsItsBranch(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)
	rec := f.startTask("t1")
	head := f.agentCommits(rec, "feature.txt", "work of unknown provenance\n")

	// The state a pre-0131 row upgrades into: released-or-integrated with no
	// integrated_sha.
	rec = f.record("t1")
	rec.State = domain.TaskWorktreeIntegrated
	rec.IntegratedSHA = ""
	if err := f.store.UpsertTaskWorktree(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	result, err := f.mgr.Cleanup(context.Background(), "t1")
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if result.BranchDeleted {
		t.Fatal("a branch was deleted without any proof of where its work went")
	}
	if !strings.Contains(result.BranchKept, "never recorded") {
		t.Fatalf("BranchKept = %q, want it to name the missing proof", result.BranchKept)
	}
	if f.dirExists(rec.Path) {
		t.Fatal("the reproducible checkout was kept too")
	}
	if got := git(t, f.binary, f.repo, "rev-parse", "refs/heads/"+rec.Branch); got != head {
		t.Fatalf("branch = %s, want the agent's commit %s", got, head)
	}
}

// The same legacy shape, once its branch is genuinely gone: absence needs no
// proof, so the record is closed and stops being reported as unfinished on
// every boot.
func TestALegacyRecordWhoseBranchIsAlreadyGoneIsClosed(t *testing.T) {
	t.Parallel()
	f := newRestartFixture(t)
	rec := f.startTask("t1")
	f.agentCommits(rec, "feature.txt", "work\n")
	tip := git(t, f.binary, f.repo, "rev-parse", "refs/heads/"+rec.Branch)

	if err := f.mgr.git.RemoveWorktree(context.Background(), f.repo, rec.Path); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	if err := f.mgr.git.DeleteBranch(context.Background(), f.repo, rec.Branch, tip); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	rec = f.record("t1")
	rec.State = domain.TaskWorktreeIntegrated
	rec.IntegratedSHA = ""
	if err := f.store.UpsertTaskWorktree(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	if got := actionFor(f.reconcile(), "t1").Action; got != ReconcileCleanedUp {
		t.Fatalf("action = %q, want cleaned_up", got)
	}
	final := f.record("t1")
	if final.State != domain.TaskWorktreeReleased || !final.BranchDeleted {
		t.Fatalf("record = %+v, want released with the branch recorded as gone", final)
	}
	// And it is no longer unfinished, so no later boot looks at it again.
	if entry := actionFor(f.reconcile(), "t1"); entry.Action != "" {
		t.Fatalf("a closed record was reconciled again: %+v", entry)
	}
}
