package workflow_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// p5_placement_carry_test.go — P5: a replacement is not a re-decision.
//
// ReplaceExecutionPlacement exists for an obligation whose PHYSICAL placement
// has to change: the worktree is gone, the checkout has to be remade. It did
// that by re-running selection, and selection reads the project's CURRENT
// execution mode. So the freeze held right up until the first thing that mints
// a new generation, and then quietly let go:
//
//	a task is frozen into isolated_worktree and writes code
//	  -> a person switches the project to direct_branch
//	  -> the worktree needs recreating, so the placement is replaced
//	  -> the replacement is direct_branch, with no transition, no override
//	     consumed, and no record that the KIND of placement ever changed
//
// which is the config-drift defect the freeze was introduced to end,
// reappearing on the one path that writes a fresh record.
//
// The rule these tests state: a replacement carries the frozen TYPE forward,
// re-derives everything a replacement legitimately should, and still yields to
// an operator who explicitly asks for something else.

// The headline. The project flips underneath a frozen isolated placement and
// the replacement is still isolated.
func TestReplacementCarriesTheFrozenTypeAcrossAProjectConfigFlip(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()

	first, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("first freeze: ok=%v err=%v", ok, err)
	}
	if first.Type != domain.PlacementIsolatedWorktree {
		t.Fatalf("first placement = %s, want isolated_worktree", first.Type)
	}

	// A person changes the project setting while the task is mid-flight. This
	// is the ordinary path through the project store, so what the coordinator
	// reads is exactly what the app would have written.
	setProjectExecutionMode(t, f.store, domain.ExecutionDirectBranch)

	replacement, err := f.coord.ReplaceExecutionPlacement(ctx, f.run, f.step, "the checkout had to be recreated")
	if err != nil {
		t.Fatalf("ReplaceExecutionPlacement: %v", err)
	}
	if replacement.Type != domain.PlacementIsolatedWorktree {
		t.Fatalf("replacement placement = %s, want isolated_worktree: recreating a checkout must not move the work onto the operator's own branch",
			replacement.Type)
	}
	if replacement.PlacementGeneration <= first.PlacementGeneration {
		t.Fatalf("replacement generation = %d, want newer than %d",
			replacement.PlacementGeneration, first.PlacementGeneration)
	}
	// A replacement is a NEW physical placement, so it gets its own branch --
	// carrying the type must not accidentally carry the identity of the
	// checkout being replaced.
	if replacement.ExecutionBranch == first.ExecutionBranch {
		t.Fatalf("replacement reused branch %q; a new generation is a new checkout", replacement.ExecutionBranch)
	}
	if live := f.live(t); live.PlacementGeneration != replacement.PlacementGeneration {
		t.Fatalf("live placement generation = %d, want the replacement's %d",
			live.PlacementGeneration, replacement.PlacementGeneration)
	}
}

// The converse, so the rule is not "always isolated": a frozen direct-branch
// placement stays direct-branch when the project flips the other way.
func TestReplacementCarriesADirectBranchTypeAcrossAProjectConfigFlip(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionDirectBranch)
	ctx := context.Background()

	first, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("first freeze: ok=%v err=%v", ok, err)
	}
	if first.Type != domain.PlacementDirectBranch {
		t.Fatalf("first placement = %s, want direct_branch", first.Type)
	}

	setProjectExecutionMode(t, f.store, domain.ExecutionIsolatedWorktree)

	replacement, err := f.coord.ReplaceExecutionPlacement(ctx, f.run, f.step, "the branch lock was retaken")
	if err != nil {
		t.Fatalf("ReplaceExecutionPlacement: %v", err)
	}
	if replacement.Type != domain.PlacementDirectBranch {
		t.Fatalf("replacement placement = %s, want direct_branch", replacement.Type)
	}
	if replacement.WorktreePath != "" {
		t.Fatalf("a direct-branch replacement recorded worktree %q", replacement.WorktreePath)
	}
}

// The carry yields to an operator. A transition is exactly the mechanism for
// changing a placement on purpose, so an explicit request still wins.
func TestAnExplicitOverrideStillWinsOverTheCarriedType(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()

	first, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil || !ok {
		t.Fatalf("first freeze: ok=%v err=%v", ok, err)
	}
	if first.Type != domain.PlacementIsolatedWorktree {
		t.Fatalf("first placement = %s, want isolated_worktree", first.Type)
	}

	requestOverride(t, f, domain.PlacementOverrideDirectBranch)

	replacement, err := f.coord.ReplaceExecutionPlacement(ctx, f.run, f.step, "operator asked for the branch")
	if err != nil {
		t.Fatalf("ReplaceExecutionPlacement: %v", err)
	}
	if replacement.Type != domain.PlacementDirectBranch {
		t.Fatalf("replacement placement = %s, want direct_branch: an explicit override must outrank the carried type",
			replacement.Type)
	}
}

// A replacement must remain possible for the case that most needs one. The
// usual reason to replace an isolated placement is that its worktree is gone --
// which is exactly the evidence legacy recovery looks for, so a replacement
// routed through recovery would fail closed on the one obligation whose own
// record already answers the question.
func TestReplacementSucceedsForARunThatHasAlreadyExecuted(t *testing.T) {
	f := newPlacementFixtureWithMode(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()

	if _, ok, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step); err != nil || !ok {
		t.Fatalf("first freeze: ok=%v err=%v", ok, err)
	}
	// The step has run: an attempt exists, which is what runHasExecuted reads.
	if _, err := f.store.CreateWorkflowAttempt(ctx, "wfa-p5-1", f.step.ID, "claude-code", "opus", f.run.CreatedAt); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}

	replacement, err := f.coord.ReplaceExecutionPlacement(ctx, f.run, f.step, "the worktree was deleted")
	if err != nil {
		t.Fatalf("ReplaceExecutionPlacement for an executed run: %v", err)
	}
	if replacement.Type != domain.PlacementIsolatedWorktree {
		t.Fatalf("replacement placement = %s, want isolated_worktree", replacement.Type)
	}
	if replacement.Provenance != domain.PlacementFrozenAtSelection {
		t.Fatalf("provenance = %s, want frozen_at_selection: the replacement was decided, not reconstructed",
			replacement.Provenance)
	}
}
