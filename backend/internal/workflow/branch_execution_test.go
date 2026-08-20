package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeBranchLocks is an in-memory workflowcore.BranchLocks that enforces the
// same one-holder-per-repo+branch rule the real partial unique index does.
type fakeBranchLocks struct {
	mu sync.Mutex
	// held maps lock key -> the lock currently held.
	held map[string]domain.BranchLock
	// targets maps project id -> the repo/branch pairs a run must own. Empty
	// means the project is not in direct-branch mode.
	targets map[domain.ProjectID][]branchTarget
	// dirty marks repo paths that hold a human's uncommitted work.
	dirty    map[string]bool
	releases []string
	acquires int
	renewals []string
	failWith error
}

type branchTarget struct {
	repoPath string
	branch   string
}

func newFakeBranchLocks() *fakeBranchLocks {
	return &fakeBranchLocks{held: map[string]domain.BranchLock{}, targets: map[domain.ProjectID][]branchTarget{}, dirty: map[string]bool{}}
}

func (f *fakeBranchLocks) Acquire(_ context.Context, req workflowcore.BranchLockRequest) ([]domain.BranchLock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquires++
	if f.failWith != nil {
		return nil, f.failWith
	}
	targets := f.targets[req.ProjectID]
	if len(targets) == 0 {
		return nil, nil
	}
	var blocked []ports.WorkspacePreflight
	for _, target := range targets {
		if f.dirty[target.repoPath] {
			blocked = append(blocked, ports.WorkspacePreflight{RepoPath: target.repoPath, ConfiguredBranch: target.branch, Dirty: true})
		}
	}
	if len(blocked) > 0 {
		return nil, fakeDirtyError{repos: blocked}
	}
	var out []domain.BranchLock
	for _, target := range targets {
		key := domain.BranchLockKey(target.repoPath, target.branch)
		if existing, ok := f.held[key]; ok {
			if existing.WorkflowRunID == req.RunID {
				out = append(out, existing)
				continue
			}
			return nil, domain.BranchLockConflictError{Holder: existing}
		}
		lock := domain.BranchLock{
			ID: "blk-" + key, LockKey: key, ProjectID: req.ProjectID,
			RepoPath: target.repoPath, RepoName: domain.RootWorkspaceRepoName, Branch: target.branch,
			WorkflowRunID: req.RunID, WorkflowStepID: req.StepID, State: domain.BranchLockHeld,
		}
		f.held[key] = lock
		out = append(out, lock)
	}
	return out, nil
}

func (f *fakeBranchLocks) ReleaseRun(_ context.Context, runID, reason string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for key, lock := range f.held {
		if lock.WorkflowRunID == runID {
			delete(f.held, key)
			n++
		}
	}
	f.releases = append(f.releases, runID+": "+reason)
	return n, nil
}

func (f *fakeBranchLocks) HeldByRun(_ context.Context, runID string) ([]domain.BranchLock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.BranchLock
	for _, lock := range f.held {
		if lock.WorkflowRunID == runID {
			out = append(out, lock)
		}
	}
	return out, nil
}

func (f *fakeBranchLocks) Renew(_ context.Context, runID, stepID, sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewals = append(f.renewals, runID+"/"+stepID+"/"+sessionID)
}

// fakeDirtyError mirrors branchlock.DirtyRepositoryError's sentinel wrapping
// without importing that package into workflow's tests.
type fakeDirtyError struct{ repos []ports.WorkspacePreflight }

func (e fakeDirtyError) Error() string {
	return fmt.Sprintf("repository has uncommitted changes: %s", e.repos[0].RepoPath)
}
func (e fakeDirtyError) Unwrap() error { return ports.ErrWorkspaceRepositoryDirty }

// fakeCommitter records the commits an autonomous run makes.
type fakeCommitter struct {
	mu       sync.Mutex
	commits  []string
	messages []string
	clean    bool
	err      error
}

func (f *fakeCommitter) CommitAll(_ context.Context, info ports.WorkspaceInfo, message string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", false, f.err
	}
	if f.clean {
		return "", false, nil
	}
	f.commits = append(f.commits, info.Path+"@"+info.Branch)
	f.messages = append(f.messages, message)
	return "sha-" + fmt.Sprint(len(f.commits)), true, nil
}

// fakeProjects is a workflowcore.Projects fake.
type fakeProjects map[string]domain.ProjectRecord

func (p fakeProjects) GetProject(_ context.Context, id string) (domain.ProjectRecord, bool, error) {
	rec, ok := p[id]
	return rec, ok, nil
}

