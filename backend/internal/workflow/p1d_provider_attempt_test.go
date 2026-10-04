package workflow_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1d_provider_attempt_test.go — P1-D §F–§J and test matrix T26–T43.
//
// The property that matters most here is the one that is easiest to lose:
//
//	A PROVIDER ATTEMPT IS NOT A TASK GENERATION.
//
// Every test below that asserts something "remains stable across a failover"
// is asserting that. If a hop advanced the lifecycle generation, the run would
// look like new work — a fresh placement, a fresh review, a fresh capacity
// claim — and the worktree provider A left behind would be orphaned.
//
// The second property is that ambiguity is never converted into permission.
// AO fails over from exactly two proven classes, and every other outcome stops
// the run for evidence.

// providerFixture is a placement fixture with a frozen placement and one
// authoritative provider attempt: the state a launch actually starts from.
type providerFixture struct {
	*placementFixture
	placement domain.ExecutionPlacement
	attempt   domain.ProviderAttempt
}

func newProviderFixture(t *testing.T) *providerFixture {
	t.Helper()
	ctx := context.Background()
	f := newPlacementFixture(t)
	placement, _, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil {
		t.Fatal(err)
	}
	attempt, ok, err := f.coord.EnsureProviderAttemptForTest(ctx, f.run, f.step, placement, domain.HarnessClaudeCode)
	if err != nil || !ok {
		t.Fatalf("first provider attempt: ok=%v err=%v", ok, err)
	}
	return &providerFixture{placementFixture: f, placement: placement, attempt: attempt}
}

func (f *providerFixture) reload(t *testing.T, id string) domain.ProviderAttempt {
	t.Helper()
	a, found, err := f.store.GetProviderAttempt(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("GetProviderAttempt(%s): found=%v err=%v", id, found, err)
	}
	return a
}

// T26: the attempt is durable BEFORE anything launches, so a crash in the
// launch window leaves something to reason about rather than nothing.
func TestProviderAttemptIsPersistedBeforeLaunch(t *testing.T) {
	f := newProviderFixture(t)
	if f.attempt.State != domain.ProviderAttemptPlanned {
		t.Fatalf("a fresh attempt is %q, want planned", f.attempt.State)
	}
	if f.attempt.Ordinal != 1 {
		t.Fatalf("the preferred provider's attempt has ordinal %d, want 1", f.attempt.Ordinal)
	}
	if f.attempt.RuntimeSessionID != "" {
		t.Fatal("an attempt that has not launched names a runtime")
	}
	stored := f.reload(t, f.attempt.ID)
	if stored.Provider != domain.HarnessClaudeCode {
		t.Fatalf("stored provider = %q, want claude", stored.Provider)
	}
}

// T27: a restart reads the same attempt identity back. The id is durable and
// never reused, so "which attempt launched this runtime" has one answer forever.
func TestRestartPreservesProviderAttemptIdentity(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	rebooted := workflowcore.New(workflowcore.Deps{
		Store: f.store, Projects: f.store,
		Placements: f.store, ProviderAttempts: f.store,
		InstanceToken: "daemon-after-restart",
	})
	again, ok, err := rebooted.EnsureProviderAttemptForTest(ctx, f.run, f.step, f.placement, domain.HarnessClaudeCode)
	if err != nil || !ok {
		t.Fatalf("after restart: ok=%v err=%v", ok, err)
	}
	if again.ID != f.attempt.ID {
		t.Fatalf("a restart minted a new attempt: %s -> %s", f.attempt.ID, again.ID)
	}
	if again.Ordinal != 1 {
		t.Fatalf("a restart advanced the ordinal to %d; the budget must survive a reboot", again.Ordinal)
	}
}

