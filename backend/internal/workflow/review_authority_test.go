package workflow_test

// Review authority: which review speaks for a review step, and what happens when
// the one it points at stops speaking.
//
// The incident these tests are written from (wf-756988ae, review run 4ac56ac5):
// AO's stall detection decided a still-working reviewer had stalled and
// cancelled its review run; the reviewer then finished, found no defect and
// submitted `approved`; AO answered REVIEW_INVALID and destroyed the verdict;
// and the review step sat at `waiting`, pointing at that same cancelled run,
// forever — because the fix-cycle idempotency guard read "a run exists for this
// fingerprint" as "this fingerprint has been reviewed".
//
// Everything below drives the real dispatch/observe/reconcile machinery through
// fakes. No reviewer process is started anywhere, and no test sleeps.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---- fixture -----------------------------------------------------------------

type reviewAuthorityFixture struct {
	t            *testing.T
	ctx          context.Context
	c            *workflowcore.Coordinator
	store        *fakeStore
	reviewRuns   *fakeReviewRuns
	launcher     *fakeReviewerLauncher
	sessionFacts *fakeSessionFacts
	wsFacts      *fakeWorkspaceFacts
	clk          *fakeClock
	messages     *fakeMessageSender

	runID string
}

// newReviewAuthorityFixture drives a run to the point the incident starts from:
// work completed, review dispatched, a reviewer attached and awaiting a verdict.
func newReviewAuthorityFixture(t *testing.T) *reviewAuthorityFixture {
	t.Helper()
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{
		rec:   domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}},
		facts: sessionFacts,
	}
	wsFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	messages := &fakeMessageSender{}
	c, store, clk := newCoordinatorWithReviewAndMessages(
		spawner, sessionFacts, wsFacts, reviewRuns, launcher, messages)

	f := &reviewAuthorityFixture{
		t: t, ctx: context.Background(), c: c, store: store, reviewRuns: reviewRuns,
		launcher: launcher, sessionFacts: sessionFacts, wsFacts: wsFacts, clk: clk,
		messages: messages,
	}
	created, err := c.CreateRun(f.ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f.runID = created.Run.ID
	completeWorkStep(t, c, store, clk, sessionFacts, wsFacts, f.runID)
	if _, err := c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun (dispatch review): %v", err)
	}
	if f.reviewStep().ReviewRunID == nil {
		t.Fatal("no reviewer was dispatched by the fixture")
	}
	return f
}

func (f *reviewAuthorityFixture) steps() []domain.WorkflowStep {
	f.t.Helper()
	steps, err := f.store.ListWorkflowSteps(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowSteps: %v", err)
	}
	return steps
}

func (f *reviewAuthorityFixture) stepOfKind(kind domain.WorkflowStepKind) domain.WorkflowStep {
	f.t.Helper()
	for _, s := range f.steps() {
		if s.Kind == kind {
			return s
		}
	}
	f.t.Fatalf("no %s step", kind)
	return domain.WorkflowStep{}
}

func (f *reviewAuthorityFixture) reviewStep() domain.WorkflowStep {
	return f.stepOfKind(domain.WorkflowStepReview)
}

func (f *reviewAuthorityFixture) run() domain.WorkflowRun {
	f.t.Helper()
	run, ok, err := f.store.GetWorkflowRun(f.ctx, f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v (ok=%v)", err, ok)
	}
	return run
}

func (f *reviewAuthorityFixture) authoritativeRunID() string {
	f.t.Helper()
	id := f.reviewStep().ReviewRunID
	if id == nil {
		return ""
	}
	return *id
}

func (f *reviewAuthorityFixture) reviewRun(id string) domain.ReviewRun {
	f.t.Helper()
	r, ok, err := f.reviewRuns.GetReviewRun(f.ctx, id)
	if err != nil || !ok {
		f.t.Fatalf("GetReviewRun(%s): %v (ok=%v)", id, err, ok)
	}
	return r
}

// submitVerdict is what `ao review submit` does: the reviewer's real verdict,
// against whatever state AO has by then put the run in.
func (f *reviewAuthorityFixture) submitVerdict(id string, verdict domain.ReviewVerdict) {
	f.t.Helper()
	r := f.reviewRun(id)
	switch r.Status {
	case domain.ReviewRunRunning:
		if _, err := f.reviewRuns.UpdateReviewRunResult(
			f.ctx, id, domain.ReviewRunComplete, verdict, "", "", true); err != nil {
			f.t.Fatalf("UpdateReviewRunResult: %v", err)
		}
	default:
		// The submit path's cancelled/failed arm: preserved, never authority.
		f.reviewRuns.RecordLateReviewVerdict(id, verdict, "", f.clk.Now())
	}
}

// stallTheReviewer reproduces exactly what AO did in the incident: the reviewer
// session reads idle after dispatch, and AO's capacity-stall path cancels the
// review run out from under it.
// driveStallDetection makes the reviewer session read idle and runs one
// observation pass, which is what trips AO's reviewer-stall path. It asserts
// nothing about the outcome: the races below are precisely about what that
// outcome turns out to be.
func (f *reviewAuthorityFixture) driveStallDetection() {
	f.t.Helper()
	work := f.stepOfKind(domain.WorkflowStepWork)
	f.clk.Advance(1 * time.Second)
	f.sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.clk.Now()},
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	f.clk.Advance(25 * time.Second) // past reviewerStallGrace
	if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
		f.t.Fatalf("GetRun (stall detection): %v", err)
	}
}

func (f *reviewAuthorityFixture) stallTheReviewer() string {
	f.t.Helper()
	stalled := f.authoritativeRunID()
	f.driveStallDetection()
	if got := f.reviewRun(stalled); got.Status != domain.ReviewRunCancelled {
		f.t.Fatalf("review run status = %q, want the stall path to have cancelled it", got.Status)
	}
	// The stall also records a provider cooldown, and routing correctly refuses
	// to pick a provider that is cooling down. Move past it, so what these tests
	// observe is the AUTHORITY decision rather than a capacity wait that would
	// resolve itself in production the moment the cooldown lapses.
	f.clk.Advance(30 * time.Minute)
	return stalled
}

func (f *reviewAuthorityFixture) checkpointPhases() []string {
	f.t.Helper()
	return ledgerPhases(f.t, f.store, f.runID)
}

func (f *reviewAuthorityFixture) hasPhase(phase string) bool {
	f.t.Helper()
	for _, p := range f.checkpointPhases() {
		if p == phase {
			return true
		}
	}
	return false
}

// ---- A. the ordinary path is untouched ----------------------------------------

func TestReviewAuthority_A_NormalVerdictStillConcludesTheStep(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	runID := f.authoritativeRunID()

	f.submitVerdict(runID, domain.VerdictApproved)
	if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed on an on-time approval", got)
	}
	if got := f.reviewRun(runID).Status; got != domain.ReviewRunComplete {
		t.Fatalf("review run status = %q, want complete", got)
	}
	if f.hasPhase("review_late_verdict_adopted") {
		t.Fatal("an on-time verdict was recorded as a late adoption")
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", f.launcher.launchCalls)
	}
}

// ---- B. the incident: stalled first, valid verdict after ----------------------

// The exact wf-756988ae shape. AO gives up on a reviewer that is still working;
// the reviewer then answers. The answer must survive, and — because nothing has
// replaced that review — it must conclude the step.
func TestReviewAuthority_B_VerdictArrivingAfterAStallIsAdoptedNotLost(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()

	// The reviewer finishes and submits. Before the fix this was REVIEW_INVALID
	// and the verdict ceased to exist.
	f.submitVerdict(stalled, domain.VerdictApproved)
	if got := f.reviewRun(stalled); got.LateVerdict != domain.VerdictApproved {
		t.Fatalf("late verdict = %q, want the reviewer's approval preserved", got.LateVerdict)
	}

	if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed: its own reviewer approved it", got)
	}
	if !f.hasPhase("review_late_verdict_adopted") {
		t.Fatalf("the adoption was not recorded; phases = %v", f.checkpointPhases())
	}
	// And AO did not run a second reviewer over a diff that had already been
	// reviewed.
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want 1: a preserved verdict must not be re-reviewed", f.launcher.launchCalls)
	}
}

// ---- C/D. a replacement took over before the old verdict arrived --------------

// Once a replacement review is authoritative, a late verdict from the review it
// replaced is evidence and nothing more. It must never overwrite the newer one.
func TestReviewAuthority_CD_ALateVerdictNeverOverridesTheReplacement(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()

	// The retry binds a replacement.
	if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun (rebind): %v", err)
	}
	replacement := f.authoritativeRunID()
	if replacement == stalled {
		t.Fatal("no replacement review was bound after the stall")
	}
	// The run it replaced is durably told so, which is what makes authority
	// answerable after a restart.
	if got := f.reviewRun(stalled).SupersededBy; got != replacement {
		t.Fatalf("superseded_by = %q, want the replacement %q", got, replacement)
	}

	// NOW the old reviewer finally answers — with the opposite verdict, so an
	// incorrect adoption would be unmistakable.
	f.submitVerdict(stalled, domain.VerdictApproved)
	if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got := f.authoritativeRunID(); got != replacement {
		t.Fatalf("authoritative review = %q, want the replacement %q: a late verdict took authority back", got, replacement)
	}
	if got := f.reviewStep().State; got == domain.WorkflowStepCompleted {
		t.Fatal("a superseded review's late approval completed the step over a newer, still-running review")
	}
	if f.hasPhase("review_late_verdict_adopted") {
		t.Fatal("a superseded run's late verdict was adopted")
	}
	// The verdict is still preserved as evidence — not lost, just not authority.
	if got := f.reviewRun(stalled).LateVerdict; got != domain.VerdictApproved {
		t.Fatalf("late verdict = %q, want it still recorded as evidence", got)
	}
}

// ---- E/H. the retry binds exactly one replacement, however often it runs ------

