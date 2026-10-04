package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// capacity_store_test.go — P1-C's bounds, at the layer that actually enforces
// them.
//
// These are the tests that matter most for the scheduler's correctness,
// because the grant is a single SQL statement: if its bound is mistyped or
// mis-ordered, every higher-level test still passes (nothing ever queues) and
// the machine quietly oversubscribes. One of them caught exactly that — sqlc
// inferred the three limit parameters as TEXT, and in SQLite `COUNT(*) < 'x'`
// is always true, so every limit was silently disabled.

func capacityFixture(t *testing.T) (*sqlite.Store, context.Context) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return store, ctx
}

// seedRun creates a real workflow run, because capacity_claims is FK-bound to
// workflow_runs and a claim for a run that does not exist is not a state the
// scheduler can be in.
func seedRun(t *testing.T, store *sqlite.Store, id string) domain.WorkflowRun {
	t.Helper()
	now := time.Now().UTC()
	run := domain.WorkflowRun{
		ID: id, ProjectID: "p", Objective: "o", State: domain.WorkflowRunRunning,
		PolicyVersion: "v1", PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now,
	}
	created, _, err := store.CreateWorkflowRun(context.Background(), run, nil)
	if err != nil {
		t.Fatalf("CreateWorkflowRun(%s): %v", id, err)
	}
	return created
}

func claimFor(runID, key string, kind domain.ExecutionKind, generation int64, at time.Time) domain.CapacityClaim {
	return domain.CapacityClaim{
		ID: "cap-" + key, Kind: kind, State: domain.CapacityClaimQueued,
		WorkflowRunID: runID, WorkflowStepID: "step-" + key, LifecycleGeneration: generation,
		DispatchKey: key, ProjectID: "p", Priority: domain.PriorityForKind(kind),
		EnqueuedAt: at, UpdatedAt: at,
	}
}

// enqueueAndAcquire is the whole scheduler protocol in one helper.
func enqueueAndAcquire(t *testing.T, store *sqlite.Store, runID, key string, kind domain.ExecutionKind, generation int64, limits domain.CapacityLimits) bool {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.EnqueueCapacityClaim(ctx, claimFor(runID, key, kind, generation, now)); err != nil {
		t.Fatalf("enqueue %s: %v", key, err)
	}
	granted, err := store.AcquireCapacity(ctx, key, generation, limits, kind, now)
	if err != nil {
		t.Fatalf("acquire %s: %v", key, err)
	}
	return granted
}