// T28: the preferred provider succeeding leaves exactly one attempt.
func TestPreferredProviderSuccessLeavesOneAttempt(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	if !f.coord.AdvanceProviderAttemptForTest(ctx, f.attempt, domain.ProviderAttemptRunning) {
		t.Fatal("could not move the attempt to running")
	}
	running := f.reload(t, f.attempt.ID)
	if !f.coord.AdvanceProviderAttemptForTest(ctx, running, domain.ProviderAttemptCompleted) {
		t.Fatal("could not complete the attempt")
	}
	all, err := f.store.ListProviderAttemptsForRun(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("a successful preferred provider left %d attempts, want 1", len(all))
	}
	if all[0].State.Authoritative() {
		t.Fatal("a completed attempt still reports itself authoritative")
	}
}

// T29/T33/T34/T35: a safe-before-execution failure fails over, the successor
// takes the NEXT ordinal, and neither the lifecycle generation nor the
// placement generation moves.
func TestSafeBeforeExecutionFailsOverAndKeepsObligationAndPlacement(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	successor, hopped, err := f.coord.FailoverProviderAttempt(ctx, f.run, f.step, f.attempt,
		domain.HarnessCodex, "", domain.FailoverSafeBeforeExecution,
		domain.WorkflowErrorAgentStartFailed, "the launcher refused and created nothing", "")
	if err != nil {
		t.Fatalf("failover: %v", err)
	}
	if !hopped {
		t.Fatal("a safe-before-execution failure did not fail over")
	}
	// T33
	if successor.Ordinal != f.attempt.Ordinal+1 {
		t.Fatalf("successor ordinal = %d, want %d", successor.Ordinal, f.attempt.Ordinal+1)
	}
	// T34 — the obligation is untouched.
	if successor.LifecycleGeneration != f.attempt.LifecycleGeneration {
		t.Fatalf("the failover advanced the lifecycle generation %d -> %d; a provider attempt is not a task generation",
			f.attempt.LifecycleGeneration, successor.LifecycleGeneration)
	}
	// T35 — §I: the same frozen placement stays authoritative.
	if successor.PlacementGeneration != f.attempt.PlacementGeneration {
		t.Fatalf("the failover minted a new placement generation %d -> %d; the provider changed, not the checkout",
			f.attempt.PlacementGeneration, successor.PlacementGeneration)
	}
	live := f.live(t)
	if live.PlacementGeneration != f.placement.PlacementGeneration {
		t.Fatalf("the live placement moved to generation %d during a provider failover", live.PlacementGeneration)
	}

	// The predecessor is terminal, chained, and no longer authoritative.
	predecessor := f.reload(t, f.attempt.ID)
	if predecessor.State != domain.ProviderAttemptFailedSafe {
		t.Fatalf("predecessor state = %q, want failed_safe", predecessor.State)
	}
	if predecessor.SuccessorAttemptID != successor.ID {
		t.Fatalf("predecessor names successor %q, want %q", predecessor.SuccessorAttemptID, successor.ID)
	}
	if successor.PredecessorAttemptID != f.attempt.ID {
		t.Fatalf("successor names predecessor %q, want %q", successor.PredecessorAttemptID, f.attempt.ID)
	}
}

// T30: safe_after_proven_no_mutation fails over — but only when it CARRIES the
// proof. The class cannot be claimed with an empty evidence digest, which is
// what stops "git status looks clean" becoming a proof.
func TestSafeAfterProvenNoMutationRequiresItsEvidence(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	if _, _, err := f.coord.FailoverProviderAttempt(ctx, f.run, f.step, f.attempt,
		domain.HarnessCodex, "", domain.FailoverSafeAfterProvenNoMutation,
		domain.WorkflowErrorAgentStartFailed, "claims the workspace is clean", ""); err == nil {
		t.Fatal("safe_after_proven_no_mutation was accepted with no evidence")
	}

	successor, hopped, err := f.coord.FailoverProviderAttempt(ctx, f.run, f.step, f.attempt,
		domain.HarnessCodex, "", domain.FailoverSafeAfterProvenNoMutation,
		domain.WorkflowErrorAgentStartFailed, "the workspace is provably unchanged",
		"workspace_unchanged:fp-before")
	if err != nil || !hopped {
		t.Fatalf("a proven mid-execution failure did not fail over: hopped=%v err=%v", hopped, err)
	}
	predecessor := f.reload(t, f.attempt.ID)
	if predecessor.Safety != domain.FailoverSafeAfterProvenNoMutation {
		t.Fatalf("predecessor safety = %q", predecessor.Safety)
	}
	if predecessor.MutationEvidenceDigest == "" {
		t.Fatal("the proven class was recorded without the evidence that proved it")
	}
	if successor.PlacementGeneration != f.attempt.PlacementGeneration {
		t.Fatal("a proven-safe failover moved the placement")
	}
}

