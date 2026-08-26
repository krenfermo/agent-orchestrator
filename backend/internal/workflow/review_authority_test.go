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