func newDirectBranchCoordinator(t *testing.T, spawner workflowcore.Spawner, locks *fakeBranchLocks, committer workflowcore.WorkspaceCommitter, projects fakeProjects) (*workflowcore.Coordinator, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	// A real Spawn's session row is immediately visible through the same store
	// SessionFacts reads from; wire the fake the same way so observation after
	// dispatch sees a live session instead of a fake-decoupling artifact.
	facts := newFakeSessionFacts()
	if fs, ok := spawner.(*fakeSpawner); ok {
		fs.facts = facts
	}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:              store,
		Projects:           projects,
		Spawner:            spawner,
		SessionFacts:       facts,
		WorkspaceFacts:     &fakeWorkspaceFacts{},
		BranchLocks:        locks,
		WorkspaceCommitter: committer,
		Clock:              clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store
}

func directProject(id, path, branch string) domain.ProjectRecord {
	return domain.ProjectRecord{
		ID: id, Path: path,
		Config: domain.ProjectConfig{DefaultBranch: branch, ExecutionMode: domain.ExecutionDirectBranch},
	}
}

func TestDirectBranchRunAcquiresTheBranchBeforeSpawning(t *testing.T) {
	locks := newFakeBranchLocks()
	locks.targets["proj"] = []branchTarget{{repoPath: "/repos/ao", branch: "feat/engineering-control-center"}}
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "feat/engineering-control-center", WorkspacePath: "/repos/ao"}}}
	c, _ := newDirectBranchCoordinator(t, spawner, locks, &fakeCommitter{}, fakeProjects{"proj": directProject("proj", "/repos/ao", "feat/engineering-control-center")})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj", "ship it")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want 1", spawner.calls)
	}
	held, _ := locks.HeldByRun(ctx, created.Run.ID)
	if len(held) != 1 || held[0].Branch != "feat/engineering-control-center" {
		t.Fatalf("held = %#v, want the configured branch locked by this run", held)
	}
	if len(locks.renewals) == 0 {
		t.Fatal("dispatch never recorded the occupying session on the lock")
	}
}

func TestSecondRunWaitsForBranchAndNamesTheOwner(t *testing.T) {
	locks := newFakeBranchLocks()
	locks.targets["proj"] = []branchTarget{{repoPath: "/repos/ao", branch: "feat/engineering-control-center"}}
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "feat/engineering-control-center", WorkspacePath: "/repos/ao"}}}
	projects := fakeProjects{"proj": directProject("proj", "/repos/ao", "feat/engineering-control-center")}
	c, store := newDirectBranchCoordinator(t, spawner, locks, &fakeCommitter{}, projects)
	ctx := context.Background()

	first, _ := c.CreateRun(ctx, "proj", "first")
	if _, err := c.StartRun(ctx, first.Run.ID); err != nil {
		t.Fatalf("start first: %v", err)
	}
	second, _ := c.CreateRun(ctx, "proj", "second")
	detail, err := c.StartRun(ctx, second.Run.ID)
	if err != nil {
		t.Fatalf("start second: %v", err)
	}

	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want only the first run to have spawned", spawner.calls)
	}
	if detail.Run.State != domain.WorkflowRunWaiting {
		t.Fatalf("second run state = %q, want waiting (never inactive, never failed)", detail.Run.State)
	}
	cp := latestCheckpointWithPhase(t, store, second.Run.ID, "waiting_for_branch")
	if !strings.Contains(cp.NextAction, "waiting_for_branch") {
		t.Fatalf("next action = %q", cp.NextAction)
	}
	if !strings.Contains(cp.NextAction, first.Run.ID) {
		t.Fatalf("next action %q does not name the owning workflow %q", cp.NextAction, first.Run.ID)
	}
	if cp.Branch != "feat/engineering-control-center" {
		t.Fatalf("checkpoint branch = %q, want the contended branch", cp.Branch)
	}
}