// T31/T32: ambiguity and completion never fail over, and each is recorded as
// the refusal AO made on purpose rather than as a hop that did not happen.
func TestAmbiguousAndCompletedExecutionNeverFailOver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		safety domain.FailoverSafety
		want   domain.ProviderAttemptState
	}{
		{"ambiguous", domain.FailoverAmbiguousExecution, domain.ProviderAttemptFailedAmbiguous},
		{"completed", domain.FailoverCompletedExecution, domain.ProviderAttemptCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProviderFixture(t)
			ctx := context.Background()
			_, hopped, err := f.coord.FailoverProviderAttempt(ctx, f.run, f.step, f.attempt,
				domain.HarnessCodex, "", tc.safety, domain.WorkflowErrorAgentStartFailed, "no", "")
			if err != nil {
				t.Fatal(err)
			}
			if hopped {
				t.Fatalf("%s execution failed over; only the two proven classes may", tc.name)
			}
			after := f.reload(t, f.attempt.ID)
			if after.State != tc.want {
				t.Fatalf("attempt state = %q, want %q", after.State, tc.want)
			}
			all, lerr := f.store.ListProviderAttemptsForRun(ctx, f.run.ID)
			if lerr != nil {
				t.Fatal(lerr)
			}
			if len(all) != 1 {
				t.Fatalf("a refused failover created %d attempts, want 1", len(all))
			}
		})
	}
}

// T37/T38/T39: a stale attempt has no authority. It cannot transition itself,
// cannot rebind a runtime, and reconnecting after its successor is
// authoritative is inert.
func TestStaleProviderAttemptHasNoAuthority(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	successor, hopped, err := f.coord.FailoverProviderAttempt(ctx, f.run, f.step, f.attempt,
		domain.HarnessCodex, "", domain.FailoverSafeBeforeExecution,
		domain.WorkflowErrorAgentStartFailed, "launch refused", "")
	if err != nil || !hopped {
		t.Fatalf("setup failover: hopped=%v err=%v", hopped, err)
	}

	// T39: A is no longer authoritative, by the state machine and by the query.
	if f.coord.ProviderAttemptIsAuthoritative(ctx, f.attempt.ID) {
		t.Fatal("the superseded attempt still reports as authoritative")
	}
	if !f.coord.ProviderAttemptIsAuthoritative(ctx, successor.ID) {
		t.Fatal("the successor is not authoritative")
	}
	authoritative, found, err := f.store.GetAuthoritativeProviderAttempt(ctx, f.run.ID, f.step.ID, f.attempt.LifecycleGeneration)
	if err != nil || !found {
		t.Fatalf("no authoritative attempt: found=%v err=%v", found, err)
	}
	if authoritative.ID != successor.ID {
		t.Fatalf("authority is %s, want the successor %s", authoritative.ID, successor.ID)
	}

	// T37/T38: A, working from its own stale view, cannot write anything. Its
	// transitions CAS on the state it thinks it is in, which is no longer true.
	if f.coord.AdvanceProviderAttemptForTest(ctx, f.attempt, domain.ProviderAttemptRunning) {
		t.Fatal("a stale attempt moved itself back to running")
	}
	if ok, _ := f.store.BindProviderAttemptRuntime(ctx, f.attempt.ID, "sess-stale", "cap-stale", f.attempt.CreatedAt); ok {
		t.Fatal("a stale attempt bound itself to a runtime")
	}
	// And it cannot claim its successor's completion.
	if f.coord.AdvanceProviderAttemptForTest(ctx, f.attempt, domain.ProviderAttemptCompleted) {
		t.Fatal("a stale attempt wrote a completion")
	}
	still := f.reload(t, successor.ID)
	if !still.State.Authoritative() {
		t.Fatalf("the successor was disturbed by its stale predecessor: %q", still.State)
	}
}

