package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1d_frozen_placement_test.go — P1-D §A/§B and test matrix R1–R13.
//
// The property under test is not "a placement row exists". It is that ONE
// placement is decided before anything is mutated, and that from then on the
// row — not the project's current configuration — is the authority. Every
// failure mode here ends the same way: a run that created a worktree being
// recovered as though it had been on a direct branch, or the reverse, which on
// a real repository means either an orphaned checkout holding the only copy of
// the work or an unlocked write into somebody's own tree.

// placementFixture is a real sqlite store, a real project, a real coordinator
// with the placement authority wired, and one task run with a work step. It is
// deliberately the production store rather than a double: the freeze's
// idempotency and its "at most one live placement" rule are enforced by
// indexes, and a double would prove nothing about either.
type placementFixture struct {
	store *crashStore
	coord *workflowcore.Coordinator
	locks *branchlock.Manager
	run   domain.WorkflowRun
	step  domain.WorkflowStep
}

func newPlacementFixture(t *testing.T) *placementFixture {
	t.Helper()
	return newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
}

func newPlacementFixtureWithMode(t *testing.T, mode domain.ExecutionMode) *placementFixture {
	t.Helper()
	ctx := context.Background()
	store, _ := newCrashFixture(t, validMasterPlan())
	setProjectExecutionMode(t, store, mode)
	locks := branchlock.New(branchlock.Deps{Store: store, OwnerToken: "daemon-p1d"})
	coord := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store,
		Placements: store, ProviderAttempts: store, TaskWorktreeRecords: store,
		// P1-E's override/transition authority rides on the same store, exactly
		// as the daemon wires it: a placement and the operator decision that
		// replaced it have to be readable in one consistent view after a crash.
		PlacementOverrides: store,
		BranchLocks:        cessionBranchLocks{mgr: locks},
		InstanceToken:      "daemon-p1d",
	})
	created, err := coord.CreateTaskRun(ctx, workflowcoreTaskRequest(t, "place this work"))
	if err != nil {
		t.Fatal(err)
	}
	return &placementFixture{
		store: store, coord: coord, locks: locks,
		run:  created.Run,
		step: workStepOf(t, store, created.Run.ID),
	}
}

// setProjectExecutionMode rewrites the project's configured execution mode.
// It goes through the ordinary project store, so what the coordinator reads is
// what a person changing the setting in the app would have produced.
func setProjectExecutionMode(t *testing.T, store *crashStore, mode domain.ExecutionMode) {
	t.Helper()
	ctx := context.Background()
	project, ok, err := store.GetProject(ctx, "p")
	if err != nil || !ok {
		t.Fatalf("GetProject: ok=%v err=%v", ok, err)
	}
	project.Config.ExecutionMode = mode
	project.Config.DefaultBranch = "main"
	if err := store.UpsertProject(ctx, project); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
}

func workStepOf(t *testing.T, store *crashStore, runID string) domain.WorkflowStep {
	t.Helper()
	steps, err := store.ListWorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepWork {
			return s
		}
	}
	t.Fatalf("run %s has no work step", runID)
	return domain.WorkflowStep{}
}

func (f *placementFixture) live(t *testing.T) domain.ExecutionPlacement {
	t.Helper()
	p, found, err := f.store.GetLiveExecutionPlacement(context.Background(), f.run.ID, "", "")
	if err != nil || !found {
		t.Fatalf("no live placement: found=%v err=%v", found, err)
	}
	return p
}

// R1: the placement is selected once and frozen, and asking again returns the
// SAME record rather than making a second decision.
func TestPlacementIsFrozenOnceAndReadBackUnchanged(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	first, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("first freeze: ok=%v err=%v", ok, err)
	}
	if first.Type != domain.PlacementIsolatedWorktree {
		t.Fatalf("placement type = %s, want isolated_worktree for an isolated project", first.Type)
	}
	if first.PlacementGeneration != 1 {
		t.Fatalf("first placement generation = %d, want 1", first.PlacementGeneration)
	}
	if first.Provenance != domain.PlacementFrozenAtSelection {
		t.Fatalf("provenance = %s, want frozen_at_selection", first.Provenance)
	}

	second, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("second freeze: ok=%v err=%v", ok, err)
	}
	if second.ID != first.ID || second.PlacementGeneration != first.PlacementGeneration {
		t.Fatalf("re-asking minted a second placement: %s gen %d then %s gen %d",
			first.ID, first.PlacementGeneration, second.ID, second.PlacementGeneration)
	}
	// The index, not the code path, is what makes this true. Prove it holds
	// over the whole table rather than only over this call.
	all, err := f.store.ListExecutionPlacementsForRun(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("the run has %d placements; a freeze must produce exactly one", len(all))
	}
}

