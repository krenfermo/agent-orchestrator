package domain_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The AUTO policy is the one place a strategy can be chosen without a person,
// so it is tested as a table of the exact decisions it is allowed to make. A
// rule that is not here is a rule that does not exist.
func TestSelectExecutionStrategyIsDeterministicAndExplainable(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	auto := domain.RequestedExecutionStrategyAuto
	tests := []struct {
		name      string
		requested domain.RequestedExecutionStrategy
		signals   domain.ExecutionStrategySignals
		want      domain.ExecutionStrategy
		source    domain.ExecutionStrategySource
		reason    domain.ExecutionStrategyReason
	}{
		{
			name:      "explicit task wins over every signal that would say master",
			requested: domain.RequestedExecutionStrategyTask,
			signals:   domain.ExecutionStrategySignals{MultiWorkstream: true, RepositoryCount: 9},
			want:      domain.ExecutionStrategyTask,
			source:    domain.ExecutionStrategyExplicit,
			reason:    domain.ExecutionStrategyReasonExplicitRequest,
		},
		{
			name:      "explicit autonomous is honoured",
			requested: domain.RequestedExecutionStrategyAutonomous,
			want:      domain.ExecutionStrategyAutonomous,
			source:    domain.ExecutionStrategyExplicit,
			reason:    domain.ExecutionStrategyReasonExplicitRequest,
		},
		{
			name:      "explicit master is honoured",
			requested: domain.RequestedExecutionStrategyMaster,
			want:      domain.ExecutionStrategyMaster,
			source:    domain.ExecutionStrategyExplicit,
			reason:    domain.ExecutionStrategyReasonExplicitRequest,
		},
		{
			name:      "auto: a small bounded change is a task",
			requested: auto,
			signals:   domain.ExecutionStrategySignals{Size: domain.ExecutionWorkSizeSmall, ExpectedSteps: 1},
			want:      domain.ExecutionStrategyTask,
			source:    domain.ExecutionStrategyPolicy,
			reason:    domain.ExecutionStrategyReasonBoundedWork,
		},
		{
			name:      "auto: one declared step with no size is still a task",
			requested: auto,
			signals:   domain.ExecutionStrategySignals{ExpectedSteps: 1},
			want:      domain.ExecutionStrategyTask,
			source:    domain.ExecutionStrategyPolicy,
			reason:    domain.ExecutionStrategyReasonBoundedWork,
		},
		{
			name:      "auto: normal multi-step project work is autonomous",
			requested: auto,
			signals:   domain.ExecutionStrategySignals{ExpectedSteps: 6, Size: domain.ExecutionWorkSizeMedium},
			want:      domain.ExecutionStrategyAutonomous,
			source:    domain.ExecutionStrategyPolicy,
			reason:    domain.ExecutionStrategyReasonMultiStepProject,
		},
		{
			name:      "auto: no signals at all is autonomous, the safe default for project work",
			requested: auto,
			want:      domain.ExecutionStrategyAutonomous,
			source:    domain.ExecutionStrategyPolicy,
			reason:    domain.ExecutionStrategyReasonMultiStepProject,
		},
		{
			name:      "auto: a multi-workstream initiative is master",
			requested: auto,
			signals:   domain.ExecutionStrategySignals{MultiWorkstream: true},
			want:      domain.ExecutionStrategyMaster,
			source:    domain.ExecutionStrategyPolicy,
			reason:    domain.ExecutionStrategyReasonMultiWorkstream,
		},
		{
			name:      "auto: a declared need to decompose is master",
			requested: auto,
			signals:   domain.ExecutionStrategySignals{RequiresDecomposition: true},
			want:      domain.ExecutionStrategyMaster,
			source:    domain.ExecutionStrategyPolicy,
			reason:    domain.ExecutionStrategyReasonDecompositionRequired,
		},
		{
			name:      "auto: more than one repository is master",
			requested: auto,
			signals:   domain.ExecutionStrategySignals{RepositoryCount: 2},
			want:      domain.ExecutionStrategyMaster,
			source:    domain.ExecutionStrategyPolicy,
			reason:    domain.ExecutionStrategyReasonMultiRepository,
		},
		{
			name:      "auto: a supplied plan hierarchy is planned, not decomposed",
			requested: auto,
			signals:   domain.ExecutionStrategySignals{SuppliedPlanHierarchy: true, Size: domain.ExecutionWorkSizeSmall},
			want:      domain.ExecutionStrategyAutonomous,
			source:    domain.ExecutionStrategyPolicy,
			reason:    domain.ExecutionStrategyReasonSuppliedPlanHierarchy,
		},
		{
			name:      "auto: a large declared size alone never reaches master",
			requested: auto,
			signals:   domain.ExecutionStrategySignals{Size: domain.ExecutionWorkSizeLarge},
			want:      domain.ExecutionStrategyAutonomous,
			source:    domain.ExecutionStrategyPolicy,
			reason:    domain.ExecutionStrategyReasonMultiStepProject,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := domain.SelectExecutionStrategy(tt.requested, tt.signals, now)
			if got.Effective != tt.want || got.Source != tt.source || got.Reason != tt.reason {
				t.Fatalf("SelectExecutionStrategy = %+v, want effective=%s source=%s reason=%s",
					got, tt.want, tt.source, tt.reason)
			}
			if got.PolicyVersion != domain.ExecutionStrategyPolicyVersion {
				t.Fatalf("policy version = %q, want %q", got.PolicyVersion, domain.ExecutionStrategyPolicyVersion)
			}
			if !got.Recorded() {
				t.Fatalf("selection %+v is not recordable", got)
			}
			// Same inputs, same answer -- the property that lets a persisted
			// decision be replayed rather than merely believed.
			if again := domain.SelectExecutionStrategy(tt.requested, tt.signals, now); again != got {
				t.Fatalf("selection is not deterministic: %+v vs %+v", got, again)
			}
		})
	}
}

