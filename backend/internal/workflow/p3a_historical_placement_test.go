package workflow_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p3a_historical_placement_test.go — the P3-A smoke follow-up.
//
// A placement answers two questions AO had conflated: "where may work launch
// right now" and "where did this run's work happen". The first is retired at
// terminal, correctly. The second must survive, because a finished run is
// exactly when somebody opens the repository to look — and asking the live
// index produced "AO has no frozen placement for this run" about a run whose
// repository and branch AO could name precisely.
//
// The tests below hold both halves at once: the read must work after terminal,
// and the WRITE must not.

// recordingPreflight is the read-only repository probe, counting calls so a
// test can assert a read stayed a read.
type recordingPreflight struct {
	calls  int
	byPath map[string]ports.WorkspacePreflight
	err    error
}

func (p *recordingPreflight) PreflightRepository(_ context.Context, repoPath, branch string) (ports.WorkspacePreflight, error) {
	p.calls++
	if p.err != nil {
		return ports.WorkspacePreflight{}, p.err
	}
	out, ok := p.byPath[repoPath]
	if !ok {
		return ports.WorkspacePreflight{}, errors.New("no such repository: " + repoPath)
	}
	out.RepoPath, out.ConfiguredBranch = repoPath, branch
	return out, nil
}

// refusingCommitter fails the test if anything ever asks it to write. Every
// commit refusal below must be decided BEFORE the git write, not by the write
// failing.
type refusingCommitter struct{ t *testing.T }

func (c refusingCommitter) CommitAll(_ context.Context, info ports.WorkspaceInfo, _ string) (string, bool, error) {
	c.t.Fatalf("a commit was attempted against %s on %s; authority should have refused first", info.Path, info.Branch)
	return "", false, nil
}

// allowingCommitter records the write authority permitted, so a test can assert
// the positive case without a real repository.
type allowingCommitter struct {
	calls int
	info  ports.WorkspaceInfo
}

func (c *allowingCommitter) CommitAll(_ context.Context, info ports.WorkspaceInfo, _ string) (string, bool, error) {
	c.calls++
	c.info = info
	return "committed-sha", true, nil
}

type historicalFixture struct {
	store     *crashStore
	locks     *branchlock.Manager
	preflight *recordingPreflight
	coord     *workflowcore.Coordinator
	run       domain.WorkflowRun
	step      domain.WorkflowStep
	repoPath  string
}

// newHistoricalFixture builds a real sqlite-backed run with the real placement
// authority and the real branch-lock manager, plus a probe over paths the test
// controls. Deliberately the production store: "the placement row survives
// retirement" is a property of the schema and the retirement rules, and a
// double would prove nothing about either.
func newHistoricalFixture(t *testing.T, mode domain.ExecutionMode) *historicalFixture {
	t.Helper()
	ctx := context.Background()
	store, _ := newCrashFixture(t, validMasterPlan())
	setProjectExecutionMode(t, store, mode)
	project, _, err := store.GetProject(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	locks := branchlock.New(branchlock.Deps{Store: store, OwnerToken: "daemon-p3a"})
	preflight := &recordingPreflight{byPath: map[string]ports.WorkspacePreflight{}}
	coord := newHistoricalCoordinator(t, store, locks, preflight, refusingCommitter{t: t})
	created, err := coord.CreateTaskRun(ctx, workflowcoreTaskRequest(t, "leave something pending"))
	if err != nil {
		t.Fatal(err)
	}
	f := &historicalFixture{
		store: store, locks: locks, preflight: preflight, coord: coord,
		run: created.Run, step: workStepOf(t, store, created.Run.ID), repoPath: project.Path,
	}
	// Freeze the placement, exactly as a dispatch would.
	if _, ok, perr := coord.EnsureExecutionPlacement(ctx, f.run, f.step); perr != nil || !ok {
		t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, perr)
	}
	return f
}

func newHistoricalCoordinator(t *testing.T, store *crashStore, locks *branchlock.Manager,
	preflight *recordingPreflight, committer workflowcore.WorkspaceCommitter,
) *workflowcore.Coordinator {
	t.Helper()
	return workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store,
		Placements: store, ProviderAttempts: store, TaskWorktreeRecords: store,
		PlacementOverrides: store,
		BranchLocks:        cessionBranchLocks{mgr: locks},
		WorkspacePreflight: preflight,
		WorkspaceCommitter: committer,
		InstanceToken:      "daemon-p3a",
	})
}

