package workflow_test

// Regression suite for the FIRST half of the wf-0aadfcde incident (task 4 of
// wf-2f06ba48): authority over a review step went back to an approval AO had
// already replaced.
//
//	workflow_steps  review  review_run_id -> e7cac283    (the STALE approval)
//	review_run      e7cac283  superseded_by = e8fe44e6
//	review_run      e8fe44e6  superseded_by = e7cac283   (a CYCLE)
//
// The route was recordReviewDispatchSuccess's "the predecessor won" branch. It
// restores the authority pointer when a replacement loses the authority CAS,
// which is the right move for exactly one reason — the predecessor produced a
// late verdict for this step's current target while its replacement was
// launching, and that verdict is adoptable only while the step still points at
// it. The branch restored unconditionally, so a predecessor that had ALREADY
// been superseded, or whose verdict was given for a fingerprint a fresh review
// had been authorized to replace, took the pointer back all the same.
//
// The invariants pinned here:
//
//	a review run that something already superseded can never be bound again;
//	a released predecessor retakes authority only with a readable, unsuperseded
//	  verdict for the target this step is currently asking about;
//	no supersession chain may contain a cycle;
//	and every refusal converges — one durable stop, not a wake-up loop.

import (
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// supersessionCycle walks every review run's superseded_by chain and reports the
// first run whose chain returns to a run it has already visited.
func supersessionCycle(f *reviewAuthorityFixture) (string, bool) {
	f.t.Helper()
	for id := range f.reviewRuns.runs {
		seen := map[string]bool{}
		at := id
		for at != "" {
			if seen[at] {
				return id, true
			}
			seen[at] = true
			r, ok := f.reviewRuns.runs[at]
			if !ok {
				break
			}
			at = r.SupersededBy
		}
	}
	return "", false
}

// restart rebuilds the coordinator over the SAME durable rows and the same
// external world, which is what a daemon restart is: nothing in memory survives,
// everything on disk does.
func (f *reviewAuthorityFixture) restart() {
	f.t.Helper()
	var idSeq int
	f.launcher.beforeLaunch = nil
	f.c = workflowcore.New(workflowcore.Deps{
		Store:            f.store,
		SessionFacts:     f.sessionFacts,
		WorkspaceFacts:   f.wsFacts,
		ReviewRuns:       f.reviewRuns,
		ReviewerLauncher: f.launcher,
		MessageSender:    f.messages,
		Clock:            f.clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("restart%d", idSeq)
		},
	})
}

// lifecycle is what the Board actually renders for this run: the canonical stop
// reason and the action it offers a person.
func (f *reviewAuthorityFixture) lifecycle() workflowcore.Lifecycle {
	f.t.Helper()
	detail, err := f.c.GetRun(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("GetRun: %v", err)
	}
	return workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{
		Detail: detail, Questions: detail.Questions,
	})
}

// ---- the incident -----------------------------------------------------------

// THE REGRESSION. The predecessor answers during the replacement handoff — but
// something had ALREADY superseded it, so its answer is evidence and never
// authority (domain.ReviewRun.EffectiveVerdict says so itself). Handing the
// pointer back to it re-arms a decision AO durably recorded as replaced.
func TestSupersededPredecessorDoesNotRetakeAuthority(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()

	f.launcher.beforeLaunch = func() {
		// The abandoned reviewer answers, which is what makes the replacement's
		// authority CAS fail…
		f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "late findings", f.clk.Now())
		// …but a replacement had already taken authority over it, so that answer
		// can never be this step's outcome.
		if _, err := f.reviewRuns.MarkReviewRunSupersededBy(f.ctx, stalled, "rr-earlier-replacement"); err != nil {
			t.Fatalf("MarkReviewRunSupersededBy: %v", err)
		}
	}

	f.converge()

	if got := f.authoritativeRunID(); got == stalled {
		t.Fatalf("authority = %q: a superseded review took this step back", got)
	}
	if id, cyclic := supersessionCycle(f); cyclic {
		t.Fatalf("the supersession chain through %s is cyclic: 'which review replaced which' has no answer", id)
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want needs_attention: nothing left can speak for this review step", got)
	}
	if !f.hasPhase("review_authority_stale") {
		t.Fatalf("the refusal was not recorded; phases = %v", f.checkpointPhases())
	}
	life := f.lifecycle()
	if life.AttentionReason != workflowcore.ReasonReviewAuthorityStale {
		t.Fatalf("attention reason = %q, want %q", life.AttentionReason, workflowcore.ReasonReviewAuthorityStale)
	}
	if life.AttentionAction == "" {
		t.Fatal("the stop names no action a person can take")
	}
}

