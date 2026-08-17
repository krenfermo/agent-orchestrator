package workflow_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeRuntimeIsolation is a hand-rolled fake for workflowcore.RuntimeIsolation.
type fakeRuntimeIsolation struct {
	env   map[string]string
	owner domain.UserID
	err   error
	calls int
}

func (f *fakeRuntimeIsolation) Resolve(_ context.Context, _ string, _ domain.AgentHarness) (map[string]string, domain.UserID, error) {
	f.calls++
	return f.env, f.owner, f.err
}

func newCoordinatorWithRuntimeIsolation(spawner workflowcore.Spawner, isolation workflowcore.RuntimeIsolation) (*workflowcore.Coordinator, *fakeStore) {
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Spawner:          spawner,
		SessionFacts:     newFakeSessionFacts(),
		WorkspaceFacts:   &fakeWorkspaceFacts{},
		Clock:            clk.Now,
		RuntimeIsolation: isolation,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store
}

// TestWorkerSpawn_AppliesIsolatedRuntimeEnv is Checkpoint 8P-B.1's core
// worker-isolation proof: when RuntimeIsolation resolves an env override,
// it must reach ports.SpawnConfig.RuntimeEnv on the actual Spawn call.
func TestWorkerSpawn_AppliesIsolatedRuntimeEnv(t *testing.T) {
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}}
	isolation := &fakeRuntimeIsolation{env: map[string]string{"HOME": "/ao/users/user-a/runtime-home"}, owner: "user-a"}
	c, _ := newCoordinatorWithRuntimeIsolation(spawner, isolation)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if isolation.calls == 0 {
		t.Fatal("expected RuntimeIsolation.Resolve to be called before Spawn")
	}
	if spawner.lastCfg.RuntimeEnv["HOME"] != "/ao/users/user-a/runtime-home" {
		t.Fatalf("spawn cfg.RuntimeEnv = %v, want isolated HOME", spawner.lastCfg.RuntimeEnv)
	}
	if spawner.lastCfg.Owner != "user-a" {
		t.Fatalf("spawn cfg.Owner = %q, want user-a", spawner.lastCfg.Owner)
	}
}

// TestWorkerSpawn_TwoUsers_NeverShareRuntimeEnv proves two different
// workflow owners resolve to different, non-overlapping env overrides.
func TestWorkerSpawn_TwoUsers_NeverShareRuntimeEnv(t *testing.T) {
	spawnerA := &fakeSpawner{}
	isolationA := &fakeRuntimeIsolation{env: map[string]string{"HOME": "/ao/users/user-a/runtime-home"}, owner: "user-a"}
	cA, _ := newCoordinatorWithRuntimeIsolation(spawnerA, isolationA)

	spawnerB := &fakeSpawner{}
	isolationB := &fakeRuntimeIsolation{env: map[string]string{"HOME": "/ao/users/user-b/runtime-home"}, owner: "user-b"}
	cB, _ := newCoordinatorWithRuntimeIsolation(spawnerB, isolationB)

	ctx := context.Background()
	runA, err := cA.CreateRun(ctx, "proj-a", "task a")
	if err != nil {
		t.Fatalf("CreateRun A: %v", err)
	}
	if _, err := cA.StartRun(ctx, runA.Run.ID); err != nil {
		t.Fatalf("StartRun A: %v", err)
	}
	runB, err := cB.CreateRun(ctx, "proj-b", "task b")
	if err != nil {
		t.Fatalf("CreateRun B: %v", err)
	}
	if _, err := cB.StartRun(ctx, runB.Run.ID); err != nil {
		t.Fatalf("StartRun B: %v", err)
	}

	homeA := spawnerA.lastCfg.RuntimeEnv["HOME"]
	homeB := spawnerB.lastCfg.RuntimeEnv["HOME"]
	if homeA == "" || homeB == "" {
		t.Fatalf("expected both spawns to receive an isolated HOME, got A=%q B=%q", homeA, homeB)
	}
	if homeA == homeB {
		t.Fatalf("user A and B workers resolved to the same HOME: %s", homeA)
	}
}

// TestWorkerSpawn_BlockedNeverCallsSpawn proves a
// ports.ErrProviderProfileRequired from RuntimeIsolation stops the launch
// entirely -- Spawn (and therefore any real provider subprocess) is never
// invoked, and the run is left in needs_attention with an "auth"-classed
// failure rather than a misleading capacity/transient one.
func TestWorkerSpawn_BlockedNeverCallsSpawn(t *testing.T) {
	spawner := &fakeSpawner{}
	isolation := &fakeRuntimeIsolation{err: ports.ErrProviderProfileRequired, owner: "user-a"}
	c, _ := newCoordinatorWithRuntimeIsolation(spawner, isolation)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if spawner.calls != 0 {
		t.Fatalf("Spawn must never be called when the runtime env is blocked, got %d calls", spawner.calls)
	}
	if detail.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %v, want needs_attention", detail.Run.State)
	}
	work := workStepFrom(detail)
	if work.Step.State != domain.WorkflowStepFailed {
		t.Fatalf("work step state = %v, want failed", work.Step.State)
	}
}

// TestWorkerSpawn_NoRuntimeIsolation_PreservesPriorBehavior proves a nil
// RuntimeIsolation (every pre-8P-B.1 wiring) is a true no-op: Spawn still
// runs exactly as before, with no RuntimeEnv/Owner set.
func TestWorkerSpawn_NoRuntimeIsolation_PreservesPriorBehavior(t *testing.T) {
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}}
	c, _, _ := newCoordinatorFull(spawner, newFakeSessionFacts(), &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want 1", spawner.calls)
	}
	if spawner.lastCfg.RuntimeEnv != nil {
		t.Fatalf("expected nil RuntimeEnv with no RuntimeIsolation wired, got %v", spawner.lastCfg.RuntimeEnv)
	}
	if spawner.lastCfg.Owner != "" {
		t.Fatalf("expected empty Owner with no RuntimeIsolation wired, got %q", spawner.lastCfg.Owner)
	}
}
