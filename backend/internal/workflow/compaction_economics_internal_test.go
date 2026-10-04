package workflow

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// compaction_economics_internal_test.go -- the classification the independent
// integration review of P7.2B2 required, asserted directly rather than through a
// projection.
//
// A shadow economic verdict records something ABOUT a run and never something
// the run DID. If it were folded into the run's derived state it could displace
// LatestCheckpointPhase -- which the lifecycle derivation reads -- and a row that
// governs nothing would be renaming the run's last activity. That is the exact
// hazard isBookkeepingPhase exists to close, and this is the test that fails if
// somebody removes the entry.

func TestTheShadowEconomicVerdictIsBookkeeping(t *testing.T) {
	if !isBookkeepingPhase(CompactionEconomicsDurablePhase) {
		t.Fatalf("%q must be bookkeeping: it cannot, on its own, change what the run owes, "+
			"and counting it lets it displace the run's latest phase",
			CompactionEconomicsDurablePhase)
	}
}

func TestTheShadowEconomicVerdictCanNeverBecomeAStopOrALifecyclePhase(t *testing.T) {
	got := classifyCheckpointPhase(CompactionEconomicsDurablePhase)
	if got != authorityObservation {
		t.Fatalf("classifyCheckpointPhase(%q) = %v, want authorityObservation. "+
			"An unknown phase defaults to LIFECYCLE, which counts in the run's timeline; "+
			"a verdict nobody reads must not.",
			CompactionEconomicsDurablePhase, got)
	}
}

// The fold is the thing that actually matters, so assert it directly: a shadow
// verdict written LAST, at the newest timestamp, must not become the run's latest
// phase.
func TestAShadowVerdictDoesNotDisplaceTheRunsLatestPhase(t *testing.T) {
	at := func(phase string, offsetSeconds int) domain.WorkflowCheckpoint {
		return domain.WorkflowCheckpoint{
			DurablePhase: phase,
			CreatedAt:    time.Date(2026, 9, 13, 12, 0, offsetSeconds, 0, time.UTC),
		}
	}
	base := at("fix_dispatched", 0)
	shadow := at(CompactionEconomicsDurablePhase, 1)

	fold := foldCheckpointAuthority([]domain.WorkflowCheckpoint{base, shadow})
	if fold.LatestPhase != "fix_dispatched" {
		t.Errorf("latest phase = %q, want fix_dispatched; the shadow verdict displaced the run's timeline",
			fold.LatestPhase)
	}
	if fold.StopPhase != "" {
		t.Errorf("stop phase = %q, want none; a shadow verdict is not a stop", fold.StopPhase)
	}
}