// T42: no A->B->A loop. The history refuses a provider this obligation has
// already been offered to, independently of the numeric budget.
func TestFailoverRefusesAProviderThisObligationAlreadyTried(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	toCodex, hopped, err := f.coord.FailoverProviderAttempt(ctx, f.run, f.step, f.attempt,
		domain.HarnessCodex, "", domain.FailoverSafeBeforeExecution,
		domain.WorkflowErrorAgentStartFailed, "claude refused", "")
	if err != nil || !hopped {
		t.Fatalf("claude -> codex: hopped=%v err=%v", hopped, err)
	}
	// Now Codex fails, and the only remaining candidate is Claude — which has
	// already had this obligation.
	_, hoppedBack, err := f.coord.FailoverProviderAttempt(ctx, f.run, f.step, toCodex,
		domain.HarnessClaudeCode, "", domain.FailoverSafeBeforeExecution,
		domain.WorkflowErrorAgentStartFailed, "codex refused", "")
	if err != nil {
		t.Fatal(err)
	}
	if hoppedBack {
		t.Fatal("the obligation was routed back to a provider it had already tried; that is the A->B->A loop")
	}
	all, err := f.store.ListProviderAttemptsForRun(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("the loop guard left %d attempts, want 2", len(all))
	}
	for _, a := range all {
		if a.State.Authoritative() {
			t.Fatalf("attempt %s is still authoritative after the budget was spent", a.ID)
		}
	}
}

// T41: the budget survives a restart. The ordinal is read from the ledger, so
// a rebooted daemon does not hand the obligation a fresh set of hops.
func TestFailoverBudgetSurvivesRestart(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	if _, hopped, err := f.coord.FailoverProviderAttempt(ctx, f.run, f.step, f.attempt,
		domain.HarnessCodex, "", domain.FailoverSafeBeforeExecution,
		domain.WorkflowErrorAgentStartFailed, "claude refused", ""); err != nil || !hopped {
		t.Fatalf("setup hop: hopped=%v err=%v", hopped, err)
	}

	rebooted := workflowcore.New(workflowcore.Deps{
		Store: f.store, Projects: f.store,
		Placements: f.store, ProviderAttempts: f.store,
		InstanceToken: "daemon-after-restart",
	})
	budget := rebooted.ProviderFailoverBudget(ctx, f.run, f.step)
	if budget.CurrentOrdinal != 2 {
		t.Fatalf("after a restart the recorded ordinal is %d, want 2; the budget must not reset", budget.CurrentOrdinal)
	}
	// And a rebooted daemon does not mint a fresh ordinal-1 attempt for an
	// obligation whose attempts are already recorded. Doing so on every
	// reconcile is exactly how a budget silently refills.
	if _, _, err := rebooted.EnsureProviderAttemptForTest(ctx, f.run, f.step, f.placement, domain.HarnessClaudeCode); err != nil {
		t.Fatal(err)
	}
	all, err := f.store.ListProviderAttemptsForRun(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("a restart produced %d attempts, want 2", len(all))
	}
}

