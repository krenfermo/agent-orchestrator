package runtimegc_test

// ReclaimSessionRuntime: the proofs a terminal workflow's runtime must satisfy
// before it is ended.
//
// The policy that decides WHICH sessions get here lives in
// workflow/terminal_runtime.go and is tested there. This file is the other
// half: given that a run says it is finished, what does AO still refuse to do?
//
// The answers are the same three the periodic sweep gives, and deliberately so
// — this entry point exists precisely so that "prove it is mine, then destroy
// that incarnation" has one implementation rather than two that could drift.

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
)

const (
	reclaimSession = "agent-orchestrator-51"
	reclaimLaunch  = "launch-1"
	reclaimHandle  = "agent-orchestrator-51"
)

func reclaimToken() string {
	return domain.SessionRuntimeOwnerToken(reclaimSession, reclaimLaunch)
}

// ownedReclaimRequest is a well-formed request: every field is the durable
// fact a P1-D session row actually holds.
func ownedReclaimRequest(instance string) runtimegc.ReclaimRequest {
	return runtimegc.ReclaimRequest{
		SessionID: reclaimSession, LaunchID: reclaimLaunch,
		Handle: reclaimHandle, InstanceID: instance, OwnerToken: reclaimToken(),
		WorkflowRunID: "wf-170b16ce", Reason: "the workflow completed",
	}
}

// ownedRuntime is that session live on AO's server, carrying its marker.
func ownedRuntime(instance string) ports.RuntimeSessionSummary {
	return ports.RuntimeSessionSummary{
		ID: reclaimHandle, InstanceID: instance, Owner: reclaimToken(), OwnerKnown: true,
	}
}

func reclaimSweeper(rt *fakeRuntime, claims *fakeClaims) *runtimegc.Sweeper {
	return sweeper(rt, claims, &fakeRuns{runs: map[string]domain.WorkflowRun{}})
}

