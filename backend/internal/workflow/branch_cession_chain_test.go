package workflow_test

// branch_cession_chain_test.go — the repair-of-repair branch lock, reproduced
// against the real branch_locks table and unwound by AO alone.
//
// THE DURABLE STATE, from ~/.ao/data:
//
//	wf-724a1e97   needs_attention   fix_budget_exhausted        (A, the origin)
//	  wf-f5025a7c   needs_attention   fix_no_verifiable_change  (B, repair gen 2)
//	    wf-c4c84f52   needs_attention                           (C, B's own repair)
//	      holds blk-dc8e9a89 on feat/engineering-control-center
//
// A's ledger records ceding blk-1c7c84b1 to generation 1's repair, which
// completed and released it. B then took blk-dc8e9a89 through the ordinary
// queue in its own name — there is no A -> B cession row and there never will
// be — and ceded THAT to C. So the two hops rest on different evidence, and the
// test seeds them differently on purpose: a custody hop and a ceded hop, in the
// order the incident has them.
//
// What is pinned: two reconciles, no Continue, no Cancel, no new repair
// generation, and the branch walks back C -> B -> A one hop at a time. The
// negatives are the substance — each removes one fact and requires the fold to
// refuse, with the branch left exactly where it was.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

const (
	chainBranch   = "feat/engineering-control-center"
	chainRepoPath = "/repo/agent-orchestrator"
)

// ---------------------------------------------------------------------------
// The lock port, over the real table.
// ---------------------------------------------------------------------------

// chainBranchLocks is the workflow package's BranchLocks port backed directly
// by the SQLite branch-lock store.
//
// It deliberately does NOT wrap branchlock.Manager: the Manager's own
// acquisition path needs a git worktree, a preflight and a project in
// direct-branch mode, none of which this incident is about. What IS load
// bearing here is the compare-and-set — CedeBranchLock's
// "id = ? AND state = 'held' AND workflow_run_id = ?" — and that is the real
// statement against the real table, which is what makes the races below real
// races.
type chainBranchLocks struct {
	store *sqlite.Store
	token string
	seq   int
}

func (l *chainBranchLocks) Acquire(ctx context.Context, req workflowcore.BranchLockRequest) ([]domain.BranchLock, error) {
	l.seq++
	lock, err := l.store.AcquireBranchLock(ctx, domain.BranchLock{
		ID:            fmt.Sprintf("blk-chain-%d", l.seq),
		LockKey:       chainRepoPath + "\x1f" + chainBranch,
		ProjectID:     req.ProjectID,
		RepoPath:      chainRepoPath,
		RepoName:      domain.RootWorkspaceRepoName,
		Branch:        chainBranch,
		OwnershipKind: domain.BranchLockOwnershipDirectBranch,
		WorkflowRunID: req.RunID,
		OwnerToken:    l.token,
		AcquiredAt:    time.Now().UTC(),
	})
	if err != nil {
		return nil, err
	}
	return []domain.BranchLock{lock}, nil
}

func (l *chainBranchLocks) ReleaseRun(ctx context.Context, runID, reason string) (int64, error) {
	return l.store.ReleaseBranchLocksByRun(ctx, runID, reason, time.Now().UTC())
}

func (l *chainBranchLocks) HeldByRun(ctx context.Context, runID string) ([]domain.BranchLock, error) {
	return l.store.ListHeldBranchLocksByRun(ctx, runID)
}

func (l *chainBranchLocks) Renew(context.Context, string, string, string) {}

func (l *chainBranchLocks) RecoverStale(context.Context, string) (int64, error) { return 0, nil }

// Cede is what makes this a branchLockCeder, and it is the real CAS.
func (l *chainBranchLocks) Cede(ctx context.Context, lockID, fromRunID, toRunID, toStepID string) (bool, error) {
	return l.store.CedeBranchLock(ctx, lockID, fromRunID, toRunID, toStepID, time.Now().UTC())
}

// ---------------------------------------------------------------------------
// The fixture.
// ---------------------------------------------------------------------------

type chainCase struct {
	*quiescenceCase
	grandRunID string // C: the repair of the repair
	lockID     string
	locks      *chainBranchLocks
}

type chainOptions struct {
	// noOriginBranchIdentity removes A's durable record of working this branch,
	// which is custody's third fact.
	noOriginBranchIdentity bool
	// contradictoryOriginMarker gives B a second origin marker naming somebody
	// else, so its provenance is no longer resolvable.
	contradictoryOriginMarker bool
	// noNestedIntent removes B's repair intent for C, so the B -> C hop has a
	// cession row but no repair binding behind it.
	noNestedIntent bool
	// lockPredatesRepair backdates the lock to before B existed, so it cannot
	// have been taken on A's behalf.
	lockPredatesRepair bool
	// buriedStopOnC parks C the way wf-c4c84f52 is really parked: on
	// reviewer_launch_failed, written in a one-second burst with its launch
	// trail, and then buried under 302 observations of one approved verdict.
	buriedStopOnC bool
	// runningReviewOnC leaves C's own review with no verdict, so a reviewer may
	// still be working and the chain must not fold.
	runningReviewOnC bool
	// clearedStopOnC writes an attention_cleared newer than C's stop, so its
	// ledger no longer says why it is parked.
	clearedStopOnC bool
	// cededOriginHop makes the A -> B hop a real cession instead of custody:
	// A holds the branch and hands it over, which is the shape a repair created
	// while its origin still held the lock produces. Both hops are then ceded.
	cededOriginHop bool
}

func newChainCase(t *testing.T) *chainCase { return newChainCaseWith(t, chainOptions{}) }