func TestReviewAuthority_EH_RepeatedReconciliationBindsExactlyOneReplacement(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()

	// Many passes: boot reconcile, wake, and ordinary polls, all against the
	// same contradiction.
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
		if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
			t.Fatalf("GetRun %d: %v", i, err)
		}
	}

	if f.launcher.launchCalls != 2 {
		t.Fatalf("reviewer launches = %d, want exactly 2 (the original and one replacement)", f.launcher.launchCalls)
	}
	replacement := f.authoritativeRunID()
	if replacement == stalled {
		t.Fatal("the step is still bound to the review AO cancelled")
	}
	running := 0
	for _, r := range f.reviewRuns.runs {
		if r.Status == domain.ReviewRunRunning {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("running review runs = %d, want exactly 1", running)
	}
}

// ---- F/G. a waiting step over a terminal review cannot wait forever -----------

// The convergence property, driven the way a daemon restart drives it: boot
// reconciliation alone, against durable state that was written before the
// process existed.
func TestReviewAuthority_FG_BootReconcileConvergesAWaitingStepOverATerminalReview(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()

	if got := f.reviewStep().State; got != domain.WorkflowStepWaiting {
		t.Fatalf("review step = %q, want waiting (the state the incident left behind)", got)
	}
	if got := f.reviewRun(stalled); got.Status != domain.ReviewRunCancelled || got.HasDurableVerdict() {
		t.Fatalf("review run = %+v, want cancelled with no verdict", got)
	}

	// Nothing but boot reconciliation.
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	step := f.reviewStep()
	stillStuck := step.State == domain.WorkflowStepWaiting &&
		step.ReviewRunID != nil && *step.ReviewRunID == stalled
	if stillStuck {
		t.Fatalf("the review step is still waiting on the review AO cancelled; phases = %v", f.checkpointPhases())
	}
	if !f.hasPhase("review_authority_rebind") {
		t.Fatalf("no rebind authorization was recorded; phases = %v", f.checkpointPhases())
	}
}

// The capacity-retry checkpoint has to be enough to say what was being retried.
// Before this fix it recorded the sentence and a `{}` payload — naming neither
// the run it closed out nor the target that run was reviewing.
func TestReviewAuthority_CapacityRetryEvidenceIsReconstructableAfterRestart(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	target := f.reviewRun(f.authoritativeRunID()).TargetSHA
	stalled := f.stallTheReviewer()

	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var retry domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.DurablePhase == "review_capacity_retry" {
			retry, found = cp, true
		}
	}
	if !found {
		t.Fatalf("no review_capacity_retry checkpoint; phases = %v", f.checkpointPhases())
	}
	if retry.ReviewRunID == nil || *retry.ReviewRunID != stalled {
		t.Fatalf("retry checkpoint review_run_id = %v, want the run it closed out (%s)", retry.ReviewRunID, stalled)
	}
	for _, want := range []string{stalled, target, "cancelled"} {
		if want == "" {
			continue
		}
		if !strings.Contains(retry.RetryState, want) {
			t.Fatalf("retry payload %s does not carry %q; a restart cannot reconstruct what was being retried",
				retry.RetryState, want)
		}
	}
}

// ---- the bounded end of the ladder --------------------------------------------

// Reconciliation may release a step for a replacement only so many times. A
// spent budget stops for a person; it never opens reviewer number four.
func TestReviewAuthority_RebindBudgetIsBoundedAndStops(t *testing.T) {
	f := newReviewAuthorityFixture(t)

	for i := 0; i < 6; i++ {
		step := f.reviewStep()
		if step.State.Terminal() || f.run().State == domain.WorkflowRunNeedsAttention {
			break
		}
		if step.ReviewRunID != nil {
			if r := f.reviewRun(*step.ReviewRunID); r.Status == domain.ReviewRunRunning {
				f.stallTheReviewer()
			}
		}
		if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
	}

	if f.launcher.launchCalls > 1+3 {
		t.Fatalf("reviewer launches = %d, want at most the original plus the bounded rebinds", f.launcher.launchCalls)
	}
	if f.run().State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention once the rebind budget is spent; phases = %v",
			f.run().State, f.checkpointPhases())
	}
}

// ---- Finding 2: the late-verdict / authority-release race --------------------

// The interleaving the reviewer loses under a non-atomic release:
//
//	reconciler reads the run   -> no late verdict
//	reviewer writes its verdict durably          <-- HERE
//	reconciler clears the pointer anyway
//	replacement reviewer dispatched over finished work
//
// The release is one statement conditioned on there being no late verdict, so
// the write above makes it affect zero rows and the reconciler re-reads instead.
// Driven by a store hook that fires INSIDE the release call, immediately before
// it takes effect: an exact interleaving, with no goroutine and no sleep.
func TestReviewAuthority_F2_AVerdictLandingDuringReleaseWinsAndIsAdopted(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()

	// The reviewer's verdict lands at the worst possible instant.
	f.store.beforeReleaseReviewRun = func(_, reviewRunID string) {
		f.reviewRuns.RecordLateReviewVerdict(reviewRunID, domain.VerdictApproved, "", f.clk.Now())
	}

	if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	step := f.reviewStep()
	if step.ReviewRunID == nil || *step.ReviewRunID != stalled {
		t.Fatalf("authority pointer = %v, want it still on %s: the release must not have won", step.ReviewRunID, stalled)
	}
	if step.State != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed from the adopted verdict", step.State)
	}
	if !f.hasPhase("review_late_verdict_adopted") {
		t.Fatalf("the verdict was not adopted; phases = %v", f.checkpointPhases())
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want 1: no replacement over an approved diff", f.launcher.launchCalls)
	}
}

// The mirror image: the release wins, and a verdict arriving afterwards is
// preserved as evidence but can never take authority back.
func TestReviewAuthority_F2_AVerdictLandingAfterReleaseNeverTakesAuthority(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()

	if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun (release + rebind): %v", err)
	}
	replacement := f.authoritativeRunID()
	if replacement == stalled || replacement == "" {
		t.Fatalf("authority = %q, want a replacement bound", replacement)
	}

	// Only now does the abandoned reviewer answer.
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "", f.clk.Now())
	for i := 0; i < 3; i++ {
		if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
			t.Fatalf("GetRun %d: %v", i, err)
		}
	}

	if got := f.authoritativeRunID(); got != replacement {
		t.Fatalf("authority = %q, want the replacement %q", got, replacement)
	}
	if f.hasPhase("review_late_verdict_adopted") {
		t.Fatal("a verdict that lost the release was adopted anyway")
	}
	if got := f.reviewRun(stalled).LateVerdict; got != domain.VerdictApproved {
		t.Fatalf("late verdict = %q, want it preserved as evidence", got)
	}
}

// ---- Finding 3: the single-winner claim -------------------------------------

// Two reconcilers landing together must not both authorize a replacement. The
// second one runs INSIDE the first one's claim insert, so both have already
// read "no authorization yet" — the exact interleaving an in-memory guard cannot
// prevent and a durable uniqueness constraint can.
func TestReviewAuthority_F3_ConcurrentReconciliationYieldsOneClaim(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	f.stallTheReviewer()

	reentered := 0
	f.store.beforeCheckpointInsert = func(cp domain.WorkflowCheckpoint) {
		if cp.DurablePhase != "review_authority_rebind" {
			return
		}
		// A second, complete reconciliation pass, from the same durable state
		// the first one read.
		reentered++
		if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("re-entrant ContinueRun: %v", err)
		}
	}

	if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if reentered == 0 {
		t.Fatal("the interleaving never happened; the test proves nothing")
	}

	claims := 0
	for _, p := range f.checkpointPhases() {
		if p == "review_authority_rebind" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("rebind authorizations = %d, want exactly 1", claims)
	}
	if f.launcher.launchCalls != 2 {
		t.Fatalf("reviewer launches = %d, want 2 (original + one replacement)", f.launcher.launchCalls)
	}
	running := 0
	for _, r := range f.reviewRuns.runs {
		if r.Status == domain.ReviewRunRunning {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("running review runs = %d, want exactly 1", running)
	}
}

// The same property from the other direction, and the one that matters after a
// restart: N callers that each read the pre-claim state and only then act.
//
// Every one of them holds a snapshot in which no authorization exists — a
// goroutine race would only ever be one accidental sample of this, so it is
// modelled directly and deterministically instead. Exactly one claim may
// survive, and exactly one replacement may be launched.
func TestReviewAuthority_F3_NCallersFromThePreClaimSnapshotConsumeOneGeneration(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	f.stallTheReviewer()

	// The snapshot every caller races from: taken once, before anyone claims.
	run := f.run()
	step := f.reviewStep()

	const n = 8
	outcomes := map[workflowcore.ReviewAuthorityOutcome]int{}
	for i := 0; i < n; i++ {
		outcome, _, _, err := f.c.ReconcileReviewAuthority(f.ctx, run, step)
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		outcomes[outcome]++
	}

	if got := outcomes[workflowcore.ReviewAuthorityRebindPending]; got != 1 {
		t.Fatalf("callers that released authority = %d, want exactly 1 (outcomes: %v)", got, outcomes)
	}
	claims := 0
	for _, p := range f.checkpointPhases() {
		if p == "review_authority_rebind" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("rebind authorizations across %d callers = %d, want exactly 1", n, claims)
	}

	// And the retry budget consumed exactly one generation, so the bound still
	// means what it says.
	if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if f.launcher.launchCalls != 2 {
		t.Fatalf("reviewer launches = %d, want 2 (original + one replacement)", f.launcher.launchCalls)
	}
}

// ---- the stall / normal-verdict race ----------------------------------------
//
// AO judges a reviewer stalled and moves to cancel its run. The reviewer is
// submitting a real verdict at that same instant. Exactly one of the two may
// win, and whichever does must be the one the workflow acts on.
//
// The losing shape this guards against — a review run durably COMPLETE with an
// approval, and its workflow parked at `waiting` under review_capacity_retry —
// is unrecoverable by construction: observation only looks at running steps, and
// authority reconciliation reads a run with a verdict as intact. Nothing would
// ever apply the approval.