// The ordinary case: a terminal run's provably-owned runtime is ended, and the
// destroy is addressed to the incarnation.
func TestReclaimEndsAProvablyOwnedRuntime(t *testing.T) {
	rt := newFakeRuntime(ownedRuntime("$42"))
	f, err := reclaimSweeper(rt, &fakeClaims{}).
		ReclaimSessionRuntime(context.Background(), ownedReclaimRequest("$42"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition != runtimegc.DispositionCleaned {
		t.Fatalf("disposition = %s (%s), want cleaned", f.Disposition, f.Reason)
	}
	if rt.alive("$42") {
		t.Fatal("the runtime survived")
	}
}

// ---- 7/8: identity mismatches refuse ----------------------------------------

// The name now answers for a DIFFERENT incarnation. This is the ABA the whole
// model exists to exclude, and the answer is to do nothing at all.
func TestReclaimRefusesWhenTheIncarnationChanged(t *testing.T) {
	rt := newFakeRuntime(ownedRuntime("$99"))
	f, err := reclaimSweeper(rt, &fakeClaims{}).
		ReclaimSessionRuntime(context.Background(), ownedReclaimRequest("$42"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition == runtimegc.DispositionCleaned {
		t.Fatal("destroyed a runtime under a reused name")
	}
	if !rt.alive("$99") {
		t.Fatal("the replacement incarnation was destroyed")
	}
}

// The live runtime's marker contradicts the row's. Two records AO holds about
// one session disagree, and the only safe reading of a contradiction is that AO
// does not know what it is looking at.
func TestReclaimRefusesWhenTheOwnerTokenDisagrees(t *testing.T) {
	stranger := ports.RuntimeSessionSummary{
		ID: reclaimHandle, InstanceID: "$42",
		Owner: domain.SessionRuntimeOwnerToken(reclaimSession, "launch-9"), OwnerKnown: true,
	}
	rt := newFakeRuntime(stranger)
	f, err := reclaimSweeper(rt, &fakeClaims{}).
		ReclaimSessionRuntime(context.Background(), ownedReclaimRequest("$42"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition != runtimegc.DispositionUnprovable {
		t.Fatalf("disposition = %s, want unprovable", f.Disposition)
	}
	if !rt.alive("$42") {
		t.Fatal("destroyed a runtime whose live marker contradicted the recorded one")
	}
}

// ---- 9: a stale lifecycle generation cannot authorize a destroy -------------

// The recorded token proves session+launch. A request naming a DIFFERENT launch
// than the token was minted for is a stale generation speaking for a runtime it
// no longer owns.
func TestReclaimRefusesAStaleLifecycleGeneration(t *testing.T) {
	rt := newFakeRuntime(ownedRuntime("$42"))
	req := ownedReclaimRequest("$42")
	req.LaunchID = "launch-2" // the token was minted for launch-1

	f, err := reclaimSweeper(rt, &fakeClaims{}).ReclaimSessionRuntime(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition != runtimegc.DispositionUnprovable {
		t.Fatalf("disposition = %s, want unprovable", f.Disposition)
	}
	if !rt.alive("$42") {
		t.Fatal("a stale launch generation destroyed a live runtime")
	}
}

// ---- 6: legacy sessions are untouchable -------------------------------------

func TestReclaimRefusesALegacySessionWithNoRecordedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*runtimegc.ReclaimRequest)
	}{
		{"no incarnation", func(r *runtimegc.ReclaimRequest) { r.InstanceID = "" }},
		{"no ownership token", func(r *runtimegc.ReclaimRequest) { r.OwnerToken = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeRuntime(unowned(reclaimHandle, "$42"))
			req := ownedReclaimRequest("$42")
			tc.mutate(&req)

			f, err := reclaimSweeper(rt, &fakeClaims{}).ReclaimSessionRuntime(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if f.Disposition != runtimegc.DispositionUnprovable {
				t.Fatalf("disposition = %s, want unprovable", f.Disposition)
			}
			if f.Class != runtimegc.OrphanUnprovableOwnership {
				t.Fatalf("class = %s, want unprovable ownership", f.Class)
			}
			if !rt.alive("$42") {
				t.Fatal("a legacy session was destroyed")
			}
			// And the operator is told what to do, because AO never will.
			if f.RecommendedAction == "" {
				t.Fatal("a session AO will never reclaim carries no recommended action")
			}
		})
	}
}

// ---- capacity authority outranks a terminal run ------------------------------

// A HELD claim protects its runtime absolutely, and that does not weaken
// because a run says it is finished: the claim is the thing that could still be
// authorizing a mutation inside that runtime.
func TestReclaimRefusesWhileAHeldClaimStillPaysForTheRuntime(t *testing.T) {
	rt := newFakeRuntime(ownedRuntime("$42"))
	claims := &fakeClaims{held: []domain.CapacityClaim{{
		ID: "cap-1", State: domain.CapacityClaimHeld, Kind: domain.ExecutionKindWorker,
		WorkflowRunID: "wf-other", DispatchKey: "dk-1",
		RuntimeHandle: reclaimHandle, RuntimeInstanceID: "$42",
	}}}

	f, err := reclaimSweeper(rt, claims).
		ReclaimSessionRuntime(context.Background(), ownedReclaimRequest("$42"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition != runtimegc.DispositionLive {
		t.Fatalf("disposition = %s, want live", f.Disposition)
	}
	if !rt.alive("$42") {
		t.Fatal("destroyed a runtime a held claim was still paying for")
	}
}

// ---- 12/13/14: convergence -----------------------------------------------------

// The runtime is already gone: a crash after a destroy, or an agent that exited
// on its own. Absent, not an error, and nothing is retried into existence.
func TestReclaimConvergesWhenTheRuntimeIsAlreadyGone(t *testing.T) {
	rt := newFakeRuntime()
	f, err := reclaimSweeper(rt, &fakeClaims{}).
		ReclaimSessionRuntime(context.Background(), ownedReclaimRequest("$42"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition != runtimegc.DispositionAbsent {
		t.Fatalf("disposition = %s, want absent", f.Disposition)
	}
}

// Repeated reclamation is idempotent: the second pass finds nothing to do
// rather than failing or destroying something else.
func TestReclaimIsIdempotent(t *testing.T) {
	rt := newFakeRuntime(ownedRuntime("$42"))
	s := reclaimSweeper(rt, &fakeClaims{})
	for i := 0; i < 3; i++ {
		f, err := s.ReclaimSessionRuntime(context.Background(), ownedReclaimRequest("$42"))
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		want := runtimegc.DispositionCleaned
		if i > 0 {
			want = runtimegc.DispositionAbsent
		}
		if f.Disposition != want {
			t.Fatalf("pass %d disposition = %s, want %s", i, f.Disposition, want)
		}
	}
}

// A runtime AO cannot reach is unknown, and unknown is not dead.
func TestReclaimRefusesWhenTheRuntimeCannotBeReached(t *testing.T) {
	rt := newFakeRuntime(ownedRuntime("$42"))
	rt.factsErr["$42"] = ports.ErrRuntimeUnavailable

	f, err := reclaimSweeper(rt, &fakeClaims{}).
		ReclaimSessionRuntime(context.Background(), ownedReclaimRequest("$42"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition != runtimegc.DispositionUnprovable {
		t.Fatalf("disposition = %s, want unprovable", f.Disposition)
	}
	if !rt.alive("$42") {
		t.Fatal("destroyed a runtime AO could not read")
	}
}

// A destroy that fails is reported, not retried here. The periodic sweep
// re-derives the candidate from durable facts on its next pass; a retry loop in
// this path would be a second scheduler.
func TestReclaimReportsAFailedDestroyWithoutRetrying(t *testing.T) {
	rt := newFakeRuntime(ownedRuntime("$42"))
	rt.destroyErr["$42"] = errors.New("kill failed")

	f, err := reclaimSweeper(rt, &fakeClaims{}).
		ReclaimSessionRuntime(context.Background(), ownedReclaimRequest("$42"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition != runtimegc.DispositionError || f.Err == "" {
		t.Fatalf("finding = %+v, want an error disposition carrying the failure", f)
	}
	if !rt.alive("$42") {
		t.Fatal("the runtime was destroyed after the destroy reported failure")
	}
}

// ---- 18: a live replacement survives a stale terminal cleanup -----------------

// The full incident shape, in one test: a run finishes, its cleanup is delayed
// (a crash, a slow reconcile), the session name is reused by a NEW launch, and
// the stale cleanup finally runs. The replacement must survive.
func TestStaleTerminalCleanupSparesALiveReplacement(t *testing.T) {
	rt := newFakeRuntime(ownedRuntime("$42"))
	s := reclaimSweeper(rt, &fakeClaims{})

	// The original is ended by its own terminal run.
	if _, err := s.ReclaimSessionRuntime(context.Background(), ownedReclaimRequest("$42")); err != nil {
		t.Fatal(err)
	}
	// A new launch takes the same NAME with a new incarnation and a new token.
	replacement := ports.RuntimeSessionSummary{
		ID: reclaimHandle, InstanceID: "$77",
		Owner: domain.SessionRuntimeOwnerToken(reclaimSession, "launch-2"), OwnerKnown: true,
	}
	rt.mu.Lock()
	rt.sessions[replacement.InstanceID] = replacement
	rt.mu.Unlock()

	// The stale cleanup replays, still naming the OLD incarnation.
	f, err := s.ReclaimSessionRuntime(context.Background(), ownedReclaimRequest("$42"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition == runtimegc.DispositionCleaned {
		t.Fatal("a stale terminal cleanup destroyed the replacement")
	}
	if !rt.alive("$77") {
		t.Fatal("the live replacement was destroyed by a replayed cleanup")
	}
	// And replaying it repeatedly changes nothing.
	for i := 0; i < 3; i++ {
		if _, err := s.ReclaimSessionRuntime(context.Background(), ownedReclaimRequest("$42")); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if !rt.alive("$77") {
			t.Fatalf("replay %d destroyed the replacement", i)
		}
	}
}

// ---- dry run ------------------------------------------------------------------

func TestReclaimDryRunDestroysNothing(t *testing.T) {
	rt := newFakeRuntime(ownedRuntime("$42"))
	req := ownedReclaimRequest("$42")
	req.DryRun = true

	f, err := reclaimSweeper(rt, &fakeClaims{}).ReclaimSessionRuntime(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if f.Disposition != runtimegc.DispositionCandidate {
		t.Fatalf("disposition = %s, want candidate", f.Disposition)
	}
	if !rt.alive("$42") {
		t.Fatal("a dry run destroyed a runtime")
	}
}
