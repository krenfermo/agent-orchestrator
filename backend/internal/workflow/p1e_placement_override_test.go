package workflow_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1e_placement_override_test.go — P1-E §B/§C.
//
// The property under test is not "an override row exists". It is the asymmetry
// the whole model rests on:
//
//	before the freeze   a request DECIDES the placement
//	after the freeze    a request DECIDES NOTHING until a transition consumes it
//
// and that a transition is refused unless every authority over the old
// placement has provably let go — proved from durable rows, never inferred from
// what a checkout looks like.

func requestOverride(t *testing.T, f *placementFixture, requested domain.PlacementOverrideRequest) workflowcore.PlacementOverrideOutcome {
	t.Helper()
	outcome, err := f.coord.RequestPlacementOverride(context.Background(), workflowcore.PlacementOverrideRequestInput{
		RunID: f.run.ID, Requested: requested, RequestedBy: "operator-1", Reason: "P1-E test",
	})
	if err != nil {
		t.Fatalf("RequestPlacementOverride(%s): %v", requested, err)
	}
	return outcome
}

func transitionTo(t *testing.T, f *placementFixture, req workflowcore.PlacementTransitionInput) workflowcore.PlacementTransitionOutcome {
	t.Helper()
	req.RunID = f.run.ID
	if req.RequestedBy == "" {
		req.RequestedBy = "operator-1"
	}
	outcome, err := f.coord.TransitionPlacement(context.Background(), req)
	if err != nil {
		t.Fatalf("TransitionPlacement: %v", err)
	}
	return outcome
}

// §B.1/§B.2: an override recorded BEFORE the freeze decides the placement, and
// the frozen record — not the project's configuration — is what results.
func TestPlacementOverrideBeforeFreezeDecidesTheFrozenPlacement(t *testing.T) {
	// The project says isolated. The operator says direct branch.
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()

	outcome := requestOverride(t, f, domain.PlacementOverrideDirectBranch)
	if !outcome.AppliesAtFreeze {
		t.Fatal("a request made before any freeze must apply at the freeze")
	}
	if outcome.RequiresTransition {
		t.Fatal("nothing is frozen yet, so no transition can be required")
	}

	placement, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
	}
	if placement.Type != domain.PlacementDirectBranch {
		t.Fatalf("placement type = %s, want direct_branch — the override was not honoured", placement.Type)
	}
	// A direct-branch placement may never name a worktree; the schema refuses
	// one, and the override path must not have invented an identity to satisfy
	// the request.
	if placement.WorktreePath != "" {
		t.Fatalf("direct-branch placement names worktree %q", placement.WorktreePath)
	}

	// The request is discharged by the generation that consumed it, so it can
	// never be applied twice.
	overrides, err := f.coord.ListPlacementOverrides(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 1 {
		t.Fatalf("recorded %d overrides, want 1", len(overrides))
	}
	if overrides[0].State != domain.PlacementOverrideApplied {
		t.Fatalf("override state = %s, want applied", overrides[0].State)
	}
	if overrides[0].AppliedGeneration != placement.PlacementGeneration {
		t.Fatalf("override names generation %d, placement is %d",
			overrides[0].AppliedGeneration, placement.PlacementGeneration)
	}
}

// §B.1: `auto` is a real request. It withdraws a standing override rather than
// being indistinguishable from never having asked.
func TestAutoOverrideWithdrawsAStandingRequest(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()

	requestOverride(t, f, domain.PlacementOverrideDirectBranch)
	requestOverride(t, f, domain.PlacementOverrideAuto)

	placement, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
	}
	if placement.Type != domain.PlacementIsolatedWorktree {
		t.Fatalf("placement type = %s; after `auto` the project's own policy must decide", placement.Type)
	}
}