// Convergence. The refused handoff leaves a pointer the claim CAS can never
// take (it refuses every replacement while the predecessor holds a late
// verdict), so "try again next wake" would be an unbounded no-progress loop.
// Repeated reconciles, continues and polls must add no reviewers and no new
// decisions.
func TestRefusedAuthorityHandoffDoesNotLoop(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.launcher.beforeLaunch = func() {
		f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "late findings", f.clk.Now())
		if _, err := f.reviewRuns.MarkReviewRunSupersededBy(f.ctx, stalled, "rr-earlier-replacement"); err != nil {
			t.Fatalf("MarkReviewRunSupersededBy: %v", err)
		}
	}

	f.converge()
	launchesAfterStop := f.launcher.launchCalls
	phasesAfterStop := len(f.checkpointPhases())

	// The autonomous re-entries: a boot reconcile and an ordinary Board poll,
	// which is exactly what "the reconciler wakes continuously and the workflow
	// does not progress" was made of. No Continue here — a person asking again
	// is a decision, and a decision is allowed to cost a retry.
	for i := 0; i < 6; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
		if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
			t.Fatalf("GetRun %d: %v", i, err)
		}
	}

	if got := f.launcher.launchCalls; got != launchesAfterStop {
		t.Fatalf("reviewer launches grew from %d to %d after the run had already stopped",
			launchesAfterStop, got)
	}
	if got := len(f.checkpointPhases()); got != phasesAfterStop {
		t.Fatalf("the ledger grew from %d to %d rows over an unchanged stop", phasesAfterStop, got)
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want needs_attention to stay put", got)
	}
	if id, cyclic := supersessionCycle(f); cyclic {
		t.Fatalf("the supersession chain through %s became cyclic under repetition", id)
	}
}

// The VALID late verdict is untouched. An unsuperseded predecessor that answered
// for the target this step is asking about still wins the handoff, still keeps
// its authority, and still concludes the step — and it does so without closing a
// loop in the supersession chain.
func TestValidLateVerdictStillWinsTheHandoffWithoutACycle(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.launcher.beforeLaunch = func() {
		f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "late findings", f.clk.Now())
	}

	f.converge()

	if got := f.authoritativeRunID(); got != stalled {
		t.Fatalf("authority = %q, want the predecessor %q: its verdict is for this step's own target", got, stalled)
	}
	if got := f.reviewRun(stalled).SupersededBy; got != "" {
		t.Fatalf("superseded_by = %q, want empty: the predecessor won", got)
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed from the adopted approval", got)
	}
	if id, cyclic := supersessionCycle(f); cyclic {
		t.Fatalf("the supersession chain through %s is cyclic", id)
	}
	if f.hasPhase("review_authority_stale") {
		t.Fatal("a valid late verdict was refused as stale authority")
	}
}

// A restart changes nothing. The refusal is derived from durable rows — the
// review run's superseded_by and its verdict — so a fresh coordinator over the
// same store reaches the same decision and does not undo it.
func TestRefusedAuthorityHandoffSurvivesARestart(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.launcher.beforeLaunch = func() {
		f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "late findings", f.clk.Now())
		if _, err := f.reviewRuns.MarkReviewRunSupersededBy(f.ctx, stalled, "rr-earlier-replacement"); err != nil {
			t.Fatalf("MarkReviewRunSupersededBy: %v", err)
		}
	}
	f.converge()
	before := f.authoritativeRunID()

	f.restart()
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}
	if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}

	if got := f.authoritativeRunID(); got != before {
		t.Fatalf("authority moved from %q to %q across a restart", before, got)
	}
	if got := f.authoritativeRunID(); got == stalled {
		t.Fatalf("authority = %q: the restart handed the superseded approval its authority back", got)
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q after restart, want needs_attention", got)
	}
}