func TestRequestedExecutionStrategyRejectsAnythingElse(t *testing.T) {
	for _, raw := range []string{"", "manual", "Master ", "TASK", "planner", "auto-master"} {
		requested := domain.NormalizeRequestedExecutionStrategy(raw)
		want := raw == "Master " || raw == "TASK"
		if got := requested.Valid(); got != want {
			t.Fatalf("NormalizeRequestedExecutionStrategy(%q).Valid() = %v, want %v", raw, got, want)
		}
	}
	if _, ok := domain.RequestedExecutionStrategyAuto.Explicit(); ok {
		t.Fatal("auto must never name a canonical strategy")
	}
}

// A child is never master and never deeper than the bound, whatever its
// parent is. This is the whole of AO's protection against uncontrolled
// recursive decomposition, so it is asserted for every parent strategy.
func TestChildExecutionStrategyIsBoundedAndNeverMaster(t *testing.T) {
	now := time.Now().UTC()
	for _, parentStrategy := range []domain.ExecutionStrategy{
		domain.ExecutionStrategyTask, domain.ExecutionStrategyAutonomous, domain.ExecutionStrategyMaster,
	} {
		parent := domain.ExecutionStrategySelection{
			Effective: parentStrategy, Source: domain.ExecutionStrategyExplicit,
			PolicyVersion: domain.ExecutionStrategyPolicyVersion,
		}
		child := domain.ChildExecutionStrategy(parent, "wf-parent", now)
		if child.Effective == domain.ExecutionStrategyMaster {
			t.Fatalf("parent %s produced a master child", parentStrategy)
		}
		if child.Effective != domain.ExecutionStrategyTask {
			t.Fatalf("parent %s child strategy = %s, want task", parentStrategy, child.Effective)
		}
		if child.Source != domain.ExecutionStrategyInherited || child.ParentRunID != "wf-parent" {
			t.Fatalf("child provenance = %+v, want inherited from wf-parent", child)
		}
		if child.Depth != domain.ExecutionStrategyMaxChildDepth {
			t.Fatalf("child depth = %d, want %d", child.Depth, domain.ExecutionStrategyMaxChildDepth)
		}
		// A grandchild cannot push past the bound either.
		grandchild := domain.ChildExecutionStrategy(child, "wf-child", now)
		if grandchild.Depth > domain.ExecutionStrategyMaxChildDepth {
			t.Fatalf("grandchild depth = %d, want <= %d", grandchild.Depth, domain.ExecutionStrategyMaxChildDepth)
		}
	}
}

func TestExecutionStrategySelectionRecordedRejectsZeroValue(t *testing.T) {
	if (domain.ExecutionStrategySelection{}).Recorded() {
		t.Fatal("the zero selection -- what a pre-P1-A run carries -- must never read as recorded")
	}
	if (domain.ExecutionStrategySelection{Effective: domain.ExecutionStrategyTask}).Recorded() {
		t.Fatal("a selection with no source is not a decision anybody took")
	}
}