// §B.1: a repeated request supersedes rather than stacking, so an operator who
// clicks twice leaves one outstanding wish, not two.
func TestRepeatedOverrideRequestLeavesExactlyOneOutstanding(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()

	requestOverride(t, f, domain.PlacementOverrideDirectBranch)
	requestOverride(t, f, domain.PlacementOverrideDirectBranch)
	requestOverride(t, f, domain.PlacementOverrideIsolatedWorktree)

	overrides, err := f.coord.ListPlacementOverrides(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	outstanding := 0
	for _, o := range overrides {
		if o.State == domain.PlacementOverrideRequested {
			outstanding++
		}
	}
	if outstanding != 1 {
		t.Fatalf("%d outstanding override requests, want exactly 1", outstanding)
	}
	if len(overrides) != 3 {
		t.Fatalf("recorded %d overrides, want all 3 retained for audit", len(overrides))
	}
}

// §B.3, the core of the model: once a placement is frozen, a request changes
// NOTHING. It is recorded, and the caller is told that a transition is needed.
func TestOverrideAfterFreezeNeverSilentlyRepointsTheFrozenPlacement(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()

	frozen, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
	}

	outcome := requestOverride(t, f, domain.PlacementOverrideDirectBranch)
	if !outcome.RequiresTransition {
		t.Fatal("a request made against a frozen placement must report that a transition is required")
	}
	if outcome.AppliesAtFreeze {
		t.Fatal("a placement is already frozen; nothing applies at a freeze that has happened")
	}

	// Every subsequent read must still see the ORIGINAL placement. This is the
	// failure the whole asymmetry exists to prevent: a standing request quietly
	// re-pointing live work on the next reconcile.
	for i := 0; i < 3; i++ {
		again, found, aerr := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
		if aerr != nil || !found {
			t.Fatalf("re-read %d: found=%v err=%v", i, found, aerr)
		}
		if again.Type != frozen.Type || again.PlacementGeneration != frozen.PlacementGeneration {
			t.Fatalf("re-read %d moved the placement: %s gen %d -> %s gen %d",
				i, frozen.Type, frozen.PlacementGeneration, again.Type, again.PlacementGeneration)
		}
	}
}

