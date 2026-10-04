package daemon

// The production-wiring end-to-end test for terminal runtime reclamation.
//
// # Why this test exists in this shape
//
// Every component involved here already had tests, and every one of them
// passed while production accumulated twenty-five live agent processes. They
// passed because each was driven with records built by hand:
//
//   - runtimegc's sweeper was given SessionRecords with the ownership columns
//     already populated, so it never noticed that nothing populated them;
//   - lifecycle's tests never asserted that MarkSpawned persisted the runtime
//     identity, so mergeMetadata silently dropping both halves was invisible;
//   - the capacity scheduler had no test for bindRuntimesToClaims at all, so
//     passing "" as the incarnation looked like working code.
//
// A unit test cannot catch a defect that lives in the seam BETWEEN units. So
// this one drives the real chain, through the real store, in the order
// production runs it:
//
//	lifecycle.Manager.MarkSpawned  (the ownership metadata survives the merge)
//	  -> sqlite store              (both columns are actually written)
//	    -> runtimegc.Sweeper       (the identity is provable from the row)
//	      -> terminalRuntimeReclaimer (intent recorded, incarnation destroyed)
//	        -> repeated sweeps     (idempotent, nothing left behind)
//
// The runtime itself is faked, because a real tmux server is a different test
// (adapters/runtime/tmux/real_tmux_terminal_cleanup_test.go) and this one is
// about the DURABLE seam. Everything between the metadata and the destroy is
// the production code path.
//
// It is written to fail if any of the three defects returns. Each is checked
// explicitly and named in its failure message, so a regression reports which
// part of the chain broke rather than "cleanup did not happen".

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
	sqlitetest "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// e2eRuntime is a runtime whose sessions carry real incarnations and real
// ownership markers, and which records exactly what it was asked to destroy.
type e2eRuntime struct {
	mu        sync.Mutex
	sessions  map[string]ports.RuntimeSessionSummary
	destroyed []string
}

func newE2ERuntime() *e2eRuntime {
	return &e2eRuntime{sessions: map[string]ports.RuntimeSessionSummary{}}
}

func (r *e2eRuntime) create(name, instance, owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[instance] = ports.RuntimeSessionSummary{
		ID: name, InstanceID: instance, Owner: owner, OwnerKnown: owner != "",
	}
}

func (r *e2eRuntime) ListSessions(context.Context) ([]ports.RuntimeSessionSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ports.RuntimeSessionSummary, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out, nil
}

func (r *e2eRuntime) SessionFacts(_ context.Context, handle ports.RuntimeHandle) (ports.SessionFacts, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		if s.ID != handle.ID {
			continue
		}
		return ports.SessionFacts{
			InstanceID: s.InstanceID, Owner: s.Owner, OwnerKnown: s.OwnerKnown,
			WorkloadAlive: true, WorkloadKnown: true,
		}, true, nil
	}
	return ports.SessionFacts{}, false, nil
}

func (r *e2eRuntime) DestroyInstance(_ context.Context, instanceID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.destroyed = append(r.destroyed, instanceID)
	delete(r.sessions, instanceID)
	return nil
}

func (r *e2eRuntime) alive(instance string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.sessions[instance]
	return ok
}

func (r *e2eRuntime) destroyCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.destroyed)
}