// Matrix 11/12/13/14/15: every configured bound actually binds.
//
// The per-kind cases are run against a deliberately generous global limit, so
// a failure can only mean the per-kind bound was not applied.
func TestCapacityLimitsAreEnforced(t *testing.T) {
	t.Run("global limit", func(t *testing.T) {
		store, _ := capacityFixture(t)
		limits := domain.CapacityLimits{Global: 2, PerWorkflow: 99,
			PerKind: map[domain.ExecutionKind]int{domain.ExecutionKindWorker: 99}}
		for i := 0; i < 4; i++ {
			run := seedRun(t, store, fmt.Sprintf("wf-g%d", i))
			granted := enqueueAndAcquire(t, store, run.ID, fmt.Sprintf("k%d", i), domain.ExecutionKindWorker, 1, limits)
			if want := i < 2; granted != want {
				t.Fatalf("acquire %d granted=%v, want %v under a global limit of 2", i, granted, want)
			}
		}
	})

	for _, kind := range domain.ExecutionKinds() {
		t.Run(string(kind)+" limit", func(t *testing.T) {
			store, _ := capacityFixture(t)
			limits := domain.CapacityLimits{Global: 99, PerWorkflow: 99,
				PerKind: map[domain.ExecutionKind]int{kind: 1}}
			first := seedRun(t, store, "wf-"+string(kind)+"-1")
			second := seedRun(t, store, "wf-"+string(kind)+"-2")
			if !enqueueAndAcquire(t, store, first.ID, "a-"+string(kind), kind, 1, limits) {
				t.Fatalf("the first %s was refused under a limit of 1", kind)
			}
			if enqueueAndAcquire(t, store, second.ID, "b-"+string(kind), kind, 1, limits) {
				t.Fatalf("a second %s was granted under a per-kind limit of 1", kind)
			}
			// A DIFFERENT kind is unaffected: the bounds are independent.
			other := domain.ExecutionKindWorker
			if kind == domain.ExecutionKindWorker {
				other = domain.ExecutionKindReviewer
			}
			third := seedRun(t, store, "wf-"+string(kind)+"-3")
			if !enqueueAndAcquire(t, store, third.ID, "c-"+string(kind), other, 1, limits) {
				t.Fatalf("a %s was refused by the %s limit", other, kind)
			}
		})
	}

	// Matrix 16/17: the per-workflow bound is the fairness rule. One workflow
	// cannot occupy the machine, and another workflow still gets in.
	t.Run("per-workflow limit leaves room for another workflow", func(t *testing.T) {
		store, _ := capacityFixture(t)
		limits := domain.CapacityLimits{Global: 8, PerWorkflow: 2,
			PerKind: map[domain.ExecutionKind]int{domain.ExecutionKindWorker: 8}}
		greedy := seedRun(t, store, "wf-greedy")
		for i := 0; i < 2; i++ {
			if !enqueueAndAcquire(t, store, greedy.ID, fmt.Sprintf("greedy%d", i), domain.ExecutionKindWorker, 1, limits) {
				t.Fatalf("the greedy workflow was refused slot %d of its own budget", i)
			}
		}
		if enqueueAndAcquire(t, store, greedy.ID, "greedy2", domain.ExecutionKindWorker, 1, limits) {
			t.Fatal("one workflow took a third slot under a per-workflow limit of 2")
		}
		other := seedRun(t, store, "wf-other")
		if !enqueueAndAcquire(t, store, other.ID, "other0", domain.ExecutionKindWorker, 1, limits) {
			t.Fatal("a second workflow was starved while the machine had five free slots")
		}
	})
}

// Matrix 5/9/10: the grant and the release are both CAS'd on identity, so a
// repeat is a no-op and a stale generation can do neither.
func TestCapacityClaimIdentityAndGenerationFencing(t *testing.T) {
	store, ctx := capacityFixture(t)
	limits := domain.DefaultCapacityLimits()
	run := seedRun(t, store, "wf-1")
	now := time.Now().UTC()

	// 10: enqueueing the same intent twice creates one claim.
	created, err := store.EnqueueCapacityClaim(ctx, claimFor(run.ID, "key-1", domain.ExecutionKindWorker, 3, now))
	if err != nil || !created {
		t.Fatalf("first enqueue: created=%v err=%v", created, err)
	}
	again, err := store.EnqueueCapacityClaim(ctx, claimFor(run.ID, "key-1", domain.ExecutionKindWorker, 3, now))
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Fatal("the same launch intent produced a second claim")
	}
	claims, err := store.ListCapacityClaimsForRun(ctx, run.ID)
	if err != nil || len(claims) != 1 {
		t.Fatalf("run holds %d claims, want exactly 1: %v", len(claims), err)
	}

	// 8: a stale generation cannot claim.
	if granted, gerr := store.AcquireCapacity(ctx, "key-1", 2, limits, domain.ExecutionKindWorker, now); gerr != nil || granted {
		t.Fatalf("a stale generation acquired capacity: granted=%v err=%v", granted, gerr)
	}
	if granted, gerr := store.AcquireCapacity(ctx, "key-1", 3, limits, domain.ExecutionKindWorker, now); gerr != nil || !granted {
		t.Fatalf("the current generation could not acquire: granted=%v err=%v", granted, gerr)
	}
	// Acquiring twice does not double-charge: the claim is already held, so
	// the CAS (state='queued') matches nothing.
	if granted, _ := store.AcquireCapacity(ctx, "key-1", 3, limits, domain.ExecutionKindWorker, now); granted {
		t.Fatal("an already-held claim was granted a second time")
	}

	// 9: a stale generation cannot release the newer claim.
	if released, rerr := store.ReleaseCapacityClaim(ctx, "key-1", 2, "stale", now); rerr != nil || released {
		t.Fatalf("a stale generation released a newer claim: released=%v err=%v", released, rerr)
	}
	held, err := store.ListHeldCapacityClaims(ctx)
	if err != nil || len(held) != 1 {
		t.Fatalf("held claims = %d, want 1 after a refused stale release: %v", len(held), err)
	}

	// 5: release is idempotent.
	if released, _ := store.ReleaseCapacityClaim(ctx, "key-1", 3, "done", now); !released {
		t.Fatal("the current generation could not release its own claim")
	}
	if released, _ := store.ReleaseCapacityClaim(ctx, "key-1", 3, "done again", now); released {
		t.Fatal("a duplicate release freed a second slot")
	}
	held, err = store.ListHeldCapacityClaims(ctx)
	if err != nil || len(held) != 0 {
		t.Fatalf("held claims = %d after release, want 0: %v", len(held), err)
	}
	// The released row is retained as evidence of what held the slot.
	claims, err = store.ListCapacityClaimsForRun(ctx, run.ID)
	if err != nil || len(claims) != 1 || claims[0].State != domain.CapacityClaimReleased {
		t.Fatalf("released claim was not retained: %+v (%v)", claims, err)
	}
	if claims[0].ReleaseReason == "" || claims[0].ReleasedAt == nil {
		t.Fatalf("a released claim must say when and why: %+v", claims[0])
	}
}