// (2)(3)(4)(5)(6) The normal verdict wins the race: the cancellation matches
// nothing, and the approval is applied immediately.
func TestReviewStall_NormalVerdictWinningTheCancelRaceIsAppliedNotParked(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	reviewRunID := f.authoritativeRunID()

	// The reviewer submits in the instant before the cancellation lands.
	f.reviewRuns.beforeCancelRunning = func() {
		if _, err := f.reviewRuns.UpdateReviewRunResult(
			f.ctx, reviewRunID, domain.ReviewRunComplete, domain.VerdictApproved, "", "", true); err != nil {
			t.Fatalf("racing submit: %v", err)
		}
	}
	f.driveStallDetection()

	// (3) the cancellation affected nothing, (4) the verdict is applied now.
	if got := f.reviewRun(reviewRunID); got.Status != domain.ReviewRunComplete || got.Verdict != domain.VerdictApproved {
		t.Fatalf("review run = %+v, want it left complete/approved by the winning submit", got)
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed: the authoritative approval must be applied", got)
	}
	// (5) no capacity retry, (6) no replacement reviewer.
	if f.hasPhase("review_capacity_retry") {
		t.Fatalf("review_capacity_retry was emitted over a winning verdict; phases = %v", f.checkpointPhases())
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want 1: nothing to replace", f.launcher.launchCalls)
	}
	if f.hasPhase("review_authority_rebind") {
		t.Fatal("authority was released over a completed, authoritative review")
	}

	// (7) repeated wakes and reconciles change nothing.
	for i := 0; i < 3; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
		if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step drifted to %q under repeated reconciliation", got)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches after repeated wakes = %d, want 1", f.launcher.launchCalls)
	}
}

// (8) The same race, then a restart: boot reconciliation alone must converge on
// the verdict rather than on a retry.
func TestReviewStall_RestartAfterTheRaceConvergesOnTheVerdict(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	reviewRunID := f.authoritativeRunID()

	f.reviewRuns.beforeCancelRunning = func() {
		if _, err := f.reviewRuns.UpdateReviewRunResult(
			f.ctx, reviewRunID, domain.ReviewRunComplete, domain.VerdictChangesRequested, "fix it", "", true); err != nil {
			t.Fatalf("racing submit: %v", err)
		}
	}
	f.driveStallDetection()

	// A changes_requested verdict rests the review at waiting and opens the fix
	// cycle — never a capacity retry, and never a replacement reviewer.
	if f.hasPhase("review_capacity_retry") {
		t.Fatalf("review_capacity_retry was emitted over a winning verdict; phases = %v", f.checkpointPhases())
	}
	if got := f.reviewRun(reviewRunID).Verdict; got != domain.VerdictChangesRequested {
		t.Fatalf("verdict = %q, want the submitted changes_requested preserved", got)
	}

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	step := f.reviewStep()
	if step.ReviewRunID == nil || *step.ReviewRunID != reviewRunID {
		t.Fatalf("authority = %v, want it still on the completed review %s", step.ReviewRunID, reviewRunID)
	}
	if f.hasPhase("review_authority_rebind") {
		t.Fatal("boot reconciliation released authority from a review that had concluded")
	}
}

// (1) The ordinary case is untouched: the cancellation wins, and the retry path
// runs exactly as before.
func TestReviewStall_CancellationWinningStillParksAndRetries(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	reviewRunID := f.authoritativeRunID()

	f.driveStallDetection()

	if got := f.reviewRun(reviewRunID).Status; got != domain.ReviewRunCancelled {
		t.Fatalf("review run = %q, want cancelled", got)
	}
	if !f.hasPhase("review_capacity_retry") {
		t.Fatalf("no capacity retry recorded; phases = %v", f.checkpointPhases())
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepWaiting {
		t.Fatalf("review step = %q, want waiting", got)
	}
}

// A cancellation AO could not even attempt proves nothing, so nothing moves.
// Parking here would strand the step exactly as the winning-verdict case would.
func TestReviewStall_AnUnprovableCancellationLeavesTheStepUntouched(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	reviewRunID := f.authoritativeRunID()
	f.reviewRuns.cancelErr = errTestCancelUnavailable

	f.driveStallDetection()

	if got := f.reviewStep().State; got != domain.WorkflowStepRunning {
		t.Fatalf("review step = %q, want it left running: AO proved nothing", got)
	}
	if got := f.reviewRun(reviewRunID).Status; got != domain.ReviewRunRunning {
		t.Fatalf("review run = %q, want it untouched", got)
	}
	if f.hasPhase("review_capacity_retry") {
		t.Fatalf("a retry was recorded on an unprovable cancellation; phases = %v", f.checkpointPhases())
	}
}

var errTestCancelUnavailable = errors.New("review store unavailable")

// ---- the effective verdict: one meaning, whichever column holds it ----------
//
// AO records a verdict in one of two places. The normal path writes `verdict`
// while the review is running; a reviewer that finished after AO closed its run
// out writes `late_verdict`, because migration 0135 deliberately never promotes
// a late arrival into the authoritative column.
//
// Downstream that split must be invisible. It was not: the fix cascade read
// `Verdict` directly, so an ADOPTED late changes_requested became authoritative
// and then dispatched nothing — the review had concluded, the findings existed,
// and AO would have gone on to review the same unchanged work again.

// (1)(2) The normal paths are unchanged.
func TestEffectiveVerdict_NormalVerdictsDriveTheirUsualCascade(t *testing.T) {
	for _, tc := range []struct {
		name      string
		verdict   domain.ReviewVerdict
		body      string
		wantStep  domain.WorkflowStepState
		wantFixed bool
	}{
		{"approved goes to verify", domain.VerdictApproved, "", domain.WorkflowStepCompleted, false},
		{"changes_requested dispatches a fix", domain.VerdictChangesRequested, "normal findings", domain.WorkflowStepWaiting, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewAuthorityFixture(t)
			id := f.authoritativeRunID()
			if _, err := f.reviewRuns.UpdateReviewRunResult(
				f.ctx, id, domain.ReviewRunComplete, tc.verdict, tc.body, "", true); err != nil {
				t.Fatalf("submit: %v", err)
			}
			if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if got := f.reviewStep().State; got != tc.wantStep {
				t.Fatalf("review step = %q, want %q", got, tc.wantStep)
			}
			if tc.wantFixed {
				if f.messages.calls == 0 {
					t.Fatal("no fix was dispatched for a changes_requested verdict")
				}
				if !strings.Contains(f.messages.lastMsg, tc.body) {
					t.Fatalf("fix prompt does not carry the reviewer's findings:\n%s", f.messages.lastMsg)
				}
			} else if f.messages.calls != 0 {
				t.Fatalf("a fix was dispatched for an approval (%d sends)", f.messages.calls)
			}
		})
	}
}

// (3) An authoritative late APPROVAL goes to verify, exactly as an on-time one.
func TestEffectiveVerdict_AdoptedLateApprovalGoesToVerify(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "nothing to fix", f.clk.Now())

	if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed", got)
	}
	if f.messages.calls != 0 {
		t.Fatalf("a fix was dispatched for an approval (%d sends)", f.messages.calls)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want 1", f.launcher.launchCalls)
	}
}

// (4)(5)(6) THE BLOCKING DEFECT: an authoritative late changes_requested must
// dispatch its fix, carry its own findings into the prompt, and never send AO
// off to review the same unchanged work again.
func TestEffectiveVerdict_AdoptedLateChangesRequestedDispatchesTheFixWithItsFindings(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	const findings = "line 42 dereferences a nil pointer on the error path"
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictChangesRequested, findings, f.clk.Now())

	if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	// (4) the fix ran.
	if f.messages.calls == 0 {
		t.Fatalf("no fix was dispatched for an adopted late changes_requested; phases = %v",
			f.checkpointPhases())
	}
	// (5) with the reviewer's own findings — which live in LateVerdictBody.
	if !strings.Contains(f.messages.lastMsg, findings) {
		t.Fatalf("the fix prompt lost the late verdict's findings:\n%s", f.messages.lastMsg)
	}
	// (6) and no second reviewer over work that has not changed yet.
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want 1: nothing may re-review unchanged work", f.launcher.launchCalls)
	}
	if f.hasPhase("review_authority_rebind") {
		t.Fatal("authority was released from a review that had produced a verdict")
	}
}

// (7)(8) A late verdict on a run a replacement has superseded is evidence, never
// a decision: it must not reach the cascade, and the replacement's own verdict
// is the one that counts.
func TestEffectiveVerdict_StaleLateChangesRequestedNeverDrivesTheCascade(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()

	if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun (rebind): %v", err)
	}
	replacement := f.authoritativeRunID()
	if replacement == stalled || replacement == "" {
		t.Fatalf("no replacement bound (got %q)", replacement)
	}

	// (7) the superseded reviewer finally answers, asking for changes.
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictChangesRequested, "stale findings", f.clk.Now())
	if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if f.messages.calls != 0 {
		t.Fatalf("a superseded run's late verdict dispatched a fix (%d sends)", f.messages.calls)
	}
	if got := f.reviewRun(stalled).EffectiveVerdict(); got != "" {
		t.Fatalf("EffectiveVerdict on a superseded run = %q, want none", got)
	}

	// (8) the replacement's verdict wins.
	if _, err := f.reviewRuns.UpdateReviewRunResult(
		f.ctx, replacement, domain.ReviewRunComplete, domain.VerdictApproved, "", "", true); err != nil {
		t.Fatalf("replacement submit: %v", err)
	}
	if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed from the replacement's approval", got)
	}
	if f.messages.calls != 0 {
		t.Fatalf("the stale changes_requested still dispatched a fix (%d sends)", f.messages.calls)
	}
}