// §B.4/§B.5 and §C: a quiesced transition mints a new generation, retires the
// old one, and binds the whole thing to an operator, a reason and a proof.
func TestQuiescedTransitionMintsANewGenerationAndPreservesTheOld(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()

	first, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
	}

	outcome := transitionTo(t, f, workflowcore.PlacementTransitionInput{
		Requested: domain.PlacementOverrideDirectBranch,
		Reason:    "the isolated checkout could not be created",
		// The operator asserts what they read. It matches, so it is not a guard
		// that fires here — but it is recorded either way.
		ExpectedState:      first.State,
		ExpectedGeneration: first.PlacementGeneration,
	})
	if !outcome.Applied {
		t.Fatalf("transition refused: %s (%s)", outcome.Refusal, outcome.Detail)
	}
	if outcome.To.PlacementGeneration != first.PlacementGeneration+1 {
		t.Fatalf("replacement generation = %d, want %d",
			outcome.To.PlacementGeneration, first.PlacementGeneration+1)
	}
	if outcome.To.Type != domain.PlacementDirectBranch {
		t.Fatalf("replacement type = %s, want direct_branch", outcome.To.Type)
	}
	if outcome.To.LifecycleGeneration != first.LifecycleGeneration {
		t.Fatalf("a placement transition moved the LIFECYCLE generation %d -> %d; "+
			"replacing a placement is not retrying the obligation",
			first.LifecycleGeneration, outcome.To.LifecycleGeneration)
	}
	if !outcome.Quiescence.Quiesced() || outcome.Quiescence.Digest == "" {
		t.Fatalf("an applied transition must carry a complete proof: %+v", outcome.Quiescence)
	}

	// §B.8: the old placement remains auditable, and an isolated placement
	// whose work never landed is PRESERVED rather than merely terminal — those
	// commits may be the only copy.
	placements, err := f.coord.ListPlacements(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var old, replacement workflowcore.PlacementView
	for _, p := range placements {
		switch p.PlacementGeneration {
		case first.PlacementGeneration:
			old = p
		case outcome.To.PlacementGeneration:
			replacement = p
		}
	}
	if old.PlacementGeneration == 0 {
		t.Fatal("the superseded placement was not retained; it must stay auditable")
	}
	if old.State != domain.PlacementPreserved {
		t.Fatalf("superseded isolated placement is %s, want preserved", old.State)
	}
	if old.Current {
		t.Fatal("the superseded generation still reports itself current")
	}
	if !replacement.Current {
		t.Fatal("the replacement generation does not report itself current")
	}

	// The transition is bound to everything §C asks for.
	transitions, err := f.coord.ListPlacementTransitions(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 1 {
		t.Fatalf("recorded %d transitions, want 1", len(transitions))
	}
	tr := transitions[0]
	switch {
	case tr.State != domain.PlacementTransitionApplied:
		t.Fatalf("transition state = %s, want applied", tr.State)
	case tr.FromGeneration != first.PlacementGeneration || tr.ToGeneration != outcome.To.PlacementGeneration:
		t.Fatalf("transition binds %d->%d, want %d->%d",
			tr.FromGeneration, tr.ToGeneration, first.PlacementGeneration, outcome.To.PlacementGeneration)
	case tr.RequestedBy != "operator-1":
		t.Fatalf("transition requester = %q, want the operator identity", tr.RequestedBy)
	case tr.Reason == "":
		t.Fatal("transition recorded no reason")
	case tr.FromType != first.Type:
		t.Fatalf("transition source provenance = %s, want %s", tr.FromType, first.Type)
	case tr.Quiescence == "":
		t.Fatal("transition recorded no quiescence proof")
	}
}

// §B.10: a repeated transition request is idempotent. It returns the transition
// that already happened rather than minting a third generation.
func TestRepeatedTransitionRequestIsIdempotent(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	if _, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step); err != nil || !ok {
		t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
	}

	first := transitionTo(t, f, workflowcore.PlacementTransitionInput{
		Requested: domain.PlacementOverrideDirectBranch, Reason: "first",
	})
	if !first.Applied {
		t.Fatalf("first transition refused: %s", first.Refusal)
	}

	// The same request again, exactly as a double-clicked button or a retried
	// HTTP call would send it. It must not supersede the replacement.
	second := transitionTo(t, f, workflowcore.PlacementTransitionInput{
		Requested: domain.PlacementOverrideDirectBranch, Reason: "first",
		ExpectedGeneration: first.From.PlacementGeneration,
	})
	if second.Applied {
		t.Fatal("the repeated request minted a second replacement")
	}
	if second.Refusal != "" && second.Refusal != domain.PlacementTransitionNotCurrent {
		t.Fatalf("unexpected refusal %s", second.Refusal)
	}

	// And with no expectation at all — targeting whatever is current — it must
	// still not create a third generation without a fresh reason to.
	newest, err := f.coord.ListPlacements(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(newest) != 2 {
		t.Fatalf("%d placement generations exist, want exactly 2", len(newest))
	}
}

// §B.9: a caller holding the superseded generation may do nothing with it. This
// is the property every other guard in the placement model rests on.
func TestStaleGenerationCannotMutateTheReplacementPlacement(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	first, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
	}
	outcome := transitionTo(t, f, workflowcore.PlacementTransitionInput{
		Requested: domain.PlacementOverrideDirectBranch, Reason: "replace it",
	})
	if !outcome.Applied {
		t.Fatalf("transition refused: %s", outcome.Refusal)
	}

	// The stale generation cannot integrate.
	moved, err := f.store.MarkExecutionPlacementIntegrated(ctx,
		f.run.ID, "", "", first.PlacementGeneration, "deadbeefdeadbeef", nowForPlacementTest())
	if err != nil {
		t.Fatal(err)
	}
	if moved {
		t.Fatal("a superseded placement generation was allowed to record an integration")
	}

	// And it cannot describe the replacement's checkout.
	prepared, err := f.store.RecordExecutionPlacementPreparation(ctx,
		f.run.ID, "", "", first.PlacementGeneration, "cafebabe", "/tmp/stale", "task-x", nowForPlacementTest())
	if err != nil {
		t.Fatal(err)
	}
	if prepared {
		t.Fatal("a superseded placement generation was allowed to record preparation")
	}

	// The replacement is untouched by either attempt.
	live := f.live(t)
	if live.PlacementGeneration != outcome.To.PlacementGeneration {
		t.Fatalf("live placement is generation %d, want %d", live.PlacementGeneration, outcome.To.PlacementGeneration)
	}
	if live.IntegratedSHA != "" || live.WorktreePath != "" {
		t.Fatalf("the stale generation wrote onto the replacement: %+v", live)
	}
}

// §B.5: a transition needs operator authority. An unattributed re-pointing of a
// running obligation is exactly what the model exists to make impossible.
func TestTransitionWithoutOperatorAuthorityIsRefused(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	if _, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step); err != nil || !ok {
		t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
	}

	outcome, err := f.coord.TransitionPlacement(ctx, workflowcore.PlacementTransitionInput{
		RunID: f.run.ID, Requested: domain.PlacementOverrideDirectBranch, RequestedBy: "   ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Applied {
		t.Fatal("an unattributed transition was applied")
	}
	if outcome.Refusal != domain.PlacementTransitionNoAuthority {
		t.Fatalf("refusal = %s, want no_operator_authority", outcome.Refusal)
	}
}