func TestWaitingRunDispatchesOnceTheBranchIsReleased(t *testing.T) {
	locks := newFakeBranchLocks()
	locks.targets["proj"] = []branchTarget{{repoPath: "/repos/ao", branch: "main"}}
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "main", WorkspacePath: "/repos/ao"}}}
	projects := fakeProjects{"proj": directProject("proj", "/repos/ao", "main")}
	c, _ := newDirectBranchCoordinator(t, spawner, locks, &fakeCommitter{}, projects)
	ctx := context.Background()

	first, _ := c.CreateRun(ctx, "proj", "first")
	if _, err := c.StartRun(ctx, first.Run.ID); err != nil {
		t.Fatalf("start first: %v", err)
	}
	second, _ := c.CreateRun(ctx, "proj", "second")
	if _, err := c.StartRun(ctx, second.Run.ID); err != nil {
		t.Fatalf("start second: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d before release, want 1", spawner.calls)
	}

	// The owner finishes and gives the branch back.
	if _, err := c.CancelRun(ctx, first.Run.ID); err != nil {
		t.Fatalf("cancel first: %v", err)
	}
	detail, err := c.ContinueRun(ctx, second.Run.ID)
	if err != nil {
		t.Fatalf("continue second: %v", err)
	}
	if spawner.calls != 2 {
		t.Fatalf("spawner calls = %d after release, want the waiting run to have dispatched", spawner.calls)
	}
	if detail.Run.State != domain.WorkflowRunRunning {
		t.Fatalf("resumed run state = %q, want running", detail.Run.State)
	}
	held, _ := locks.HeldByRun(ctx, second.Run.ID)
	if len(held) != 1 {
		t.Fatalf("resumed run holds %d locks, want 1", len(held))
	}
}

func TestCancellationReleasesTheBranchLock(t *testing.T) {
	locks := newFakeBranchLocks()
	locks.targets["proj"] = []branchTarget{{repoPath: "/repos/ao", branch: "main"}}
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "main", WorkspacePath: "/repos/ao"}}}
	projects := fakeProjects{"proj": directProject("proj", "/repos/ao", "main")}
	c, _ := newDirectBranchCoordinator(t, spawner, locks, &fakeCommitter{}, projects)
	ctx := context.Background()

	run, _ := c.CreateRun(ctx, "proj", "obj")
	if _, err := c.StartRun(ctx, run.Run.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := c.CancelRun(ctx, run.Run.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	held, _ := locks.HeldByRun(ctx, run.Run.ID)
	if len(held) != 0 {
		t.Fatalf("cancelled run still holds %d lock(s)", len(held))
	}
	if len(locks.releases) == 0 || !strings.Contains(locks.releases[0], "cancelled") {
		t.Fatalf("releases = %#v, want a cancellation release", locks.releases)
	}
}

func TestDirtyRepositoryBlocksTheRunWithNeedsAttention(t *testing.T) {
	locks := newFakeBranchLocks()
	locks.targets["proj"] = []branchTarget{{repoPath: "/repos/ao", branch: "main"}}
	locks.dirty["/repos/ao"] = true
	spawner := &fakeSpawner{}
	projects := fakeProjects{"proj": directProject("proj", "/repos/ao", "main")}
	c, store := newDirectBranchCoordinator(t, spawner, locks, &fakeCommitter{}, projects)
	ctx := context.Background()

	run, _ := c.CreateRun(ctx, "proj", "obj")
	detail, err := c.StartRun(ctx, run.Run.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if spawner.calls != 0 {
		t.Fatalf("spawner calls = %d, want no session spawned over a human's work", spawner.calls)
	}
	if detail.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", detail.Run.State)
	}
	cp := latestCheckpointWithPhase(t, store, run.Run.ID, "dirty_worktree")
	if !strings.Contains(cp.NextAction, "dirty_worktree") || !strings.Contains(cp.NextAction, "/repos/ao") {
		t.Fatalf("next action = %q, want a dirty_worktree explanation naming the repository", cp.NextAction)
	}
	if !strings.Contains(cp.NextAction, "Commit, stash, or discard") {
		t.Fatalf("next action = %q, want actionable guidance", cp.NextAction)
	}
}

// An isolated-worktree project must behave exactly as it did before this
// checkpoint: no lock is taken, and nothing serializes.
func TestIsolatedWorktreeProjectTakesNoBranchLock(t *testing.T) {
	locks := newFakeBranchLocks() // no targets registered => not direct-branch
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}}
	projects := fakeProjects{"proj": {ID: "proj", Path: "/repos/ao", Config: domain.ProjectConfig{DefaultBranch: "main"}}}
	c, _ := newDirectBranchCoordinator(t, spawner, locks, &fakeCommitter{}, projects)
	ctx := context.Background()

	first, _ := c.CreateRun(ctx, "proj", "first")
	if _, err := c.StartRun(ctx, first.Run.ID); err != nil {
		t.Fatalf("start first: %v", err)
	}
	second, _ := c.CreateRun(ctx, "proj", "second")
	if _, err := c.StartRun(ctx, second.Run.ID); err != nil {
		t.Fatalf("start second: %v", err)
	}
	if spawner.calls != 2 {
		t.Fatalf("spawner calls = %d, want both worktree-mode runs to dispatch independently", spawner.calls)
	}
	if len(locks.held) != 0 {
		t.Fatalf("worktree-mode runs took %d branch lock(s), want none", len(locks.held))
	}
}

