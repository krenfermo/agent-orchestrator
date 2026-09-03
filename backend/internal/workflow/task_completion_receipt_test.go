package workflow

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The Completed status for ordinary tasks is derived, at read time, from a
// session fact (SessionRecord.TurnCompletedAt). Workflow decides its own
// lifecycle from git-verified evidence plus activity state and never from a
// display status, and this pins the half of that which is an invariant: the
// receipt may never stand in for the workspace evidence a workflow step is
// required to see before it COMPLETES.
//
// P3-A narrowed what this test asserts, and the narrowing is deliberate. It
// used to compare the two decisions field-for-field, which pinned something
// stronger than its own reason: that the receipt is invisible to workflow. That
// turned out to be the bug. A TUI worker never exits, so for an idle session
// the receipt is the ONLY fact that distinguishes "finished" from "between
// turns" -- and while it was invisible, a finished worker whose workspace AO
// could not read was evaluated as "idle, look again later", forever. So the
// receipt is now allowed to decide one thing and one thing only: whether a step
// with no provable outcome STOPS or keeps waiting. It still cannot complete
// anything, which is what the assertions below check.
func TestWorkflowDecisionsIgnoreTheTaskCompletionReceipt(t *testing.T) {
	now := time.Date(2026, 8, 22, 20, 0, 0, 0, time.UTC)
	dispatchedAt := now.Add(-10 * time.Minute)

	session := func(state domain.ActivityState, terminated bool, completed bool) domain.SessionRecord {
		rec := domain.SessionRecord{
			ID:            "sess-1",
			IsTerminated:  terminated,
			Activity:      domain.Activity{State: state, LastActivityAt: dispatchedAt},
			FirstSignalAt: dispatchedAt,
		}
		if completed {
			rec.TurnCompletedAt = now.Add(-time.Minute)
		}
		return rec
	}

	cases := []struct {
		name      string
		state     domain.ActivityState
		term      bool
		available bool
		obs       ports.WorkspaceObservation
		// stopsOnReceipt marks the one shape whose decision the receipt is
		// allowed to change: an idle worker with no observation to decide on.
		stopsOnReceipt bool
	}{
		{"idle worker with no work evidence", domain.ActivityIdle, false, true, ports.WorkspaceObservation{HeadSHA: "base"}, false},
		{"idle worker with dirty worktree", domain.ActivityIdle, false, true, ports.WorkspaceObservation{HeadSHA: "base", Dirty: true}, false},
		{"idle worker, workspace unobservable", domain.ActivityIdle, false, false, ports.WorkspaceObservation{}, true},
		{"worker still active", domain.ActivityActive, false, true, ports.WorkspaceObservation{HeadSHA: "base"}, false},
		{"worker awaiting input", domain.ActivityWaitingInput, false, true, ports.WorkspaceObservation{HeadSHA: "base"}, false},
		{"worker process exited", domain.ActivityExited, false, true, ports.WorkspaceObservation{HeadSHA: "base"}, false},
		{"worker session terminated", domain.ActivityIdle, true, true, ports.WorkspaceObservation{HeadSHA: "base"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			without := evaluateWorkStepProgress(
				true, session(tc.state, tc.term, false), tc.available, tc.obs, "base", now, dispatchedAt, false, false, workerEvidence{SessionAlive: true}, readOnlyExpectation{},
			)
			with := evaluateWorkStepProgress(
				true, session(tc.state, tc.term, true), tc.available, tc.obs, "base", now, dispatchedAt, false, true, workerEvidence{SessionAlive: true}, readOnlyExpectation{},
			)
			// The invariant. A completion the evidence did not already justify
			// may never appear because a receipt arrived.
			if with.NextStep == domain.WorkflowStepCompleted && without.NextStep != domain.WorkflowStepCompleted {
				t.Fatalf("a completion receipt completed a work step the evidence did not: %+v", with)
			}
			if tc.stopsOnReceipt {
				// The one difference the receipt is allowed to make, and the
				// bug it closes: a finished worker whose workspace AO cannot
				// read stops the run instead of polling itself forever.
				if without.NoChange != true {
					t.Fatalf("precondition: without a receipt this case must be a no-op, got %+v", without)
				}
				if with.NoChange || !with.Ambiguous || with.NextRun != domain.WorkflowRunNeedsAttention {
					t.Fatalf("a finished worker AO cannot observe must stop the run, not keep polling: %+v", with)
				}
				return
			}
			// Every other case: the receipt changes nothing material. Progress
			// and NextAction are the ledger's wording and may name the receipt.
			if with.NoChange != without.NoChange || with.NextStep != without.NextStep ||
				with.NextRun != without.NextRun || with.ErrorClass != without.ErrorClass ||
				with.Ambiguous != without.Ambiguous {
				t.Fatalf("a completion receipt changed a decision the evidence had already settled:\n with = %+v\n without = %+v", with, without)
			}
			// The attention reason is checked as a DISPOSITION rather than as a
			// string (P3-D §1). A receipt is allowed to change which sentence a
			// person reads, and on one stop it should: "AO cannot tell whether
			// this worker did anything" and "AO knows it finished and produced
			// nothing" are different findings, and telling somebody the first
			// when AO holds the second sends them to establish a fact AO has.
			//
			// What it may never change is what the stop MEANS -- whether AO can
			// remediate it, whether a repair applies, and whose turn it is.
			// That is the property this case was protecting, and it still holds
			// exactly: both readings are a human decision with no automatic
			// remedy, so nothing downstream behaves differently.
			if a, b := dispositionOf(with.AttentionReason), dispositionOf(without.AttentionReason); a != b {
				t.Fatalf("a completion receipt changed what the stop MEANS:\n with = %+v (%+v)\n without = %+v (%+v)",
					with, a, without, b)
			}

			healthWithout := sessionHealthFromFacts(session(tc.state, tc.term, false), true)
			healthWith := sessionHealthFromFacts(session(tc.state, tc.term, true), true)
			if healthWith != healthWithout {
				t.Fatalf("session health changed with a completion receipt: %q, want %q", healthWith, healthWithout)
			}
		})
	}
}

// dispositionOf resolves the decision's reason the way every consumer does,
// including the empty reason the raise gate fills in with the generic dispatch
// ambiguity.
// The HumanAction sentence is deliberately excluded: it IS the wording, and
// wording is what a receipt is allowed to improve. Everything that decides
// BEHAVIOUR -- who owns the stop, whether AO retries it, whether Continue can
// do anything, what AO recommends, whether a repair applies -- is compared.
func dispositionOf(reason string) AttentionDisposition {
	if reason == "" {
		reason = ReasonWorkerDispatchAmbiguous
	}
	d := attentionDispositions[reason]
	// Present/absent still matters: a reason with NO action at all is a reason
	// AO has no advice about, which is a different kind of stop.
	if d.HumanAction != "" {
		d.HumanAction = "(present)"
	}
	return d
}