// (9)(10) Restart resumes into the fix, and repeated cascade passes never
// dispatch it twice.
func TestEffectiveVerdict_AdoptedLateChangesRequestedSurvivesRestartAndStaysIdempotent(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	const findings = "the retry budget is never decremented"
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictChangesRequested, findings, f.clk.Now())

	// (9) boot reconciliation alone, against durable state.
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if f.messages.calls == 0 {
		t.Fatalf("boot recovery did not resume into the fix; phases = %v", f.checkpointPhases())
	}
	if !strings.Contains(f.messages.lastMsg, findings) {
		t.Fatalf("the resumed fix prompt lost the findings:\n%s", f.messages.lastMsg)
	}
	sends := f.messages.calls

	// (10) every further pass is a no-op.
	for i := 0; i < 4; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
		if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
		if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
			t.Fatalf("GetRun %d: %v", i, err)
		}
	}
	if f.messages.calls != sends {
		t.Fatalf("fix dispatches %d -> %d under repeated cascade passes", sends, f.messages.calls)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want 1", f.launcher.launchCalls)
	}
}

// ---- crash-safe completion of late-verdict adoption -------------------------

func countReviewPhase(t *testing.T, f *reviewAuthorityFixture, phase, reviewRunID string) int {
	t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	count := 0
	for _, cp := range cps {
		if cp.DurablePhase != phase || cp.ReviewRunID == nil || *cp.ReviewRunID != reviewRunID {
			continue
		}
		count++
	}
	return count
}

func TestLateVerdictAdoption_CheckpointIsWrittenAfterApprovedOutcome(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "", f.clk.Now())

	if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed", got)
	}
	if got := countReviewPhase(t, f, "review_observed", stalled); got != 1 {
		t.Fatalf("review outcomes = %d, want 1", got)
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 1 {
		t.Fatalf("adoption markers = %d, want 1", got)
	}
}

func TestLateVerdictAdoption_TransitionFailureLeavesNoCompletedMarkerAndRestartRepairs(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "", f.clk.Now())

	fail := true
	f.store.stepStateWriteErr = func(_ string, _, _ domain.WorkflowStepState) error {
		if fail {
			fail = false
			return errors.New("injected step transition failure")
		}
		return nil
	}
	if _, err := f.c.GetRun(f.ctx, f.runID); err == nil {
		t.Fatal("GetRun succeeded despite the injected transition failure")
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepWaiting {
		t.Fatalf("review step = %q, want waiting after failed transition", got)
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 0 {
		t.Fatalf("adoption markers = %d, want 0 before the outcome is durable", got)
	}

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed after recovery", got)
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 1 {
		t.Fatalf("adoption markers = %d, want 1 after recovery", got)
	}
}

func TestLateVerdictAdoption_OutcomeWithoutMarkerIsFinalizedWithoutDuplicateOutcome(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "", f.clk.Now())

	failMarker := true
	f.store.checkpointWriteErr = func(cp domain.WorkflowCheckpoint) error {
		if failMarker && cp.DurablePhase == "review_late_verdict_adopted" {
			failMarker = false
			return errors.New("injected crash before adoption marker")
		}
		return nil
	}
	if _, err := f.c.GetRun(f.ctx, f.runID); err == nil {
		t.Fatal("GetRun succeeded despite the injected marker failure")
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed before marker failure", got)
	}
	if got := countReviewPhase(t, f, "review_observed", stalled); got != 1 {
		t.Fatalf("review outcomes before restart = %d, want 1", got)
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 0 {
		t.Fatalf("adoption markers before restart = %d, want 0", got)
	}

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := countReviewPhase(t, f, "review_observed", stalled); got != 1 {
		t.Fatalf("review outcomes after restart = %d, want the original one only", got)
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 1 {
		t.Fatalf("adoption markers after restart = %d, want 1", got)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want 1", f.launcher.launchCalls)
	}
}

func TestLateVerdictAdoption_ConcurrentFinalizersWriteOneMarker(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "", f.clk.Now())

	// Leave the durable outcome complete and only its final receipt missing.
	failMarker := true
	f.store.checkpointWriteErr = func(cp domain.WorkflowCheckpoint) error {
		if failMarker && cp.DurablePhase == "review_late_verdict_adopted" {
			failMarker = false
			return errors.New("injected missing adoption marker")
		}
		return nil
	}
	if _, err := f.c.GetRun(f.ctx, f.runID); err == nil {
		t.Fatal("GetRun succeeded despite the injected marker failure")
	}

	reentered := false
	f.store.beforeCheckpointInsert = func(cp domain.WorkflowCheckpoint) {
		if cp.DurablePhase != "review_late_verdict_adopted" {
			return
		}
		reentered = true
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("concurrent Reconcile: %v", err)
		}
	}
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !reentered {
		t.Fatal("concurrent finalization interleaving did not run")
	}
	if got := countReviewPhase(t, f, "review_observed", stalled); got != 1 {
		t.Fatalf("review outcomes = %d, want 1", got)
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 1 {
		t.Fatalf("adoption markers = %d, want one single-winner receipt", got)
	}
}

func TestLateVerdictAdoption_HistoricalMarkerWithoutOutcomeIsRepaired(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "", f.clk.Now())
	step := f.reviewStep()
	stepID, reviewRunID := step.ID, stalled
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-historical-bad-adoption", WorkflowRunID: f.runID,
		WorkflowStepID: &stepID, ReviewRunID: &reviewRunID,
		ReviewVerdict: string(domain.VerdictApproved), DurablePhase: "review_late_verdict_adopted",
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: f.clk.Now(),
	}); err != nil {
		t.Fatalf("seed historical marker: %v", err)
	}

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed after historical repair", got)
	}
	if got := countReviewPhase(t, f, "review_observed", stalled); got != 1 {
		t.Fatalf("review outcomes = %d, want 1 repaired outcome", got)
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 1 {
		t.Fatalf("adoption markers = %d, want the original marker only", got)
	}
}

func TestLateVerdictAdoption_ChangesRequestedFailureRecoversOnce(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	const findings = "repair this exact race"
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictChangesRequested, findings, f.clk.Now())

	fail := true
	f.store.stepStateWriteErr = func(_ string, _, _ domain.WorkflowStepState) error {
		if fail {
			fail = false
			return errors.New("injected changes-requested transition failure")
		}
		return nil
	}
	if _, err := f.c.GetRun(f.ctx, f.runID); err == nil {
		t.Fatal("GetRun succeeded despite the injected transition failure")
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 0 {
		t.Fatalf("adoption markers = %d, want 0 before recovery", got)
	}

	for i := 0; i < 4; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepWaiting {
		t.Fatalf("review step = %q, want waiting while its fix runs", got)
	}
	if f.messages.calls != 1 {
		t.Fatalf("fix dispatches = %d, want exactly 1", f.messages.calls)
	}
	if !strings.Contains(f.messages.lastMsg, findings) {
		t.Fatalf("fix prompt lost findings:\n%s", f.messages.lastMsg)
	}
	if got := countReviewPhase(t, f, "review_observed", stalled); got != 1 {
		t.Fatalf("review outcomes = %d, want 1", got)
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 1 {
		t.Fatalf("adoption markers = %d, want 1", got)
	}
	if f.launcher.launchCalls != 1 {
		t.Fatalf("reviewer launches = %d, want 1", f.launcher.launchCalls)
	}
}

// ---- B1: the stale-cached-adoption race ------------------------------------
//
// An adopter reaches the apply step holding a review R it read some store calls
// ago. A replacement R2 can be created, supersede R and take the step in that
// window. Acting on the cached R would then complete or park a step that belongs
// to a different review.
//
// Both orderings are driven deterministically by a store hook that fires INSIDE
// the adopter's first guarded write — no goroutine, no sleep.

// Ordering A: the replacement wins the step while the adopter is mid-flight.
// Nothing the adopter decided may land.
func TestAuthorityRace_ReplacementWinningMidAdoptionLandsNothing(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "", f.clk.Now())

	// The instant the adopter attempts its first authority-guarded write, a
	// replacement takes the pointer.
	f.store.beforeStepStateWrite = func(stepID string) {
		if stepID != f.reviewStep().ID {
			return
		}
		f.store.beforeStepStateWrite = nil
		if _, err := f.reviewRuns.MarkReviewRunSupersededBy(f.ctx, stalled, "rr-replacement"); err != nil {
			t.Fatalf("supersede: %v", err)
		}
		if ok, err := f.store.RebindWorkflowStepReviewRunFrom(
			f.ctx, stepID, stalled, "", "rr-replacement", f.clk.Now()); err != nil || !ok {
			t.Fatalf("replacement rebind: ok=%v err=%v", ok, err)
		}
	}

	if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got := f.authoritativeRunID(); got != "rr-replacement" {
		t.Fatalf("authority = %q, want the replacement to still own the step", got)
	}
	if got := f.reviewStep().State; got == domain.WorkflowStepCompleted {
		t.Fatal("a stale adopter completed a step that a replacement had taken")
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 0 {
		t.Fatalf("adoption markers = %d, want 0: the adoption never became authoritative", got)
	}
	if got := countReviewPhase(t, f, "review_observed", stalled); got != 0 {
		t.Fatalf("review outcomes = %d, want 0 from a stale adopter", got)
	}
}

// Ordering B: the replacement has ALREADY won before the adopter is entered.
// The revalidation catches it before any write is attempted.
func TestAuthorityRace_AdoptionOnAnAlreadySupersededRunIsRefused(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "", f.clk.Now())

	if _, err := f.reviewRuns.MarkReviewRunSupersededBy(f.ctx, stalled, "rr-replacement"); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if ok, err := f.store.RebindWorkflowStepReviewRunFrom(
		f.ctx, f.reviewStep().ID, stalled, "", "rr-replacement", f.clk.Now()); err != nil || !ok {
		t.Fatalf("replacement rebind: ok=%v err=%v", ok, err)
	}

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := f.authoritativeRunID(); got != "rr-replacement" {
		t.Fatalf("authority = %q, want the replacement", got)
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 0 {
		t.Fatalf("adoption markers = %d, want 0", got)
	}
}

// ---- B2: concurrent replacement dispatchers, changing routing --------------

// N dispatchers race to replace one abandoned review. Exactly one replacement
// may exist however many of them run and whatever provider each would route to.
//
// The harness-independence of the identity itself is asserted directly in
// review_dispatch_identity_internal_test.go: a key that varied by harness is
// what let two dispatchers each pass the outbox single-flight guard and launch
// their own reviewer, and no black-box test can pin that as precisely.
func TestAuthorityRace_ConcurrentReplacementDispatchersProduceOneReviewer(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()

	// Every entry point that can reach replacement dispatch, repeatedly: boot
	// reconciliation, wakes and polls all race for the same abandoned review.
	const n = 6
	for i := 0; i < n; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
		if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("dispatcher %d: %v", i, err)
		}
		if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}

	if f.launcher.launchCalls != 2 {
		t.Fatalf("reviewer launches = %d, want 2 (the original plus exactly one replacement)",
			f.launcher.launchCalls)
	}
	running := 0
	var replacement string
	for id, r := range f.reviewRuns.runs {
		if r.Status == domain.ReviewRunRunning {
			running++
			replacement = id
		}
	}
	if running != 1 {
		t.Fatalf("running review runs = %d, want exactly 1", running)
	}
	if got := f.authoritativeRunID(); got != replacement {
		t.Fatalf("authority = %q, want the single replacement %q", got, replacement)
	}
	if got := f.reviewRun(stalled).SupersededBy; got != replacement {
		t.Fatalf("superseded_by = %q, want %q", got, replacement)
	}
	claims := 0
	for _, p := range f.checkpointPhases() {
		if p == "review_authority_rebind" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("rebind generations consumed = %d, want exactly 1", claims)
	}
}