func newChainCaseWith(t *testing.T, opts chainOptions) *chainCase {
	t.Helper()
	q := newQuiescenceCase(t)

	// The lock port, wired into a rebuilt Coordinator over the same rows —
	// which is exactly what a daemon restart is on this fixture.
	locks := &chainBranchLocks{store: q.store, token: "daemon-a"}
	q.locks = locks
	q.build()

	c := &chainCase{quiescenceCase: q, locks: locks}

	// A's own durable account of working this branch in this repository. In the
	// real incident it comes from A's ordinary dispatch checkpoints; here it is
	// seeded as the branch-wait row the production path writes, because that
	// row is one of the ones that carries it.
	if !opts.noOriginBranchIdentity {
		c.seedBranchIdentity(t, q.runID)
	}

	// B takes the branch itself, after it exists: generation 1's repair had
	// already released it, so there was nothing for A to cede.
	repairRun, _, err := q.store.GetWorkflowRun(q.ctx, q.repairRunID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(B): %v", err)
	}
	acquiredAt := repairRun.CreatedAt.Add(time.Minute)
	if opts.lockPredatesRepair {
		acquiredAt = repairRun.CreatedAt.Add(-time.Hour)
	}
	firstOwner := q.repairRunID
	if opts.cededOriginHop {
		firstOwner = q.runID
	}
	lock, err := q.store.AcquireBranchLock(q.ctx, domain.BranchLock{
		ID: "blk-dc8e9a89", LockKey: chainRepoPath + "\x1f" + chainBranch,
		ProjectID: "agent-orchestrator", RepoPath: chainRepoPath,
		RepoName: domain.RootWorkspaceRepoName, Branch: chainBranch,
		OwnershipKind: domain.BranchLockOwnershipDirectBranch,
		WorkflowRunID: firstOwner, OwnerToken: locks.token, AcquiredAt: acquiredAt,
	})
	if err != nil {
		t.Fatalf("B acquires the branch: %v", err)
	}
	c.lockID = lock.ID
	if opts.cededOriginHop {
		c.cede(t, q.runID, q.repairRunID, 2, "wfr-quiescence-2")
	}

	if opts.contradictoryOriginMarker {
		c.seedConflictingOriginMarker(t, q.repairRunID)
	}

	// C: B's own repair, parked for a person, and the branch ceded to it.
	c.grandRunID = c.seedNestedRepair(t, !opts.noNestedIntent)
	if opts.buriedStopOnC {
		c.buryCStop(t)
	}
	if opts.runningReviewOnC {
		c.seedRunningReviewOnC(t)
	}
	if opts.clearedStopOnC {
		c.clearCStop(t)
	}
	c.cede(t, q.repairRunID, c.grandRunID, 1, "wfr-chain-nested")
	return c
}

// seedBranchIdentity writes the branch-wait checkpoint the ordinary
// direct-branch path writes, which is how a run's ledger says which branch in
// which repository is its.
func (c *chainCase) seedBranchIdentity(t *testing.T, runID string) {
	t.Helper()
	run, _, err := c.store.GetWorkflowRun(c.ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(%s): %v", runID, err)
	}
	if _, err := c.store.CreateWorkflowCheckpoint(c.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-branch-identity-" + runID, WorkflowRunID: runID, ProjectID: run.ProjectID,
		DurablePhase: "waiting_for_branch", NextAction: "waiting_for_branch: " + chainBranch,
		Branch: chainBranch, WorktreePath: chainRepoPath,
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: c.clk.Now(),
	}); err != nil {
		t.Fatalf("seed branch identity for %s: %v", runID, err)
	}
}

// seedNestedRepair creates C the way LaunchRepair does: a real run, marked as a
// repair of B before it starts, parked on a human-owned stop, with B's dispatch
// intent naming it.
func (c *chainCase) seedNestedRepair(t *testing.T, withIntent bool) string {
	t.Helper()
	created, err := c.c.CreateRun(c.ctx, "agent-orchestrator", "Repair a stopped AO repair run (generation 1)")
	if err != nil {
		t.Fatalf("CreateRun(C): %v", err)
	}
	nested := created.Run.ID
	if _, err := c.store.CreateWorkflowCheckpoint(c.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-repair-origin-nested", WorkflowRunID: nested, ProjectID: created.Run.ProjectID,
		DurablePhase:   "workflow_repair_run_origin",
		NextAction:     fmt.Sprintf("repair run for %s, generation 1", c.repairRunID),
		PayloadVersion: "v1",
		RetryState:     fmt.Sprintf(`{"originRunId":%q,"generation":1}`, c.repairRunID),
		CreatedAt:      c.clk.Now(),
	}); err != nil {
		t.Fatalf("seed C's origin marker: %v", err)
	}
	moveRunRow(t, c.quiescenceCase, nested, domain.WorkflowRunNeedsAttention)
	for _, step := range listSteps(t, c.quiescenceCase, nested) {
		switch step.Kind {
		case domain.WorkflowStepPlan:
			moveStepRow(t, c.quiescenceCase, step, domain.WorkflowStepRunning)
			moveStepRow(t, c.quiescenceCase, refreshStep(t, c.quiescenceCase, nested, domain.WorkflowStepPlan), domain.WorkflowStepCompleted)
		case domain.WorkflowStepWork:
			moveStepRow(t, c.quiescenceCase, step, domain.WorkflowStepReady)
			moveStepRow(t, c.quiescenceCase, refreshStep(t, c.quiescenceCase, nested, domain.WorkflowStepWork), domain.WorkflowStepRunning)
			moveStepRow(t, c.quiescenceCase, refreshStep(t, c.quiescenceCase, nested, domain.WorkflowStepWork), domain.WorkflowStepCompleted)
		case domain.WorkflowStepReview, domain.WorkflowStepFix:
			moveStepRow(t, c.quiescenceCase, step, domain.WorkflowStepWaiting)
		}
	}
	if _, err := c.store.CreateWorkflowCheckpoint(c.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-repair-stop-nested", WorkflowRunID: nested, ProjectID: created.Run.ProjectID,
		DurablePhase:   workflowcore.ReasonFixBudgetExhausted,
		NextAction:     "the repair's own review/fix budget is spent; the next step is a person's",
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: c.clk.Now(),
	}); err != nil {
		t.Fatalf("seed C's stop: %v", err)
	}
	if withIntent {
		c.seedIntent(t, c.repairRunID, nested, 1, "wfr-chain-nested")
	}
	c.clk.Advance(time.Second)
	return nested
}

