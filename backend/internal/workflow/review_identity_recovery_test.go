package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeIdentityLedger stands in for service/agentauth's credential ledger: it
// answers only "was a credential ever minted for this review run", which is the
// single fact the recovery reads.
type fakeIdentityLedger struct {
	issued map[string]bool
	err    error
	calls  int
}

func (f *fakeIdentityLedger) EverIssuedForReviewRun(_ context.Context, reviewRunID string) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return f.issued[reviewRunID], nil
}

func newCoordinatorWithIdentity(
	spawner workflowcore.Spawner, sessionFacts workflowcore.SessionFacts,
	workspaceFacts workflowcore.WorkspaceFacts, reviewRuns *fakeReviewRuns,
	launcher *fakeReviewerLauncher, ledger workflowcore.ReviewerIdentityLedger, trustedLocal bool,
) (*workflowcore.Coordinator, *fakeStore, *fakeClock) {
	store := newFakeStore()
	store.reviewRuns = reviewRuns
	clk := &fakeClock{t: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Spawner:          spawner,
		SessionFacts:     sessionFacts,
		WorkspaceFacts:   workspaceFacts,
		ReviewRuns:       reviewRuns,
		ReviewerLauncher: launcher,
		ReviewerIdentity: ledger,
		TrustedLocal:     trustedLocal,
		Clock:            clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, clk
}

// strandedReviewer drives a run to the EXACT state wf-98ab416c was parked in:
// work complete, one reviewer dispatched and confirmed, the staleness threshold
// fired, the run needs_attention, the review step resting at waiting, and the
// review_run row still 'running' with no verdict -- while the reviewer itself is
// alive, owned, and (in the real incident) sitting at its prompt having already
// decided, unable to tell AO because AO refused to identify it.
//
// The reviewer is deliberately NOT removed from the launcher's live map: this is
// the case reviewerRuntimeGone cannot see, and the whole point of the rule under
// test is that it needs no observation of the agent at all.
func strandedReviewer(t *testing.T, ledger workflowcore.ReviewerIdentityLedger, trustedLocal bool) (
	*workflowcore.Coordinator, *fakeStore, *fakeClock, *fakeReviewRuns, *fakeReviewerLauncher, string, string,
) {
	t.Helper()
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{strictOwnership: true}
	c, store, clk := newCoordinatorWithIdentity(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, ledger, trustedLocal)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "make the terminal selection visible")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	dispatched, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(dispatched).Step.ReviewRunID

	clk.Advance(31 * time.Minute)
	parked, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if parked.Run.State != domain.WorkflowRunNeedsAttention ||
		reviewStepFrom(parked).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("run/step = %q/%q, want needs_attention/waiting", parked.Run.State, reviewStepFrom(parked).Step.State)
	}
	rr, ok, _ := reviewRuns.GetReviewRun(ctx, reviewRunID)
	if !ok || rr.Status != domain.ReviewRunRunning || rr.HasEffectiveVerdict() {
		t.Fatalf("review run = %q/%q, want the still-running, silent row this regression is about", rr.Status, rr.Verdict)
	}
	if !launcher.externalLive["workflow-review-"+reviewRunID] {
		t.Fatalf("the reviewer is not live; this regression is about one that IS")
	}
	return c, store, clk, reviewRuns, launcher, created.Run.ID, reviewRunID
}