// ---- B3/B4: the read models and the Incident Advisor ------------------------

// Everything a person or an API sees about a review must show the EFFECTIVE
// outcome. An adopted late verdict is not a blank review.
func TestAdoptedLateVerdictIsVisibleToReadModelsAndIncidentEvidence(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	const findings = "the retry budget is never decremented"
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictChangesRequested, findings, f.clk.Now())

	detail, err := f.c.GetRun(f.ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	// B3: StepDetail.Review — the workflow detail API's own source, and what
	// reviewReadiness reads.
	var review *workflowcore.ReviewSummary
	for _, sd := range detail.Steps {
		if sd.Step.Kind == domain.WorkflowStepReview {
			review = sd.Review
		}
	}
	if review == nil {
		t.Fatal("the review step exposes no review summary")
	}
	if review.Verdict != domain.VerdictChangesRequested {
		t.Fatalf("StepDetail.Review.Verdict = %q, want the adopted late verdict", review.Verdict)
	}
	if !strings.Contains(review.FindingsSummary, findings) {
		t.Fatalf("StepDetail.Review.FindingsSummary lost the findings: %q", review.FindingsSummary)
	}

	// B4: the evidence an incident is diagnosed from. The child pack and the
	// parent advisor both read the review run directly, so the assertion that
	// matters is that the effective outcome is what they would find.
	rr := f.reviewRun(stalled)
	if rr.EffectiveVerdict() != domain.VerdictChangesRequested {
		t.Fatalf("EffectiveVerdict = %q, want the adopted verdict", rr.EffectiveVerdict())
	}
	if rr.EffectiveBody() != findings {
		t.Fatalf("EffectiveBody = %q, want the reviewer's findings", rr.EffectiveBody())
	}
	if rr.Verdict.Valid() || rr.Body == findings {
		t.Fatal("the raw columns were mutated; 0135's separation must hold")
	}
}

// The revalidation inside adoption, on its own. The top-level gate has already
// read the run and decided to adopt; authority changes in the window before the
// adopter re-reads. Nothing it decided may land.
func TestAuthorityRace_RevalidationCatchesATakeoverAfterTheGateRead(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "", f.clk.Now())

	// Let the gate read the run, then steal the step before the adopter's own
	// revalidation runs.
	reads := 0
	f.reviewRuns.beforeGetReviewRun = func(id string) {
		if id != stalled {
			return
		}
		reads++
		if reads != 1 {
			return
		}
		f.reviewRuns.beforeGetReviewRun = nil
		if ok, err := f.store.RebindWorkflowStepReviewRunFrom(
			f.ctx, f.reviewStep().ID, stalled, "", "rr-replacement", f.clk.Now()); err != nil || !ok {
			t.Fatalf("replacement rebind: ok=%v err=%v", ok, err)
		}
	}

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := f.authoritativeRunID(); got != "rr-replacement" {
		t.Fatalf("authority = %q, want the replacement to keep the step", got)
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 0 {
		t.Fatalf("adoption markers = %d, want 0", got)
	}
	if got := countReviewPhase(t, f, "review_observed", stalled); got != 0 {
		t.Fatalf("review outcomes = %d, want 0 from an adopter that lost authority", got)
	}
}

// The outcome write's own guard, reached when there is no lift to catch the race
// first: an already-RUNNING step whose late verdict is being adopted.
func TestAuthorityRace_OutcomeWriteGuardCatchesATakeoverOnARunningStep(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "", f.clk.Now())

	// Put the step back to running, so adoption performs no lift and the FIRST
	// guarded write is the outcome transition itself.
	step := f.reviewStep()
	if _, err := f.store.UpdateWorkflowStepState(
		f.ctx, step.ID, step.State, domain.WorkflowStepRunning, f.clk.Now()); err != nil {
		t.Fatalf("seed running step: %v", err)
	}

	f.store.beforeStepStateWrite = func(stepID string) {
		f.store.beforeStepStateWrite = nil
		if ok, err := f.store.RebindWorkflowStepReviewRunFrom(
			f.ctx, stepID, stalled, "", "rr-replacement", f.clk.Now()); err != nil || !ok {
			t.Fatalf("replacement rebind: ok=%v err=%v", ok, err)
		}
	}

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := f.reviewStep().State; got == domain.WorkflowStepCompleted {
		t.Fatal("the outcome landed on a step a replacement had taken")
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 0 {
		t.Fatalf("adoption markers = %d, want 0", got)
	}
}

// ---- B1: crash after superseded_by, before the rebind -----------------------
//
// The replacement's supersede write is durable and the pointer rebind is not.
// On replay the write-once CAS matches nothing, because the value it wants is
// already there. Reading that as "another replacement owns this run" strands the
// step forever: it is never rebound, and nothing reviews the work again.
func TestReplacementReplay_SupersededByAlreadyWrittenStillRebinds(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()

	// Release the step, then simulate the crash: the supersede landed for a
	// replacement whose rebind never did.
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile (release): %v", err)
	}
	replacement := f.authoritativeRunID()
	if replacement == "" || replacement == stalled {
		t.Fatalf("no replacement bound (got %q)", replacement)
	}
	if got := f.reviewRun(stalled).SupersededBy; got != replacement {
		t.Fatalf("superseded_by = %q, want %q", got, replacement)
	}

	// The idempotent replay: the same claim, asserted again.
	ok, err := f.reviewRuns.MarkReviewRunSupersededBy(f.ctx, stalled, replacement)
	if err != nil {
		t.Fatalf("replayed supersede: %v", err)
	}
	if !ok {
		t.Fatal("the replay was reported as a lost race; the rebind would be abandoned and the step stranded")
	}

	// A DIFFERENT replacement is still refused — the distinction that matters.
	if other, oerr := f.reviewRuns.MarkReviewRunSupersededBy(f.ctx, stalled, "rr-other"); oerr != nil || other {
		t.Fatalf("a different replacement claimed an already-superseded run: ok=%v err=%v", other, oerr)
	}

	// And repeated wakes converge without a second reviewer.
	launches := f.launcher.launchCalls
	for i := 0; i < 3; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
		if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
	}
	if f.launcher.launchCalls != launches {
		t.Fatalf("reviewer launches %d -> %d across replay", launches, f.launcher.launchCalls)
	}
	if got := f.authoritativeRunID(); got != replacement {
		t.Fatalf("authority = %q, want the same replacement %q reused", got, replacement)
	}
}

// ---- B3: adoption vs replacement rebind, both orderings ---------------------

// Adoption wins: its outcome stands and no replacement may bind over it.
func TestAdoptionVsRebind_AdoptionWinsAndBlocksTheRebind(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "", f.clk.Now())

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed from the adopted approval", got)
	}

	// A replacement now tries to take the step. Both guards refuse it: the step
	// has resolved, and the run it would replace carries a late verdict.
	bound, err := f.store.RebindWorkflowStepReviewRunFrom(
		f.ctx, f.reviewStep().ID, stalled, stalled, "rr-late", f.clk.Now())
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if bound {
		t.Fatal("a replacement rebound a step that an adopted verdict had already resolved")
	}
	if got := f.authoritativeRunID(); got != stalled {
		t.Fatalf("authority = %q, want the adopted run to keep the step", got)
	}
}

// Replacement wins: it binds first, and the late verdict that arrives afterwards
// is stale — no mixed state where the step carries the old outcome.
func TestAdoptionVsRebind_ReplacementWinsAndTheLateVerdictGoesStale(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile (rebind): %v", err)
	}
	replacement := f.authoritativeRunID()
	if replacement == stalled || replacement == "" {
		t.Fatalf("no replacement bound (got %q)", replacement)
	}

	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "too late", f.clk.Now())
	for i := 0; i < 3; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
		if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
			t.Fatalf("GetRun %d: %v", i, err)
		}
	}

	if got := f.authoritativeRunID(); got != replacement {
		t.Fatalf("authority = %q, want the replacement %q", got, replacement)
	}
	if got := f.reviewStep().State; got == domain.WorkflowStepCompleted {
		t.Fatal("a stale late verdict completed a step the replacement owns")
	}
	if got := countReviewPhase(t, f, "review_late_verdict_adopted", stalled); got != 0 {
		t.Fatalf("adoption markers = %d, want 0 for a superseded run", got)
	}
	if got := f.reviewRun(stalled).EffectiveVerdict(); got != "" {
		t.Fatalf("EffectiveVerdict on a superseded run = %q, want none", got)
	}
}