// seedIntent writes one run's repair dispatch intent naming another as its
// repair run — the origin's half of the two-sided binding.
func (c *chainCase) seedIntent(t *testing.T, originID, repairID string, generation int, intentID string) {
	t.Helper()
	origin, _, err := c.store.GetWorkflowRun(c.ctx, originID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(%s): %v", originID, err)
	}
	payload, err := json.Marshal(domain.RepairIntent{
		ID: intentID, WorkflowRunID: originID, TargetRunID: originID,
		ConditionReason: workflowcore.ReasonFixNoVerifiableChange, EvidenceDigest: "digest-chain",
		Generation: generation, ProjectID: "agent-orchestrator", RepairRunID: repairID,
		AuthorizedBy: "operator", At: c.clk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.CreateWorkflowCheckpoint(c.ctx, domain.WorkflowCheckpoint{
		ID:            fmt.Sprintf("wfc-chain-intent-%s-%d", originID, generation),
		WorkflowRunID: originID, ProjectID: origin.ProjectID,
		DurablePhase:   "workflow_repair_dispatched",
		NextAction:     fmt.Sprintf("repair generation %d dispatched as run %s", generation, repairID),
		PayloadVersion: "v1", RetryState: string(payload), CreatedAt: c.clk.Now(),
	}); err != nil {
		t.Fatalf("seed intent %s: %v", intentID, err)
	}
}

// cede performs one hand-over the way production does: the ledger row first,
// then the compare-and-set.
func (c *chainCase) cede(t *testing.T, fromRunID, toRunID string, generation int, intentID string) {
	t.Helper()
	from, _, err := c.store.GetWorkflowRun(c.ctx, fromRunID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(%s): %v", fromRunID, err)
	}
	payload, err := json.Marshal(map[string]any{
		"lockId": c.lockID, "fromRunId": fromRunID, "toRunId": toRunID,
		"repairIntentId": intentID, "repairGeneration": generation,
		"branch": chainBranch, "repoPath": chainRepoPath, "kind": "ceded",
		"at": c.clk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.CreateWorkflowCheckpoint(c.ctx, domain.WorkflowCheckpoint{
		ID:            fmt.Sprintf("wfc-chain-cede-%s-%s", fromRunID, toRunID),
		WorkflowRunID: fromRunID, ProjectID: from.ProjectID,
		DurablePhase:   "branch_lock_ceded_to_repair",
		NextAction:     fmt.Sprintf("branch %s ceded to repair run %s (generation %d)", chainBranch, toRunID, generation),
		PayloadVersion: "v1", RetryState: string(payload), CreatedAt: c.clk.Now(),
	}); err != nil {
		t.Fatalf("seed cession %s -> %s: %v", fromRunID, toRunID, err)
	}
	moved, err := c.store.CedeBranchLock(c.ctx, c.lockID, fromRunID, toRunID, "", c.clk.Now())
	if err != nil || !moved {
		t.Fatalf("cede %s -> %s: moved=%v err=%v", fromRunID, toRunID, moved, err)
	}
}

// seedConflictingOriginMarker makes the run's provenance contradict itself: two
// origin markers naming two different runs. Neither is evidence any more.
func (c *chainCase) seedConflictingOriginMarker(t *testing.T, runID string) {
	t.Helper()
	run, _, err := c.store.GetWorkflowRun(c.ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(%s): %v", runID, err)
	}
	if _, err := c.store.CreateWorkflowCheckpoint(c.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-repair-origin-conflict", WorkflowRunID: runID, ProjectID: run.ProjectID,
		DurablePhase:   "workflow_repair_run_origin",
		NextAction:     "a second, disagreeing account of whose repair this is",
		PayloadVersion: "v1",
		RetryState:     `{"originRunId":"wf-somebody-else","generation":7}`,
		CreatedAt:      c.clk.Now(),
	}); err != nil {
		t.Fatalf("seed a conflicting origin marker on %s: %v", runID, err)
	}
}

// seedHeldCapacityClaimForRun grants a run a held slot of the given kind, which
// is what "a live mutating execution" looks like in the capacity ledger.
func seedHeldCapacityClaimForRun(t *testing.T, q *quiescenceCase, runID string, kind domain.ExecutionKind) {
	t.Helper()
	step := refreshStep(t, q, runID, domain.WorkflowStepVerify)
	key := "cap:" + string(kind) + ":chain-test:" + runID + ":gen0"
	if enqueued, err := q.store.EnqueueCapacityClaim(q.ctx, domain.CapacityClaim{
		ID: "cc-chain-" + runID, Kind: kind, State: domain.CapacityClaimQueued,
		WorkflowRunID: runID, WorkflowStepID: step.ID,
		DispatchKey: key, ProjectID: "agent-orchestrator",
		LifecycleGeneration: 0, Priority: domain.PriorityForKind(kind),
		EnqueuedAt: q.clk.Now(), UpdatedAt: q.clk.Now(),
	}); err != nil {
		t.Fatalf("EnqueueCapacityClaim: %v", err)
	} else if !enqueued {
		t.Fatal("the fixture could not enqueue a capacity claim")
	}
	granted, err := q.store.AcquireCapacity(q.ctx, key, 0, domain.CapacityLimits{}.Normalize(), kind, q.clk.Now())
	if err != nil || !granted {
		t.Fatalf("AcquireCapacity: granted=%v err=%v", granted, err)
	}
}

// holder reports which run holds the branch right now, straight from the table.
func (c *chainCase) holder(t *testing.T) string {
	t.Helper()
	locks, err := c.store.ListHeldBranchLocks(c.ctx)
	if err != nil {
		t.Fatalf("ListHeldBranchLocks: %v", err)
	}
	for _, lock := range locks {
		if lock.ID == c.lockID {
			return lock.WorkflowRunID
		}
	}
	return ""
}

// folds counts the durable records of one hop, which is how "exactly once" is
// asserted about a transfer rather than about a log line.
func (c *chainCase) folds(t *testing.T, ownerRunID, phase string) int {
	t.Helper()
	cps, err := c.store.ListWorkflowCheckpoints(c.ctx, ownerRunID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints(%s): %v", ownerRunID, err)
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase != phase {
			continue
		}
		var rec struct {
			LockID string `json:"lockId"`
		}
		if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.LockID == c.lockID {
			n++
		}
	}
	return n
}

func (c *chainCase) chainView(t *testing.T) *workflowcore.BranchCessionChain {
	t.Helper()
	detail, err := c.c.GetRun(c.ctx, c.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return detail.Repair.CessionChain
}

// ---------------------------------------------------------------------------
// The positive case: the real incident, unwound.
// ---------------------------------------------------------------------------

func TestARepairOfRepairChainReturnsTheBranchOneLinkPerReconcile(t *testing.T) {
	c := newChainCase(t)
	if got := c.holder(t); got != c.grandRunID {
		t.Fatalf("fixture precondition: branch is held by %s, want C (%s)", got, c.grandRunID)
	}

	// What the API says before anything is folded: the branch is two repair
	// hops away from its origin, and AO can bring it back by itself.
	view := c.chainView(t)
	if view == nil {
		t.Fatal("the origin reports no cession chain while its branch is two repairs away")
	}
	if view.Depth != 2 || view.CurrentHolderRunID != c.grandRunID || view.OriginRunID != c.runID {
		t.Fatalf("chain view = %+v, want depth 2 from %s held by %s", view, c.runID, c.grandRunID)
	}
	if !view.Returnable || view.BlockedReason != "" {
		t.Fatalf("chain reports not returnable: %+v", view)
	}

	// Reconcile #1: C -> B, exactly once.
	c.reconcileOnly(t)
	if got := c.holder(t); got != c.repairRunID {
		t.Fatalf("after reconcile 1 the branch is held by %s, want B (%s)", got, c.repairRunID)
	}
	if n := c.folds(t, c.repairRunID, "branch_lock_returned_from_repair"); n != 1 {
		t.Fatalf("C -> B fold records = %d, want exactly 1", n)
	}
	if n := c.folds(t, c.runID, "branch_custody_returned_to_origin"); n != 0 {
		t.Fatalf("B -> A folded in the same pass (%d records); the chain must unwind one link at a time", n)
	}

	// Reconcile #2: B -> A, exactly once.
	c.reconcileOnly(t)
	if got := c.holder(t); got != c.runID {
		t.Fatalf("after reconcile 2 the branch is held by %s, want A (%s)", got, c.runID)
	}
	if n := c.folds(t, c.runID, "branch_custody_returned_to_origin"); n != 1 {
		t.Fatalf("B -> A fold records = %d, want exactly 1", n)
	}
	if n := c.folds(t, c.repairRunID, "branch_lock_returned_from_repair"); n != 1 {
		t.Fatalf("C -> B fold records = %d after the second pass, want still exactly 1", n)
	}

	// And nothing else moved: no human action, no new repair generation, and
	// the branch is not out any more.
	if n := c.countPhase("workflow_repair_dispatched"); n != 2 {
		t.Fatalf("repair dispatches = %d, want still 2: unwinding a chain must not buy a generation", n)
	}
	if view := c.chainView(t); view != nil {
		t.Fatalf("the origin still reports a cession chain after its branch came home: %+v", view)
	}

	// Further passes are no-ops: the branch stays with A and nothing is
	// recorded twice.
	c.reconcileOnly(t)
	c.reconcileOnly(t)
	if got := c.holder(t); got != c.runID {
		t.Fatalf("the branch left A on a later pass, to %s", got)
	}
	if n := c.folds(t, c.runID, "branch_custody_returned_to_origin"); n != 1 {
		t.Fatalf("B -> A fold records = %d after four passes, want exactly 1", n)
	}
}

// A restart at every boundary still produces exactly one transfer per link.
func TestChainFoldSurvivesRestartsExactlyOnce(t *testing.T) {
	c := newChainCase(t)
	for i := 0; i < 5; i++ {
		c.restart()
		c.reconcileOnly(t)
	}
	if got := c.holder(t); got != c.runID {
		t.Fatalf("branch holder = %s, want A (%s) after repeated restarts", got, c.runID)
	}
	if n := c.folds(t, c.repairRunID, "branch_lock_returned_from_repair"); n != 1 {
		t.Fatalf("C -> B fold records = %d across restarts, want exactly 1", n)
	}
	if n := c.folds(t, c.runID, "branch_custody_returned_to_origin"); n != 1 {
		t.Fatalf("B -> A fold records = %d across restarts, want exactly 1", n)
	}
}

// Two daemons folding the same link produce one transfer, because the transfer
// is a compare-and-set on who holds it and the record is keyed by what it is
// about.
func TestTwoDaemonsFoldingTheSameLinkProduceOneTransfer(t *testing.T) {
	c := newChainCase(t)
	second := c.newCoordinatorOverSameStore()

	c.reconcileOnly(t)
	if err := second.Reconcile(c.ctx); err != nil {
		t.Fatalf("Reconcile (second daemon): %v", err)
	}

	if n := c.folds(t, c.repairRunID, "branch_lock_returned_from_repair"); n != 1 {
		t.Fatalf("C -> B fold records = %d with two daemons, want exactly 1", n)
	}
	if holder := c.holder(t); holder != c.repairRunID && holder != c.runID {
		t.Fatalf("branch holder = %s, want B or A", holder)
	}
}

// A crash between the compare-and-set and its record: the branch has already
// moved, and the next pass completes the bookkeeping without a second transfer.
func TestACrashBetweenTheTransferAndItsRecordIsCompletedNotRepeated(t *testing.T) {
	c := newChainCase(t)

	// The CAS happened; the daemon died before writing the row.
	moved, err := c.store.CedeBranchLock(c.ctx, c.lockID, c.grandRunID, c.repairRunID, "", c.clk.Now())
	if err != nil || !moved {
		t.Fatalf("simulate the transfer: moved=%v err=%v", moved, err)
	}
	c.restart()
	c.reconcileOnly(t)

	if n := c.folds(t, c.repairRunID, "branch_lock_returned_from_repair"); n != 1 {
		t.Fatalf("C -> B records = %d after completing bookkeeping, want exactly 1", n)
	}
	if got := c.holder(t); got == c.grandRunID {
		t.Fatalf("the branch went back to C (%s); bookkeeping must never re-transfer", got)
	}
}

// ---------------------------------------------------------------------------
// The negatives. Each removes ONE fact; the branch must not move.
// ---------------------------------------------------------------------------

// The holder can still write: a held worker slot is a live mutating execution,
// and no amount of chain structure makes it foldable.
func TestAHolderThatCanStillWriteKeepsTheBranch(t *testing.T) {
	c := newChainCase(t)
	seedHeldCapacityClaimForRun(t, c.quiescenceCase, c.grandRunID, domain.ExecutionKindWorker)

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if got := c.holder(t); got != c.grandRunID {
		t.Fatalf("branch holder = %s, want C (%s): a repair holding a worker slot keeps its branch", got, c.grandRunID)
	}
	if n := c.folds(t, c.repairRunID, "branch_lock_returned_from_repair"); n != 0 {
		t.Fatalf("a fold was recorded (%d) for a holder that can still write", n)
	}
	view := c.chainView(t)
	if view == nil || view.Returnable {
		t.Fatalf("chain reports returnable while its holder can write: %+v", view)
	}
	if view.BlockedReason != "holder_can_still_write" {
		t.Fatalf("blockedReason = %q, want holder_can_still_write (%s)", view.BlockedReason, view.Detail)
	}
}

// The holder is quiescent but the branch is no longer its: nothing is taken
// from whoever holds it now, and the stale cession row is closed rather than
// acted on.
func TestAQuiescentHolderThatNoLongerOwnsTheLockIsNotFolded(t *testing.T) {
	c := newChainCase(t)
	stranger, err := c.c.CreateRun(c.ctx, "agent-orchestrator", "an unrelated run")
	if err != nil {
		t.Fatal(err)
	}
	moved, err := c.store.CedeBranchLock(c.ctx, c.lockID, c.grandRunID, stranger.Run.ID, "", c.clk.Now())
	if err != nil || !moved {
		t.Fatalf("hand the branch to a stranger: moved=%v err=%v", moved, err)
	}

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if got := c.holder(t); got != stranger.Run.ID {
		t.Fatalf("branch holder = %s, want the unrelated run %s: a fold must never take a branch back from a third party",
			got, stranger.Run.ID)
	}
}

// No proof of the B -> C hop: the cession row is there, but nothing binds C to
// B as its repair. Fail closed, with the reason named.
func TestAnUnprovableHopIsRefusedAndReported(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{noNestedIntent: true})

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if got := c.holder(t); got != c.grandRunID {
		t.Fatalf("branch holder = %s, want C (%s): an unprovable hop must not be folded", got, c.grandRunID)
	}
	view := c.chainView(t)
	if view == nil || view.Returnable {
		t.Fatalf("chain reports returnable over an unprovable hop: %+v", view)
	}
	if view.BlockedReason != "legacy_unprovable_branch_cession" {
		t.Fatalf("blockedReason = %q, want legacy_unprovable_branch_cession (%s)", view.BlockedReason, view.Detail)
	}
}

// No proof of the A -> B hop: C -> B is still provable and still folds, and
// B -> A does not. This is the exact partial-provenance case the chain model
// has to get right — the deepest hop is not held hostage by the shallowest.
func TestAMissingOriginHopFoldsTheProvableLinkAndRefusesTheRest(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts chainOptions
	}{
		{"no branch identity", chainOptions{noOriginBranchIdentity: true}},
		{"contradictory origin marker", chainOptions{contradictoryOriginMarker: true}},
		{"lock predates the repair", chainOptions{lockPredatesRepair: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newChainCaseWith(t, tc.opts)

			c.reconcileOnly(t)
			c.reconcileOnly(t)
			c.reconcileOnly(t)

			if got := c.holder(t); got != c.repairRunID {
				t.Fatalf("branch holder = %s, want B (%s): C -> B is provable and B -> A is not", got, c.repairRunID)
			}
			if n := c.folds(t, c.repairRunID, "branch_lock_returned_from_repair"); n != 1 {
				t.Fatalf("C -> B fold records = %d, want exactly 1", n)
			}
			if n := c.folds(t, c.runID, "branch_custody_returned_to_origin"); n != 0 {
				t.Fatalf("B -> A was folded (%d records) without provenance", n)
			}
		})
	}
}

// A superseded generation cannot fold anything: B mints a newer repair, and
// generation 1's cession stops being what a fold may act on.
func TestAStaleCessionGenerationCannotFold(t *testing.T) {
	c := newChainCase(t)
	newer, err := c.c.CreateRun(c.ctx, "agent-orchestrator", "Repair a stopped AO repair run (generation 2)")
	if err != nil {
		t.Fatal(err)
	}
	c.seedIntent(t, c.repairRunID, newer.Run.ID, 2, "wfr-chain-nested-2")

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if got := c.holder(t); got != c.grandRunID {
		t.Fatalf("branch holder = %s, want C (%s): a superseded generation must not return a branch", got, c.grandRunID)
	}
	if n := c.folds(t, c.repairRunID, "branch_lock_returned_from_repair"); n != 0 {
		t.Fatalf("a superseded generation folded a branch (%d records)", n)
	}
}

// The run a branch would go back to is over: there is no authority to return it
// to, so nothing is returned and no owner is invented.
func TestAChainWhosePreviousOwnerIsTerminalIsRefused(t *testing.T) {
	c := newChainCase(t)
	moveRunRow(t, c.quiescenceCase, c.repairRunID, domain.WorkflowRunCancelled)

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if got := c.holder(t); got != c.grandRunID {
		t.Fatalf("branch holder = %s, want C (%s): a cancelled previous owner is not an authority", got, c.grandRunID)
	}
	if n := c.folds(t, c.repairRunID, "branch_lock_returned_from_repair"); n != 0 {
		t.Fatalf("a branch was handed to a cancelled run (%d records)", n)
	}
}

// The same incident with both hops recorded as cessions — the shape a repair
// created while its origin still held the branch produces.
//
// It exercises what custody does not: a live cession row at the ROOT of the
// chain. Bookkeeping must leave that row alone while the branch is still two
// hops away, because closing it would erase the evidence the fold walks back
// through — and the only way to observe that is to hold the chain still, which
// is what the unfoldable holder below does.
func TestABookkeepingPassLeavesALiveRootCessionAlone(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{cededOriginHop: true})
	seedHeldCapacityClaimForRun(t, c.quiescenceCase, c.grandRunID, domain.ExecutionKindWorker)

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if got := c.holder(t); got != c.grandRunID {
		t.Fatalf("branch holder = %s, want C (%s)", got, c.grandRunID)
	}
	// The root row is still open, which is what makes the chain still readable
	// as two hops from A down to C.
	view := c.chainView(t)
	if view == nil || view.Depth != 2 || view.CurrentHolderRunID != c.grandRunID {
		t.Fatalf("chain view = %+v, want the root cession still open at depth 2 held by C", view)
	}
	if n := c.folds(t, c.runID, "branch_lock_returned_from_repair"); n != 0 {
		t.Fatalf("the root cession A -> B was closed (%d records) while the branch was still out", n)
	}
}

// And the same chain, unwound. Both hops are cessions, so both are recorded as
// returns; each happens exactly once and the branch ends up with A.
//
// Deliberately no assertion about WHICH pass each hop lands in. When the root
// hop is a recorded cession, the origin's own quiescence fold — P1-D §L's
// original single-hop return — can complete it in the same pass the chain fold
// completed the hop below, because that hop's proof is the very proof it just
// ran. What is guaranteed, and what is asserted, is the ordering that matters:
// a hop is never folded before the one below it is durably done, and no hop is
// ever folded twice.
func TestAFullyCededChainUnwindsToItsOrigin(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{cededOriginHop: true})

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if got := c.holder(t); got != c.runID {
		t.Fatalf("branch holder = %s, want A (%s)", got, c.runID)
	}
	if n := c.folds(t, c.repairRunID, "branch_lock_returned_from_repair"); n != 1 {
		t.Fatalf("C -> B fold records = %d, want exactly 1", n)
	}
	if n := c.folds(t, c.runID, "branch_lock_returned_from_repair"); n != 1 {
		t.Fatalf("B -> A fold records = %d, want exactly 1", n)
	}
	if view := c.chainView(t); view != nil {
		t.Fatalf("the origin still reports a cession chain after its branch came home: %+v", view)
	}
}