// R2: the whole point. Changing the project's execution mode AFTER the freeze
// does not change where a running task's work happens.
func TestProjectConfigChangeAfterFreezeDoesNotMovePlacement(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	frozen, _, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Type != domain.PlacementIsolatedWorktree {
		t.Fatalf("setup: placement type = %s", frozen.Type)
	}

	// A person switches the project to direct_branch while this run is going.
	setProjectExecutionMode(t, f.store, domain.ExecutionDirectBranch)

	after, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("read back after config change: ok=%v err=%v", ok, err)
	}
	if after.Type != domain.PlacementIsolatedWorktree {
		t.Fatalf("the placement followed the project's new mode (%s); a frozen placement must not be re-derived", after.Type)
	}
	if after.ExecutionBranch != frozen.ExecutionBranch || after.PlacementGeneration != frozen.PlacementGeneration {
		t.Fatalf("the frozen placement changed under a config edit: %+v -> %+v", frozen, after)
	}
}

// R3: a restart reads the record rather than recomputing policy. The new
// coordinator is built against the SAME store and a project whose mode has
// since changed — the shape a daemon actually comes back to.
func TestRestartPreservesPlacementRatherThanRecomputingIt(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	frozen, _, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil {
		t.Fatal(err)
	}
	setProjectExecutionMode(t, f.store, domain.ExecutionDirectBranch)

	rebooted := workflowcore.New(workflowcore.Deps{
		Store: f.store, Projects: f.store,
		Placements: f.store, ProviderAttempts: f.store, TaskWorktreeRecords: f.store,
		InstanceToken: "daemon-p1d-second-boot",
	})
	after, ok, err := rebooted.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("after restart: ok=%v err=%v", ok, err)
	}
	if after.Type != frozen.Type || after.ID != frozen.ID {
		t.Fatalf("a restart recomputed the placement: %s(%s) -> %s(%s)",
			frozen.Type, frozen.ID, after.Type, after.ID)
	}
	if after.OwnerToken != "ao-placement:daemon-p1d" {
		t.Fatalf("owner token = %q; the record must keep naming the incarnation that froze it", after.OwnerToken)
	}
}

// R4/R5/R6/R7/R8/R9: a stale placement generation has no authority of any kind.
//
// The six matrix rows are one predicate — requireCurrentPlacement, which every
// authority-bearing operation calls — so they are asserted against that
// predicate directly and against the two paths that carry it, rather than by
// six near-identical stagings of the same refusal.
func TestStalePlacementGenerationHasNoAuthority(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	first, _, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := f.coord.ReplaceExecutionPlacement(ctx, f.run, f.step, "the checkout had to be recreated")
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if replacement.PlacementGeneration != first.PlacementGeneration+1 {
		t.Fatalf("replacement generation = %d, want %d", replacement.PlacementGeneration, first.PlacementGeneration+1)
	}

	// R4: the stale generation is no longer current.
	if f.coord.PlacementIsCurrentForTest(ctx, f.run, first.PlacementGeneration) {
		t.Fatal("the superseded placement generation still reports as current")
	}
	if !f.coord.PlacementIsCurrentForTest(ctx, f.run, replacement.PlacementGeneration) {
		t.Fatal("the replacement generation does not report as current")
	}

	// R5/R6/R7/R8: every authority-bearing operation is gated on exactly this,
	// and it refuses.
	if _, err := f.coord.RequireCurrentPlacementForTest(ctx, f.run, first.PlacementGeneration); err == nil {
		t.Fatal("a stale placement generation was granted authority")
	}
	if _, err := f.coord.RequireCurrentPlacementForTest(ctx, f.run, replacement.PlacementGeneration); err != nil {
		t.Fatalf("the current placement generation was refused authority: %v", err)
	}

	// R9: the stale generation cannot GC the replacement. Retiring is
	// generation-conditioned, so a sweep run on the old holder's behalf
	// touches nothing.
	retired, err := f.store.RetireSupersededExecutionPlacements(ctx, f.run.ID, "", "",
		first.PlacementGeneration, "a stale holder tried to tidy up", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if retired != 0 {
		t.Fatalf("a stale placement generation retired %d newer placements; it must retire none", retired)
	}
	live := f.live(t)
	if live.PlacementGeneration != replacement.PlacementGeneration {
		t.Fatalf("the live placement is generation %d after a stale sweep, want %d",
			live.PlacementGeneration, replacement.PlacementGeneration)
	}
}

// R10: a legacy run — one that already executed before placements were durable
// — has its placement RECOVERED from the worktree record that proves the mode,
// never re-selected from configuration.
func TestLegacyPlacementIsRecoveredOnlyFromDurableProof(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	taskID := f.asLegacyChildRun(t)
	// The run has already executed: an attempt exists. Nothing froze a
	// placement, because this build did not have them.
	seedExecutedAttempt(t, f.store, f.step.ID)
	// And it left the durable proof of what it did: an AO worktree.
	seedTaskWorktree(t, f.store, "wf-master-legacy", taskID, "/tmp/ao/wt", "ao/legacy", "abc1234")

	// The project has since been switched to direct_branch. A re-selection
	// would produce a direct-branch placement; a recovery must not.
	setProjectExecutionMode(t, f.store, domain.ExecutionDirectBranch)

	placement, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("legacy recovery: ok=%v err=%v", ok, err)
	}
	if placement.Type != domain.PlacementIsolatedWorktree {
		t.Fatalf("legacy placement type = %s; the worktree record proves isolated_worktree", placement.Type)
	}
	if placement.Provenance != domain.PlacementRecoveredFromDurableFacts {
		t.Fatalf("provenance = %s, want recovered_from_durable_facts", placement.Provenance)
	}
	// R12: the worktree identity is copied from the record, not invented.
	if placement.WorktreePath != "/tmp/ao/wt" || placement.ExecutionBranch != "ao/legacy" || placement.BaseSHA != "abc1234" {
		t.Fatalf("the recovered placement did not copy the worktree record: %+v", placement)
	}
}

