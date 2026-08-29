package workflow

// The second half of the Runtime GC production-wiring defect found alongside
// incident wf-170b16ce.
//
// bindRuntimesToClaims exists so a held capacity claim names the runtime it
// actually paid for. Its own doc comment calls that "GC's strongest candidate
// source, because it identifies an exact incarnation AO can prove it launched".
// It passed "" as the incarnation.
//
// runtimegc.Sweeper.claimCandidates skips every claim whose RuntimeInstanceID
// is empty — a name is a discovery key and never an authority key — so the
// claim-derived candidate source produced nothing in production, exactly as
// the session-derived one did. In the incident database, the worker claim for
// agent-orchestrator-51 carried runtime_handle='agent-orchestrator-51' and an
// empty runtime_instance_id, which is that defect on disk.

import (
	stdctx "context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// bindingCapacityStore records what BindCapacityClaimRuntime was actually
// asked to write. Only the one method is exercised; the rest satisfy the
// interface and must never be reached by this path.
type bindingCapacityStore struct {
	CapacityStore
	handles   []string
	instances []string
}

func (s *bindingCapacityStore) BindCapacityClaimRuntime(
	_ stdctx.Context, _, handle, instanceID string, _ int64, _ time.Time,
) (bool, error) {
	s.handles = append(s.handles, handle)
	s.instances = append(s.instances, instanceID)
	return true, nil
}

// bindingSessionFacts serves one session record by id.
type bindingSessionFacts struct {
	SessionFacts
	rec domain.SessionRecord
}

func (f *bindingSessionFacts) GetSession(_ stdctx.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	if id != f.rec.ID {
		return domain.SessionRecord{}, false, nil
	}
	return f.rec, true, nil
}

func bindingFixture(t *testing.T, rec domain.SessionRecord) (*Coordinator, *bindingCapacityStore) {
	t.Helper()
	claims := &bindingCapacityStore{}
	c := &Coordinator{
		capacity:     claims,
		sessionFacts: &bindingSessionFacts{rec: rec},
		clock:        func() time.Time { return time.Date(2026, 8, 29, 21, 7, 40, 0, time.UTC) },
	}
	return c, claims
}

func heldWorkerClaim(handle, instance string) domain.CapacityClaim {
	return domain.CapacityClaim{
		ID: "cap-b2dfa56e", Kind: domain.ExecutionKindWorker, State: domain.CapacityClaimHeld,
		WorkflowRunID: "wf-170b16ce", WorkflowStepID: "wfs-work", DispatchKey: "dk-1",
		RuntimeHandle: handle, RuntimeInstanceID: instance,
	}
}

func workerStep(sessionID string) []domain.WorkflowStep {
	return []domain.WorkflowStep{{ID: "wfs-work", Kind: domain.WorkflowStepWork, SessionID: &sessionID}}
}

// The incarnation, not just the name. This is the exact value that was ""
// in production and made every claim an unprovable GC candidate.
func TestBindRuntimesToClaimsRecordsTheIncarnation(t *testing.T) {
	c, claims := bindingFixture(t, domain.SessionRecord{
		ID: "agent-orchestrator-51",
		Metadata: domain.SessionMetadata{
			RuntimeHandleID:   "agent-orchestrator-51",
			RuntimeInstanceID: "$42",
			RuntimeLaunchID:   "launch-1",
		},
	})

	c.bindRuntimesToClaims(stdctx.Background(),
		[]domain.CapacityClaim{heldWorkerClaim("", "")},
		workerStep("agent-orchestrator-51"))

	if len(claims.instances) != 1 {
		t.Fatalf("BindCapacityClaimRuntime called %d times, want 1", len(claims.instances))
	}
	if claims.handles[0] != "agent-orchestrator-51" {
		t.Fatalf("handle = %q, want the session's runtime handle", claims.handles[0])
	}
	if claims.instances[0] != "$42" {
		t.Fatalf("instance id = %q, want $42: Runtime GC skips a claim with no incarnation, so an empty value here silently disables the whole claim-derived sweep", claims.instances[0])
	}
}

// A claim bound before the incarnation was known must be completed, not left
// half-written forever. Without this the fix would only help claims created
// after it, and every claim already on disk would stay unprovable.
func TestBindRuntimesToClaimsCompletesAHandleOnlyBinding(t *testing.T) {
	c, claims := bindingFixture(t, domain.SessionRecord{
		ID: "agent-orchestrator-51",
		Metadata: domain.SessionMetadata{
			RuntimeHandleID:   "agent-orchestrator-51",
			RuntimeInstanceID: "$42",
		},
	})

	c.bindRuntimesToClaims(stdctx.Background(),
		[]domain.CapacityClaim{heldWorkerClaim("agent-orchestrator-51", "")},
		workerStep("agent-orchestrator-51"))

	if len(claims.instances) != 1 || claims.instances[0] != "$42" {
		t.Fatalf("binding calls = %v, want the incarnation to be filled in", claims.instances)
	}
}

// A complete binding is left alone: no write per reconciliation pass.
func TestBindRuntimesToClaimsIsIdempotentOnceComplete(t *testing.T) {
	c, claims := bindingFixture(t, domain.SessionRecord{
		ID: "agent-orchestrator-51",
		Metadata: domain.SessionMetadata{
			RuntimeHandleID:   "agent-orchestrator-51",
			RuntimeInstanceID: "$42",
		},
	})

	bound := []domain.CapacityClaim{heldWorkerClaim("agent-orchestrator-51", "$42")}
	for i := 0; i < 3; i++ {
		c.bindRuntimesToClaims(stdctx.Background(), bound, workerStep("agent-orchestrator-51"))
	}
	if len(claims.instances) != 0 {
		t.Fatalf("rebound an already-complete claim %d times", len(claims.instances))
	}
}

// A session whose own row records no incarnation cannot lend one. The claim
// stays unprovable, which is the correct fail-closed answer rather than an
// invented identity.
func TestBindRuntimesToClaimsNeverInventsAnIncarnation(t *testing.T) {
	c, claims := bindingFixture(t, domain.SessionRecord{
		ID:       "agent-orchestrator-40",
		Metadata: domain.SessionMetadata{RuntimeHandleID: "agent-orchestrator-40"},
	})

	c.bindRuntimesToClaims(stdctx.Background(),
		[]domain.CapacityClaim{heldWorkerClaim("", "")},
		workerStep("agent-orchestrator-40"))

	for _, got := range claims.instances {
		if got != "" {
			t.Fatalf("instance id = %q, want empty: the session record proves no incarnation", got)
		}
	}
}