// A fold's own record must not become the run's reason for being stopped. It is
// written on the ORIGIN, which is parked on something of its own, and the rule
// that gets the branch back is the same one that reads that stop.
func TestAFoldRecordDoesNotDisplaceTheOriginsStopReason(t *testing.T) {
	c := newChainCase(t)
	stateBefore := c.detail().Run.State
	reasonBefore := attentionReasonOf(t, c.quiescenceCase, c.runID)
	if reasonBefore == "" {
		t.Fatal("fixture precondition: the origin cannot say why it stopped")
	}

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if got := c.holder(t); got != c.runID {
		t.Fatalf("branch holder = %s, want A (%s)", got, c.runID)
	}
	if got := c.detail().Run.State; got != stateBefore {
		t.Fatalf("the origin's state changed from %s to %s as a side effect of the fold", stateBefore, got)
	}
	if got := attentionReasonOf(t, c.quiescenceCase, c.runID); got != reasonBefore {
		t.Fatalf("the origin's stop reason became %q (was %q) because a fold record displaced it", got, reasonBefore)
	}
}

// attentionReasonOf reports the reason AO would give for a parked run's stop,
// through the same classifier the Board reads.
func attentionReasonOf(t *testing.T, q *quiescenceCase, runID string) string {
	t.Helper()
	detail, err := q.c.GetRun(q.ctx, runID)
	if err != nil {
		t.Fatalf("GetRun(%s): %v", runID, err)
	}
	return workflowcore.ClassifyAttention(detail, nil, workflowcore.PhaseNeedsAttention).Reason
}