// Matrix 4: releasing a slot lets queued work through. Deterministic order:
// priority, then age.
func TestQueueOrderIsPriorityThenAge(t *testing.T) {
	store, ctx := capacityFixture(t)
	limits := domain.CapacityLimits{Global: 1, PerWorkflow: 9,
		PerKind: map[domain.ExecutionKind]int{domain.ExecutionKindWorker: 9, domain.ExecutionKindRepair: 9}}
	base := time.Now().UTC()
	holder := seedRun(t, store, "wf-holder")
	if !enqueueAndAcquire(t, store, holder.ID, "held", domain.ExecutionKindWorker, 1, limits) {
		t.Fatal("the first claim was refused")
	}

	// Two ordinary workers, oldest first, and a repair enqueued last. The
	// repair's priority boost must put it ahead of both.
	older := seedRun(t, store, "wf-older")
	newer := seedRun(t, store, "wf-newer")
	repairRun := seedRun(t, store, "wf-repair")
	for _, seed := range []struct {
		run  string
		key  string
		kind domain.ExecutionKind
		at   time.Time
	}{
		{older.ID, "older", domain.ExecutionKindWorker, base},
		{newer.ID, "newer", domain.ExecutionKindWorker, base.Add(time.Minute)},
		{repairRun.ID, "repair", domain.ExecutionKindRepair, base.Add(2 * time.Minute)},
	} {
		if _, err := store.EnqueueCapacityClaim(ctx, claimFor(seed.run, seed.key, seed.kind, 1, seed.at)); err != nil {
			t.Fatal(err)
		}
		if granted, _ := store.AcquireCapacity(ctx, seed.key, 1, limits, seed.kind, seed.at); granted {
			t.Fatalf("%s was granted while the only slot was held", seed.key)
		}
	}

	queued, err := store.ListQueuedCapacityClaims(ctx, 10)
	if err != nil || len(queued) != 3 {
		t.Fatalf("queued = %d, want 3: %v", len(queued), err)
	}
	if got := []string{queued[0].DispatchKey, queued[1].DispatchKey, queued[2].DispatchKey}; got[0] != "repair" || got[1] != "older" || got[2] != "newer" {
		t.Fatalf("queue order = %v, want repair (priority) then older then newer (age)", got)
	}

	// 4: freeing the slot lets the front of the queue in.
	if released, _ := store.ReleaseCapacityClaim(ctx, "held", 1, "finished", base); !released {
		t.Fatal("could not release the held slot")
	}
	if granted, _ := store.AcquireCapacity(ctx, "repair", 1, limits, domain.ExecutionKindRepair, base); !granted {
		t.Fatal("the front of the queue was not admitted after a slot was freed")
	}
}