// placementOf reads the run's placement record straight from the store.
func (f *historicalFixture) placementOf(t *testing.T) workflowcore.PlacementView {
	t.Helper()
	views, err := f.coord.ListPlacements(context.Background(), f.run.ID)
	if err != nil {
		t.Fatalf("ListPlacements: %v", err)
	}
	for _, v := range views {
		if v.Current {
			return v
		}
	}
	t.Fatal("no current placement recorded for the run")
	return workflowcore.PlacementView{}
}

func (f *historicalFixture) cancel(t *testing.T) {
	t.Helper()
	if _, err := f.coord.CancelRun(context.Background(), f.run.ID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	run, ok, err := f.store.GetWorkflowRun(context.Background(), f.run.ID)
	if err != nil || !ok {
		t.Fatalf("re-read run: ok=%v err=%v", ok, err)
	}
	f.run = run
}

// A terminal run's placement is retired OPERATIONALLY and preserved as history:
// both halves in one assertion, because either alone is a different bug.
func TestTerminalRunRetiresPlacementOperationallyAndKeepsItAsHistory(t *testing.T) {
	f := newHistoricalFixture(t, domain.ExecutionDirectBranch)
	before := f.placementOf(t)
	if before.State.Terminal() {
		t.Fatalf("placement is already terminal before the run ended: %+v", before)
	}
	f.cancel(t)

	after := f.placementOf(t)
	if !after.State.Terminal() {
		t.Fatalf("placement state = %q after a terminal run; it must stop being an execution authority", after.State)
	}
	if after.Type != before.Type || after.RepoPath != before.RepoPath ||
		after.ExecutionBranch != before.ExecutionBranch || after.MergeTarget != before.MergeTarget ||
		after.PlacementGeneration != before.PlacementGeneration {
		t.Fatalf("the historical placement changed identity when the run ended:\n before = %+v\n after  = %+v", before, after)
	}
	if after.Provenance == "" {
		t.Fatal("the historical placement lost its provenance, so nothing can say who chose it")
	}
}

// The blocker itself: the read works after the run is over.
func TestPendingChangesReadsAfterTerminalDirectBranch(t *testing.T) {
	f := newHistoricalFixture(t, domain.ExecutionDirectBranch)
	f.preflight.byPath[f.repoPath] = ports.WorkspacePreflight{
		HeadSHA: "head", Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "src/greeter.js", Status: " M"}},
	}
	f.cancel(t)

	out, err := f.coord.PendingChanges(context.Background(), f.run.ID)
	if err != nil {
		t.Fatalf("PendingChanges: %v", err)
	}
	if !out.Available {
		t.Fatalf("pending changes unavailable after a terminal run: %q", out.Unavailable)
	}
	if !out.Historical {
		t.Fatal("the answer came from a retired placement but was not marked historical")
	}
	if out.RepoPath != f.repoPath || out.Branch == "" {
		t.Fatalf("pending changes named repo=%q branch=%q, want the run's own", out.RepoPath, out.Branch)
	}
	if !out.Dirty || len(out.Changes) != 1 {
		t.Fatalf("pending changes did not report the real repository state: %+v", out)
	}
	if out.Placement != domain.PlacementDirectBranch {
		t.Fatalf("placement = %q, want direct_branch", out.Placement)
	}
}