// buryCStop replaces C's seeded stop with the shape wf-c4c84f52 really has: a
// reviewer_launch_failed written in the same instant as its launch trail, then
// 302 observations of one approved verdict on top of it.
func (c *chainCase) buryCStop(t *testing.T) {
	t.Helper()
	run, _, err := c.store.GetWorkflowRun(c.ctx, c.grandRunID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(C): %v", err)
	}
	// The rest of wf-c4c84f52's real shape, without which this fixture would be
	// testing something else: its work step HAS a session (so nothing later
	// calls the run ambiguous and writes a fresh stop over the buried one), and
	// its review step FAILED with the launch (so nothing tries to dispatch a
	// review while the chain is unwinding). Both are load-bearing — with a
	// newer stop of its own, C would fold for a reason that has nothing to do
	// with the 302 rows this test exists for.
	c.attachSessionTo(t, refreshStep(t, c.quiescenceCase, c.grandRunID, domain.WorkflowStepWork))
	moveStepRow(t, c.quiescenceCase,
		refreshStep(t, c.quiescenceCase, c.grandRunID, domain.WorkflowStepReview), domain.WorkflowStepFailed)

	burst := c.clk.Now()
	for i, phase := range []string{
		"review_dispatched", workflowcore.ReasonReviewerLaunchFailed,
		"review_launch_confirmed", "review_launch_abandoned",
	} {
		if _, err := c.store.CreateWorkflowCheckpoint(c.ctx, domain.WorkflowCheckpoint{
			ID: fmt.Sprintf("wfc-c-burst-%d", i), WorkflowRunID: c.grandRunID, ProjectID: run.ProjectID,
			DurablePhase: phase, NextAction: "the reviewer could not be launched",
			PayloadVersion: "v1", RetryState: "{}", CreatedAt: burst,
		}); err != nil {
			t.Fatalf("seed C's burst row %s: %v", phase, err)
		}
	}
	for i := 0; i < 302; i++ {
		if _, err := c.store.CreateWorkflowCheckpoint(c.ctx, domain.WorkflowCheckpoint{
			ID: fmt.Sprintf("wfc-c-observed-%03d", i), WorkflowRunID: c.grandRunID, ProjectID: run.ProjectID,
			DurablePhase: "review_observed", NextAction: "verify",
			ReviewVerdict:  string(domain.VerdictApproved),
			PayloadVersion: "v1", RetryState: "{}",
			CreatedAt: burst.Add(time.Duration(i+1) * time.Second),
		}); err != nil {
			t.Fatalf("seed C's observation %d: %v", i, err)
		}
	}
	c.clk.Advance(305 * time.Second)
}