// TestStrandedReviewerWithNoIdentityIsRecoverableByAnExplicitHumanResume is the
// wf-98ab416c smoke.
//
// The reviewer approved the work, ran `ao review submit`, and was answered 401
// NOT_AUTHENTICATED because an SSO installation resolved no identity for a pane
// with no cookie. It then idled -- alive, owned, finished -- and thirty minutes
// later the staleness rule parked the run on review_state_ambiguous. Every
// recovery refused, correctly, because the reviewer was provably PRESENT.
//
// What this asserts is that the recovery now turns on evidence AO owns about its
// OWN launch rather than on an observation of the agent, and that it still
// refuses everything it refused before: an unattended poll changes nothing, no
// verdict is fabricated, and exactly one bounded replacement is authorized over
// the same target.
func TestStrandedReviewerWithNoIdentityIsRecoverableByAnExplicitHumanResume(t *testing.T) {
	ledger := &fakeIdentityLedger{issued: map[string]bool{}} // nothing was ever minted
	c, store, clk, reviewRuns, launcher, runID, reviewRunID := strandedReviewer(t, ledger, false)
	ctx := context.Background()

	// An unattended poll must change NOTHING. A rule that fired on a poll would
	// relaunch a reviewer every two seconds for as long as the run existed.
	polled, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := *reviewStepFrom(polled).Step.ReviewRunID; got != reviewRunID {
		t.Fatalf("an unattended poll rebound the review to %q; only an explicit resume may", got)
	}
	if polled.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("an unattended poll unparked the run (%q)", polled.Run.State)
	}

	resumed, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun (human resume): %v", err)
	}
	abandoned, ok, _ := reviewRuns.GetReviewRun(ctx, reviewRunID)
	if !ok || abandoned.Status != domain.ReviewRunCancelled {
		t.Fatalf("stranded review run status = %q, want cancelled", abandoned.Status)
	}
	if abandoned.HasEffectiveVerdict() {
		t.Fatalf("a verdict was fabricated for the stranded review: %q", abandoned.Verdict)
	}
	if launcher.cancelCalls == 0 {
		t.Fatalf("the stranded reviewer was left running while its run was closed out")
	}
	if resumed.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("the run is still parked on an ambiguity that has been resolved by evidence")
	}

	cps, err := store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	rebinds := 0
	for _, cp := range cps {
		if cp.DurablePhase == "review_authority_rebind" {
			rebinds++
		}
	}
	if rebinds != 1 {
		t.Fatalf("replacement authorizations = %d, want exactly 1", rebinds)
	}

	// The loop closes: a replacement reviews the SAME target and is born with
	// no verdict of its own.
	clk.Advance(6 * time.Hour)
	final, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after cooldown: %v", err)
	}
	replacement := reviewStepFrom(final).Step.ReviewRunID
	if replacement == nil || *replacement == reviewRunID {
		t.Fatalf("no replacement review bound after the authorization (still %v)", replacement)
	}
	fresh, ok, _ := reviewRuns.GetReviewRun(ctx, *replacement)
	if !ok {
		t.Fatalf("replacement review run %q does not exist", *replacement)
	}
	if fresh.TargetSHA != abandoned.TargetSHA {
		t.Fatalf("replacement reviews %q, want the same target %q", fresh.TargetSHA, abandoned.TargetSHA)
	}
	if fresh.HasEffectiveVerdict() {
		t.Fatalf("the replacement was born with a verdict: %q", fresh.Verdict)
	}
}

// The rule is SELF-EXTINGUISHING, and this is the test that says so: a reviewer
// that WAS given a credential can record its verdict, so a slow review is a slow
// review and nothing here may touch it -- however long it has been running, and
// however explicitly a person resumes.
//
// Without this property the rule would be a relaunch loop with extra steps.
func TestReviewerHoldingAnIdentityIsNeverReplacedForBeingSlow(t *testing.T) {
	ledger := &fakeIdentityLedger{issued: map[string]bool{}}
	c, _, _, reviewRuns, launcher, runID, reviewRunID := strandedReviewer(t, ledger, false)
	ledger.issued[reviewRunID] = true // this launch DID get a credential
	ctx := context.Background()

	resumed, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if got := *reviewStepFrom(resumed).Step.ReviewRunID; got != reviewRunID {
		t.Fatalf("a replacement (%q) was launched over a reviewer that can still answer", got)
	}
	if rr, ok, _ := reviewRuns.GetReviewRun(ctx, reviewRunID); !ok || rr.Status != domain.ReviewRunRunning {
		t.Fatalf("a reviewer holding an identity was closed out (status %q)", rr.Status)
	}
	if launcher.cancelCalls != 0 {
		t.Fatalf("a live, credentialed reviewer was terminated")
	}
}