// A read is a read. It must acquire no branch lock and create nothing.
func TestTerminalPendingChangesReadTakesNoLockAndCreatesNothing(t *testing.T) {
	f := newHistoricalFixture(t, domain.ExecutionDirectBranch)
	f.preflight.byPath[f.repoPath] = ports.WorkspacePreflight{HeadSHA: "head"}
	f.cancel(t)
	ctx := context.Background()

	// Cancelling released the run's locks; reading must not take them back.
	if _, err := f.coord.PendingChanges(ctx, f.run.ID); err != nil {
		t.Fatalf("PendingChanges: %v", err)
	}
	held, err := f.locks.HeldByRun(ctx, f.run.ID)
	if err != nil {
		t.Fatalf("HeldByRun: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("a read-only pending-changes call is holding %d branch lock(s)", len(held))
	}
	// And nothing was resurrected: the placement is still terminal.
	if !f.placementOf(t).State.Terminal() {
		t.Fatal("reading pending changes revived the run's placement as an execution authority")
	}
	// The probe is the ONLY repository access, and it is read-only by contract;
	// the committer would have failed the test if anything had tried to write.
	if f.preflight.calls != 1 {
		t.Fatalf("preflight calls = %d, want exactly 1", f.preflight.calls)
	}
}

// A terminal isolated placement whose worktree is still on disk is inspected
// IN THE WORKTREE — not in the parent checkout, whose dirty state belongs to
// the user and not to the task.
func TestTerminalIsolatedWorktreePresentIsInspectedInTheWorktree(t *testing.T) {
	f := newHistoricalFixture(t, domain.ExecutionIsolatedWorktree)
	worktree := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	f.recordWorktreeCheckpoint(t, worktree)
	f.preflight.byPath[worktree] = ports.WorkspacePreflight{
		HeadSHA: "wt-head", Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "src/greeter.js", Status: " M"}},
	}
	// The parent repository is deliberately NOT registered with the probe: a
	// call against it fails the lookup, so aiming there is a visible failure
	// rather than a silently plausible answer.
	f.cancel(t)

	out, err := f.coord.PendingChanges(context.Background(), f.run.ID)
	if err != nil {
		t.Fatalf("PendingChanges: %v", err)
	}
	if !out.Available {
		t.Fatalf("an isolated worktree still on disk was reported unavailable: %q", out.Unavailable)
	}
	if out.WorktreePath != worktree || out.RepoPath != worktree {
		t.Fatalf("inspection did not happen in the worktree: worktree=%q repo=%q", out.WorktreePath, out.RepoPath)
	}
	if !out.Dirty {
		t.Fatalf("the worktree's real state was not reported: %+v", out)
	}
}

// And when it is gone, that is an ANSWER — not "AO has no placement".
func TestTerminalIsolatedWorktreeRemovedGivesAHumanAnswer(t *testing.T) {
	f := newHistoricalFixture(t, domain.ExecutionIsolatedWorktree)
	worktree := filepath.Join(t.TempDir(), "already-collected")
	f.recordWorktreeCheckpoint(t, worktree)
	f.cancel(t)

	out, err := f.coord.PendingChanges(context.Background(), f.run.ID)
	if err != nil {
		t.Fatalf("PendingChanges: %v", err)
	}
	if out.Available {
		t.Fatal("a removed worktree was reported as inspectable")
	}
	if !strings.Contains(out.Unavailable, "already been removed") {
		t.Fatalf("unavailable reason = %q, want it to say the worktree was removed", out.Unavailable)
	}
	if strings.Contains(out.Unavailable, "placement") {
		t.Fatalf("a collected worktree was reported as a missing placement: %q", out.Unavailable)
	}
	// It still names where the work happened, which is the whole point.
	if out.Branch == "" || out.WorktreePath != worktree {
		t.Fatalf("the answer lost the run's identity: %+v", out)
	}
	if f.preflight.calls != 0 {
		t.Fatal("AO probed a worktree it had already determined was gone")
	}
}

// The other half of the split: a historical placement may be described and must
// never authorise a write.
func TestCommitRefusesAHistoricalPlacement(t *testing.T) {
	f := newHistoricalFixture(t, domain.ExecutionDirectBranch)
	f.preflight.byPath[f.repoPath] = ports.WorkspacePreflight{HeadSHA: "head", Dirty: true}
	f.cancel(t)

	_, err := f.coord.CommitPendingChanges(context.Background(), f.run.ID, "commit it anyway")
	if err == nil {
		t.Fatal("a terminal run was allowed to commit through its retired placement")
	}
	if !errors.Is(err, workflowcore.ErrPendingChangesNoAuthority) {
		t.Fatalf("error = %v, want ErrPendingChangesNoAuthority", err)
	}
	// The read still works, which is the property this whole change exists for.
	out, rerr := f.coord.PendingChanges(context.Background(), f.run.ID)
	if rerr != nil || !out.Available {
		t.Fatalf("the read stopped working alongside the refused write: %+v err=%v", out, rerr)
	}
}