// attachSessionTo gives a step a real, terminated session: evidence that a
// worker ran, without a live writer.
func (c *chainCase) attachSessionTo(t *testing.T, step domain.WorkflowStep) domain.SessionID {
	t.Helper()
	now := c.clk.Now()
	rec, err := c.store.CreateSession(c.ctx, domain.SessionRecord{
		ProjectID: "agent-orchestrator", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.store.UpdateWorkflowStepSession(c.ctx, step.ID, string(rec.ID), now); err != nil {
		t.Fatalf("attach session to %s step: %v", step.Kind, err)
	}
	touchSession(t, c.quiescenceCase, rec.ID, domain.ActivityIdle, now.Add(-time.Hour))
	return rec.ID
}

// clearCStop writes the one row that ends a stop's authority, so C's ledger no
// longer says why it is parked.
func (c *chainCase) clearCStop(t *testing.T) {
	t.Helper()
	run, _, err := c.store.GetWorkflowRun(c.ctx, c.grandRunID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(C): %v", err)
	}
	c.clk.Advance(time.Minute)
	if _, err := c.store.CreateWorkflowCheckpoint(c.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-c-cleared", WorkflowRunID: c.grandRunID, ProjectID: run.ProjectID,
		DurablePhase: "attention_cleared", NextAction: "resumed automatically",
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: c.clk.Now(),
	}); err != nil {
		t.Fatalf("seed C's clear: %v", err)
	}
}

// seedRunningReviewOnC binds C's review step to a reviewer that has not
// concluded: a verdict is outstanding and could still dispatch a fix.
func (c *chainCase) seedRunningReviewOnC(t *testing.T) {
	t.Helper()
	harness := domain.ReviewerHarness("codex")
	session := c.attachSessionTo(t, refreshStep(t, c.quiescenceCase, c.grandRunID, domain.WorkflowStepPlan))
	if err := c.store.UpsertReview(c.ctx, domain.Review{
		ID: "rev-chain-c", SessionID: session, ProjectID: "agent-orchestrator",
		Harness: harness, CreatedAt: c.clk.Now(), UpdatedAt: c.clk.Now(),
	}); err != nil {
		t.Fatalf("UpsertReview: %v", err)
	}
	if err := c.store.InsertReviewRun(c.ctx, domain.ReviewRun{
		ID: "rr-chain-c", ReviewID: "rev-chain-c", SessionID: session, Harness: harness,
		TargetSHA: "cafe", Status: domain.ReviewRunRunning, CreatedAt: c.clk.Now(),
	}); err != nil {
		t.Fatalf("InsertReviewRun: %v", err)
	}
	step := refreshStep(t, c.quiescenceCase, c.grandRunID, domain.WorkflowStepReview)
	if _, err := c.store.SetWorkflowStepReviewRun(c.ctx, step.ID, "rr-chain-c", c.clk.Now()); err != nil {
		t.Fatalf("bind C's review run: %v", err)
	}
}

// stopReasonOf reports what every surface says about a run's stop.
func stopReasonOf(t *testing.T, q *quiescenceCase, runID string) string {
	t.Helper()
	run, ok, err := q.store.GetWorkflowRun(q.ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): %v", runID, err)
	}
	disp, err := q.c.ClassifyLockOwner(q.ctx, run)
	if err != nil {
		t.Fatalf("ClassifyLockOwner(%s): %v", runID, err)
	}
	return disp.Reason
}

