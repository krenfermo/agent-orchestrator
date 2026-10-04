package wfmemory_test

import (
	stdctx "context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory/wfmemory"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// nil_provisioner_test.go — the typed-nil hazard at this package's boundary.
//
// Instrument takes an INTERFACE. A caller holding a nil *projectmemory.Provisioner
// -- which is what the composition root has whenever memory is switched off, the
// default -- produces a non-nil interface wrapping a nil pointer if it assigns it
// directly. Instrument's `prov == nil` guard is then false, every dispatch surface
// gets wrapped, and the first worker spawn dereferences the nil pointer inside
// Provision: a panic on the spawn path of a daemon whose memory is simply off.
//
// The composition root converts the nil explicitly (memoryProvisionerFor). This
// test pins the property that makes that conversion the right fix: a genuinely
// absent provisioner leaves the dependencies untouched, so nothing can be
// dereferenced later.

// stubProvisioner is a genuinely present provisioner, so the inverse case
// exercises Instrument's wrapping rather than its guard.
type stubProvisioner struct{}

func (stubProvisioner) Provision(stdctx.Context, projectmemory.ProvisionRequest) projectmemory.Provisioned {
	return projectmemory.Provisioned{}
}

type nilGuardSpawner struct{ called bool }

func (s *nilGuardSpawner) Spawn(stdctx.Context, ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	s.called = true
	return domain.SessionRecord{ID: "s-1"}, 0, 0, nil
}

func TestInstrumentLeavesDepsUntouchedWithoutAProvisioner(t *testing.T) {
	spawner := &nilGuardSpawner{}
	deps := workflowcore.Deps{Spawner: spawner}

	out := wfmemory.Instrument(deps, nil, nil)

	if out.Spawner != workflowcore.Spawner(spawner) {
		t.Fatal("Instrument wrapped the spawner despite having no provisioner")
	}
	// And the unwrapped spawner still spawns, rather than panicking inside a
	// wrapper that has nothing to provision from.
	if _, _, _, err := out.Spawner.Spawn(stdctx.Background(), ports.SpawnConfig{}); err != nil {
		t.Fatalf("spawn through the untouched dependency failed: %v", err)
	}
	if !spawner.called {
		t.Fatal("the spawn did not reach the real spawner")
	}
}

// The inverse, so the guard cannot be "fixed" by making Instrument a no-op: a
// real provisioner is still attached.
func TestInstrumentWrapsWithARealProvisioner(t *testing.T) {
	spawner := &nilGuardSpawner{}
	deps := workflowcore.Deps{Spawner: spawner}
	out := wfmemory.Instrument(deps, stubProvisioner{}, nil)

	if out.Spawner == workflowcore.Spawner(spawner) {
		t.Fatal("Instrument left the spawner unwrapped despite having a provisioner")
	}
}
