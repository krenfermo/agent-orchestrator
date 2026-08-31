package workflow

// checkpoint_authority_internal_test.go — the policy itself, tested as a policy.
//
// The incident these pin is wf-c4c84f52: a run parked on reviewer_launch_failed
// that then re-read one approved verdict 302 times, and whose stop reason every
// reader consequently lost. The unit under test is the rule that decides which
// checkpoint is allowed to answer "why is this run stopped".

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func cp(phase string, at time.Time) domain.WorkflowCheckpoint {
	return domain.WorkflowCheckpoint{DurablePhase: phase, CreatedAt: at}
}

// Every phase named in the audit, classified explicitly. This is the policy
// document: a phase moving between categories has to move here first.
func TestCheckpointPhaseAuthorityIsExplicit(t *testing.T) {
	cases := []struct {
		phase string
		want  checkpointAuthority
	}{
		// Stops: the registry is the single source of truth.
		{ReasonFixBudgetExhausted, authorityStop},
		{ReasonFixNoVerifiableChange, authorityStop},
		{ReasonReviewerLaunchFailed, authorityStop},
		{ReasonReviewerAuthInvalid, authorityStop},
		{ReasonReviewStateAmbiguous, authorityStop},
		{branchWaitPhase, authorityStop},

		// The one clear.
		{attentionClearedPhase, authorityStopCleared},

		// Observations that are part of the run's story but decide nothing.
		{reviewObservedPhase, authorityObservation},
		{"review_launch_intent", authorityObservation},
		{"review_launch_attempt", authorityObservation},
		{"review_launch_claimed", authorityObservation},
		{"review_launch_confirmed", authorityObservation},
		{"review_launch_abandoned", authorityObservation},
		{"routing_decision", authorityObservation},
		{"session_lifecycle_decision", authorityObservation},
		{"fix_observed_waiting", authorityObservation},

		// Bookkeeping ledgers: excluded from the timeline entirely, and never a
		// stop. The repair and cession rows are written WHILE a run is stopped
		// for its own reason, which is exactly why they may not become it.
		{repairDispatchPhase, authorityObservation},
		{repairResolvedPhase, authorityObservation},
		{repairQuiescentPhase, authorityObservation},
		{repairRunOriginPhase, authorityObservation},
		{executionAuthorityRetiredPhase, authorityObservation},
		{branchLockCededPhase, authorityObservation},
		{branchLockReturnedPhase, authorityObservation},
		{branchCustodyReturnedPhase, authorityObservation},
		{incidentOpenedPhase, authorityObservation},
		{incidentDiagnosisStartedPhase, authorityObservation},
		// Both an incident-ledger row AND a registered reason. Bookkeeping wins,
		// which is the behaviour it has always had: it describes an
		// investigation, never the run's own stop.
		{ReasonIncidentDiagnosisCapacityWait, authorityObservation},

		// Real transitions of the run's own progress.
		{"worker_dispatched", authorityLifecycle},
		{"review_dispatched", authorityLifecycle},
		{"fix_dispatched", authorityLifecycle},
		{"human_applied_fix_observed", authorityLifecycle},

		// A phase nothing knows about is never a stop.
		{"a_phase_from_a_future_version", authorityLifecycle},
		{"", authorityObservation},
	}
	for _, tc := range cases {
		if got := classifyCheckpointPhase(tc.phase); got != tc.want {
			t.Errorf("classifyCheckpointPhase(%q) = %d, want %d", tc.phase, got, tc.want)
		}
	}
}

// Every stop reason in the registry classifies as a stop, so a reason added
// later cannot be silently unable to explain a run.
func TestEveryRegisteredReasonHasStopAuthority(t *testing.T) {
	for reason := range attentionDispositions {
		if isBookkeepingPhase(reason) {
			continue // deliberate: see incident_diagnosis_capacity_wait above.
		}
		if got := classifyCheckpointPhase(reason); got != authorityStop {
			t.Errorf("registered reason %q classifies as %d, want authorityStop", reason, got)
		}
	}
}

// THE INCIDENT, as a unit: one stop, then 302 observations of one verdict.
func Test302ObservationsDoNotDisplaceTheStop(t *testing.T) {
	base := time.Date(2026, 8, 30, 2, 30, 32, 0, time.UTC)
	cps := []domain.WorkflowCheckpoint{
		cp("review_dispatched", base),
		cp("review_launch_confirmed", base),
		cp("review_launch_abandoned", base),
		cp(ReasonReviewerLaunchFailed, base),
	}
	for i := 0; i < 302; i++ {
		cps = append(cps, cp(reviewObservedPhase, base.Add(time.Duration(i+1)*time.Second)))
	}

	fold := foldCheckpointAuthority(cps)

	if fold.StopPhase != ReasonReviewerLaunchFailed {
		t.Fatalf("stop authority = %q, want %q", fold.StopPhase, ReasonReviewerLaunchFailed)
	}
	if !fold.StopAt.Equal(base) {
		t.Fatalf("stop at = %s, want %s", fold.StopAt, base)
	}
	// The timeline is unchanged: the newest checkpoint is still the newest
	// checkpoint. Only the STOP is protected from it.
	if fold.LatestPhase != reviewObservedPhase {
		t.Fatalf("latest phase = %q, want the newest observation to remain the run's latest activity", fold.LatestPhase)
	}
}