// §C: a request made against a state that has since moved is refused, not
// silently applied to whatever is true now. The operator's reading is stale, and
// acting on it would act on a world that no longer exists.
func TestTransitionAgainstADriftedStateIsRefused(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	first, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
	}
	if first.State != domain.PlacementSelected {
		t.Fatalf("a freshly frozen placement is %s, expected selected", first.State)
	}

	outcome := transitionTo(t, f, workflowcore.PlacementTransitionInput{
		Requested:     domain.PlacementOverrideDirectBranch,
		ExpectedState: domain.PlacementReady, // it is `selected`
	})
	if outcome.Applied {
		t.Fatal("a transition against a drifted state was applied")
	}
	if outcome.Refusal != domain.PlacementTransitionStateDrifted {
		t.Fatalf("refusal = %s, want lifecycle_state_drifted", outcome.Refusal)
	}

	// §B.4: a stale generation number is refused for the same reason.
	stale := transitionTo(t, f, workflowcore.PlacementTransitionInput{
		Requested:          domain.PlacementOverrideDirectBranch,
		ExpectedGeneration: first.PlacementGeneration + 7,
	})
	if stale.Applied {
		t.Fatal("a transition naming a generation that is not current was applied")
	}
	if stale.Refusal != domain.PlacementTransitionNotCurrent {
		t.Fatalf("refusal = %s, want placement_not_current", stale.Refusal)
	}
}

// §B.6/§B.7 and §C: the quiescence proof. Each authority that can still own the
// old placement refuses the transition by name, and every refusal is durable.
func TestTransitionIsRefusedWhileAnyAuthorityStillOwnsThePlacement(t *testing.T) {
	ctx := context.Background()

	t.Run("outstanding integration authority", func(t *testing.T) {
		f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
		first, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
		if err != nil || !ok {
			t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
		}
		// A merge is half-applied by definition while `integrating` holds.
		if moved, terr := f.store.TransitionExecutionPlacement(ctx, f.run.ID, "", "",
			first.PlacementGeneration, first.State, domain.PlacementIntegrating, "", "merging",
			nowForPlacementTest()); terr != nil || !moved {
			t.Fatalf("could not set up the integrating state: moved=%v err=%v", moved, terr)
		}
		outcome := transitionTo(t, f, workflowcore.PlacementTransitionInput{
			Requested: domain.PlacementOverrideDirectBranch,
		})
		if outcome.Applied {
			t.Fatal("a placement was moved out from under an in-progress integration")
		}
		if outcome.Refusal != domain.PlacementTransitionIntegrating {
			t.Fatalf("refusal = %s, want outstanding_integration", outcome.Refusal)
		}
		if outcome.Quiescence.NoIntegrationAuthority {
			t.Fatal("the proof claims no integration authority while the placement is integrating")
		}
	})

	t.Run("terminal run", func(t *testing.T) {
		f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
		if _, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step); err != nil || !ok {
			t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
		}
		if _, err := f.store.UpdateWorkflowRunState(ctx, f.run.ID, f.run.State,
			domain.WorkflowRunCompleted, nowForPlacementTest()); err != nil {
			t.Fatal(err)
		}
		outcome := transitionTo(t, f, workflowcore.PlacementTransitionInput{
			Requested: domain.PlacementOverrideDirectBranch,
		})
		if outcome.Applied {
			t.Fatal("a replacement placement was minted for a run that is over")
		}
		if outcome.Refusal != domain.PlacementTransitionRunTerminal {
			t.Fatalf("refusal = %s, want run_is_terminal", outcome.Refusal)
		}
	})

	t.Run("refusals are durable and do not block a later yes", func(t *testing.T) {
		f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
		first, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
		if err != nil || !ok {
			t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
		}
		// Refuse twice for the same generation, then succeed. A refusal that
		// occupied the idempotency key would make the first "not yet"
		// permanent, which is the bug the partial index exists to avoid.
		for i := 0; i < 2; i++ {
			refused := transitionTo(t, f, workflowcore.PlacementTransitionInput{
				Requested: domain.PlacementOverrideDirectBranch, ExpectedState: domain.PlacementReady,
			})
			if refused.Applied {
				t.Fatal("a drifted-state transition was applied")
			}
		}
		applied := transitionTo(t, f, workflowcore.PlacementTransitionInput{
			Requested: domain.PlacementOverrideDirectBranch, ExpectedState: first.State,
		})
		if !applied.Applied {
			t.Fatalf("a transition that is now safe was refused: %s (%s)", applied.Refusal, applied.Detail)
		}

		transitions, lerr := f.coord.ListPlacementTransitions(ctx, f.run.ID)
		if lerr != nil {
			t.Fatal(lerr)
		}
		refusals, applies := 0, 0
		for _, tr := range transitions {
			switch tr.State {
			case domain.PlacementTransitionRefused:
				refusals++
				if tr.RefusalReason == "" {
					t.Fatal("a refusal was recorded without naming the authority")
				}
			case domain.PlacementTransitionApplied:
				applies++
			}
		}
		if refusals != 2 || applies != 1 {
			t.Fatalf("recorded %d refusals and %d applications, want 2 and 1", refusals, applies)
		}
	})
}