func TestAcquireFailureIsNotSilentlyTreatedAsAWait(t *testing.T) {
	locks := newFakeBranchLocks()
	locks.targets["proj"] = []branchTarget{{repoPath: "/repos/ao", branch: "main"}}
	locks.failWith = errors.New("database is gone")
	spawner := &fakeSpawner{}
	projects := fakeProjects{"proj": directProject("proj", "/repos/ao", "main")}
	c, _ := newDirectBranchCoordinator(t, spawner, locks, &fakeCommitter{}, projects)
	ctx := context.Background()

	run, _ := c.CreateRun(ctx, "proj", "obj")
	if _, err := c.StartRun(ctx, run.Run.ID); err == nil {
		t.Fatal("StartRun succeeded despite a genuine lock-store failure")
	}
	if spawner.calls != 0 {
		t.Fatalf("spawner calls = %d, want none", spawner.calls)
	}
}

func latestCheckpointWithPhase(t *testing.T, store *fakeStore, runID, phase string) domain.WorkflowCheckpoint {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	for i := len(cps) - 1; i >= 0; i-- {
		if cps[i].DurablePhase == phase {
			return cps[i]
		}
	}
	t.Fatalf("no %q checkpoint for run %s; got %d checkpoints", phase, runID, len(cps))
	return domain.WorkflowCheckpoint{}
}

// ---- autonomous local commit ----

// directBranchVerifyFixture builds a run parked at the verify step of a
// direct-branch project, so a single GetRun drives verify -> commit ->
// completed. It mirrors verifyFixture's shape, adding the branch-lock,
// committer, and project dependencies this checkpoint introduces.
func directBranchVerifyFixture(t *testing.T, policy domain.GitPolicy, locks *fakeBranchLocks, committer workflowcore.WorkspaceCommitter) (*workflowcore.Coordinator, *fakeStore, string) {
	t.Helper()
	dir := t.TempDir()
	obs := ports.WorkspaceObservation{Path: dir, Branch: "feat/x", HeadSHA: "abc123"}
	plan := workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{{Command: "go", Args: []string{"test", "./..."}, RequiredExitCode: 0, RetrySafe: true}}}
	artifact := workflowcore.BuildPlanArtifact("proj", "ship the control center", "v1", plan)
	raw, err := workflowcore.MarshalPlanArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	runID := "wf-direct-verify"
	sid := "sess-1"
	reviewID := "review-1"
	store.runs[runID] = domain.WorkflowRun{ID: runID, ProjectID: "proj", Objective: "ship the control center", State: domain.WorkflowRunWaiting, PolicyVersion: "v1", PolicySnapshot: `{"maxFixCycles":3}`, CreatedAt: now, UpdatedAt: now}
	store.steps[runID] = []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: raw},
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &sid},
		{ID: "review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 3, State: domain.WorkflowStepCompleted, ReviewRunID: &reviewID},
		{ID: "fix", WorkflowRunID: runID, Kind: domain.WorkflowStepFix, Ordinal: 4, State: domain.WorkflowStepWaiting},
		{ID: "verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 5, State: domain.WorkflowStepPending},
		{ID: "advance", WorkflowRunID: runID, Kind: domain.WorkflowStepAdvance, Ordinal: 6, State: domain.WorkflowStepPending},
	}
	workStepID := "work"
	store.checkpoints[runID] = []domain.WorkflowCheckpoint{{ID: "work-cp", WorkflowRunID: runID, WorkflowStepID: &workStepID, ProjectID: "proj", SessionID: &sid, Branch: "feat/x", WorktreePath: dir, FingerprintAfter: workflowcore.WorkspaceFingerprint(obs), CreatedAt: now}}
	reviews := newFakeReviewRuns()
	reviews.runs[reviewID] = domain.ReviewRun{ID: reviewID, SessionID: domain.SessionID(sid), Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, TargetSHA: workflowcore.WorkspaceFingerprint(obs)}

	project := directProject("proj", dir, "feat/x")
	project.Config.Git = policy
	clock := &fakeClock{t: now}
	ids := 0
	c := workflowcore.New(workflowcore.Deps{
		Store:              store,
		Projects:           fakeProjects{"proj": project},
		ReviewRuns:         reviews,
		WorkspaceFacts:     &sequenceWorkspaceFacts{observations: []ports.WorkspaceObservation{obs}},
		Verifier:           &fakeVerifyRunner{result: workflowcore.VerifyCommandExecution{ExitCode: 0, DurationMS: 5}},
		BranchLocks:        locks,
		WorkspaceCommitter: committer,
		Clock:              clock.Now,
		NewID:              func() string { ids++; return fmt.Sprintf("v%d", ids) },
	})
	locks.targets["proj"] = []branchTarget{{repoPath: dir, branch: "feat/x"}}
	if _, err := locks.Acquire(context.Background(), workflowcore.BranchLockRequest{ProjectID: "proj", RunID: runID}); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	return c, store, runID
}