// T36: capacity authority moves with the attempt. The successor binds its own
// claim, and the predecessor's binding is not disturbed — which is what makes
// "a stale attempt cannot release its successor's slot" checkable.
func TestCapacityAuthorityTransitionsWithTheAttempt(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	if ok, err := f.store.BindProviderAttemptRuntime(ctx, f.attempt.ID, "sess-a", "cap-a", f.attempt.CreatedAt); err != nil || !ok {
		t.Fatalf("bind A: ok=%v err=%v", ok, err)
	}
	successor, hopped, err := f.coord.FailoverProviderAttempt(ctx, f.run, f.step, f.reload(t, f.attempt.ID),
		domain.HarnessCodex, "", domain.FailoverSafeBeforeExecution,
		domain.WorkflowErrorAgentStartFailed, "claude refused", "")
	if err != nil || !hopped {
		t.Fatalf("hop: hopped=%v err=%v", hopped, err)
	}
	if ok, err := f.store.BindProviderAttemptRuntime(ctx, successor.ID, "sess-b", "cap-b", successor.CreatedAt); err != nil || !ok {
		t.Fatalf("bind B: ok=%v err=%v", ok, err)
	}
	a := f.reload(t, f.attempt.ID)
	b := f.reload(t, successor.ID)
	if a.CapacityClaimID != "cap-a" || b.CapacityClaimID != "cap-b" {
		t.Fatalf("claims crossed: A=%q B=%q", a.CapacityClaimID, b.CapacityClaimID)
	}
	// A, stale, cannot overwrite B's binding with its own.
	if ok, _ := f.store.BindProviderAttemptRuntime(ctx, a.ID, "sess-a2", "cap-b", a.CreatedAt); ok {
		t.Fatal("a stale attempt rebound a runtime")
	}
}

// T40: a crash mid-failover converges. The predecessor is terminated FIRST, so
// the worst a crash leaves is a terminal predecessor and no successor — which
// the next pass finishes rather than a state with two live attempts.
func TestCrashMidFailoverConverges(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	// The crash: the predecessor was terminated, the successor never written.
	if !f.coord.AdvanceProviderAttemptForTest(ctx, f.attempt, domain.ProviderAttemptFailedSafe) {
		t.Fatal("could not terminate the predecessor")
	}
	if _, found, err := f.store.GetAuthoritativeProviderAttempt(ctx, f.run.ID, f.step.ID, f.attempt.LifecycleGeneration); err != nil || found {
		t.Fatalf("an attempt is still authoritative after the crash: found=%v err=%v", found, err)
	}

	// The next pass. It reads the durable ordinal, so it neither restarts the
	// budget nor produces a second live attempt.
	rebooted := workflowcore.New(workflowcore.Deps{
		Store: f.store, Projects: f.store,
		Placements: f.store, ProviderAttempts: f.store,
		InstanceToken: "daemon-after-crash",
	})
	successor, hopped, err := rebooted.FailoverProviderAttempt(ctx, f.run, f.step, f.reload(t, f.attempt.ID),
		domain.HarnessCodex, "", domain.FailoverSafeBeforeExecution,
		domain.WorkflowErrorAgentStartFailed, "resumed after a crash", "")
	if err != nil {
		t.Fatal(err)
	}
	// The predecessor is already terminal, so it can no longer authorize a hop:
	// this pass's view of it is stale and it is refused rather than allowed to
	// mint a successor for an obligation it no longer speaks for.
	if hopped {
		t.Fatalf("a terminated predecessor authorized a hop to %s", successor.ID)
	}
	all, err := f.store.ListProviderAttemptsForRun(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("the crash window produced %d attempts, want 1", len(all))
	}
}