// The changes_requested half of the same invariant, and the one where the
// late-verdict guard is what actually binds: the step rests at `waiting` after
// adoption, which is NOT terminal, so the step-state guard alone would let a
// replacement bind over an outcome that is already driving a fix.
func TestAdoptionVsRebind_AdoptedChangesRequestedAlsoBlocksTheRebind(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictChangesRequested, "fix this", f.clk.Now())

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	step := f.reviewStep()
	if step.State == domain.WorkflowStepCompleted || step.State.Terminal() {
		t.Fatalf("review step = %q, want a non-terminal resting state for this test to mean anything", step.State)
	}
	if f.messages.calls == 0 {
		t.Fatal("the adopted changes_requested did not drive its fix")
	}

	bound, err := f.store.RebindWorkflowStepReviewRunFrom(
		f.ctx, step.ID, stalled, stalled, "rr-late", f.clk.Now())
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if bound {
		t.Fatal("a replacement rebound over an adopted changes_requested that is already driving a fix")
	}
	if got := f.authoritativeRunID(); got != stalled {
		t.Fatalf("authority = %q, want the adopted run to keep the step", got)
	}
}

// ---- B2: the outbox claim is the single launch-ownership gate ---------------
//
// Two dispatchers share the harness-independent replacement identity, so both
// reach the same pending outbox row. Exactly one may claim it, and ONLY that one
// may bring a reviewer into existence. The loser used to ignore the compare-and-
// swap result and launch its own with whatever harness it had locally routed to.
//
// The second dispatcher runs INSIDE the first one's CAS, so both are genuinely
// past the point of no return — the interleaving a sequential test cannot reach.
func TestReplacementDispatch_OutboxCASLoserLaunchesNothing(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	f.stallTheReviewer()

	launchesBefore := f.launcher.launchCalls
	insertsBefore := f.reviewRuns.insertCalls
	reentered := false
	f.store.beforeOutboxCAS = func(_ string, expected, next domain.WorkflowOutboxStatus) {
		if expected != domain.WorkflowOutboxPending || next != domain.WorkflowOutboxDispatched {
			return
		}
		reentered = true
		// A competing dispatcher claims the same pending replacement dispatch.
		if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("competing dispatcher: %v", err)
		}
	}

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !reentered {
		t.Fatal("the outbox-claim interleaving never ran; the test proves nothing")
	}

	if got := f.launcher.launchCalls - launchesBefore; got != 1 {
		t.Fatalf("reviewer launches during the race = %d, want exactly 1", got)
	}
	// The loser must not even ATTEMPT to create a review. Asserting only on
	// launches would be satisfied by the natural-key dedupe catching a second
	// insert — which is luck, not ownership, and would not hold the moment the
	// two dispatchers routed to different harnesses (different natural keys).
	if got := f.reviewRuns.insertCalls - insertsBefore; got != 1 {
		t.Fatalf("review-run creation attempts during the race = %d, want exactly 1: "+
			"the caller that lost the outbox claim must not create a review at all", got)
	}
	running := 0
	for _, r := range f.reviewRuns.runs {
		if r.Status == domain.ReviewRunRunning {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("running review runs = %d, want exactly 1", running)
	}
	harnesses := map[domain.ReviewerHarness]bool{}
	for _, r := range f.reviewRuns.runs {
		if r.Status == domain.ReviewRunRunning {
			harnesses[r.Harness] = true
		}
	}
	if len(harnesses) != 1 {
		t.Fatalf("distinct harnesses persisted for the replacement = %d, want exactly 1", len(harnesses))
	}
}

// ---- the reviewer-launch crash matrix ---------------------------------------
//
// Review dispatch performs five durable acts with an external launch in the
// middle: claim, identity, LAUNCH, confirm, bind. A crash at any boundary leaves
// a distinct durable state, and each must converge — on repeated boot and wake —
// to one reviewer, one authoritative review, no phantom and no permanently
// dispatched outbox.
//
// Each case seeds exactly the state its boundary leaves behind, because that IS
// what a crash leaves behind, and then drives the real recovery paths.

// seedCrashBeforeBind reproduces the durable state a crash leaves at a chosen
// boundary of the reviewer launch: the outbox claimed, the pointer NOT yet bound
// (binding is the last act), and whichever launch phases had been written.
func (f *reviewAuthorityFixture) seedCrashBeforeBind(reviewRunID string, phases ...string) string {
	f.t.Helper()
	step := f.reviewStep()
	stepID := step.ID
	key := "workflow-step-review:" + stepID + ":cycle1:codex"
	e := f.store.outbox[key]
	e.ID, e.WorkflowRunID, e.WorkflowStepID = "wfo-crash", f.runID, &stepID
	e.IdempotencyKey, e.CommandType = key, domain.WorkflowOutboxTriggerReview
	e.Status, e.Payload = domain.WorkflowOutboxDispatched, "{}"
	f.store.outbox[key] = e

	// The bind is the last durable act, so every pre-bind crash leaves the
	// pointer unset.
	if _, err := f.store.RebindWorkflowStepReviewRunFrom(
		f.ctx, stepID, reviewRunID, "", "", f.clk.Now()); err != nil {
		f.t.Fatalf("unbind: %v", err)
	}
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-crash-claim", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-1", DurablePhase: "review_launch_claimed", PayloadVersion: "v1",
		RetryState: `{"idempotencyKey":"` + key + `"}`, CreatedAt: f.clk.Now(),
	}); err != nil {
		f.t.Fatalf("seed claim: %v", err)
	}
	for _, phase := range phases {
		rid := reviewRunID
		if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
			ID: "wfc-" + phase, WorkflowRunID: f.runID, WorkflowStepID: &stepID,
			ProjectID: "proj-1", ReviewRunID: &rid, DurablePhase: phase, PayloadVersion: "v1",
			RetryState: `{"reviewRunId":"` + reviewRunID + `","handleId":"workflow-review-` + reviewRunID + `"}`,
			CreatedAt:  f.clk.Now(),
		}); err != nil {
			f.t.Fatalf("seed %s: %v", phase, err)
		}
	}
	return key
}

func (f *reviewAuthorityFixture) seedLaunchPhaseFor(reviewRunID, phase string) {
	f.t.Helper()
	stepID := f.reviewStep().ID
	rid := reviewRunID
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-" + phase + "-" + reviewRunID, WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-1", ReviewRunID: &rid, DurablePhase: phase, PayloadVersion: "v1",
		// The recorded handle IS the deterministic identity — that is the whole
		// point of recording it before the launch, and what probe addresses.
		RetryState: `{"reviewRunId":"` + reviewRunID + `","handleId":"workflow-review-` + reviewRunID + `"}`,
		CreatedAt:  f.clk.Now(),
	}); err != nil {
		f.t.Fatalf("seed %s: %v", phase, err)
	}
}

func (f *reviewAuthorityFixture) converge() {
	f.t.Helper()
	for i := 0; i < 3; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			f.t.Fatalf("Reconcile %d: %v", i, err)
		}
		if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
			f.t.Fatalf("ContinueRun %d: %v", i, err)
		}
		if _, err := f.c.GetRun(f.ctx, f.runID); err != nil {
			f.t.Fatalf("GetRun %d: %v", i, err)
		}
	}
}

func (f *reviewAuthorityFixture) outboxStatus(key string) domain.WorkflowOutboxStatus {
	f.t.Helper()
	return f.store.outbox[key].Status
}

// (1) Crash after the claim, before any review identity exists.
func TestReviewCrash_AfterClaimBeforeIdentity(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := f.authoritativeRunID()
	before := f.launcher.launchCalls
	key := f.seedCrashBeforeBind(subject)
	// No identity at all: remove the run this dispatch would have created.
	delete(f.reviewRuns.runs, subject)

	f.converge()

	if f.outboxStatus(key) == domain.WorkflowOutboxDispatched {
		t.Fatalf("the outbox is permanently dispatched over a launch that never happened")
	}
	if got := f.launcher.launchCalls - before; got > 1 {
		t.Fatalf("reviewer launches = %d, want at most 1", got)
	}
}

// crashIdentity replaces the step's review with a purpose-made one carrying only
// the phases a crash had reached — not the fully-dispatched run the fixture
// created, whose own review_dispatched marker would make any launch look proven.
func (f *reviewAuthorityFixture) crashIdentity(id string, phases ...string) string {
	f.t.Helper()
	original := f.authoritativeRunID()
	prev := f.reviewRun(original)
	delete(f.reviewRuns.runs, original)
	f.reviewRuns.runs[id] = domain.ReviewRun{
		ID: id, ReviewID: prev.ReviewID, SessionID: prev.SessionID,
		Harness: prev.Harness, PRURL: prev.PRURL, TargetSHA: prev.TargetSHA,
		Status: domain.ReviewRunRunning, CreatedAt: f.clk.Now(),
	}
	f.seedCrashBeforeBind(original, phases...)
	// Re-point the seeded phase rows at the crashed identity.
	return id
}

// (2) Crash after the review identity, before the external launch. The row says
// `running`; nothing was launched. Adopting it would be a phantom reviewer.
func TestReviewCrash_AfterIdentityBeforeLaunchNeverAdoptsAPhantom(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	before := f.launcher.launchCalls
	subject := "rr-crash-intent"
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")

	f.converge()

	if got := f.reviewRun(subject).Status; got == domain.ReviewRunRunning {
		t.Fatalf("review run status = %q: an identity with no recorded launch was left as a live reviewer", got)
	}
	if got := f.authoritativeRunID(); got == subject {
		t.Fatal("a review run that was never launched became this step's authority")
	}
	if got := f.launcher.launchCalls - before; got > 1 {
		t.Fatalf("reviewer launches = %d, want at most 1", got)
	}
}