// The headline autonomy rule: an automatic local-commit policy means the run
// commits and completes on its own, with no human decision in the middle.
func TestAutomaticPolicyCommitsAndCompletesWithoutAsking(t *testing.T) {
	locks := newFakeBranchLocks()
	committer := &fakeCommitter{}
	c, _, runID := directBranchVerifyFixture(t, domain.GitPolicy{LocalCommit: domain.GitActionAutomatic, Push: domain.GitActionNever}, locks, committer)

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", detail.Run.State)
	}
	if len(committer.commits) != 1 {
		t.Fatalf("commits = %#v, want exactly one autonomous local commit", committer.commits)
	}
	if !strings.Contains(committer.messages[0], "ship the control center") {
		t.Fatalf("commit message = %q, want the objective as the subject", committer.messages[0])
	}
	if !strings.Contains(committer.messages[0], runID) {
		t.Fatalf("commit message = %q, want the workflow attributed", committer.messages[0])
	}
	// Completing gives the branch back so the next run can start.
	if held, _ := locks.HeldByRun(context.Background(), runID); len(held) != 0 {
		t.Fatalf("completed run still holds %d lock(s)", len(held))
	}
}

func TestNeverPolicyLeavesWorkUncommittedAndSaysSo(t *testing.T) {
	locks := newFakeBranchLocks()
	committer := &fakeCommitter{}
	c, store, runID := directBranchVerifyFixture(t, domain.GitPolicy{LocalCommit: domain.GitActionNever}, locks, committer)

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", detail.Run.State)
	}
	if len(committer.commits) != 0 {
		t.Fatalf("commits = %#v, want none under a 'never' policy", committer.commits)
	}
	cp := latestCheckpointWithPhase(t, store, runID, "autonomous_local_commit_deferred")
	if !strings.Contains(cp.NextAction, "commit_skipped") {
		t.Fatalf("next action = %q, want the skipped commit recorded", cp.NextAction)
	}
}

func TestRequireApprovalPolicyDefersTheCommit(t *testing.T) {
	locks := newFakeBranchLocks()
	committer := &fakeCommitter{}
	c, store, runID := directBranchVerifyFixture(t, domain.GitPolicy{LocalCommit: domain.GitActionRequireApproval}, locks, committer)

	if _, err := c.GetRun(context.Background(), runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(committer.commits) != 0 {
		t.Fatalf("commits = %#v, want none pending approval", committer.commits)
	}
	cp := latestCheckpointWithPhase(t, store, runID, "autonomous_local_commit_deferred")
	if !strings.Contains(cp.NextAction, "awaiting_commit_approval") {
		t.Fatalf("next action = %q", cp.NextAction)
	}
}

// A commit that genuinely fails must not be reported as a completed run, and
// must not hand the branch to somebody else while the work sits uncommitted.
func TestCommitFailureParksTheRunAndKeepsTheBranch(t *testing.T) {
	locks := newFakeBranchLocks()
	committer := &fakeCommitter{err: errors.New("index.lock exists")}
	c, _, runID := directBranchVerifyFixture(t, domain.GitPolicy{LocalCommit: domain.GitActionAutomatic}, locks, committer)

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if detail.Run.State == domain.WorkflowRunCompleted {
		t.Fatal("run reported completed while its work is uncommitted")
	}
	if detail.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", detail.Run.State)
	}
	if held, _ := locks.HeldByRun(context.Background(), runID); len(held) != 1 {
		t.Fatalf("held = %d, want the branch still locked to this run", len(held))
	}
}
