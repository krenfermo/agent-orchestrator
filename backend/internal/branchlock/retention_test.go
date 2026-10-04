package branchlock_test

import (
	"context"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// fakeClassifier answers for the runs a test has explicitly described. A run it
// was never told about comes back unclassified, which is the same thing the
// real coordinator reports for a stop nothing durable named.
type fakeClassifier struct {
	mu    sync.Mutex
	byRun map[string]branchlock.OwnerDisposition
	calls int
}

func (f *fakeClassifier) ClassifyLockOwner(_ context.Context, run domain.WorkflowRun) (branchlock.OwnerDisposition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	disp, ok := f.byRun[run.ID]
	if !ok {
		return branchlock.OwnerDisposition{Reason: "unclassified_stop"}, nil
	}
	return disp, nil
}

func newClassifiedManager(t *testing.T, store *sqlite.Store, owner string, byRun map[string]branchlock.OwnerDisposition) (*branchlock.Manager, *fakeClassifier) {
	t.Helper()
	mgr := newManager(t, store, &fakePreflight{}, owner)
	cls := &fakeClassifier{byRun: byRun}
	mgr.SetClassifier(cls)
	return mgr, cls
}

func heldRunIDs(t *testing.T, store *sqlite.Store) []string {
	t.Helper()
	locks, err := store.ListHeldBranchLocks(context.Background())
	if err != nil {
		t.Fatalf("list held: %v", err)
	}
	out := make([]string, 0, len(locks))
	for _, l := range locks {
		out = append(out, l.WorkflowRunID)
	}
	return out
}

// The deadlock this checkpoint exists to end: a workflow parked in
// needs_attention on a decision only a human can make, which has nothing in the
// working tree to protect, must not keep the branch.
func TestReconcileReleasesLockOfPermanentlyStoppedOwnerWithNothingToProtect(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	ctx := context.Background()
	stopped := seedRun(t, store, "proj", domain.WorkflowRunNeedsAttention)

	previous := newManager(t, store, &fakePreflight{}, "owner-previous")
	mustAcquireFor(t, previous, "proj", stopped)

	current, cls := newClassifiedManager(t, store, "owner-current", map[string]branchlock.OwnerDisposition{
		stopped: {Reason: "fix_budget_exhausted"},
	})
	result, err := current.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Released != 1 {
		t.Fatalf("reconcile = %#v, want the stranded lock released", result)
	}
	if cls.calls == 0 {
		t.Fatal("the owner's stop was never classified: the decision was not evidence-based")
	}
	if ids := heldRunIDs(t, store); len(ids) != 0 {
		t.Fatalf("still held by %v, want the branch free", ids)
	}
	// And the branch is genuinely takeable now.
	if _, err := current.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-next"}); err != nil {
		t.Fatalf("acquire after recovery: %v", err)
	}
}

// The opposite direction, and the reason this is not simply "release every
// needs_attention lock": a stopped run that already wrote to the branch keeps
// it, because a second workflow writing on top of that work is worse than a
// wait.
func TestReconcileKeepsLockProtectingUncommittedWork(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	ctx := context.Background()
	stopped := seedRun(t, store, "proj", domain.WorkflowRunNeedsAttention)

	previous := newManager(t, store, &fakePreflight{}, "owner-previous")
	mustAcquireFor(t, previous, "proj", stopped)

	current, _ := newClassifiedManager(t, store, "owner-current", map[string]branchlock.OwnerDisposition{
		stopped: {ProtectsWork: true, Reason: "fix_budget_exhausted"},
	})
	result, err := current.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Released != 0 || result.Adopted != 1 {
		t.Fatalf("reconcile = %#v, want the lock kept and adopted by this instance", result)
	}
	if _, err := current.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-other"}); !branchlock.IsConflict(err) {
		t.Fatalf("acquire err = %v, want the protected branch still exclusive", err)
	}
}

// A stop AO is going to resume by itself is not stale at all — releasing it
// would hand the branch away from a run that is about to write to it again.
func TestReconcileKeepsLockOfSelfRemediableOwner(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	stopped := seedRun(t, store, "proj", domain.WorkflowRunNeedsAttention)

	previous := newManager(t, store, &fakePreflight{}, "owner-previous")
	mustAcquireFor(t, previous, "proj", stopped)

	current, _ := newClassifiedManager(t, store, "owner-current", map[string]branchlock.OwnerDisposition{
		stopped: {SelfRemediable: true, Reason: "planner_retry_scheduled"},
	})
	result, err := current.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Released != 0 || result.Adopted != 1 {
		t.Fatalf("reconcile = %#v, want the retrying run's branch kept", result)
	}
}

// With no classifier wired at all — the pre-8P-E.13A shape, and any daemon that
// fails to build one — a stopped owner keeps its lock. Failing toward "hold" is
// the only safe direction: a wrongly released lock can corrupt work, a wrongly
// held one only delays it.
func TestReconcileWithoutAClassifierKeepsStoppedOwnersLock(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	stopped := seedRun(t, store, "proj", domain.WorkflowRunNeedsAttention)

	previous := newManager(t, store, &fakePreflight{}, "owner-previous")
	mustAcquireFor(t, previous, "proj", stopped)

	current := newManager(t, store, &fakePreflight{}, "owner-current")
	result, err := current.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Released != 0 {
		t.Fatalf("reconcile = %#v, want an unclassifiable stop to keep its lock", result)
	}
}