// TestProductionWiringEndsATerminalRunsRuntime drives the whole durable chain.
func TestProductionWiringEndsATerminalRunsRuntime(t *testing.T) {
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	now := time.Date(2026, 8, 29, 21, 7, 40, 0, time.UTC)

	const (
		projectID = "agent-orchestrator"
		launchID  = "launch-1"
		instance  = "$42"
		runID     = "wf-170b16ce"
	)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: projectID, Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}

	// ---- 1. the session is created and its runtime marked, as spawn does ----
	created, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: projectID, Kind: domain.KindWorker,
		Harness: domain.HarnessClaudeCode, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := created.ID
	ownerToken := domain.SessionRuntimeOwnerToken(sessionID, launchID)
	rt := newE2ERuntime()
	rt.create(string(sessionID), instance, ownerToken)

	// ---- 2. THE SEAM. The ownership metadata goes through the real lifecycle
	// manager, exactly as Session Manager hands it over after Create returns.
	lcm := lifecycle.New(store, nil)
	if err := lcm.MarkSpawned(ctx, sessionID, domain.SessionMetadata{
		RuntimeHandleID:   string(sessionID),
		RuntimeLaunchID:   launchID,
		RuntimeInstanceID: instance,
		RuntimeOwnerToken: ownerToken,
		WorkspacePath:     t.TempDir(),
		Branch:            "feature",
	}); err != nil {
		t.Fatal(err)
	}

	// Defect 1, named explicitly: mergeMetadata dropping the ownership fields.
	// Every session row in the incident database looked like this.
	persisted, ok, gerr := store.GetSession(ctx, sessionID)
	err = gerr
	if err != nil || !ok {
		t.Fatalf("session not readable after MarkSpawned: %v", err)
	}
	if persisted.Metadata.RuntimeInstanceID != instance {
		t.Fatalf("runtime_instance_id = %q, want %q — the ownership metadata did not survive lifecycle.mergeMetadata, "+
			"which makes every terminal runtime unprovable and disables Runtime GC entirely",
			persisted.Metadata.RuntimeInstanceID, instance)
	}
	if persisted.Metadata.RuntimeOwnerToken != ownerToken {
		t.Fatalf("runtime_owner_token = %q, want %q — same defect, other half",
			persisted.Metadata.RuntimeOwnerToken, ownerToken)
	}

	// ---- 3. the capacity claim binds the incarnation ------------------------
	// Defect 2: bindRuntimesToClaims passing "" as the incarnation. Runtime GC
	// skips every claim without one, so the claim-derived candidate source --
	// which its own comment calls the strongest -- produced nothing.
	if _, _, err := store.CreateWorkflowRun(ctx, domain.WorkflowRun{
		ID: runID, ProjectID: projectID, Objective: "production wiring", State: domain.WorkflowRunRunning,
		PolicyVersion: "v1", PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now,
	}, []domain.WorkflowStep{{
		ID: "wfs-work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 1,
		State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	claim := domain.CapacityClaim{
		ID: "cap-b2dfa56e", Kind: domain.ExecutionKindWorker, State: domain.CapacityClaimQueued,
		WorkflowRunID: runID, WorkflowStepID: "wfs-work", DispatchKey: "dk-1",
		ProjectID: projectID, EnqueuedAt: now, UpdatedAt: now,
	}
	if _, err := store.EnqueueCapacityClaim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireCapacity(ctx, claim.DispatchKey, 0, domain.CapacityLimits{Global: 8, PerKind: map[domain.ExecutionKind]int{domain.ExecutionKindWorker: 4}}, domain.ExecutionKindWorker, now); err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindCapacityClaimRuntime(ctx, claim.DispatchKey,
		persisted.Metadata.RuntimeHandleID, persisted.Metadata.RuntimeInstanceID, 0, now)
	if err != nil || !bound {
		t.Fatalf("binding the claim's runtime failed: bound=%v err=%v", bound, err)
	}
	claims, err := store.ListCapacityClaimsForRun(ctx, runID)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims = %d, err = %v", len(claims), err)
	}
	if claims[0].RuntimeInstanceID != instance {
		t.Fatalf("claim runtime_instance_id = %q, want %q — a claim that names only a session NAME is invisible to Runtime GC, "+
			"because a name is a discovery key and never an authority key",
			claims[0].RuntimeInstanceID, instance)
	}

	// ---- 4. while the claim is HELD, nothing may end the runtime ------------
	sweeper := &runtimegc.Sweeper{
		Claims: store, Runs: store, Sessions: store, Inventory: rt, Facts: rt,
		Now: func() time.Time { return now },
	}
	reclaimer := newTerminalRuntimeReclaimer(sweeper, lcm, nil)
	if reclaimer == nil {
		t.Fatal("the terminal runtime reclaimer is not wired")
	}
	held, _, err := reclaimer.ReclaimSessionRuntime(ctx, terminalRequest(persisted, runID))
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("ended a runtime a HELD capacity claim was still paying for")
	}
	if !rt.alive(instance) {
		t.Fatal("the runtime was destroyed while its claim was still held")
	}

	// The refusal must not have left the session record wrong. MarkTerminated
	// ran first (it is the durable intent), so the row is terminal and the
	// runtime is still there -- which is exactly the state Runtime GC's
	// ordinary terminated-session candidate exists to finish.
	afterRefusal, _, err := store.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !afterRefusal.IsTerminated {
		t.Fatal("the termination intent was not recorded")
	}

	// ---- 5. the run completes: the claim is released, then the runtime ends --
	if _, err := store.ReleaseCapacityClaim(ctx, claim.DispatchKey, 0, "run completed", now); err != nil {
		t.Fatal(err)
	}
	reclaimed, reason, err := reclaimer.ReclaimSessionRuntime(ctx, terminalRequest(afterRefusal, runID))
	if err != nil {
		t.Fatal(err)
	}
	if !reclaimed {
		t.Fatalf("a terminal run's provably-owned runtime was not ended: %s", reason)
	}
	if rt.alive(instance) {
		t.Fatal("the runtime survived reclamation")
	}
	if got := rt.destroyCount(); got != 1 {
		t.Fatalf("destroy calls = %d, want exactly 1", got)
	}

	// ---- 6. Runtime GC now sees nothing left to do --------------------------
	report, err := sweeper.Sweep(ctx, runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Cleaned != 0 {
		t.Fatalf("the sweep still found %d runtimes to clean after the terminal path ran", report.Cleaned)
	}

	// ---- 7. repeated reconciliation stays clean -----------------------------
	for i := 0; i < 3; i++ {
		again, _, rerr := reclaimer.ReclaimSessionRuntime(ctx, terminalRequest(afterRefusal, runID))
		if rerr != nil {
			t.Fatalf("replay %d: %v", i, rerr)
		}
		if again {
			t.Fatalf("replay %d reclaimed a runtime that was already gone", i)
		}
		if _, serr := sweeper.Sweep(ctx, runtimegc.Options{Trigger: "test"}); serr != nil {
			t.Fatalf("replay sweep %d: %v", i, serr)
		}
	}
	if got := rt.destroyCount(); got != 1 {
		t.Fatalf("destroy calls after replays = %d, want exactly 1", got)
	}
}

// TestProductionWiringSparesALiveReplacementUnderTheSameName is §F(7): a stale
// terminal cleanup replaying after the session name has been reused.
func TestProductionWiringSparesALiveReplacementUnderTheSameName(t *testing.T) {
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	now := time.Date(2026, 8, 29, 21, 7, 40, 0, time.UTC)

	const (
		projectID = "agent-orchestrator"
		runID     = "wf-170b16ce"
	)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: projectID, Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: projectID, Kind: domain.KindWorker,
		Harness: domain.HarnessClaudeCode, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := created.ID

	lcm := lifecycle.New(store, nil)
	firstToken := domain.SessionRuntimeOwnerToken(sessionID, "launch-1")
	if err := lcm.MarkSpawned(ctx, sessionID, domain.SessionMetadata{
		RuntimeHandleID: string(sessionID), RuntimeLaunchID: "launch-1",
		RuntimeInstanceID: "$42", RuntimeOwnerToken: firstToken,
	}); err != nil {
		t.Fatal(err)
	}
	stale, _, serr := store.GetSession(ctx, sessionID)
	if serr != nil {
		t.Fatal(serr)
	}

	// The name is reused by a NEW launch with a NEW incarnation, and the row
	// now describes that one.
	rt := newE2ERuntime()
	secondToken := domain.SessionRuntimeOwnerToken(sessionID, "launch-2")
	rt.create(string(sessionID), "$77", secondToken)
	if err := lcm.MarkSpawned(ctx, sessionID, domain.SessionMetadata{
		RuntimeHandleID: string(sessionID), RuntimeLaunchID: "launch-2",
		RuntimeInstanceID: "$77", RuntimeOwnerToken: secondToken,
	}); err != nil {
		t.Fatal(err)
	}

	sweeper := &runtimegc.Sweeper{
		Claims: store, Runs: store, Sessions: store, Inventory: rt, Facts: rt,
		Now: func() time.Time { return now },
	}
	reclaimer := newTerminalRuntimeReclaimer(sweeper, lcm, nil)

	// The stale cleanup finally runs, still naming the OLD incarnation.
	reclaimed, _, err := reclaimer.ReclaimSessionRuntime(ctx, terminalRequest(stale, runID))
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed {
		t.Fatal("a stale terminal cleanup reported destroying the replacement")
	}
	if !rt.alive("$77") {
		t.Fatal("a stale terminal cleanup destroyed a live replacement under the same session name")
	}
}

// TestProductionWiringLeavesLegacySessionsAlone is §K: the historical sessions.
func TestProductionWiringLeavesLegacySessionsAlone(t *testing.T) {
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	now := time.Date(2026, 8, 29, 21, 7, 40, 0, time.UTC)

	const projectID = "agent-orchestrator"
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: projectID, Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	rt := newE2ERuntime()
	// A pre-P1-D session: on AO's server, no ownership marker, no recorded
	// identity. Exactly the twenty-five found in production.
	legacy, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: projectID, Kind: domain.KindWorker,
		Harness: domain.HarnessClaudeCode, IsTerminated: true,
		Activity:  domain.Activity{State: domain.ActivityExited, LastActivityAt: now},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy.Metadata = domain.SessionMetadata{RuntimeHandleID: string(legacy.ID)}
	// On AO's own server, with no ownership marker: a pre-P1-D session, exactly
	// the twenty-five found in production.
	rt.create(string(legacy.ID), "$1", "")
	if err := store.UpdateSession(ctx, legacy); err != nil {
		t.Fatal(err)
	}

	sweeper := &runtimegc.Sweeper{
		Claims: store, Runs: store, Sessions: store, Inventory: rt, Facts: rt,
		Now: func() time.Time { return now },
	}
	lcm := lifecycle.New(store, nil)
	reclaimer := newTerminalRuntimeReclaimer(sweeper, lcm, nil)

	reclaimed, reason, err := reclaimer.ReclaimSessionRuntime(ctx, terminalRequest(legacy, "wf-old"))
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed {
		t.Fatal("a legacy session with no ownership proof was destroyed")
	}
	if reason == "" {
		t.Fatal("a refusal with no explanation")
	}
	if !rt.alive("$1") {
		t.Fatal("the legacy runtime was destroyed")
	}

	// And a full sweep leaves it alone too, while reporting it so it cannot
	// become an orphan nobody knows about.
	report, err := sweeper.Sweep(ctx, runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Cleaned != 0 || !rt.alive("$1") {
		t.Fatalf("the sweep reclaimed a legacy session: %+v", report.Findings)
	}
	if report.SkippedUnprovable != 1 {
		t.Fatalf("unprovable = %d, want the legacy session reported", report.SkippedUnprovable)
	}
	for _, f := range report.Findings {
		if f.InstanceID == "$1" {
			if f.OwnershipProven {
				t.Fatal("a session with no marker was reported as owned")
			}
			if f.RecommendedAction == "" {
				t.Fatal("a session AO will never reclaim carries no operator recommendation")
			}
		}
	}
}

func terminalRequest(rec domain.SessionRecord, runID string) workflowcore.TerminalRuntimeRequest {
	return workflowcore.TerminalRuntimeRequest{
		SessionID:     rec.ID,
		Handle:        rec.Metadata.RuntimeHandleID,
		InstanceID:    rec.Metadata.RuntimeInstanceID,
		OwnerToken:    rec.Metadata.RuntimeOwnerToken,
		LaunchID:      rec.Metadata.RuntimeLaunchID,
		WorkflowRunID: runID,
		Reason:        "the workflow completed",
	}
}