// (3) Crash after the launch was confirmed, before the bind. A reviewer exists;
// it must be ADOPTED, never relaunched.
func TestReviewCrash_AfterLaunchConfirmedBeforeBindAdopts(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	before := f.launcher.launchCalls
	subject := "rr-crash-confirmed"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	// A real confirmation names the exact incarnation it confirmed, and the
	// reviewer it describes is actually there. Both are what make this an
	// adoption rather than a guess.
	f.launcher.externalLive[identity] = true
	f.launcher.instances = map[string]string{identity: "$1"}
	f.seedConfirmedLaunch(subject, identity, "$1")

	f.converge()

	if got := f.launcher.launchCalls - before; got != 0 {
		t.Fatalf("reviewer launches = %d, want 0: a confirmed reviewer must be adopted, not relaunched", got)
	}
	if got := f.authoritativeRunID(); got != subject {
		t.Fatalf("authority = %q, want the confirmed reviewer %q adopted", got, subject)
	}
}

// (10) A step that goes terminal after the claim must create nothing and launch
// nothing.
func TestReviewCrash_TerminalStepAfterClaimCreatesAndLaunchesNothing(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	f.stallTheReviewer()
	launchesBefore := f.launcher.launchCalls
	insertsBefore := f.reviewRuns.insertCalls

	step := f.reviewStep()
	if _, err := f.store.UpdateWorkflowStepState(
		f.ctx, step.ID, step.State, domain.WorkflowStepCancelled, f.clk.Now()); err != nil {
		t.Fatalf("cancel the step: %v", err)
	}

	f.converge()

	if got := f.launcher.launchCalls - launchesBefore; got != 0 {
		t.Fatalf("reviewer launches = %d, want 0 on a cancelled step", got)
	}
	if got := f.reviewRuns.insertCalls - insertsBefore; got != 0 {
		t.Fatalf("review-run creation attempts = %d, want 0 on a cancelled step", got)
	}
}

// (9) Cancellation lands after the launch claim. The step-state transition then
// loses, and NOTHING may be created or launched on its behalf.
func TestReviewCrash_CancellationAfterClaimCreatesAndLaunchesNothing(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	f.stallTheReviewer()
	launchesBefore := f.launcher.launchCalls
	insertsBefore := f.reviewRuns.insertCalls

	// The step is cancelled in the window between claiming the launch and the
	// step-state transition that the claim was authorized against.
	f.store.beforeOutboxCAS = func(_ string, expected, next domain.WorkflowOutboxStatus) {
		if expected != domain.WorkflowOutboxPending || next != domain.WorkflowOutboxDispatched {
			return
		}
		step := f.reviewStep()
		if _, err := f.store.UpdateWorkflowStepState(
			f.ctx, step.ID, step.State, domain.WorkflowStepCancelled, f.clk.Now()); err != nil {
			t.Fatalf("cancel mid-claim: %v", err)
		}
	}

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := f.reviewRuns.insertCalls - insertsBefore; got != 0 {
		t.Fatalf("review-run creation attempts = %d, want 0 after the step-state CAS lost", got)
	}
	if got := f.launcher.launchCalls - launchesBefore; got != 0 {
		t.Fatalf("reviewer launches = %d, want 0 after the step-state CAS lost", got)
	}
}

// (7)(8) A late verdict lands during the replacement handoff — after the pointer
// was released and while the replacement is being launched.
//
// The predecessor must win: it keeps its authority, it is NOT superseded, its
// verdict is adopted, and the replacement that was already launched is closed
// out. The state this forbids is the unrecoverable one: predecessor superseded,
// replacement unbound, and the only valid verdict in the run unusable.
func TestReviewHandoff_LateVerdictDuringReplacementLaunchWins(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict domain.ReviewVerdict
		want    domain.WorkflowStepState
	}{
		{"approved goes to verify", domain.VerdictApproved, domain.WorkflowStepCompleted},
		{"changes_requested goes to fix", domain.VerdictChangesRequested, domain.WorkflowStepWaiting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewAuthorityFixture(t)
			stalled := f.stallTheReviewer()

			// The abandoned reviewer answers while its replacement is mid-launch.
			f.launcher.beforeLaunch = func() {
				f.reviewRuns.RecordLateReviewVerdict(stalled, tc.verdict, "late findings", f.clk.Now())
			}

			f.converge()

			if got := f.reviewRun(stalled).SupersededBy; got != "" {
				t.Fatalf("superseded_by = %q, want empty: the predecessor won and must keep its authority", got)
			}
			if got := f.reviewRun(stalled).EffectiveVerdict(); got != tc.verdict {
				t.Fatalf("EffectiveVerdict = %q, want %q still usable", got, tc.verdict)
			}
			if got := f.authoritativeRunID(); got != stalled {
				t.Fatalf("authority = %q, want the predecessor %q", got, stalled)
			}
			if got := f.reviewStep().State; got != tc.want {
				t.Fatalf("review step = %q, want %q from the adopted verdict", got, tc.want)
			}
			// The replacement that was launched must not be left live and unowned.
			for id, r := range f.reviewRuns.runs {
				if id != stalled && r.Status == domain.ReviewRunRunning {
					t.Fatalf("replacement %s left live and unbound", id)
				}
			}
		})
	}
}

// ---- the distributed-systems boundary: launch, lost confirmation ------------
//
// A reviewer is created and the confirmation write is lost. On replay AO cannot
// tell "never launched" from "launched, confirmation lost" — unless it probes
// the deterministic identity it decided and persisted BEFORE the launch.
//
// The fake models the world outside AO: `externalLive` survives the crash, as a
// real reviewer process would.
func TestReviewEnsure_LostConfirmationAdoptsInsteadOfLaunchingASecond(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	launchesBefore := f.launcher.launchCalls
	// A purpose-made identity carrying ONLY the intent — never the fixture's own
	// fully-dispatched run, whose review_dispatched marker would make the launch
	// look proven and the probe unnecessary.
	subject := "rr-lost-confirmation"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	// deliberately NO review_launch_confirmed

	// The reviewer DOES exist externally; only AO's confirmation was lost.
	f.launcher.externalLive[identity] = true
	liveBefore := map[string]bool{}
	for id, alive := range f.launcher.externalLive {
		liveBefore[id] = alive
	}

	f.converge()

	if got := f.launcher.launchCalls - launchesBefore; got != 0 {
		t.Fatalf("external launches during recovery = %d, want 0: the existing reviewer must be adopted", got)
	}
	if f.launcher.probeCalls == 0 {
		t.Fatal("recovery never probed the deterministic identity; it cannot have distinguished the two cases")
	}
	// The reviewer was ADOPTED, not closed out and replaced: the same review run
	// still owns the step, and its launch is now confirmed.
	if got := f.authoritativeRunID(); got != subject {
		t.Fatalf("authority = %q, want the adopted reviewer's run %q", got, subject)
	}
	if got := f.reviewRun(subject).Status; got == domain.ReviewRunFailed {
		t.Fatal("a reviewer that demonstrably exists was closed out as a failed launch")
	}
	if !f.hasPhase("review_launch_confirmed") {
		t.Fatalf("the recovered launch was never confirmed; phases = %v", f.checkpointPhases())
	}
	// The adopted reviewer is still live, and recovery brought no new process
	// into existence. (The fixture's own earlier reviewer stays live — a real
	// orphan would too — so the assertion is about what recovery ADDED.)
	if !f.launcher.externalLive[identity] {
		t.Fatalf("the adopted reviewer %s is no longer live", identity)
	}
	for id := range f.launcher.externalLive {
		if !liveBefore[id] && f.launcher.externalLive[id] {
			t.Fatalf("recovery created a second reviewer %s instead of adopting %s", id, identity)
		}
	}
}

// The opposite reading of the same durable state: the reviewer does NOT exist,
// so the identity may be launched — exactly once, under that same identity.
func TestReviewEnsure_MissingReviewerIsLaunchedExactlyOnceUnderTheSameIdentity(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	launchesBefore := f.launcher.launchCalls
	subject := "rr-never-launched"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	// The reviewer is NOT there: the crash landed before Create took effect.
	delete(f.launcher.externalLive, identity)

	f.converge()

	if got := f.launcher.launchCalls - launchesBefore; got > 1 {
		t.Fatalf("external launches = %d, want at most 1", got)
	}
	if f.launcher.externalLive[identity] && f.launcher.launchCalls-launchesBefore == 0 {
		t.Fatal("the identity is live but nothing launched it; the fixture is not modelling the crash")
	}
}

// ---- B4 through the real dispatch path --------------------------------------

// Cancellation lands after the claim and after the step transition, in the
// window before the external launch. The pre-launch revalidation must refuse.
func TestReviewLaunch_CancellationBeforeLaunchProducesNoExternalReviewer(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	f.stallTheReviewer()
	launchesBefore := f.launcher.launchCalls

	// The run is cancelled in the instant before the reviewer is created.
	f.launcher.beforeLaunch = func() {
		if _, err := f.store.UpdateWorkflowRunState(
			f.ctx, f.runID, f.run().State, domain.WorkflowRunCancelled, f.clk.Now()); err != nil {
			t.Fatalf("cancel the run: %v", err)
		}
	}

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// beforeLaunch fires INSIDE Launch, so this dispatch's own launch is not the
	// one under test — what matters is that no FURTHER reviewer appears once the
	// run is terminal, on any number of subsequent passes.
	afterFirst := f.launcher.launchCalls
	for i := 0; i < 3; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	if got := f.launcher.launchCalls - afterFirst; got != 0 {
		t.Fatalf("external launches after cancellation = %d, want 0", got)
	}
	_ = launchesBefore
}