// ---------------------------------------------------------------------------
// The real incident, end to end: a stop buried under 302 observations, a chain
// two repairs deep, and no human anywhere.
// ---------------------------------------------------------------------------

func TestABuriedStopStillUnwindsTheWholeCessionChain(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{buriedStopOnC: true})

	// Precondition: C is parked on the reason it really stopped for, not on the
	// newest thing that happened to it.
	if got := stopReasonOf(t, c.quiescenceCase, c.grandRunID); got != workflowcore.ReasonReviewerLaunchFailed {
		t.Fatalf("C's stop reads %q, want %q", got, workflowcore.ReasonReviewerLaunchFailed)
	}
	if got := c.holder(t); got != c.grandRunID {
		t.Fatalf("fixture precondition: branch is held by %s, want C (%s)", got, c.grandRunID)
	}

	// Reconcile #1: C -> B.
	c.reconcileOnly(t)
	// The fold ran on the buried stop, not on some fresh stop C acquired along
	// the way — without this the test could pass for the wrong reason.
	if got := stopReasonOf(t, c.quiescenceCase, c.grandRunID); got != workflowcore.ReasonReviewerLaunchFailed {
		t.Fatalf("C's stop reads %q after reconciling, want the buried %q",
			got, workflowcore.ReasonReviewerLaunchFailed)
	}
	if got := c.holder(t); got != c.repairRunID {
		t.Fatalf("after reconcile 1 the branch is held by %s, want B (%s)", got, c.repairRunID)
	}
	if n := c.folds(t, c.repairRunID, "branch_lock_returned_from_repair"); n != 1 {
		t.Fatalf("C -> B fold records = %d, want exactly 1", n)
	}

	// Reconcile #2: B -> A.
	c.reconcileOnly(t)
	if got := c.holder(t); got != c.runID {
		t.Fatalf("after reconcile 2 the branch is held by %s, want A (%s)", got, c.runID)
	}
	if n := c.folds(t, c.runID, "branch_custody_returned_to_origin"); n != 1 {
		t.Fatalf("B -> A fold records = %d, want exactly 1", n)
	}

	// And nothing a person did: no cancel, no continue, no new generation.
	for _, id := range []string{c.runID, c.repairRunID, c.grandRunID} {
		run, _, err := c.store.GetWorkflowRun(c.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if run.State == domain.WorkflowRunCancelled {
			t.Fatalf("run %s was cancelled; the chain must unwind without one", id)
		}
	}
	if n := c.countPhase("workflow_repair_dispatched"); n != 2 {
		t.Fatalf("repair dispatches = %d, want still 2", n)
	}
	if view := c.chainView(t); view != nil {
		t.Fatalf("the origin still reports a cession chain: %+v", view)
	}
}