// A LIVE run that holds the branch may commit. This is the flow's real use
// case -- a run stopped on a dirty repository -- and it has to keep working, or
// the tightening above would have removed the feature rather than secured it.
func TestCommitProceedsWhileTheRunStillHoldsItsBranch(t *testing.T) {
	f := newHistoricalFixture(t, domain.ExecutionDirectBranch)
	f.preflight.byPath[f.repoPath] = ports.WorkspacePreflight{HeadSHA: "head", Dirty: true}
	ctx := context.Background()
	if _, err := f.locks.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: "p", RunID: f.run.ID, StepID: f.step.ID,
	}); err != nil {
		t.Fatalf("the run could not acquire its own branch: %v", err)
	}
	committer := &allowingCommitter{}
	coord := newHistoricalCoordinator(t, f.store, f.locks, f.preflight, committer)

	out, err := coord.CommitPendingChanges(ctx, f.run.ID, "save pending work")
	if errors.Is(err, workflowcore.ErrPendingChangesNoAuthority) {
		t.Fatalf("a run holding its own branch was refused authority: %v", err)
	}
	if err != nil {
		t.Fatalf("CommitPendingChanges: %v", err)
	}
	if committer.calls != 1 {
		t.Fatalf("commits attempted = %d, want exactly 1", committer.calls)
	}
	if committer.info.Path != f.repoPath {
		t.Fatalf("the commit was aimed at %q, want the placement's own repository %q", committer.info.Path, f.repoPath)
	}
	if !out.Committed || out.CommitSHA != "committed-sha" {
		t.Fatalf("outcome did not report the commit: %+v", out)
	}
}

// A LIVE run whose branch a newer workflow has taken cannot commit to it, and
// neither can one that holds no branch at all. The lock is the ownership proof:
// it is exclusive, so holding it is exactly the evidence that nobody else has
// taken the branch, and holding none is the absence of that evidence.
func TestCommitRefusesWithoutProofTheBranchIsStillOwned(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *historicalFixture)
	}{
		{
			name: "a newer workflow holds the branch",
			setup: func(t *testing.T, f *historicalFixture) {
				t.Helper()
				if _, err := f.locks.Acquire(context.Background(), branchlock.AcquireRequest{
					ProjectID: "p", RunID: "wf-newer", StepID: "wfs-newer",
				}); err != nil {
					t.Fatalf("newer run could not acquire the branch: %v", err)
				}
			},
		},
		{
			// A run that gave its branch to another OWNER is refused for the
			// same reason a newer workflow holding it is: somebody else's name
			// is on the lock. Spelled out separately because the holder is a
			// session rather than a run, and the refusal must not depend on
			// which kind of owner it is.
			name: "a session holds the branch",
			setup: func(t *testing.T, f *historicalFixture) {
				t.Helper()
				if _, err := f.locks.Acquire(context.Background(), branchlock.AcquireRequest{
					ProjectID: "p", SessionID: "sess-other",
				}); err != nil {
					t.Fatalf("session could not acquire the branch: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHistoricalFixture(t, domain.ExecutionDirectBranch)
			f.preflight.byPath[f.repoPath] = ports.WorkspacePreflight{HeadSHA: "head", Dirty: true}
			ctx := context.Background()
			tc.setup(t, f)

			_, err := f.coord.CommitPendingChanges(ctx, f.run.ID, "commit into a branch I cannot claim")
			if err == nil {
				t.Fatal("a run committed to a branch it could not prove was its own")
			}
			if !errors.Is(err, workflowcore.ErrPendingChangesNoAuthority) {
				t.Fatalf("error = %v, want ErrPendingChangesNoAuthority", err)
			}
			if !strings.Contains(err.Error(), "execution lock") {
				t.Fatalf("refusal did not name the missing ownership proof: %v", err)
			}
			// Still readable: seeing what is pending never required owning the
			// branch, and that separation is the point of the whole change.
			if out, rerr := f.coord.PendingChanges(ctx, f.run.ID); rerr != nil || !out.Available {
				t.Fatalf("the read was collateral damage of the refused write: %+v err=%v", out, rerr)
			}
		})
	}
}

// P3-C: a FREE branch does not refuse the commit, and this is the case the
// whole commit-and-continue flow exists for.
//
// P3-A originally asserted the opposite here, on a premise that was true when
// it was written: a direct-branch placement in an isolated-default project took
// no branch lock at all, so "this run holds no lock" really did mean "AO cannot
// show this branch is its to write". P3-C §28 closed that gap — such a run now
// takes a real lock — and with it the premise. What is left is the state the
// dirty-worktree stop is DEFINED by: the acquisition was refused because of the
// user's own uncommitted work, so nobody holds the branch, and committing that
// work is precisely what lets AO take it next.
//
// Demanding the lock here made the flow refuse its own primary case every time.
// The refusal that matters — somebody ELSE owns this branch — is the two cases
// above, and it is unchanged.
func TestCommitProceedsWhenNobodyHoldsTheBranch(t *testing.T) {
	f := newHistoricalFixture(t, domain.ExecutionDirectBranch)
	f.preflight.byPath[f.repoPath] = ports.WorkspacePreflight{
		HeadSHA: "head", Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "a.txt", Status: " M"}},
	}
	ctx := context.Background()
	// Nobody acquires the branch: that IS the dirty-worktree state. The run
	// asked for it, the preflight refused because of the user's uncommitted
	// work, and the lock table is empty.
	committer := &allowingCommitter{}
	coord := newHistoricalCoordinator(t, f.store, f.locks, f.preflight, committer)

	if _, err := coord.CommitPendingChanges(ctx, f.run.ID, "save the local work"); err != nil {
		t.Fatalf("a free branch refused the commit the dirty-worktree stop exists to offer: %v", err)
	}
	if committer.calls != 1 {
		t.Fatalf("the committer was called %d times, want exactly 1", committer.calls)
	}
	if committer.info.Branch != "main" {
		t.Fatalf("the commit was aimed at %q, want the placement's own branch", committer.info.Branch)
	}
}