// R11: a legacy run with NO durable proof fails closed. AO does not guess.
func TestAmbiguousLegacyPlacementFailsClosed(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	seedExecutedAttempt(t, f.store, f.step.ID)
	// No worktree record and no held branch lock: nothing proves the mode.

	_, _, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err == nil {
		t.Fatal("an ambiguous legacy run was given a placement; it must fail closed instead")
	}
	all, lerr := f.store.ListExecutionPlacementsForRun(ctx, f.run.ID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(all) != 0 {
		t.Fatalf("a guessed placement was written for an ambiguous legacy run: %+v", all)
	}
}

// R11 (second shape): a legacy run with BOTH proofs is also ambiguous. That
// state means the project's mode changed under a live run — precisely the
// situation the freeze exists to prevent — and nobody should resolve it by
// picking one.
func TestLegacyPlacementWithBothProofsFailsClosed(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionDirectBranch)
	ctx := context.Background()

	taskID := f.asLegacyChildRun(t)
	seedExecutedAttempt(t, f.store, f.step.ID)
	seedTaskWorktree(t, f.store, "wf-master-legacy", taskID, "/tmp/ao/wt", "ao/legacy", "abc1234")
	seedHeldBranchLock(t, f.store, f.run.ID)

	if _, _, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step); err == nil {
		t.Fatal("a legacy run with contradictory proofs was given a placement")
	}
}

// R13: a direct-branch placement persists the branch and base authority, and
// deliberately names NO worktree — recording one would fabricate an identity
// AO never created.
func TestDirectBranchPlacementPersistsBranchAuthorityAndNoWorktree(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionDirectBranch)
	ctx := context.Background()

	placement, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("freeze: ok=%v err=%v", ok, err)
	}
	if placement.Type != domain.PlacementDirectBranch {
		t.Fatalf("placement type = %s, want direct_branch", placement.Type)
	}
	if placement.WorktreePath != "" {
		t.Fatalf("a direct-branch placement named a worktree (%q); AO created none", placement.WorktreePath)
	}
	if placement.ExecutionBranch != "main" || placement.MergeTarget != "main" {
		t.Fatalf("direct-branch placement branch/target = %q/%q, want main/main",
			placement.ExecutionBranch, placement.MergeTarget)
	}
	if placement.RepoPath == "" {
		t.Fatal("the placement records no repository identity")
	}
}