// Matrix 22: a terminal run cannot retain capacity, whatever generation its
// claims were taken under.
func TestTerminalRunReleaseIsUnconditional(t *testing.T) {
	store, ctx := capacityFixture(t)
	limits := domain.DefaultCapacityLimits()
	run := seedRun(t, store, "wf-term")
	now := time.Now().UTC()
	for i, key := range []string{"a", "b"} {
		if _, err := store.EnqueueCapacityClaim(ctx, claimFor(run.ID, key, domain.ExecutionKindWorker, int64(i+1), now)); err != nil {
			t.Fatal(err)
		}
		if granted, _ := store.AcquireCapacity(ctx, key, int64(i+1), limits, domain.ExecutionKindWorker, now); !granted {
			t.Fatalf("claim %s was refused", key)
		}
	}
	n, err := store.ReleaseCapacityClaimsForRun(ctx, run.ID, "run cancelled", now)
	if err != nil || n != 2 {
		t.Fatalf("released %d claims, want 2: %v", n, err)
	}
	held, err := store.ListHeldCapacityClaims(ctx)
	if err != nil || len(held) != 0 {
		t.Fatalf("a terminal run still holds %d slots: %v", len(held), err)
	}
	// Idempotent: a second sweep frees nothing more.
	if n, _ := store.ReleaseCapacityClaimsForRun(ctx, run.ID, "again", now); n != 0 {
		t.Fatalf("a repeated terminal release freed %d more claims", n)
	}
}

// Matrix 6/7/23: the queue and every claim's identity are durable, so a
// restart reconstructs them exactly. Reopening the store IS the restart.
func TestCapacityStateSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	store := sqlitetest.MustOpenAt(t, dir)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	limits := domain.CapacityLimits{Global: 1, PerWorkflow: 9,
		PerKind: map[domain.ExecutionKind]int{domain.ExecutionKindWorker: 9}}
	now := time.Now().UTC()
	holder := seedRun(t, store, "wf-hold")
	waiter := seedRun(t, store, "wf-wait")
	if !enqueueAndAcquire(t, store, holder.ID, "hold", domain.ExecutionKindWorker, 7, limits) {
		t.Fatal("the holder was refused")
	}
	if _, err := store.BindCapacityClaimRuntime(ctx, "hold", "ao-worker-1", "$42", 7, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueCapacityClaim(ctx, claimFor(waiter.ID, "wait", domain.ExecutionKindWorker, 1, now)); err != nil {
		t.Fatal(err)
	}

	// The restart: a second Store over the SAME database file, opened the way
	// the daemon opens it. Nothing is carried across in memory.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dir)
	if err != nil {
		t.Fatalf("reopen the store the way a restarted daemon does: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	held, err := reopened.ListHeldCapacityClaims(ctx)
	if err != nil || len(held) != 1 {
		t.Fatalf("after restart: held = %d, want 1 (%v)", len(held), err)
	}
	// 7/23: the claim's identity, generation and bound runtime all survive.
	if held[0].DispatchKey != "hold" || held[0].LifecycleGeneration != 7 {
		t.Fatalf("claim identity changed across restart: %+v", held[0])
	}
	if held[0].RuntimeInstanceID != "$42" || held[0].RuntimeHandle != "ao-worker-1" {
		t.Fatalf("the runtime the claim paid for was lost: %+v", held[0])
	}
	// 6: the queue is reconstructed, not forgotten.
	queued, err := reopened.ListQueuedCapacityClaims(ctx, 10)
	if err != nil || len(queued) != 1 || queued[0].DispatchKey != "wait" {
		t.Fatalf("after restart: queued = %+v (%v)", queued, err)
	}
	// And the restarted daemon still refuses to oversubscribe.
	if granted, _ := reopened.AcquireCapacity(ctx, "wait", 1, limits, domain.ExecutionKindWorker, now); granted {
		t.Fatal("a restarted daemon granted a slot over its own limit")
	}
}

// A partially configured limit set must read as a default, never as a
// scheduler that grants nothing.
func TestZeroLimitsNormalizeToDefaultsRatherThanDeadlock(t *testing.T) {
	store, _ := capacityFixture(t)
	run := seedRun(t, store, "wf-zero")
	if !enqueueAndAcquire(t, store, run.ID, "z", domain.ExecutionKindWorker, 1, domain.CapacityLimits{}) {
		t.Fatal("an unconfigured limit set refused every launch; a misconfiguration must degrade to the defaults")
	}
}