// T43: an unrelated workflow is untouched by any of this. The ledger is keyed
// on the obligation, so one run's failover cannot supersede another's attempt.
func TestFailoverDoesNotDisturbAnUnrelatedWorkflow(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	other := newSiblingRun(t, f.placementFixture, "an unrelated objective")
	otherPlacement, _, err := f.coord.EnsureExecutionPlacement(ctx, other.run, other.step)
	if err != nil {
		t.Fatal(err)
	}
	otherAttempt, ok, err := f.coord.EnsureProviderAttemptForTest(ctx, other.run, other.step, otherPlacement, domain.HarnessClaudeCode)
	if err != nil || !ok {
		t.Fatalf("unrelated attempt: ok=%v err=%v", ok, err)
	}

	if _, hopped, err := f.coord.FailoverProviderAttempt(ctx, f.run, f.step, f.attempt,
		domain.HarnessCodex, "", domain.FailoverSafeBeforeExecution,
		domain.WorkflowErrorAgentStartFailed, "claude refused", ""); err != nil || !hopped {
		t.Fatalf("hop: hopped=%v err=%v", hopped, err)
	}

	after := f.reload(t, otherAttempt.ID)
	if after.State != domain.ProviderAttemptPlanned {
		t.Fatalf("an unrelated run's attempt changed to %q during another run's failover", after.State)
	}
	otherLive, found, err := f.store.GetLiveExecutionPlacement(ctx, other.run.ID, "", "")
	if err != nil || !found {
		t.Fatalf("the unrelated run lost its placement: found=%v err=%v", found, err)
	}
	if otherLive.PlacementGeneration != otherPlacement.PlacementGeneration {
		t.Fatal("an unrelated run's placement generation moved")
	}
}

// §H: the proof is an AND over five durable facts, and a clean workspace is one
// of them. This asserts that no subset satisfies it — which is the difference
// between a proof and a heuristic with a confident name.
func TestMutationProofRequiresEveryCondition(t *testing.T) {
	full := workflowcore.MutationProof{
		RuntimeIdentified: true, RuntimeTerminal: true, AttemptTerminal: true,
		NoAuthoritativeMutation: true,
		FingerprintBefore:       "fp", FingerprintAfter: "fp",
	}
	if !full.Proven() {
		t.Fatal("a complete proof was rejected")
	}
	if full.Digest() == "" {
		t.Fatal("a complete proof produced no evidence digest")
	}

	weaken := map[string]func(p *workflowcore.MutationProof){
		"no exact runtime identity":        func(p *workflowcore.MutationProof) { p.RuntimeIdentified = false },
		"the runtime may still be running": func(p *workflowcore.MutationProof) { p.RuntimeTerminal = false },
		"the attempt is not terminal":      func(p *workflowcore.MutationProof) { p.AttemptTerminal = false },
		"AO holds mutation evidence":       func(p *workflowcore.MutationProof) { p.NoAuthoritativeMutation = false },
		"no before-state to compare":       func(p *workflowcore.MutationProof) { p.FingerprintBefore = "" },
		"no after-state to compare":        func(p *workflowcore.MutationProof) { p.FingerprintAfter = "" },
		"the workspace changed":            func(p *workflowcore.MutationProof) { p.FingerprintAfter = "different" },
	}
	for name, weaken := range weaken {
		p := full
		weaken(&p)
		if p.Proven() {
			t.Fatalf("the proof survived %q; every condition is load-bearing", name)
		}
		if p.Digest() != "" {
			t.Fatalf("an unproven claim produced an evidence digest (%s)", name)
		}
		if got := workflowcore.ClassifyMidExecutionFailoverSafety(domain.ProviderAttempt{State: domain.ProviderAttemptRunning}, p); got != domain.FailoverAmbiguousExecution {
			t.Fatalf("with %q the class is %q, want ambiguous_execution", name, got)
		}
	}
}

// §H, against the real store: a bare "git status is clean right now" does NOT
// satisfy the proof, because AO has no exact runtime identity for the attempt.
func TestACleanWorkspaceAloneIsNotAProof(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	proof := f.coord.ProveNoMutationForTest(ctx, f.attempt, f.placement, "fp-before")
	if proof.Proven() {
		t.Fatal("an attempt with no recorded runtime was proven mutation-free")
	}
	if proof.RuntimeIdentified {
		t.Fatal("an attempt that never named a runtime reports an identified one")
	}
	if got := workflowcore.ClassifyMidExecutionFailoverSafety(f.attempt, proof); got != domain.FailoverAmbiguousExecution {
		t.Fatalf("class = %q, want ambiguous_execution", got)
	}
}