// On a trusted-local installation a cookie-less `ao review submit` resolves the
// bootstrap admin and works, so nothing is blocked and nothing is proven. The
// rule must be inert there -- otherwise it would terminate working reviewers on
// every desktop install.
func TestTrustedLocalInstallationNeverReplacesAReviewerForLackingAnIdentity(t *testing.T) {
	ledger := &fakeIdentityLedger{issued: map[string]bool{}}
	c, _, _, reviewRuns, launcher, runID, reviewRunID := strandedReviewer(t, ledger, true)
	ctx := context.Background()

	resumed, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if got := *reviewStepFrom(resumed).Step.ReviewRunID; got != reviewRunID {
		t.Fatalf("a replacement (%q) was launched on an installation where the reviewer can already speak", got)
	}
	if rr, ok, _ := reviewRuns.GetReviewRun(ctx, reviewRunID); !ok || rr.Status != domain.ReviewRunRunning {
		t.Fatalf("trusted-local reviewer was closed out (status %q)", rr.Status)
	}
	if ledger.calls != 0 {
		t.Fatalf("the identity ledger was consulted on a trusted-local install (%d reads)", ledger.calls)
	}
	if launcher.cancelCalls != 0 {
		t.Fatalf("a live reviewer was terminated on a trusted-local install")
	}
}

// A presence AO cannot correlate to its own launch proves nothing about
// ownership, and terminating on it is the one failure worse than leaving an
// orphan. `unknown` and `foreign` are both refused.
func TestUnprovableReviewerIsNeverReplacedForLackingAnIdentity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*fakeReviewerLauncher, string)
	}{
		{"unknown probe", func(l *fakeReviewerLauncher, _ string) { l.probeUnknown = true }},
		{"foreign session", func(l *fakeReviewerLauncher, handle string) {
			l.foreign = map[string]bool{handle: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &fakeIdentityLedger{issued: map[string]bool{}}
			c, _, _, reviewRuns, launcher, runID, reviewRunID := strandedReviewer(t, ledger, false)
			tc.apply(launcher, "workflow-review-"+reviewRunID)
			ctx := context.Background()

			resumed, err := c.ContinueRun(ctx, runID)
			if err != nil {
				t.Fatalf("ContinueRun: %v", err)
			}
			if got := *reviewStepFrom(resumed).Step.ReviewRunID; got != reviewRunID {
				t.Fatalf("a replacement (%q) was launched over a reviewer AO could not prove is its own", got)
			}
			if rr, ok, _ := reviewRuns.GetReviewRun(ctx, reviewRunID); !ok || rr.Status != domain.ReviewRunRunning {
				t.Fatalf("an unprovable reviewer was closed out (status %q)", rr.Status)
			}
		})
	}
}

// A ledger AO cannot read proves nothing either. Failing closed here is what
// keeps a transient database error from being read as "this reviewer was never
// given an identity" and terminating a live, working one.
func TestUnreadableIdentityLedgerNeverLicensesAReplacement(t *testing.T) {
	ledger := &fakeIdentityLedger{issued: map[string]bool{}, err: errors.New("database is locked")}
	c, _, _, reviewRuns, launcher, runID, reviewRunID := strandedReviewer(t, ledger, false)
	ctx := context.Background()

	resumed, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if got := *reviewStepFrom(resumed).Step.ReviewRunID; got != reviewRunID {
		t.Fatalf("a replacement (%q) was launched off an unreadable ledger", got)
	}
	if rr, ok, _ := reviewRuns.GetReviewRun(ctx, reviewRunID); !ok || rr.Status != domain.ReviewRunRunning {
		t.Fatalf("a reviewer was closed out on an unreadable ledger (status %q)", rr.Status)
	}
	if launcher.cancelCalls != 0 {
		t.Fatalf("a live reviewer was terminated on an unreadable ledger")
	}
}

// With no ledger wired at all -- every pre-P4-I configuration, and every test
// double that predates it -- the rule is inert.
func TestUnwiredIdentityLedgerLeavesTheRuleInert(t *testing.T) {
	c, _, _, reviewRuns, launcher, runID, reviewRunID := strandedReviewer(t, nil, false)
	ctx := context.Background()

	resumed, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if got := *reviewStepFrom(resumed).Step.ReviewRunID; got != reviewRunID {
		t.Fatalf("a replacement (%q) was launched with no identity ledger wired", got)
	}
	if rr, ok, _ := reviewRuns.GetReviewRun(ctx, reviewRunID); !ok || rr.Status != domain.ReviewRunRunning {
		t.Fatalf("a reviewer was closed out with no identity ledger wired (status %q)", rr.Status)
	}
	if launcher.cancelCalls != 0 {
		t.Fatalf("a live reviewer was terminated with no identity ledger wired")
	}
}