// RecoverStale is the online path: the run that actually needs the branch
// reclaims it mid-daemon-lifetime, without a restart.
func TestRecoverStaleFreesOnlyTheLocksThatAreActuallyStale(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	seedDirectProject(t, store, "proj-2", "/repos/other", "main")
	ctx := context.Background()

	strandedRun := seedRun(t, store, "proj", domain.WorkflowRunNeedsAttention)
	protectedRun := seedRun(t, store, "proj-2", domain.WorkflowRunNeedsAttention)
	mgr, _ := newClassifiedManager(t, store, "owner-1", map[string]branchlock.OwnerDisposition{
		strandedRun:  {Reason: "fix_budget_exhausted"},
		protectedRun: {ProtectsWork: true, Reason: "fix_budget_exhausted"},
	})
	mustAcquireFor(t, mgr, "proj", strandedRun)
	mustAcquireFor(t, mgr, "proj-2", protectedRun)

	freed, err := mgr.RecoverStale(ctx, strandedRun)
	if err != nil {
		t.Fatalf("recover stranded: %v", err)
	}
	if freed != 1 {
		t.Fatalf("freed = %d, want the stranded lock released", freed)
	}
	kept, err := mgr.RecoverStale(ctx, protectedRun)
	if err != nil {
		t.Fatalf("recover protected: %v", err)
	}
	if kept != 0 {
		t.Fatalf("freed = %d for a lock protecting work, want 0", kept)
	}
	if ids := heldRunIDs(t, store); len(ids) != 1 || ids[0] != protectedRun {
		t.Fatalf("held = %v, want only the protected lock", ids)
	}
}

// A live run's lock is never touched by recovery, whatever anyone claims about
// it: liveness is decided by the run row, not by the classifier.
func TestRecoverStaleNeverTouchesALiveRunsLock(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	live := seedRun(t, store, "proj", domain.WorkflowRunRunning)

	mgr, _ := newClassifiedManager(t, store, "owner-1", map[string]branchlock.OwnerDisposition{
		live: {Reason: "fix_budget_exhausted"},
	})
	mustAcquireFor(t, mgr, "proj", live)

	freed, err := mgr.RecoverStale(context.Background(), live)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if freed != 0 {
		t.Fatalf("freed = %d, want a live run's branch left alone", freed)
	}
}

// A terminal or missing owner is stale under RecoverStale exactly as it is
// under boot reconciliation — one policy, two entry points.
func TestRecoverStaleReleasesTerminalAndMissingOwners(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	ctx := context.Background()
	cancelled := seedRun(t, store, "proj", domain.WorkflowRunCancelled)

	mgr, _ := newClassifiedManager(t, store, "owner-1", nil)
	mustAcquireRaw(t, store, "/repos/ao", "main", cancelled, "owner-1")
	mustAcquireRaw(t, store, "/repos/gone", "main", "WF-does-not-exist", "owner-1")

	if freed, err := mgr.RecoverStale(ctx, cancelled); err != nil || freed != 1 {
		t.Fatalf("recover cancelled = (%d, %v), want 1 freed", freed, err)
	}
	if freed, err := mgr.RecoverStale(ctx, "WF-does-not-exist"); err != nil || freed != 1 {
		t.Fatalf("recover missing = (%d, %v), want 1 freed", freed, err)
	}
	if ids := heldRunIDs(t, store); len(ids) != 0 {
		t.Fatalf("held = %v, want nothing left", ids)
	}
}

// Recovery must not open a hole in the exclusivity guarantee: releasing a stale
// lock while several runs race for the freed branch still produces exactly one
// winner.
func TestConcurrentRecoveryAndAcquireStillProducesOneOwner(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, "proj", "/repos/ao", "main")
	ctx := context.Background()
	stranded := seedRun(t, store, "proj", domain.WorkflowRunNeedsAttention)

	mgr, _ := newClassifiedManager(t, store, "owner-1", map[string]branchlock.OwnerDisposition{
		stranded: {Reason: "fix_budget_exhausted"},
	})
	mustAcquireFor(t, mgr, "proj", stranded)

	const contenders = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	wg.Add(contenders)
	for i := 0; i < contenders; i++ {
		go func(i int) {
			defer wg.Done()
			if _, err := mgr.RecoverStale(ctx, stranded); err != nil {
				return
			}
			locks, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-contender"})
			if err != nil || len(locks) == 0 {
				return
			}
			mu.Lock()
			winners++
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	held, err := store.ListHeldBranchLocks(ctx)
	if err != nil {
		t.Fatalf("list held: %v", err)
	}
	if len(held) != 1 {
		t.Fatalf("held = %#v, want exactly one owner of the branch", held)
	}
	if winners == 0 {
		t.Fatal("nobody ever took the freed branch")
	}
}