// A reviewer that has not concluded is an outstanding verdict that could still
// dispatch a fix into C's own worktree. The buried stop changes nothing about
// that: the chain must not fold.
func TestAnOutstandingVerdictKeepsTheBranchEvenWithAReadableStop(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{buriedStopOnC: true, runningReviewOnC: true})

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if got := c.holder(t); got != c.grandRunID {
		t.Fatalf("branch holder = %s, want C (%s): an outstanding verdict is a live obligation", got, c.grandRunID)
	}
	view := c.chainView(t)
	if view == nil || view.Returnable {
		t.Fatalf("chain reports returnable while a verdict is outstanding: %+v", view)
	}
	if view.BlockedReason != "holder_can_still_write" {
		t.Fatalf("blockedReason = %q, want holder_can_still_write (%s)", view.BlockedReason, view.Detail)
	}
}

// A ledger whose stop was cleared no longer says why the run is parked, and an
// unreadable stop is a refusal — never a guess, and never a fold.
func TestAClearedStopLeavesTheChainFailClosed(t *testing.T) {
	c := newChainCaseWith(t, chainOptions{buriedStopOnC: true, clearedStopOnC: true})

	c.reconcileOnly(t)
	c.reconcileOnly(t)

	if got := c.holder(t); got != c.grandRunID {
		t.Fatalf("branch holder = %s, want C (%s): an unreadable stop must fail closed", got, c.grandRunID)
	}
	if n := c.folds(t, c.repairRunID, "branch_lock_returned_from_repair"); n != 0 {
		t.Fatalf("a fold was recorded (%d) for a run whose stop AO cannot read", n)
	}
}