// Observations must not displace any human-owned stop, not just the one from
// the incident.
func TestObservationsDoNotDisplaceAnyStop(t *testing.T) {
	base := time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)
	for _, reason := range []string{
		ReasonFixBudgetExhausted,
		ReasonFixNoVerifiableChange,
		ReasonReviewerLaunchFailed,
		ReasonReviewerAuthInvalid,
	} {
		fold := foldCheckpointAuthority([]domain.WorkflowCheckpoint{
			cp(reason, base),
			cp(reviewObservedPhase, base.Add(time.Hour)),
			cp("routing_decision", base.Add(2*time.Hour)),
			cp(repairDispatchPhase, base.Add(3*time.Hour)),
		})
		if fold.StopPhase != reason {
			t.Errorf("stop authority = %q, want %q", fold.StopPhase, reason)
		}
	}
}

// A newer stop replaces an older one: recency among stops is untouched.
func TestANewerStopReplacesAnOlderOne(t *testing.T) {
	base := time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)
	fold := foldCheckpointAuthority([]domain.WorkflowCheckpoint{
		cp(ReasonReviewerLaunchFailed, base),
		cp(reviewObservedPhase, base.Add(time.Minute)),
		cp(ReasonFixBudgetExhausted, base.Add(2*time.Minute)),
		cp(reviewObservedPhase, base.Add(3*time.Minute)),
	})
	if fold.StopPhase != ReasonFixBudgetExhausted {
		t.Fatalf("stop authority = %q, want the newer stop %q", fold.StopPhase, ReasonFixBudgetExhausted)
	}
}

// A genuine clearing transition ends the older stop's authority.
func TestAClearedStopStopsBeingAuthoritative(t *testing.T) {
	base := time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)
	fold := foldCheckpointAuthority([]domain.WorkflowCheckpoint{
		cp(ReasonFixBudgetExhausted, base),
		cp(attentionClearedPhase, base.Add(time.Minute)),
		cp(reviewObservedPhase, base.Add(2*time.Minute)),
	})
	if fold.StopPhase != "" {
		t.Fatalf("stop authority = %q, want none: the stop was cleared", fold.StopPhase)
	}
	// And a stop AFTER the clear is authoritative again.
	fold = foldCheckpointAuthority([]domain.WorkflowCheckpoint{
		cp(ReasonFixBudgetExhausted, base),
		cp(attentionClearedPhase, base.Add(time.Minute)),
		cp(ReasonReviewerLaunchFailed, base.Add(2*time.Minute)),
		cp(reviewObservedPhase, base.Add(3*time.Minute)),
	})
	if fold.StopPhase != ReasonReviewerLaunchFailed {
		t.Fatalf("stop authority = %q, want %q after a stop that follows a clear", fold.StopPhase, ReasonReviewerLaunchFailed)
	}
}

// Authority must not depend on the order rows come back in, nor on the
// lexicographic order of their ids. wf-c4c84f52 wrote four rows in one second
// and only one of them was the stop.
func TestSameInstantAuthorityDoesNotDependOnRowOrder(t *testing.T) {
	at := time.Date(2026, 8, 30, 2, 30, 32, 0, time.UTC)
	stop := domain.WorkflowCheckpoint{ID: "wfc-zzzz", DurablePhase: ReasonReviewerLaunchFailed, CreatedAt: at}
	noise := domain.WorkflowCheckpoint{ID: "wfc-aaaa", DurablePhase: "review_dispatched", CreatedAt: at}
	cleared := domain.WorkflowCheckpoint{ID: "wfc-mmmm", DurablePhase: attentionClearedPhase, CreatedAt: at}

	for _, order := range [][]domain.WorkflowCheckpoint{
		{stop, noise, cleared},
		{cleared, noise, stop},
		{noise, cleared, stop},
		{noise, stop, cleared},
	} {
		fold := foldCheckpointAuthority(order)
		if fold.StopPhase != ReasonReviewerLaunchFailed {
			t.Fatalf("stop authority = %q for order %v, want the stop to win its own instant",
				fold.StopPhase, phasesOf(order))
		}
	}
}

// A ledger with nothing but observations names no stop. AO never invents one.
func TestAnObservationOnlyLedgerNamesNoStop(t *testing.T) {
	base := time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)
	fold := foldCheckpointAuthority([]domain.WorkflowCheckpoint{
		cp(reviewObservedPhase, base),
		cp("a_phase_from_a_future_version", base.Add(time.Minute)),
		cp("worker_dispatched", base.Add(2*time.Minute)),
	})
	if fold.StopPhase != "" {
		t.Fatalf("stop authority = %q, want none", fold.StopPhase)
	}
}

// A hand-built RunDetail — the shape much of this package's own test surface
// uses — keeps the older single-phase behaviour, and a folded one does not fall
// back to it.
func TestStopAuthorityFallbackAppliesOnlyToUnfoldedDetails(t *testing.T) {
	unfolded := RunDetail{LatestCheckpointPhase: ReasonFixBudgetExhausted}
	if reason, _, ok := resolveAttentionReason(unfolded); !ok || reason != ReasonFixBudgetExhausted {
		t.Fatalf("unfolded detail resolved %q (ok=%v), want %q", reason, ok, ReasonFixBudgetExhausted)
	}

	base := time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)
	var folded RunDetail
	applyCheckpointAuthority(&folded, []domain.WorkflowCheckpoint{
		cp(ReasonFixBudgetExhausted, base),
		cp(attentionClearedPhase, base.Add(time.Minute)),
	})
	if reason, _, ok := resolveAttentionReason(folded); ok {
		t.Fatalf("a folded detail whose stop was cleared resolved %q; a cleared stop must not be re-explained", reason)
	}
}

func phasesOf(cps []domain.WorkflowCheckpoint) []string {
	out := make([]string, 0, len(cps))
	for _, c := range cps {
		out = append(out, c.DurablePhase)
	}
	return out
}