// The storage layer refuses a fabricated worktree on a direct-branch placement
// outright, so the rule above is a constraint rather than a convention the
// selection code has to remember.
func TestStorageRefusesAWorktreeOnADirectBranchPlacement(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionDirectBranch)
	now := time.Now().UTC()
	_, err := f.store.FreezeExecutionPlacement(context.Background(), domain.ExecutionPlacement{
		ID: "plc-bogus", WorkflowRunID: f.run.ID, ProjectID: "p",
		PlacementGeneration: 1, Type: domain.PlacementDirectBranch,
		RepoPath: "/repo", ExecutionBranch: "main", WorktreePath: "/tmp/invented",
		State: domain.PlacementSelected, Provenance: domain.PlacementFrozenAtSelection,
		CreatedAt: now, UpdatedAt: now,
	})
	if err == nil {
		t.Fatal("a direct-branch placement naming a worktree was accepted")
	}
}

// ---------------------------------------------------------------------------
// seeds
// ---------------------------------------------------------------------------

// seedExecutedAttempt makes a step look like it has already run, which is what
// marks a run as legacy for placement purposes.
func seedExecutedAttempt(t *testing.T, store *crashStore, stepID string) {
	t.Helper()
	if _, err := store.CreateWorkflowAttempt(context.Background(), "wfa-legacy", stepID, string(domain.HarnessCodex), "", time.Now().UTC()); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
}

// asLegacyChildRun re-points a fixture at a CHILD run of a master objective:
// the shape a planned task actually executes in, and the only shape an AO
// worktree record can exist for (its task_id is a foreign key into the planned
// tasks). The placement scope is then (child run, planned task), which is what
// production uses.
func (f *placementFixture) asLegacyChildRun(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	master := domain.WorkflowRun{
		ID: "wf-master-legacy", ProjectID: "p", Objective: "master", State: domain.WorkflowRunRunning,
		PolicyVersion: "v1", PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := f.store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatalf("seed master run: %v", err)
	}
	taskID := "wft-legacy"
	if err := f.store.InsertWorkflowTasks(ctx, []domain.WorkflowTask{{
		ID: taskID, WorkflowRunID: master.ID, PlanStepID: "s1", Ordinal: 1,
		Title: "legacy task", Description: "work that already ran", AcceptanceCriteriaJSON: "[]", VerifyJSON: "{}",
		ScopeJSON: "{}", State: domain.WorkflowTaskRunning, PlanRevision: 1,
		CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatalf("seed planned task: %v", err)
	}
	if tasks, err := f.store.ListWorkflowTasksAtRevision(ctx, master.ID, 1); err != nil || len(tasks) != 1 {
		t.Fatalf("planned task was not persisted: n=%d err=%v", len(tasks), err)
	}
	// Re-point the run under test at the child shape by re-creating it with the
	// parent/task link the coordinator reads.
	parent, task := master.ID, taskID
	child := domain.WorkflowRun{
		ID: "wf-child-legacy", ProjectID: "p", Objective: "child", State: domain.WorkflowRunRunning,
		PolicyVersion: "v1", PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now,
		ParentWorkflowID: &parent, PlannedTaskID: &task,
	}
	steps := []domain.WorkflowStep{{
		ID: "wfs-child-legacy", WorkflowRunID: child.ID, Kind: domain.WorkflowStepWork,
		Ordinal: 1, State: domain.WorkflowStepPending, CreatedAt: now, UpdatedAt: now,
	}}
	createdRun, createdSteps, err := f.store.CreateWorkflowRun(ctx, child, steps)
	if err != nil {
		t.Fatalf("seed child run: %v", err)
	}
	f.run, f.step = createdRun, createdSteps[0]
	return taskID
}

func seedTaskWorktree(t *testing.T, store *crashStore, runID, taskID, path, branch, baseSHA string) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.UpsertTaskWorktree(context.Background(), domain.TaskWorktreeRecord{
		WorkflowRunID: runID, TaskID: taskID, ProjectID: "p",
		RepoPath: "/repo", Path: path, Branch: branch, TargetBranch: "main",
		BaseSHA: baseSHA, ExecutionMode: domain.ExecutionIsolatedWorktree,
		State: domain.TaskWorktreeActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed worktree record: %v", err)
	}
}

func seedHeldBranchLock(t *testing.T, store *crashStore, runID string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := store.AcquireBranchLock(context.Background(), domain.BranchLock{
		ID: "bl-" + runID, LockKey: "/repo\x1fmain", ProjectID: "p",
		RepoPath: "/repo", RepoName: domain.RootWorkspaceRepoName, Branch: "main",
		OwnershipKind: domain.BranchLockOwnershipDirectBranch,
		WorkflowRunID: runID, OwnerToken: "daemon-p1d",
		State: domain.BranchLockHeld, BaseSHA: "abc123",
		AcquiredAt: now, RenewedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed branch lock: %v", err)
	}
}
