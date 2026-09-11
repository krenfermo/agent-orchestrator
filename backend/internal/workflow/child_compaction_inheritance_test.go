package workflow_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// child_compaction_inheritance_test.go -- PHASE B, against a real store.
//
// The domain-level rules live in internal/domain; these prove the wiring that
// carries them: inheritExecutionPolicySnapshot copying the choice onto the
// child row, stamping which parent it came from, and
// requireInheritedExecutionPolicy accepting the result.
//
// It matters more here than for any other inherited field. A child run is
// where an objective's fix cycles execute, so a parent opted into compaction
// whose children were not would spend the whole objective's repair budget on
// uncompacted conversations while the parent's own row said it had opted in --
// the F1 failure shape exactly, and invisible for the same reason.

// TestChildInheritsParentSessionCompactionChoice: both directions travel, and
// the child's record names the parent it came from.
func TestChildInheritsParentSessionCompactionChoice(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "opted in", false: "explicitly refused"}[enabled], func(t *testing.T) {
			c, store, parentID := f1Parent(t, domain.QuestionAutonomyAutoDecideLowRisk, domain.RepairModeAutomatic)
			if err := c.ApplySessionCompactionPolicy(context.Background(), parentID, enabled, "operator-1"); err != nil {
				t.Fatalf("ApplySessionCompactionPolicy: %v", err)
			}
			childID := f1FirstChild(t, c, parentID)

			parent, child := f1PolicyOf(t, store, parentID), f1PolicyOf(t, store, childID)
			if got, recorded := parent.SessionCompactionRequested(); !recorded || got != enabled {
				t.Fatalf("parent = (%t, %t), want (%t, true) (fixture broken)", got, recorded, enabled)
			}
			if child.SessionCompactionEnabled != enabled {
				t.Errorf("child compaction = %t, want the parent's %t", child.SessionCompactionEnabled, enabled)
			}
			if child.CompactionProvenance.Source != domain.SessionCompactionInherited {
				t.Errorf("child source = %q, want inherited", child.CompactionProvenance.Source)
			}
			if child.CompactionProvenance.ParentRunID != parentID {
				t.Errorf("child parentRunId = %q, want %q", child.CompactionProvenance.ParentRunID, parentID)
			}
			if child.CompactionProvenance.RequestedBy != "operator-1" {
				t.Errorf("child lost the requester: %+v", child.CompactionProvenance)
			}
			if err := domain.RequireInheritedWorkflowPolicy(parent, child); err != nil {
				t.Errorf("RequireInheritedWorkflowPolicy: %v", err)
			}
		})
	}
}

// A parent nobody asked produces a child nobody asked. Inheritance carries
// decisions, never defaults -- so the opt-in cannot spread by accident through
// the one mechanism that touches every child run AO creates.
func TestChildOfAnUnaskedParentStaysAtTheCompactionDefault(t *testing.T) {
	c, store, parentID := f1Parent(t, domain.QuestionAutonomyAutoDecideLowRisk, domain.RepairModeAutomatic)
	childID := f1FirstChild(t, c, parentID)

	parent, child := f1PolicyOf(t, store, parentID), f1PolicyOf(t, store, childID)
	if _, recorded := parent.SessionCompactionRequested(); recorded {
		t.Fatalf("the parent recorded a choice nobody made: %+v", parent.CompactionProvenance)
	}
	if child.SessionCompactionEnabled {
		t.Error("a child of a parent nobody asked has compaction enabled")
	}
	if child.CompactionProvenance.Recorded() {
		t.Errorf("a child invented a compaction record: %+v", child.CompactionProvenance)
	}
	if err := domain.RequireInheritedWorkflowPolicy(parent, child); err != nil {
		t.Errorf("RequireInheritedWorkflowPolicy refused an untouched pair: %v", err)
	}
}

// The context-pressure threshold travels with the parent's usage policy, so
// both halves of a run family are judged against the same line. Without this
// an opted-in objective would compact its parent at one threshold and its
// children at another.
func TestChildInheritsParentContextPerCallThreshold(t *testing.T) {
	c, store, parentID := f1Parent(t, domain.QuestionAutonomyAutoDecideLowRisk, domain.RepairModeAutomatic)
	ctx := context.Background()
	if err := c.ApplyContextPerCallWarnTokens(ctx, parentID, 70_000); err != nil {
		t.Fatalf("ApplyContextPerCallWarnTokens: %v", err)
	}
	if err := c.ApplySessionCompactionPolicy(ctx, parentID, true, "operator-1"); err != nil {
		t.Fatalf("ApplySessionCompactionPolicy: %v", err)
	}
	childID := f1FirstChild(t, c, parentID)

	parent, child := f1PolicyOf(t, store, parentID), f1PolicyOf(t, store, childID)
	parentThreshold := domain.UsageBudgetProfileFor(parent.Strategy.Effective).
		WithOverrides(parent.EffectiveUsageBudgetPolicy()).ContextPerCallTokens
	childThreshold := domain.UsageBudgetProfileFor(child.Strategy.Effective).
		WithOverrides(child.EffectiveUsageBudgetPolicy()).ContextPerCallTokens
	if parentThreshold != 70_000 {
		t.Fatalf("parent threshold = %d, want 70000 (fixture broken)", parentThreshold)
	}
	if childThreshold != 70_000 {
		t.Errorf("child threshold = %d, want the parent's 70000", childThreshold)
	}
	if err := domain.RequireInheritedWorkflowPolicy(parent, child); err != nil {
		t.Errorf("RequireInheritedWorkflowPolicy: %v", err)
	}
}