// A daemon restart rebuilds the same history from the same rows.
func TestRestartReconstructsTheHistoricalPlacementIdentically(t *testing.T) {
	f := newHistoricalFixture(t, domain.ExecutionDirectBranch)
	f.preflight.byPath[f.repoPath] = ports.WorkspacePreflight{
		HeadSHA: "head", Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "a.txt", Status: " M"}},
	}
	f.cancel(t)
	ctx := context.Background()

	before, err := f.coord.PendingChanges(ctx, f.run.ID)
	if err != nil {
		t.Fatalf("PendingChanges: %v", err)
	}

	// A new coordinator over the same durable state — the daemon coming back.
	restarted := newHistoricalCoordinator(t, f.store, f.locks, f.preflight, refusingCommitter{t: t})
	after, err := restarted.PendingChanges(ctx, f.run.ID)
	if err != nil {
		t.Fatalf("PendingChanges after restart: %v", err)
	}
	if after.Available != before.Available || after.RepoPath != before.RepoPath ||
		after.Branch != before.Branch || after.Placement != before.Placement ||
		after.Historical != before.Historical || after.Dirty != before.Dirty {
		t.Fatalf("a restart changed the reconstructed history:\n before = %+v\n after  = %+v", before, after)
	}
	// And the restart did not make the retired placement live again.
	if _, cerr := restarted.CommitPendingChanges(ctx, f.run.ID, "after restart"); !errors.Is(cerr, workflowcore.ErrPendingChangesNoAuthority) {
		t.Fatalf("commit error after restart = %v, want ErrPendingChangesNoAuthority", cerr)
	}
}

// recordWorktreeCheckpoint writes the worktree path onto the run's ledger the
// way a real work-step dispatch does, which is where the path durably lives for
// a plain task run (a run with no planned-task decomposition never gets a
// task-worktree record for the placement row to adopt).
func (f *historicalFixture) recordWorktreeCheckpoint(t *testing.T, path string) {
	t.Helper()
	stepID := f.step.ID
	if _, err := f.store.CreateWorkflowCheckpoint(context.Background(), domain.WorkflowCheckpoint{
		ID:             "wfc-p3a-history",
		WorkflowRunID:  f.run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      f.run.ProjectID,
		Branch:         f.placementOf(t).ExecutionBranch,
		WorktreePath:   path,
		NextAction:     "worker_dispatched",
		DurablePhase:   "worker_dispatched",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      f.run.CreatedAt,
	}); err != nil {
		t.Fatalf("CreateWorkflowCheckpoint: %v", err)
	}
}