// ---- B5: a replacement that loses authority after launching ----------------
//
// R2 is externally live when R1's late verdict wins. Marking R2's row cancelled
// is not enough — the reviewer process is still running. It must be terminated,
// and the termination must itself be restart-safe.
func TestReviewCancel_LosingReplacementTerminatesItsExternalReviewer(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()

	// The predecessor answers while its replacement is mid-launch.
	f.launcher.beforeLaunch = func() {
		f.reviewRuns.RecordLateReviewVerdict(stalled, domain.VerdictApproved, "late", f.clk.Now())
	}

	f.converge()

	// The REPLACEMENT's reviewer must be gone. (The stalled predecessor's own
	// reviewer may still be live — AO's stall path closes the review row without
	// terminating its process, which is a separate orphan and is reported as an
	// additional finding rather than fixed here.)
	for id, alive := range f.launcher.externalLive {
		if alive && id != "workflow-review-"+stalled {
			t.Fatalf("the losing replacement's reviewer %s is still live", id)
		}
	}
	if f.launcher.cancelCalls == 0 {
		t.Fatal("the losing replacement's external reviewer was never terminated")
	}
	if got := f.reviewRun(stalled).SupersededBy; got != "" {
		t.Fatalf("superseded_by = %q, want empty: the predecessor won", got)
	}
	if got := f.reviewStep().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("review step = %q, want completed from the adopted approval", got)
	}
}

// A cancellation interrupted between its intent and its confirmation must
// finish on the next pass, idempotently.
func TestReviewCancel_InterruptedCancellationFinishesOnRestart(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := f.authoritativeRunID()
	identity := "workflow-review-" + subject
	stepID := f.reviewStep().ID
	rid := subject

	// Durable intent, no confirmation, reviewer still live: exactly what a crash
	// between the two leaves.
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-cancel-intent", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-1", ReviewRunID: &rid, DurablePhase: "review_cancel_intent",
		PayloadVersion: "v1",
		RetryState:     `{"reviewRunId":"` + subject + `","handleId":"` + identity + `"}`,
		CreatedAt:      f.clk.Now(),
	}); err != nil {
		t.Fatalf("seed cancel intent: %v", err)
	}
	if !f.launcher.externalLive[identity] {
		t.Fatal("fixture has no live reviewer to cancel")
	}

	f.converge()

	if f.launcher.externalLive[identity] {
		t.Fatal("the interrupted cancellation was never finished; the reviewer is still live")
	}
	if !f.hasPhase("review_cancel_confirmed") {
		t.Fatalf("no cancel confirmation recorded; phases = %v", f.checkpointPhases())
	}
	// Idempotent: further passes do not re-cancel.
	cancels := f.launcher.cancelCalls
	f.converge()
	if f.launcher.cancelCalls != cancels {
		t.Fatalf("cancel calls %d -> %d on replay; the confirmation did not stop it", cancels, f.launcher.cancelCalls)
	}
}

// ---- B4: uncertainty is never a licence to launch ---------------------------

// A transient probe failure over a reviewer that is genuinely alive. Launching
// here would put a second reviewer on the same work; "I could not tell" must
// never become "safe to launch".
func TestReviewProbe_TransientFailureOverALiveReviewerNeverLaunches(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	launchesBefore := f.launcher.launchCalls
	subject := "rr-probe-error"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.launcher.externalLive[identity] = true // it IS alive
	f.launcher.probeErrorsOnce = true

	f.converge()

	if got := f.launcher.launchCalls - launchesBefore; got != 0 {
		t.Fatalf("external launches = %d, want 0 while the probe could not answer", got)
	}
	if !f.launcher.externalLive[identity] {
		t.Fatal("the live reviewer was destroyed on an unanswered probe")
	}
}

// A session that merely bears the right NAME is not AO's reviewer. It may
// neither be adopted nor destroyed.
func TestReviewProbe_ForeignSessionIsNeitherAdoptedNorDestroyed(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-foreign"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.launcher.externalLive[identity] = true
	f.launcher.foreign = map[string]bool{identity: true}

	f.converge()

	if !f.launcher.externalLive[identity] {
		t.Fatal("a session AO does not own was destroyed")
	}
	if got := f.authoritativeRunID(); got == subject {
		t.Fatal("a session AO cannot correlate was adopted as this step's reviewer")
	}
}

// ---- B1: finalization resumes past the bound pointer ------------------------

// Bound, but the protocol tail never finished. A pointer is not a finished
// dispatch, and repeated restarts must complete it without relaunching.
func TestReviewFinalize_BoundButUnfinishedProtocolCompletesWithoutRelaunch(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	stalled := f.stallTheReviewer()
	f.converge()

	replacement := f.authoritativeRunID()
	if replacement == stalled || replacement == "" {
		t.Fatalf("no replacement bound (got %q)", replacement)
	}
	launchesBefore := f.launcher.launchCalls

	// Rewind the tail: predecessor un-superseded, outbox back to dispatched.
	r := f.reviewRun(stalled)
	r.SupersededBy = ""
	f.reviewRuns.runs[stalled] = r
	// Only THIS replacement's own dispatch row — finalization finishes its own
	// protocol tail, not unrelated historical dispatches.
	replacementKey := ""
	for key, e := range f.store.outbox {
		if strings.Contains(key, "review-replacement") &&
			e.Status == domain.WorkflowOutboxAcknowledged {
			e.Status = domain.WorkflowOutboxDispatched
			f.store.outbox[key] = e
			replacementKey = key
		}
	}
	if replacementKey == "" {
		t.Fatal("no replacement dispatch row to rewind; the test proves nothing")
	}

	f.converge()

	if got := f.reviewRun(stalled).SupersededBy; got != replacement {
		t.Fatalf("superseded_by = %q, want the finalization resumed to %q", got, replacement)
	}
	if got := f.store.outbox[replacementKey].Status; got == domain.WorkflowOutboxDispatched {
		t.Fatalf("the replacement's outbox row %s is left permanently dispatched", replacementKey)
	}
	if got := f.launcher.launchCalls - launchesBefore; got != 0 {
		t.Fatalf("external launches during finalization = %d, want 0", got)
	}
}

// ---- B7: a terminal run still owes its external sessions --------------------

// Cancellation wins, the reviewer exists anyway, and the confirmation was never
// written. Every ordinary recovery path skips a terminal run — this obligation
// must still be found and discharged.
func TestReviewTerminal_UnresolvedReviewerIsTerminatedEvenWhenTheRunIsOver(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-orphan"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.launcher.externalLive[identity] = true

	// The run goes terminal with that launch unresolved.
	if _, err := f.store.UpdateWorkflowRunState(
		f.ctx, f.runID, f.run().State, domain.WorkflowRunCancelled, f.clk.Now()); err != nil {
		t.Fatalf("cancel the run: %v", err)
	}

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if f.launcher.externalLive[identity] {
		t.Fatal("a reviewer AO started was left running after its workflow ended")
	}
	if !f.hasPhase("review_cancel_confirmed") {
		t.Fatalf("the obligation was never discharged durably; phases = %v", f.checkpointPhases())
	}
	// Idempotent across further boots.
	cancels := f.launcher.cancelCalls
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile again: %v", err)
	}
	if f.launcher.cancelCalls != cancels {
		t.Fatalf("cancel calls %d -> %d on a discharged obligation", cancels, f.launcher.cancelCalls)
	}
}

// ---- B3: a stalled reviewer's process must actually be terminated -----------
//
// The stall path used to close the review ROW and leave the pane running, so a
// replacement was launched beside a reviewer that never went away.
func TestReviewStall_TerminatesTheStalledReviewersExternalSession(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	original := f.authoritativeRunID()
	identity := "workflow-review-" + original
	if !f.launcher.externalLive[identity] {
		t.Fatalf("fixture has no live reviewer at %s", identity)
	}

	f.stallTheReviewer()

	if f.launcher.externalLive[identity] {
		t.Fatal("the stalled reviewer's external session is still running")
	}
	if !f.hasPhase("review_cancel_confirmed") {
		t.Fatalf("the stall did not durably confirm termination; phases = %v", f.checkpointPhases())
	}
}

// seedConfirmedLaunch records a launch confirmation carrying the exact runtime
// instance, exactly as a real launch does once the runtime reports it.
func (f *reviewAuthorityFixture) seedConfirmedLaunch(reviewRunID, identity, instance string) {
	f.t.Helper()
	stepID := f.reviewStep().ID
	rid := reviewRunID
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-confirmed-" + reviewRunID, WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-1", ReviewRunID: &rid, DurablePhase: "review_launch_confirmed",
		PayloadVersion: "v1",
		RetryState: `{"reviewRunId":"` + reviewRunID + `","handleId":"` + identity +
			`","instanceId":"` + instance + `"}`,
		CreatedAt: f.clk.Now(),
	}); err != nil {
		f.t.Fatalf("seed confirmation: %v", err)
	}
}

// hasPhaseWithInstance reports whether any checkpoint of that phase recorded the
// given runtime instance.
func (f *reviewAuthorityFixture) hasPhaseWithInstance(phase, instance string) bool {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if cp.DurablePhase != phase {
			continue
		}
		if strings.Contains(cp.RetryState, `"instanceId":"`+instance+`"`) {
			return true
		}
	}
	return false
}

// hasConfirmationFor reports whether a launch confirmation exists for one
// specific review run — as opposed to anywhere in the workflow, which would also
// match earlier, legitimately confirmed cycles.
func (f *reviewAuthorityFixture) hasConfirmationFor(reviewRunID string) bool {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if cp.DurablePhase != "review_launch_confirmed" {
			continue
		}
		if strings.Contains(cp.RetryState, `"reviewRunId":"`+reviewRunID+`"`) {
			return true
		}
	}
	return false
}