// §C: an isolated replacement generation gets its OWN branch. Git refuses
// refs/heads/a/b while refs/heads/a exists, so a nested name would make the
// replacement uncreatable exactly when its predecessor is being preserved.
func TestIsolatedReplacementGetsItsOwnExecutionBranch(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	first, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
	}

	outcome := transitionTo(t, f, workflowcore.PlacementTransitionInput{
		Requested: domain.PlacementOverrideIsolatedWorktree,
		Reason:    "the checkout was corrupt",
	})
	if !outcome.Applied {
		t.Fatalf("transition refused: %s (%s)", outcome.Refusal, outcome.Detail)
	}
	if outcome.To.ExecutionBranch == first.ExecutionBranch {
		t.Fatalf("the replacement reuses branch %q; the preserved predecessor still holds it",
			first.ExecutionBranch)
	}
	if !strings.HasPrefix(outcome.To.ExecutionBranch, first.ExecutionBranch) {
		t.Fatalf("replacement branch %q is unrelated to %q", outcome.To.ExecutionBranch, first.ExecutionBranch)
	}
	if strings.HasPrefix(outcome.To.ExecutionBranch, first.ExecutionBranch+"/") {
		t.Fatalf("replacement branch %q nests under the predecessor and git will refuse to create it",
			outcome.To.ExecutionBranch)
	}
	// The replacement has not been cut yet, so it must not claim a base commit
	// it was never made from.
	if outcome.To.BaseSHA != "" {
		t.Fatalf("replacement claims base %q before it exists", outcome.To.BaseSHA)
	}
	// The merge target is inherited, not re-derived: a transition changes where
	// the work happens, never where it lands.
	if outcome.To.MergeTarget != first.MergeTarget {
		t.Fatalf("replacement merge target %q != %q", outcome.To.MergeTarget, first.MergeTarget)
	}
}

// §B: a placement request AO cannot read is refused rather than coerced to the
// default. Substituting `auto` would hand an operator a placement they did not
// ask for, with no signal that it happened.
func TestUnknownPlacementRequestIsRefusedNotCoerced(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()

	if _, err := f.coord.RequestPlacementOverride(ctx, workflowcore.PlacementOverrideRequestInput{
		RunID: f.run.ID, Requested: domain.PlacementOverrideRequest("somewhere_else"), RequestedBy: "operator-1",
	}); err == nil {
		t.Fatal("an unknown placement request was accepted")
	}

	if _, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step); err != nil || !ok {
		t.Fatalf("EnsureExecutionPlacement: ok=%v err=%v", ok, err)
	}
	outcome := transitionTo(t, f, workflowcore.PlacementTransitionInput{
		Requested: domain.PlacementOverrideRequest("somewhere_else"),
	})
	if outcome.Applied {
		t.Fatal("an unknown placement request produced a transition")
	}
	if outcome.Refusal != domain.PlacementTransitionUnknownRequest {
		t.Fatalf("refusal = %s, want unknown_placement_request", outcome.Refusal)
	}
}

// §B.5: a transition asked for before anything is frozen says so by name. It
// does not freeze something on the operator's behalf — a freeze needs the step
// this run has not necessarily reached.
func TestTransitionBeforeAnyFreezeNamesTheMissingPlacement(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	outcome := transitionTo(t, f, workflowcore.PlacementTransitionInput{
		Requested: domain.PlacementOverrideDirectBranch,
	})
	if outcome.Applied {
		t.Fatal("a transition was applied with nothing frozen to transition from")
	}
	if outcome.Refusal != domain.PlacementTransitionNoPlacement {
		t.Fatalf("refusal = %s, want no_frozen_placement", outcome.Refusal)
	}
}

// nowForPlacementTest is the timestamp these tests hand to store writes that
// take one. The coordinator has its own injectable clock; this is only for the
// direct store calls a test makes to set up or inspect a state.
func nowForPlacementTest() time.Time { return time.Now().UTC() }
