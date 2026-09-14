package workflow_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// P9 §19 — terminal run immutability through startup reconciliation.
//
// P8's read-only visual validation watched daemon boot append
// `review_reviewer_unproven` to an already COMPLETED run (wf-0aadfcde). That
// phase classifies as LIFECYCLE, so it moved the closed run's NextAction, latest
// checkpoint phase and "last activity" on every boot; and past the probe budget
// the same sweep escalated into stopReviewAmbiguous, i.e. an attention STOP, a
// notification and a possible wake on a run nobody can continue.
//
// The rule now: a terminal run's ledger receives nothing from reconciliation
// that is not the action on a process AO PROVES it owns (a proven reviewer is
// still terminated — see TestReviewCase10_*). Every other outcome is logged.
// These tests hold the whole observable surface of a closed run constant across
// repeated boots: row, ledger, and the projection a person reads.
func TestP9_TerminalRunIsImmutableThroughStartupReconciliation(t *testing.T) {
	for _, terminal := range []domain.WorkflowRunState{
		domain.WorkflowRunCompleted, domain.WorkflowRunFailed, domain.WorkflowRunCancelled,
	} {
		for _, probe := range []string{"unknown", "foreign"} {
			t.Run(string(terminal)+"/"+probe, func(t *testing.T) {
				f := newReviewAuthorityFixture(t)
				subject := "rr-p9-terminal-" + probe
				identity := "workflow-review-" + subject
				f.crashIdentity(subject)
				f.seedLaunchPhaseFor(subject, "review_launch_intent")
				if _, err := f.store.UpdateWorkflowRunState(f.ctx, f.runID, f.run().State, terminal, f.clk.Now()); err != nil {
					t.Fatalf("close the run as %s: %v", terminal, err)
				}
				switch probe {
				case "unknown":
					f.launcher.probeUnknown = true
				case "foreign":
					f.launcher.foreign = map[string]bool{identity: true}
				}

				before := p9TerminalSurface(t, f)
				launches, cancels := f.launcher.launchCalls, f.launcher.cancelCalls

				// Well past the probe budget (5): every boot, then the read and
				// resume entry points, then more boots.
				for i := 0; i < 8; i++ {
					if err := f.c.Reconcile(f.ctx); err != nil {
						t.Fatalf("Reconcile %d: %v", i, err)
					}
				}
				_, _ = f.c.GetRun(f.ctx, f.runID)
				_, _ = f.c.ContinueRun(f.ctx, f.runID)
				if err := f.c.Reconcile(f.ctx); err != nil {
					t.Fatalf("Reconcile after entry points: %v", err)
				}

				after := p9TerminalSurface(t, f)
				if after.checkpoints != before.checkpoints {
					t.Fatalf("ledger grew on a %s run: %d -> %d; phases = %v",
						terminal, before.checkpoints, after.checkpoints, f.checkpointPhases())
				}
				if f.countPhase("review_reviewer_unproven") != 0 {
					t.Fatalf("review_reviewer_unproven was written to a %s run", terminal)
				}
				if f.countPhase(workflowcore.ReasonReviewStateAmbiguous) != 0 || f.hasPhase(workflowcore.AmbiguousWorkerStateEvidencePhase) {
					t.Fatalf("an attention STOP was escalated onto a %s run; phases = %v", terminal, f.checkpointPhases())
				}
				if after.state != before.state || !after.updatedAt.Equal(before.updatedAt) {
					t.Fatalf("run row moved: %s@%s -> %s@%s", before.state, before.updatedAt, after.state, after.updatedAt)
				}
				if after.projection != before.projection {
					t.Fatalf("the projection of a %s run changed:\nbefore %s\nafter  %s", terminal, before.projection, after.projection)
				}
				if f.launcher.launchCalls != launches {
					t.Fatal("a reviewer was launched for a closed run")
				}
				if f.launcher.cancelCalls != cancels {
					t.Fatal("a session AO cannot prove it owns was destroyed")
				}
			})
		}
	}
}

// A terminal REVIEW STEP on a run that is still live keeps the full evidence
// budget and escalation: that run is still somebody's to continue, so an
// unresolvable reviewer obligation must reach a person. Only CLOSED runs are
// exempt.
func TestP9_LiveRunWithTerminalReviewStepStillEscalatesAnUnprovableReviewer(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-p9-live-run"
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	failReviewStepOnLiveRun(t, f)
	f.launcher.probeUnknown = true
	for i := 0; i < 8; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	if got := f.countPhase("review_reviewer_unproven"); got != 5 {
		t.Fatalf("evidence records = %d, want the probe budget of 5 on a live run", got)
	}
	if !f.hasPhase(workflowcore.AmbiguousWorkerStateEvidencePhase) {
		t.Fatalf("a live run's unprovable reviewer never escalated; phases = %v", f.checkpointPhases())
	}
}

type p9TerminalSnapshot struct {
	checkpoints int
	state       domain.WorkflowRunState
	updatedAt   time.Time
	projection  string
}

// p9TerminalSurface is everything a person or a later boot can observe about a
// closed run: its row, its ledger length, and the complete projection derived
// from them.
func p9TerminalSurface(t *testing.T, f *reviewAuthorityFixture) p9TerminalSnapshot {
	t.Helper()
	run := f.run()
	detail, err := f.c.GetRun(f.ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	// The WHOLE run detail a person reads -- next action, latest phase, stop
	// authority, steps, presentation -- byte for byte.
	projection, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	return p9TerminalSnapshot{
		checkpoints: len(f.checkpointPhases()),
		state:       run.State,
		updatedAt:   run.UpdatedAt,
		projection:  string(projection),
	}
}
