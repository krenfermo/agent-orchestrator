package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// review_ownership_recovery_test.go — the workflow-side consequences of the
// ownership model: what an EXITED reviewer licenses, what an UNIDENTIFIABLE one
// costs, and the two crash boundaries that previously had only a traced
// argument behind them.

// A reviewer AO owns whose process has finished must never be adopted as a live
// one. Adopting it records a confirmed reviewer over a corpse and then waits
// forever for a verdict that cannot arrive.
func TestReviewProbe_ExitedReviewerIsReclaimedRatherThanAdopted(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-exited"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")

	// The reviewer was launched and has since exited; its session lingers.
	f.launcher.externalExited = map[string]bool{identity: true}

	before := f.launcher.launchCalls
	f.converge()

	if f.launcher.externalExited[identity] {
		t.Fatal("the exited reviewer's session was never reclaimed, so its identity is stuck forever")
	}
	// It must not have been adopted: adoption means zero launches AND a
	// confirmation recorded over a dead process.
	if f.launcher.launchCalls == before {
		t.Fatal("a finished reviewer was adopted as a live one instead of being replaced")
	}
}

// UNKNOWN over an unresolved obligation used to be a bare `continue`: skipped
// every pass, nothing written, nobody told. That silence is what let a live
// orphan stay invisible.
func TestReviewSweep_UnprovableReviewerAccumulatesEvidenceThenEscalates(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-unprovable"
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")

	// The run ends with that launch unresolved, and the identity is
	// unclassifiable from here on.
	if _, err := f.store.UpdateWorkflowRunState(
		f.ctx, f.runID, f.run().State, domain.WorkflowRunCancelled, f.clk.Now()); err != nil {
		t.Fatalf("cancel the run: %v", err)
	}
	f.launcher.probeUnknown = true

	launches := f.launcher.launchCalls
	cancels := f.launcher.cancelCalls

	// Well inside the retry budget: evidence accrues, nothing is acted on.
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !f.hasPhase("review_reviewer_unproven") {
		t.Fatalf("no durable evidence was written for an unprovable obligation; phases = %v", f.checkpointPhases())
	}
	if f.launcher.launchCalls != launches {
		t.Fatal("a replacement was launched over a reviewer AO could not classify")
	}
	if f.launcher.cancelCalls != cancels {
		t.Fatal("AO tried to destroy a session it cannot prove it owns")
	}

	// Spend the budget. The obligation must become a person's problem rather
	// than an infinite quiet loop.
	for i := 0; i < 8; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	if f.launcher.launchCalls != launches {
		t.Fatal("a replacement was eventually launched over an unclassifiable reviewer")
	}
	if f.launcher.cancelCalls != cancels {
		t.Fatal("AO eventually destroyed a session it cannot prove it owns")
	}
	if !f.hasPhase(workflowcore.AmbiguousWorkerStateEvidencePhase) {
		t.Fatalf("the unresolved obligation never escalated to a bounded incident; phases = %v",
			f.checkpointPhases())
	}
}

// FOREIGN must stay untouched, but it must not be silent either: a session
// sitting on a reviewer identity is a fact worth recording.
func TestReviewSweep_ForeignSessionIsRecordedAndLeftAlone(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-foreign-sweep"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	if _, err := f.store.UpdateWorkflowRunState(
		f.ctx, f.runID, f.run().State, domain.WorkflowRunCancelled, f.clk.Now()); err != nil {
		t.Fatalf("cancel the run: %v", err)
	}
	f.launcher.foreign = map[string]bool{identity: true}

	cancels := f.launcher.cancelCalls
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if f.launcher.cancelCalls != cancels {
		t.Fatal("a foreign session was destroyed")
	}
	if !f.hasPhase("review_reviewer_unproven") {
		t.Fatalf("a foreign session on a reviewer identity went unrecorded; phases = %v", f.checkpointPhases())
	}
}

// Case 10 of the crash matrix: the workflow is cancelled AFTER the final
// pre-launch authority check passes, so the reviewer is launched into a run that
// is already over. It must be terminated, not left behind.
func TestReviewCase10_CancellationRightAfterThePreLaunchCheckLeavesNoOrphan(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-case10"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")

	// The cancellation lands in the instant between the last authority check and
	// the launch — the one window re-reading the store cannot close.
	f.launcher.beforeLaunch = func() {
		if _, err := f.store.UpdateWorkflowRunState(
			f.ctx, f.runID, f.run().State, domain.WorkflowRunCancelled, f.clk.Now()); err != nil {
			t.Fatalf("cancel mid-launch: %v", err)
		}
	}

	// One drive to reach the launch (and the cancellation inside it), then the
	// boot path, which is what a terminal run gets from then on. ContinueRun
	// refuses a cancelled run, so its error here is the expected outcome rather
	// than a failure.
	_, _ = f.c.ContinueRun(f.ctx, f.runID)
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if f.launcher.externalLive[identity] {
		t.Fatal("a reviewer launched into a cancelled run was left running")
	}
}

// Case 11: the launch succeeds after that same race and the process dies before
// the confirmation is written. The terminal sweep must find the identity from
// the INTENT alone and discharge it — twice over, idempotently.
func TestReviewCase11_LaunchedThenCrashedBeforeConfirmationIsSweptExactlyOnce(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-case11"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)

	// Durable state at the crash: an intent naming the identity, no
	// confirmation. Externally: a live reviewer.
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.launcher.externalLive[identity] = true

	if _, err := f.store.UpdateWorkflowRunState(
		f.ctx, f.runID, f.run().State, domain.WorkflowRunCancelled, f.clk.Now()); err != nil {
		t.Fatalf("cancel the run: %v", err)
	}

	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if f.launcher.externalLive[identity] {
		t.Fatal("the unconfirmed reviewer survived the sweep")
	}
	if !f.hasPhase("review_cancel_confirmed") {
		t.Fatalf("the obligation was not discharged durably; phases = %v", f.checkpointPhases())
	}

	// A second sweep must do nothing at all.
	cancels := f.launcher.cancelCalls
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile again: %v", err)
	}
	if f.launcher.cancelCalls != cancels {
		t.Fatalf("second sweep acted again: cancel calls %d -> %d", cancels, f.launcher.cancelCalls)
	}
}

// countPhase counts durable checkpoints of one phase for this run.
func (f *reviewAuthorityFixture) countPhase(phase string) int {
	f.t.Helper()
	n := 0
	for _, p := range f.checkpointPhases() {
		if p == phase {
			n++
		}
	}
	return n
}

// TestReviewSweep_UnprovenEvidenceIsBoundedByItsProbeBudget is BLOCKER 3.
//
// The budget used to govern only the incident, never the ledger: the evidence
// row was appended BEFORE the budget was consulted, so every boot, wake, and
// reconcile added another identical record for an obligation that had already
// been escalated. Incident dedupe hid it; the checkpoint table grew forever.
func TestReviewSweep_UnprovenEvidenceIsBoundedByItsProbeBudget(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-bounded"
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	if _, err := f.store.UpdateWorkflowRunState(
		f.ctx, f.runID, f.run().State, domain.WorkflowRunCancelled, f.clk.Now()); err != nil {
		t.Fatalf("cancel the run: %v", err)
	}
	f.launcher.probeUnknown = true

	const budget = 5

	// Probes 1..5 each write exactly one observation.
	for i := 1; i <= budget; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
		if got := f.countPhase("review_reviewer_unproven"); got != i {
			t.Fatalf("after probe %d: evidence records = %d, want %d", i, got, i)
		}
	}

	// Probe 6 and everything after it must add nothing. This is the assertion
	// that fails if the append moves back before the budget check.
	for i := 0; i < 12; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile past budget %d: %v", i, err)
		}
	}
	if got := f.countPhase("review_reviewer_unproven"); got != budget {
		t.Fatalf("evidence records = %d after 12 further reconciles, want it capped at %d", got, budget)
	}

	// Every other entry point must be a no-op too — the ledger must not grow on
	// a read, a resume, or a wake.
	_, _ = f.c.GetRun(f.ctx, f.runID)
	_, _ = f.c.ContinueRun(f.ctx, f.runID)
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile after other entry points: %v", err)
	}
	if got := f.countPhase("review_reviewer_unproven"); got != budget {
		t.Fatalf("evidence records = %d after GetRun/ContinueRun/Reconcile, want %d", got, budget)
	}
}

// The budget belongs to the OBLIGATION, so it survives restart and a genuinely
// different reviewer obligation gets its own.
func TestReviewSweep_ProbeBudgetIsDurablePerReviewerObligation(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	first := "rr-budget-a"
	f.crashIdentity(first)
	f.seedLaunchPhaseFor(first, "review_launch_intent")
	if _, err := f.store.UpdateWorkflowRunState(
		f.ctx, f.runID, f.run().State, domain.WorkflowRunCancelled, f.clk.Now()); err != nil {
		t.Fatalf("cancel the run: %v", err)
	}
	f.launcher.probeUnknown = true

	for i := 0; i < 3; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	spent := f.countPhase("review_reviewer_unproven")
	if spent != 3 {
		t.Fatalf("evidence records = %d, want 3", spent)
	}

	// "Restart": a fresh coordinator over the SAME durable store must resume the
	// budget rather than reset it. If the count came from process memory, the
	// next three probes would write three more and the cap would never bind.
	for i := 0; i < 6; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile after restart %d: %v", i, err)
		}
	}
	if got := f.countPhase("review_reviewer_unproven"); got != 5 {
		t.Fatalf("evidence records = %d across a restart, want the durable cap of 5", got)
	}

	// A DIFFERENT unresolved reviewer obligation gets its own budget: capping
	// per step rather than per obligation would silence a real second orphan.
	second := "rr-budget-b"
	f.seedLaunchPhaseFor(second, "review_launch_intent")
	f.reviewRuns.runs[second] = domain.ReviewRun{
		ID: second, Status: domain.ReviewRunRunning, CreatedAt: f.clk.Now(),
	}
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile with a second obligation: %v", err)
	}
	if got := f.countPhase("review_reviewer_unproven"); got != 6 {
		t.Fatalf("evidence records = %d, want 6 — a new obligation must get its own budget", got)
	}
}

// TestReviewRecovery_LiveReviewerIsNeverReplacedWhileItWorks is Codex's blocker
// at the level where it would actually have hurt: a live reviewer that happens
// to be running `cat` must not be destroyed and duplicated.
//
// The fake models presence, not commands — which is the point. Whatever the
// reviewer is executing, its presence stays `owned`, and `owned` must produce
// adoption rather than a second launch.
func TestReviewRecovery_LiveReviewerIsNeverReplacedWhileItWorks(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-live-cat"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")

	// A reviewer AO started, still working.
	f.launcher.externalLive[identity] = true

	launches := f.launcher.launchCalls
	cancels := f.launcher.cancelCalls

	f.converge()

	if got := f.launcher.launchCalls - launches; got != 0 {
		t.Fatalf("reviewer launches = %d over a live reviewer, want 0 — active work would be duplicated", got)
	}
	if got := f.launcher.cancelCalls - cancels; got != 0 {
		t.Fatalf("cancel calls = %d against a live reviewer, want 0 — active work would be killed", got)
	}
	if !f.launcher.externalLive[identity] {
		t.Fatal("the live reviewer was destroyed")
	}
	if !f.hasPhase("review_launch_confirmed") {
		t.Fatalf("the live reviewer was not adopted; phases = %v", f.checkpointPhases())
	}
}

// BLOCKER 1: the exact instance must survive a restart.
//
// A launch that persists only the reusable name leaves recovery no choice but to
// re-resolve it — and after the reviewer exits and a stranger takes the name,
// that resolution answers about the stranger. The instance recorded with the
// confirmation is what makes the reviewer addressable across a restart.
func TestRecovery_PersistedInstanceStopsAReplacementBeingAdopted(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-instance"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")

	// A real launch: the reviewer exists and its instance is recorded.
	f.launcher.externalLive[identity] = true
	f.launcher.instances = map[string]string{identity: "$1"}
	f.seedConfirmedLaunch(subject, identity, "$1")

	// AO's reviewer exits and a STRANGER takes the same name.
	delete(f.launcher.externalLive, identity)
	f.launcher.instances[identity] = "$2"
	f.launcher.externalLive[identity] = true

	f.converge()

	// The stranger must survive untouched...
	if f.launcher.instances[identity] != "$2" {
		t.Fatal("the replacement incarnation was destroyed or swapped out")
	}
	if !f.launcher.externalLive[identity] {
		t.Fatal("the replacement's session was torn down")
	}
	// ...and must never be recorded as the reviewer AO launched.
	if f.hasPhaseWithInstance("review_launch_confirmed", "$2") {
		t.Fatal("the replacement's instance was recorded as AO's confirmed reviewer")
	}
}

// The stranger must never be terminated by a recovery pass aimed at $1.
func TestRecovery_ReplacementUnderTheSameNameIsNeverTerminated(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-instance-kill"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.launcher.instances = map[string]string{identity: "$1"}
	f.launcher.externalLive[identity] = true
	f.seedConfirmedLaunch(subject, identity, "$1")

	// AO's reviewer is gone; a stranger holds the name.
	f.launcher.instances[identity] = "$2"

	if _, err := f.store.UpdateWorkflowRunState(
		f.ctx, f.runID, f.run().State, domain.WorkflowRunCancelled, f.clk.Now()); err != nil {
		t.Fatalf("cancel the run: %v", err)
	}
	if err := f.c.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if f.launcher.instances[identity] != "$2" {
		t.Fatal("THE REPLACEMENT WAS DESTROYED by a sweep aimed at the original instance")
	}
	if !f.launcher.externalLive[identity] {
		t.Fatal("the replacement's session was torn down")
	}
}

// The persisted record must carry the instance, written by the PRODUCTION
// launch path rather than seeded by a test.
//
// This is the write half of the invariant: every other instance test reads a
// record, so if the confirmation ever stopped recording the instance they would
// all keep passing while recovery quietly lost its authority key.
func TestLaunch_ConfirmationPersistsTheRuntimeInstance(t *testing.T) {
	f := newReviewAuthorityFixture(t)

	// The fixture's own first review dispatch is a real launch: it goes through
	// dispatchReviewStep -> ensureReviewerLaunched -> recordReviewLaunchConfirmed.
	identity := "workflow-review-" + f.authoritativeRunID()
	instance := f.launcher.instances[identity]
	if instance == "" {
		t.Fatalf("precondition: the launch recorded no runtime instance for %s (instances=%v)",
			identity, f.launcher.instances)
	}

	if !f.hasPhaseWithInstance("review_launch_confirmed", instance) {
		t.Fatalf("the launch confirmation did not persist the runtime instance %q; "+
			"recovery would have to re-resolve the reusable name. checkpoints=%v",
			instance, f.checkpointPhases())
	}
}

// BLOCKER 2: a confirmation without an exact instance must never reach the
// ledger. It is the record every later pass trusts, so one that names only a
// reusable session is not a weaker proof — it is a false one.
func TestConfirmation_IsRefusedWithoutAnExactRuntimeInstance(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-no-instance"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")

	// A reviewer is genuinely there, but the launcher cannot pin it to an
	// incarnation.
	f.launcher.externalLive[identity] = true
	f.launcher.ownedWithoutInstance = true

	launches := f.launcher.launchCalls
	f.converge()

	if f.hasConfirmationFor(subject) {
		t.Fatalf("a confirmation was written for %s without an exact instance; phases = %v",
			subject, f.checkpointPhases())
	}
	// And crucially: refusing to confirm must not turn into launching a second
	// reviewer over the live one.
	if got := f.launcher.launchCalls - launches; got != 0 {
		t.Fatalf("reviewer launches = %d over a live reviewer, want 0", got)
	}
	if !f.launcher.externalLive[identity] {
		t.Fatal("the live reviewer was destroyed")
	}
}

// Repeating that situation must converge without ever duplicating the reviewer.
func TestConfirmation_UnprovableIdentityNeverDuplicatesTheReviewer(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-no-instance-loop"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.launcher.externalLive[identity] = true
	f.launcher.ownedWithoutInstance = true

	launches := f.launcher.launchCalls
	for i := 0; i < 6; i++ {
		_, _ = f.c.ContinueRun(f.ctx, f.runID)
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	if got := f.launcher.launchCalls - launches; got != 0 {
		t.Fatalf("reviewer launches = %d across 6 passes, want 0", got)
	}
	if f.hasConfirmationFor(subject) {
		t.Fatalf("a confirmation was eventually written for %s without an exact instance", subject)
	}
}

// BLOCKER 1: adoption must persist the instance from the SAME probe that proved
// ownership. A second look-up is a second moment, and a replacement arriving in
// between would be recorded as this reviewer's confirmation.
func TestAdoption_ReplacementDuringTheProbeIsNeverConfirmed(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-adopt-race"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")

	// AO's reviewer is live under instance $7. The instant it is observed, it
	// exits and a stranger takes the name under $9.
	f.launcher.externalLive[identity] = true
	f.launcher.instances = map[string]string{identity: "$7"}
	f.launcher.afterProbe = func() {
		f.launcher.instances[identity] = "$9"
	}

	f.converge()

	// Whatever was decided, the STRANGER's incarnation must never appear as this
	// reviewer's confirmation.
	if f.hasPhaseWithInstance("review_launch_confirmed", "$9") {
		t.Fatal("a replacement's instance was persisted as the adopted reviewer's confirmation")
	}
}

// ---- no authority without exact instance proof ---------------------------

// BLOCKER 1: a review_run created before the external launch must never be
// bound. The row is an intent; dispatch writes it BEFORE launching anything.
func TestRecovery_ReviewRunWithNoLaunchRecordIsNeverBound(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-no-launch"
	f.crashIdentity(subject)
	// Deliberately NO launch markers at all: the crash landed between inserting
	// the review run and recording any launch intent.

	launches := f.launcher.launchCalls
	f.converge()

	if got := f.authoritativeRunID(); got == subject {
		t.Fatal("a review run with no launch record became the step's authority; " +
			"nothing would ever launch a reviewer for it")
	}
	if st := f.reviewRun(subject).Status; st != domain.ReviewRunFailed {
		t.Fatalf("the unlaunched review run status = %q, want failed so it cannot be re-adopted", st)
	}
	// The protocol resumed and produced exactly one reviewer.
	if got := f.launcher.launchCalls - launches; got != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", got)
	}
}

// BLOCKER 2 (1): a confirmation carrying no instance is not a confirmation.
func TestLegacy_ConfirmationWithoutInstanceIsNotBindable(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-legacy-confirmed"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	// A pre-instance confirmation: names the reviewer, not the incarnation.
	f.seedLaunchPhaseFor(subject, "review_launch_confirmed")
	// And nothing is actually there any more — the name is free.
	delete(f.launcher.externalLive, identity)

	f.converge()

	if got := f.authoritativeRunID(); got == subject {
		t.Fatal("a name-only confirmation conferred authority without exact runtime proof")
	}
}

// BLOCKER 2 (3): a legacy record whose reviewer IS still there is upgraded by a
// coherent probe — the instance is persisted, and only then does it confer
// authority.
func TestLegacy_LiveReviewerIsUpgradedToAnExactInstanceThenConfirmed(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-legacy-upgrade"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.seedLaunchPhaseFor(subject, "review_launch_confirmed") // no instance

	// The reviewer really is running, under a real incarnation.
	f.launcher.externalLive[identity] = true
	f.launcher.instances = map[string]string{identity: "$4"}

	launches := f.launcher.launchCalls
	f.converge()

	if !f.hasPhaseWithInstance("review_launch_confirmed", "$4") {
		t.Fatalf("the legacy record was not upgraded with an exact instance; phases = %v",
			f.checkpointPhases())
	}
	if got := f.authoritativeRunID(); got != subject {
		t.Fatalf("authority = %q, want the upgraded reviewer %q", got, subject)
	}
	if got := f.launcher.launchCalls - launches; got != 0 {
		t.Fatalf("reviewer launches = %d, want 0 — the live reviewer must be adopted", got)
	}
}

// BLOCKER 2 (5): a legacy record over something AO cannot prove is its own must
// neither adopt nor destroy, and must not relaunch over a possibly-live process.
func TestLegacy_UnprovableReviewerIsNeitherAdoptedNorDestroyed(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-legacy-foreign"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.seedLaunchPhaseFor(subject, "review_launch_confirmed") // no instance
	f.launcher.foreign = map[string]bool{identity: true}

	launches := f.launcher.launchCalls
	cancels := f.launcher.cancelCalls
	f.converge()

	if got := f.authoritativeRunID(); got == subject {
		t.Fatal("a session AO cannot prove it owns conferred authority")
	}
	if got := f.launcher.cancelCalls - cancels; got != 0 {
		t.Fatalf("cancel calls = %d against a foreign session, want 0", got)
	}
	if got := f.launcher.launchCalls - launches; got != 0 {
		t.Fatalf("reviewer launches = %d over an unprovable session, want 0", got)
	}
}

// BLOCKER 2 (6): once upgraded, restart uses the persisted instance and does not
// re-probe by name.
func TestLegacy_AfterUpgradeRestartUsesThePersistedInstance(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-legacy-restart"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.seedLaunchPhaseFor(subject, "review_launch_confirmed")
	f.launcher.externalLive[identity] = true
	f.launcher.instances = map[string]string{identity: "$5"}

	f.converge()
	if !f.hasPhaseWithInstance("review_launch_confirmed", "$5") {
		t.Fatalf("precondition: the upgrade did not persist; phases = %v", f.checkpointPhases())
	}

	// "Restart": the reviewer exits and a stranger takes the name. The persisted
	// $5 must keep the stranger from being treated as this reviewer.
	f.launcher.instances[identity] = "$6"

	launches := f.launcher.launchCalls
	cancels := f.launcher.cancelCalls
	f.converge()

	if f.hasPhaseWithInstance("review_launch_confirmed", "$6") {
		t.Fatal("the stranger's instance was recorded as this reviewer's confirmation")
	}
	if got := f.launcher.cancelCalls - cancels; got != 0 {
		t.Fatalf("cancel calls = %d against a stranger, want 0", got)
	}
	if f.launcher.instances[identity] != "$6" {
		t.Fatal("the stranger's session was destroyed")
	}
	_ = launches
}

// ---- interrupted abandon converges --------------------------------------
//
// Abandoning an unlaunched review identity is two writes: mark the run failed,
// then give the outbox claim back. A crash between them leaves failed row +
// dispatched outbox, which used to read as ambiguity — no reviewer, no way to
// get one, needs_attention forever.

// seedInterruptedAbandon reproduces that exact durable state: the abandon intent
// and the failed row are on disk, and the claim was never released.
func (f *reviewAuthorityFixture) seedInterruptedAbandon(reviewRunID, key string) {
	f.t.Helper()
	stepID := f.reviewStep().ID
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-abandon-" + reviewRunID, WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-1", DurablePhase: "review_launch_abandoned", PayloadVersion: "v1",
		RetryState: `{"reviewRunId":"` + reviewRunID + `","idempotencyKey":"` + key +
			`","why":"no reviewer was ever launched"}`,
		CreatedAt: f.clk.Now(),
	}); err != nil {
		f.t.Fatalf("seed abandon intent: %v", err)
	}
	run := f.reviewRuns.runs[reviewRunID]
	run.Status = domain.ReviewRunFailed
	f.reviewRuns.runs[reviewRunID] = run
}

// A. Crash between marking the row failed and releasing the claim.
func TestAbandon_InterruptedRecoveryConvergesOnRestart(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-abandon-crash"
	f.crashIdentity(subject)
	key := f.seedCrashBeforeBind(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.seedInterruptedAbandon(subject, key)

	launches := f.launcher.launchCalls
	f.converge()

	if f.outboxStatus(key) == domain.WorkflowOutboxDispatched {
		t.Fatal("the outbox is still dispatched over a review run that will never have a reviewer")
	}
	if got := f.launcher.launchCalls - launches; got != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1 — the protocol must resume", got)
	}
	if f.run().State == domain.WorkflowRunNeedsAttention {
		t.Fatal("an interrupted abandon was reported as ambiguity instead of being finished")
	}
}

// B. Same, for a legacy record whose reviewer is proven gone.
func TestAbandon_InterruptedLegacyAbsentRecoveryConverges(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-abandon-legacy"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	key := f.seedCrashBeforeBind(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.seedLaunchPhaseFor(subject, "review_launch_confirmed") // name-only, legacy
	delete(f.launcher.externalLive, identity)                // proven gone
	f.seedInterruptedAbandon(subject, key)

	launches := f.launcher.launchCalls
	f.converge()

	if f.outboxStatus(key) == domain.WorkflowOutboxDispatched {
		t.Fatal("the outbox is still dispatched after an interrupted legacy abandon")
	}
	if got := f.launcher.launchCalls - launches; got != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1", got)
	}
}

// C. Repeating restart/reconcile must still yield exactly one reviewer.
func TestAbandon_RepeatedRestartsStillProduceExactlyOneReviewer(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-abandon-repeat"
	f.crashIdentity(subject)
	key := f.seedCrashBeforeBind(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.seedInterruptedAbandon(subject, key)

	launches := f.launcher.launchCalls
	for i := 0; i < 5; i++ {
		_, _ = f.c.ContinueRun(f.ctx, f.runID)
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	if got := f.launcher.launchCalls - launches; got != 1 {
		t.Fatalf("reviewer launches = %d across 5 restarts, want exactly 1", got)
	}
}

// D. A failed row WITHOUT the abandon marker must NOT be treated as recoverable.
// Recovery requires durable proof that this specific failure came from the
// unlaunched/absent path; every other failure keeps its conservative ambiguity.
func TestAbandon_FailedRunWithoutTheMarkerIsStillAmbiguous(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-failed-other-cause"
	f.crashIdentity(subject)
	key := f.seedCrashBeforeBind(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")

	// Failed for some OTHER reason — no abandon intent on disk.
	run := f.reviewRuns.runs[subject]
	run.Status = domain.ReviewRunFailed
	f.reviewRuns.runs[subject] = run

	launches := f.launcher.launchCalls
	f.converge()

	if got := f.launcher.launchCalls - launches; got != 0 {
		t.Fatalf("reviewer launches = %d, want 0 — a failure AO cannot attribute "+
			"must not silently resume a dispatch", got)
	}
	if f.outboxStatus(key) != domain.WorkflowOutboxDispatched {
		t.Fatal("an unattributable failure released its claim without proof")
	}
	if f.run().State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention for an unattributable failure", f.run().State)
	}
}

// The abandon INTENT must be written by production before the two writes it
// protects — not merely understood when a test seeds it.
//
// Without this, every convergence test above would keep passing while the
// marker stopped being written, and the crash window would silently reopen: the
// pair (failed row, dispatched outbox) would once again be unattributable.
func TestAbandon_ProductionWritesTheIntentBeforeAbandoning(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-abandon-write"
	identity := "workflow-review-" + subject
	f.crashIdentity(subject)
	f.seedCrashBeforeBind(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	// Nothing was ever launched: the identity is free.
	delete(f.launcher.externalLive, identity)

	f.converge()

	if !f.hasAbandonIntentFor(subject) {
		t.Fatalf("production abandoned review run %s without recording the durable intent; "+
			"a crash mid-abandon would be unattributable again. phases = %v",
			subject, f.checkpointPhases())
	}
	// And the abandon actually completed.
	if st := f.reviewRun(subject).Status; st != domain.ReviewRunFailed {
		t.Fatalf("review run status = %q, want failed", st)
	}
}

// hasAbandonIntentFor reports whether an abandon intent exists for one review
// run.
func (f *reviewAuthorityFixture) hasAbandonIntentFor(reviewRunID string) bool {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if cp.DurablePhase != "review_launch_abandoned" {
			continue
		}
		if strings.Contains(cp.RetryState, `"reviewRunId":"`+reviewRunID+`"`) {
			return true
		}
	}
	return false
}

// ---- abandon-marker correlation and fail-closed evidence -----------------

// BLOCKER 1: an abandon marker authorises releasing ONE claim — the one it
// names. A step is dispatched many times across cycles and replacements, so a
// marker from an earlier generation must not release a newer dispatch's claim.
func TestAbandon_StaleMarkerCannotReleaseANewerClaim(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-stale-marker"
	f.crashIdentity(subject)
	currentKey := f.seedCrashBeforeBind(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")

	// An abandon marker from an OLD dispatch generation: same run, same step,
	// same review run — different claim.
	f.seedInterruptedAbandon(subject, "workflow-step-review:"+f.reviewStep().ID+":cycle0:codex")

	launches := f.launcher.launchCalls
	f.converge()

	if f.outboxStatus(currentKey) != domain.WorkflowOutboxDispatched {
		t.Fatal("a stale abandon marker released a claim it never named")
	}
	if got := f.launcher.launchCalls - launches; got != 0 {
		t.Fatalf("reviewer launches = %d, want 0 — a stale marker must authorise nothing", got)
	}
	if f.run().State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention for an unattributable failure", f.run().State)
	}
}

// BLOCKER 2: an unreadable ledger is the ABSENCE of proof. It must never be
// converted into proof.
func TestAbandon_UnreadableLedgerFailsClosed(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-ledger-error"
	f.crashIdentity(subject)
	key := f.seedCrashBeforeBind(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.seedInterruptedAbandon(subject, key)

	// From here AO cannot read its own evidence.
	f.store.checkpointListErr = errors.New("checkpoint ledger unavailable")

	launches := f.launcher.launchCalls
	cancels := f.launcher.cancelCalls
	// The ledger error may surface to the caller; what matters is that nothing
	// unsafe was done on the way. Drive several passes and tolerate it.
	for i := 0; i < 4; i++ {
		_, _ = f.c.ContinueRun(f.ctx, f.runID)
		_ = f.c.Reconcile(f.ctx)
	}

	if f.outboxStatus(key) != domain.WorkflowOutboxDispatched {
		t.Fatal("a claim was released on evidence AO could not read")
	}
	if got := f.launcher.launchCalls - launches; got != 0 {
		t.Fatalf("reviewer launches = %d on unreadable evidence, want 0", got)
	}
	if got := f.launcher.cancelCalls - cancels; got != 0 {
		t.Fatalf("cancel calls = %d on unreadable evidence, want 0", got)
	}
}

// BLOCKER 3: the authority-loss path must write the abandon intent too. It used
// to mark the row failed and release the claim directly, so a crash between
// those two writes left the unattributable pair this protocol exists to remove.
//
// Built without the shared fixture, because the hook has to be armed BEFORE the
// step's first review dispatch: the authorization is re-read between creating
// the review row and launching, and only a fresh dispatch passes through there.
func TestAbandon_AuthorityLossWritesTheIntentBeforeFailing(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{
		rec:   domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}},
		facts: sessionFacts,
	}
	wsFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, wsFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, wsFacts, created.Run.ID)

	// The step is cancelled once the launch intent is durable — after the review
	// row exists, before the pre-launch re-read. That re-read then refuses, and
	// the authority-loss path runs with a review row to close out.
	var reviewStepID string
	steps, _ := store.ListWorkflowSteps(ctx, created.Run.ID)
	for _, st := range steps {
		if st.Kind == domain.WorkflowStepReview {
			reviewStepID = st.ID
		}
	}
	reviewRuns.afterInsertReviewRun = func(string) {
		now, _ := store.ListWorkflowSteps(ctx, created.Run.ID)
		for _, st := range now {
			if st.ID != reviewStepID {
				continue
			}
			if _, err := store.UpdateWorkflowStepState(
				ctx, reviewStepID, st.State, domain.WorkflowStepCancelled, clk.Now()); err != nil {
				t.Fatalf("cancel the step: %v", err)
			}
		}
	}

	_, _ = c.ContinueRun(ctx, created.Run.ID)

	// Every review run closed out as failed must carry an abandon intent naming
	// its claim; a bare failed row is the unattributable state.
	failed, attributed := 0, 0
	cps, _ := store.ListWorkflowCheckpoints(ctx, created.Run.ID)
	for id, rr := range reviewRuns.runs {
		if rr.Status != domain.ReviewRunFailed {
			continue
		}
		failed++
		for _, cp := range cps {
			if cp.DurablePhase == "review_launch_abandoned" &&
				strings.Contains(cp.RetryState, `"reviewRunId":"`+id+`"`) &&
				strings.Contains(cp.RetryState, `"idempotencyKey":"`) {
				attributed++
				break
			}
		}
	}
	if failed == 0 {
		t.Fatal("precondition: the authority-loss path never closed out a review run")
	}
	if attributed != failed {
		t.Fatalf("%d of %d failed review runs carry no abandon intent naming their claim; "+
			"a crash mid-cleanup would be unattributable", failed-attributed, failed)
	}
}

// The fail-closed branch itself: the failed-row recovery reaches the point of
// asking for its evidence, and THAT read is the one that fails.
//
// The broader test above proves nothing unsafe happens when the whole ledger is
// down; this one proves the specific branch refuses rather than assuming.
func TestAbandon_EvidenceReadFailureAtTheDecisionFailsClosed(t *testing.T) {
	f := newReviewAuthorityFixture(t)
	subject := "rr-evidence-error"
	f.crashIdentity(subject)
	key := f.seedCrashBeforeBind(subject)
	f.seedLaunchPhaseFor(subject, "review_launch_intent")
	f.seedInterruptedAbandon(subject, key)

	launches := f.launcher.launchCalls

	// Let the decision get underway, then make the evidence unreadable. Sweeping
	// the offset finds the pass in which the abandon-marker read is the one that
	// meets the failure; the invariant asserted is the same at every offset.
	for offset := 1; offset <= 12; offset++ {
		f.store.checkpointListCalls = 0
		f.store.checkpointListErrAfter = offset
		f.store.checkpointListErr = errors.New("checkpoint ledger unavailable")
		_, _ = f.c.ContinueRun(f.ctx, f.runID)
		_ = f.c.Reconcile(f.ctx)
		f.store.checkpointListErr = nil

		if f.outboxStatus(key) != domain.WorkflowOutboxDispatched {
			t.Fatalf("offset %d: a claim was released on evidence AO could not read", offset)
		}
		if got := f.launcher.launchCalls - launches; got != 0 {
			t.Fatalf("offset %d: reviewer launches = %d on unreadable evidence, want 0", offset, got)
		}
	}
}

// ---- retry budget survives crashes ---------------------------------------
//
// The budget used to be derived only from `reviewer_launch_error`, which is
// written AFTER the review run is marked failed. A crash in between left an
// attempt that had demonstrably happened with nothing on the ledger to say so,
// and recovery relaunched as attempt 1. Repeated, that produces review
// generations without limit.

// budgetFixture drives a run up to the point of its FIRST review dispatch, so a
// test can make that launch fail. The shared authority fixture has already
// dispatched successfully by the time it is handed over, which is why these
// tests build their own.
type budgetFixture struct {
	t        *testing.T
	ctx      context.Context
	c        *workflowcore.Coordinator
	store    *fakeStore
	runs     *fakeReviewRuns
	launcher *fakeReviewerLauncher
	clk      *fakeClock
	runID    string
}

func newBudgetFixture(t *testing.T) *budgetFixture {
	t.Helper()
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{
		rec:   domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}},
		facts: sessionFacts,
	}
	wsFacts := &fakeWorkspaceFacts{}
	runs := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, wsFacts, runs, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, wsFacts, created.Run.ID)
	return &budgetFixture{
		t: t, ctx: ctx, c: c, store: store, runs: runs, launcher: launcher, clk: clk,
		runID: created.Run.ID,
	}
}

// failLaunch makes the next reviewer launch fail and runs one pass.
func (f *budgetFixture) failLaunch() {
	f.t.Helper()
	f.launcher.launchErr = errors.New("reviewer binary exploded")
	_, _ = f.c.ContinueRun(f.ctx, f.runID)
	f.launcher.launchErr = nil
}

func (f *budgetFixture) checkpoints() []domain.WorkflowCheckpoint {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		return nil
	}
	return cps
}

func (f *budgetFixture) countPhase(phase string) int {
	f.t.Helper()
	n := 0
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

// hasAttemptRecorded reports whether any durable record consumed that attempt.
func (f *budgetFixture) hasAttemptRecorded(attempt int) bool {
	f.t.Helper()
	needle := `"attempt":` + strconv.Itoa(attempt)
	for _, cp := range f.checkpoints() {
		switch cp.DurablePhase {
		case "review_launch_attempt", "review_launch_abandoned", "reviewer_launch_error":
		default:
			continue
		}
		if strings.Contains(cp.RetryState, needle) {
			return true
		}
	}
	return false
}

// crashBeforeFailureCheckpoint runs one failing launch that dies just before the
// bookkeeping checkpoint — the exact window the budget used to leak through.
func (f *budgetFixture) crashBeforeFailureCheckpoint() {
	f.t.Helper()
	// The fake clears the hook before invoking it, so a filtered hook has to
	// re-arm itself or it is spent on the first unrelated insert.
	var arm func()
	arm = func() {
		f.store.beforeCheckpointInsert = func(cp domain.WorkflowCheckpoint) {
			if cp.DurablePhase != "reviewer_launch_error" {
				arm()
				return
			}
			panic("crash-before-failure-checkpoint")
		}
	}
	arm()
	defer func() {
		f.store.beforeCheckpointInsert = nil
		if r := recover(); r != nil && r != "crash-before-failure-checkpoint" {
			panic(r)
		}
	}()
	f.failLaunch()
}

// 1 + 4. A crash after the failed-row write but before the failure checkpoint
// must still consume the attempt, and the next retry must be N+1.
func TestRetryBudget_AttemptSurvivesACrashBeforeTheFailureCheckpoint(t *testing.T) {
	f := newBudgetFixture(t)
	f.crashBeforeFailureCheckpoint()

	// The bookkeeping checkpoint never landed...
	if got := f.countPhase("reviewer_launch_error"); got != 0 {
		t.Fatalf("precondition: %d failure checkpoints written, want 0", got)
	}
	// ...but the attempt is durable in the abandon intent.
	if !f.hasAttemptRecorded(1) {
		t.Fatal("attempt 1 was not durably consumed; a crash returned it to the pool")
	}

	// The next failure must be attempt 2, not 1 again.
	f.failLaunch()
	if !f.hasAttemptRecorded(2) {
		t.Fatal("the next retry restarted the budget instead of continuing it")
	}
}

// 2. Repeating that exact crash window must not exceed the budget.
func TestRetryBudget_RepeatedCrashesCannotExceedTheBudget(t *testing.T) {
	f := newBudgetFixture(t)
	for i := 0; i < 8; i++ {
		f.crashBeforeFailureCheckpoint()
	}

	// maxReviewerLaunchAttempts is 3. However many times the crash recurs, the
	// cycle cannot consume more than that.
	for attempt := 4; attempt <= 9; attempt++ {
		if f.hasAttemptRecorded(attempt) {
			t.Fatalf("attempt %d was consumed; the budget is 3", attempt)
		}
	}
	if !f.hasAttemptRecorded(3) {
		t.Fatal("the budget was never actually reached")
	}
}

// 3. An unreadable retry ledger must not fabricate budget.
//
// The failure is armed at the moment the review row is inserted, so the read it
// meets is the one that decides how much budget is left — not some earlier read
// that would abort the dispatch before the question is ever asked.
func TestRetryBudget_UnreadableLedgerFailsClosed(t *testing.T) {
	f := newBudgetFixture(t)

	// Spend two attempts, so "count as zero" is visibly wrong.
	f.failLaunch()
	f.failLaunch()
	if !f.hasAttemptRecorded(2) {
		t.Fatalf("precondition: two attempts should be spent; checkpoints = %v", f.countPhase("review_launch_abandoned"))
	}
	onesBefore := f.countAttempt(1)

	// From the instant the third attempt's review row exists, the ledger is
	// unreadable.
	// Exactly ONE read fails: the retry-history read that decides how much budget
	// is left. Failing every read afterwards would let the abandon-marker guard
	// (which also fails closed) mask a fail-open bug here.
	f.runs.afterInsertReviewRun = func(string) {
		f.store.checkpointListErrOnce = errors.New("checkpoint ledger unavailable")
	}
	f.failLaunch()
	f.store.checkpointListErrOnce = nil

	// Nothing may be recorded as attempt 1 again: that is exactly what treating
	// unreadable history as "no retries consumed" produces.
	if got := f.countAttempt(1); got != onesBefore {
		t.Fatalf("attempt 1 records %d -> %d; unreadable history fabricated budget", onesBefore, got)
	}
}

// countAttempt counts durable records claiming a given attempt number.
func (f *budgetFixture) countAttempt(attempt int) int {
	f.t.Helper()
	needle := `"attempt":` + strconv.Itoa(attempt)
	n := 0
	for _, cp := range f.checkpoints() {
		switch cp.DurablePhase {
		case "review_launch_attempt", "review_launch_abandoned", "reviewer_launch_error":
		default:
			continue
		}
		if strings.Contains(cp.RetryState, needle) {
			n++
		}
	}
	return n
}

// 5. An abandon that is NOT a launch attempt must not consume budget.
func TestRetryBudget_NonAttemptAbandonsConsumeNoBudget(t *testing.T) {
	f := newBudgetFixture(t)

	// Drive one real failure so an attempt exists to compare against.
	f.failLaunch()
	if !f.hasAttemptRecorded(1) {
		t.Fatalf("precondition: the first launch failure was not attempt 1; phases = %v", f.checkpoints())
	}

	// Every non-launch abandon leaves attempt unset, so it cannot be counted.
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase != "review_launch_abandoned" {
			continue
		}
		if strings.Contains(cp.RetryState, `"attempt":0`) {
			t.Fatal("a non-attempt abandon recorded attempt 0 as a consumed attempt")
		}
	}
}

// 6. A duplicate reconcile must not double-consume one attempt.
func TestRetryBudget_DuplicateReconcileDoesNotDoubleConsume(t *testing.T) {
	f := newBudgetFixture(t)
	f.failLaunch()

	before := f.countPhase("review_launch_abandoned")
	for i := 0; i < 4; i++ {
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	if got := f.countPhase("review_launch_abandoned"); got != before {
		t.Fatalf("abandon records %d -> %d across repeated reconciles; one attempt was counted twice",
			before, got)
	}
}

// ---- attempts are consumed before any row exists -------------------------

// failRowCreation makes the next dispatch fail while creating the review ROW,
// so it dies before any review run id exists.
func (f *budgetFixture) failRowCreation() {
	f.t.Helper()
	f.runs.rowCreateErr = errors.New("review row unavailable")
	_, _ = f.c.ContinueRun(f.ctx, f.runID)
	f.runs.rowCreateErr = nil
}

// crashBeforeFailureCheckpointDuring runs one pass that dies just before the
// bookkeeping checkpoint, whatever made it fail.
func (f *budgetFixture) crashBeforeFailureCheckpointDuring(drive func()) {
	f.t.Helper()
	var arm func()
	arm = func() {
		f.store.beforeCheckpointInsert = func(cp domain.WorkflowCheckpoint) {
			if cp.DurablePhase != "reviewer_launch_error" {
				arm()
				return
			}
			panic("crash-before-failure-checkpoint")
		}
	}
	arm()
	defer func() {
		f.store.beforeCheckpointInsert = nil
		if r := recover(); r != nil && r != "crash-before-failure-checkpoint" {
			panic(r)
		}
	}()
	drive()
}

// 1. The attempt dies before a review run id exists, and must still be spent.
func TestRetryBudget_AttemptWithNoReviewRunIsStillConsumed(t *testing.T) {
	f := newBudgetFixture(t)
	f.crashBeforeFailureCheckpointDuring(f.failRowCreation)

	if got := f.countPhase("reviewer_launch_error"); got != 0 {
		t.Fatalf("precondition: %d failure checkpoints written, want 0", got)
	}
	if got := f.countPhase("review_launch_abandoned"); got != 0 {
		t.Fatalf("precondition: %d abandon records, want 0 — there was no review run to abandon", got)
	}
	if !f.hasAttemptRecorded(1) {
		t.Fatal("an attempt that failed before the review row existed was not durably consumed")
	}

	// The next failure is attempt 2, not 1 again.
	f.failLaunch()
	if !f.hasAttemptRecorded(2) {
		t.Fatal("the next retry restarted the budget instead of continuing it")
	}
}

// 2. Repeating that crash cannot exceed the budget.
func TestRetryBudget_RepeatedPreRowCrashesCannotExceedTheBudget(t *testing.T) {
	f := newBudgetFixture(t)
	for i := 0; i < 8; i++ {
		f.crashBeforeFailureCheckpointDuring(f.failRowCreation)
	}
	for attempt := 4; attempt <= 9; attempt++ {
		if f.hasAttemptRecorded(attempt) {
			t.Fatalf("attempt %d was consumed; the budget is %d", attempt, 3)
		}
	}
	if !f.hasAttemptRecorded(3) {
		t.Fatal("the budget was never actually reached")
	}
}

// 3. The cycle must be recovered from the durable record, not by parsing a key
// that need not carry it.
func TestRetryBudget_CycleIsRecoveredFromTheDurableRecordNotTheKey(t *testing.T) {
	f := newBudgetFixture(t)
	f.failLaunch()
	f.failLaunch()
	f.crashBeforeFailureCheckpointDuring(f.failLaunch)

	if !f.hasAttemptRecorded(3) {
		t.Fatalf("precondition: three attempts should be spent; records = %v", f.attemptRecords())
	}

	// Every attempt record must name its cycle explicitly, so recovery never has
	// to infer it.
	for _, rs := range f.attemptRecords() {
		if !strings.Contains(rs, `"cycle":`) {
			t.Fatalf("an attempt record carries no explicit cycle: %s", rs)
		}
		if !strings.Contains(rs, `"idempotencyKey":"`) {
			t.Fatalf("an attempt record carries no claim key: %s", rs)
		}
	}

	// And the budget stays closed across further passes.
	for i := 0; i < 4; i++ {
		_, _ = f.c.ContinueRun(f.ctx, f.runID)
		_ = f.c.Reconcile(f.ctx)
	}
	if f.hasAttemptRecorded(4) {
		t.Fatal("attempt 4 was consumed after a crash; the cycle was not recovered")
	}
}

// attemptRecords returns the raw payloads of every durable attempt record.
func (f *budgetFixture) attemptRecords() []string {
	f.t.Helper()
	var out []string
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase == "review_launch_attempt" {
			out = append(out, cp.RetryState)
		}
	}
	return out
}

// 4 + 5. Retry history that cannot be DECODED must fail closed. Skipping a
// corrupt record computes a smaller count than the truth — the same failure as
// not reading at all.
func TestRetryBudget_CorruptHistoryFailsClosed(t *testing.T) {
	for _, phase := range []string{"review_launch_attempt", "review_launch_abandoned", "reviewer_launch_error"} {
		t.Run(phase, func(t *testing.T) {
			f := newBudgetFixture(t)
			f.failLaunch()
			f.failLaunch()
			spentBefore := f.countAttempt(1) + f.countAttempt(2)

			// Corrupt one budget-relevant record.
			corrupted := false
			for runID, list := range f.store.checkpoints {
				for i := range list {
					if list[i].DurablePhase != phase {
						continue
					}
					list[i].RetryState = "{not json at all"
					corrupted = true
					break
				}
				f.store.checkpoints[runID] = list
			}
			if !corrupted {
				t.Skipf("no %s record to corrupt", phase)
			}

			attemptsBefore := f.countPhase("review_launch_attempt")
			f.failLaunch()

			// Undecodable history must authorise nothing: no new attempt may be
			// allocated from a count AO cannot stand behind.
			if got := f.countPhase("review_launch_attempt"); got != attemptsBefore {
				t.Fatalf("attempt allocations %d -> %d over corrupt history; a smaller count was computed",
					attemptsBefore, got)
			}
			_ = spentBefore
		})
	}
}

// 6. Duplicate reconciliation must not double-allocate one attempt.
func TestRetryBudget_DuplicateReconcileDoesNotDoubleAllocate(t *testing.T) {
	f := newBudgetFixture(t)
	f.failLaunch()

	before := f.countPhase("review_launch_attempt")
	for i := 0; i < 4; i++ {
		_, _ = f.c.ContinueRun(f.ctx, f.runID)
		if err := f.c.Reconcile(f.ctx); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	// One allocation per attempt, however many passes run over it.
	seen := map[string]int{}
	for _, rs := range f.attemptRecords() {
		seen[rs]++
	}
	for rs, n := range seen {
		if n > 1 {
			t.Fatalf("the same attempt was allocated %d times: %s", n, rs)
		}
	}
	if got := f.countPhase("review_launch_attempt"); got < before {
		t.Fatalf("attempt allocations went backwards: %d -> %d", before, got)
	}
}

// A REPLACEMENT claim carries no cycle in its key at all
// (reviewReplacementIdempotencyKey is "…-replacement:<step>:<replacedRun>"), so
// parsing one back out yields zero — a cycle that matches no history, making a
// fully spent budget read as untouched.
//
// The attempt records name their cycle explicitly, and that is what recovery
// must consult.
func TestRetryBudget_ReplacementClaimBudgetComesFromTheRecordNotTheKey(t *testing.T) {
	f := newBudgetFixture(t)
	steps, _ := f.store.ListWorkflowSteps(f.ctx, f.runID)
	var stepID string
	for _, st := range steps {
		if st.Kind == domain.WorkflowStepReview {
			stepID = st.ID
		}
	}

	// A replacement claim, dispatched and mid-recovery, whose key encodes no
	// cycle. Its budget is fully spent — recorded explicitly against cycle 2.
	key := "workflow-step-review-replacement:" + stepID + ":rr-replaced"
	e := f.store.outbox[key]
	e.ID, e.WorkflowRunID, e.WorkflowStepID = "wfo-replacement", f.runID, &stepID
	e.IdempotencyKey, e.CommandType = key, domain.WorkflowOutboxTriggerReview
	e.Status, e.Payload = domain.WorkflowOutboxDispatched, "{}"
	f.store.outbox[key] = e

	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-repl-claim", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-1", DurablePhase: "review_launch_claimed", PayloadVersion: "v1",
		RetryState: `{"idempotencyKey":"` + key + `"}`, CreatedAt: f.clk.Now(),
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
			ID: "wfc-repl-attempt-" + strconv.Itoa(attempt), WorkflowRunID: f.runID,
			WorkflowStepID: &stepID, ProjectID: "proj-1",
			DurablePhase:   "review_launch_attempt",
			PayloadVersion: "v1",
			RetryState: `{"idempotencyKey":"` + key + `","cycle":2,"epoch":1,"attempt":` +
				strconv.Itoa(attempt) + `}`,
			CreatedAt: f.clk.Now(),
		}); err != nil {
			t.Fatalf("seed attempt %d: %v", attempt, err)
		}
	}

	// Parsing the key gives cycle 0, which matches none of those records; the
	// records themselves say cycle 2 is exhausted.
	if got := workflowcore.ReviewCycleOfKeyForTest(key); got != 0 {
		t.Fatalf("precondition: the replacement key parses to cycle %d, want 0", got)
	}

	// The gate every claim-release path consults must refuse: cycle 2 is spent.
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	var reviewStep domain.WorkflowStep
	for _, st := range steps {
		if st.ID == stepID {
			reviewStep = st
		}
	}
	ok, why, err := f.c.ReviewLaunchBudgetRemainsForTest(f.ctx, run, reviewStep, f.store.outbox[key])
	if err != nil {
		t.Fatalf("budget gate: %v", err)
	}
	if ok {
		t.Fatal("a replacement claim with an exhausted budget was cleared for another launch; " +
			"its cycle was taken from the key (which has none) rather than from the durable record")
	}
	if !strings.Contains(why, "launch attempts") {
		t.Fatalf("refusal reason = %q, want it to name the spent budget", why)
	}

	// And a claim whose cycle genuinely has budget left is still cleared.
	spare := f.store.outbox[key]
	spare.IdempotencyKey = "workflow-step-review-replacement:" + stepID + ":rr-untouched"
	if ok, _, err := f.c.ReviewLaunchBudgetRemainsForTest(f.ctx, run, reviewStep, spare); err != nil || !ok {
		t.Fatalf("an untouched claim was refused (ok=%t err=%v)", ok, err)
	}
}

// ---- attempts are allocated at CLAIM time --------------------------------

// crashAtClaim runs one pass that dies the instant the claim is durable — before
// any row creation, launch, or failure recording.
func (f *budgetFixture) crashAtClaim() {
	f.t.Helper()
	var arm func()
	arm = func() {
		f.store.beforeCheckpointInsert = func(cp domain.WorkflowCheckpoint) {
			// Let the claim AND its attempt allocation land, then die.
			if cp.DurablePhase != "review_launch_intent" {
				arm()
				return
			}
			panic("crash-at-claim")
		}
	}
	arm()
	defer func() {
		f.store.beforeCheckpointInsert = nil
		if r := recover(); r != nil && r != "crash-at-claim" {
			panic(r)
		}
	}()
	_, _ = f.c.ContinueRun(f.ctx, f.runID)
}

// 1. A crash immediately after the claim must still consume the attempt.
func TestRetryBudget_AttemptIsAllocatedAtClaimTime(t *testing.T) {
	f := newBudgetFixture(t)
	f.crashAtClaim()

	if got := f.countPhase("reviewer_launch_error"); got != 0 {
		t.Fatalf("precondition: %d failure records written, want 0", got)
	}
	if got := f.countPhase("review_launch_attempt"); got != 1 {
		t.Fatalf("attempt allocations = %d, want 1 — the claim must consume an attempt", got)
	}
	if !f.hasAttemptRecorded(1) {
		t.Fatal("a crash right after the claim returned the attempt to the pool")
	}
}

// 2. Repeating that crash cannot exceed the budget.
func TestRetryBudget_RepeatedClaimTimeCrashesCannotExceedTheBudget(t *testing.T) {
	f := newBudgetFixture(t)
	for i := 0; i < 8; i++ {
		f.crashAtClaim()
		_ = f.c.Reconcile(f.ctx)
	}
	for attempt := 4; attempt <= 9; attempt++ {
		if f.hasAttemptRecorded(attempt) {
			t.Fatalf("attempt %d was consumed; the budget is 3", attempt)
		}
	}
}

// 4. Every dispatched -> pending transition for a review claim must go through
// the budget gate. Enforced structurally: there is one release function, and it
// applies the gate itself.
func TestRetryBudget_EveryClaimReleaseIsGated(t *testing.T) {
	f := newBudgetFixture(t)

	// Spend the whole budget.
	for i := 0; i < 3; i++ {
		f.failLaunch()
	}
	if !f.hasAttemptRecorded(3) {
		t.Fatalf("precondition: the budget should be spent; records = %v", f.attemptRecords())
	}

	launchesBefore := f.launcher.launchCalls
	// Every AUTOMATIC recovery entry point, repeatedly. None may hand out
	// another launch, whichever release path it would have taken.
	//
	// ContinueRun is deliberately excluded: on a stopped run it is the HUMAN
	// resume, which resets the budget on purpose (reviewer_launch_human_retry).
	// That is the one sanctioned way past the limit, and it is asserted below.
	for i := 0; i < 6; i++ {
		_ = f.c.Reconcile(f.ctx)
		_, _ = f.c.GetRun(f.ctx, f.runID)
	}
	if got := f.launcher.launchCalls - launchesBefore; got != 0 {
		t.Fatalf("%d reviewer launches after the budget was spent, want 0", got)
	}
	if f.hasAttemptRecorded(4) {
		t.Fatal("a fourth attempt was allocated by automatic recovery past the budget")
	}

	// A person continuing the run is the sanctioned reset, and it must still
	// work — a budget nothing can lift is a run nobody can rescue.
	if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("human resume: %v", err)
	}
	if f.launcher.launchCalls == launchesBefore {
		t.Fatal("a human resume did not restart the reviewer launch budget")
	}
}

// 5-8. Semantically empty or incomplete history must fail closed. `{}` parses
// perfectly and describes nothing; skipping it under-counts the budget.
func TestRetryBudget_SemanticallyInvalidHistoryFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		phase   string
		payload string
	}{
		{"empty object", "review_launch_attempt", `{}`},
		{"missing claim", "review_launch_attempt", `{"cycle":1,"attempt":1}`},
		{"attempt zero", "review_launch_attempt", `{"idempotencyKey":"k","cycle":1,"attempt":0}`},
		{"missing cycle", "review_launch_attempt", `{"idempotencyKey":"k","attempt":1}`},
		{"failure record empty", "reviewer_launch_error", `{}`},
		{"abandon names no claim", "review_launch_abandoned", `{"reviewRunId":"rr","attempt":1,"cycle":1}`},
		{"abandon half-stamped", "review_launch_abandoned", `{"idempotencyKey":"k","attempt":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBudgetFixture(t)
			f.failLaunch()

			steps, _ := f.store.ListWorkflowSteps(f.ctx, f.runID)
			var stepID string
			for _, st := range steps {
				if st.Kind == domain.WorkflowStepReview {
					stepID = st.ID
				}
			}
			if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
				ID: "wfc-bad-" + tc.phase, WorkflowRunID: f.runID, WorkflowStepID: &stepID,
				ProjectID: "proj-1", DurablePhase: tc.phase, PayloadVersion: "v1",
				RetryState: tc.payload, CreatedAt: f.clk.Now(),
			}); err != nil {
				t.Fatalf("seed bad record: %v", err)
			}
			// Counted AFTER seeding: the corrupt record lives in the same phase,
			// so measuring before it would score the seed itself as an
			// allocation and hide what production actually did.
			allocationsBefore := f.countPhase("review_launch_attempt")

			launches := f.launcher.launchCalls
			for i := 0; i < 3; i++ {
				_, _ = f.c.ContinueRun(f.ctx, f.runID)
				_ = f.c.Reconcile(f.ctx)
			}

			if got := f.countPhase("review_launch_attempt"); got != allocationsBefore {
				t.Fatalf("attempt allocations %d -> %d over semantically invalid history",
					allocationsBefore, got)
			}
			if got := f.launcher.launchCalls - launches; got != 0 {
				t.Fatalf("%d reviewer launches from history AO could not trust, want 0", got)
			}
		})
	}
}

// 9. A duplicate reconcile must not double-allocate the claim-time attempt.
func TestRetryBudget_DuplicateReconcileDoesNotDoubleAllocateAtClaimTime(t *testing.T) {
	f := newBudgetFixture(t)
	f.crashAtClaim()

	before := f.countPhase("review_launch_attempt")
	for i := 0; i < 5; i++ {
		_ = f.c.Reconcile(f.ctx)
	}
	seen := map[string]int{}
	for _, rs := range f.attemptRecords() {
		seen[rs]++
	}
	for rs, n := range seen {
		if n > 1 {
			t.Fatalf("the same attempt was allocated %d times: %s", n, rs)
		}
	}
	if got := f.countPhase("review_launch_attempt"); got < before {
		t.Fatalf("attempt allocations went backwards: %d -> %d", before, got)
	}
}

// The release gate has a distinct job from the claim-time gate: it keeps a claim
// that will never be retried out of the `pending` state.
//
// Claim-time allocation already stops a relaunch, so a bypassed release gate
// costs no extra reviewer — but it leaves the outbox advertising a retry that
// cannot happen, which is exactly the "will retry forever, never does" state
// this work exists to remove.
func TestRetryBudget_ExhaustedClaimIsNotAdvertisedAsPending(t *testing.T) {
	f := newBudgetFixture(t)
	steps, _ := f.store.ListWorkflowSteps(f.ctx, f.runID)
	var stepID string
	for _, st := range steps {
		if st.Kind == domain.WorkflowStepReview {
			stepID = st.ID
		}
	}
	key := "workflow-step-review:" + stepID + ":cycle1:codex"

	// A dispatched claim whose cycle has spent every attempt, with no review run
	// behind it — the shape that takes the "created no review run" release.
	e := f.store.outbox[key]
	e.ID, e.WorkflowRunID, e.WorkflowStepID = "wfo-exhausted", f.runID, &stepID
	e.IdempotencyKey, e.CommandType = key, domain.WorkflowOutboxTriggerReview
	e.Status, e.Payload = domain.WorkflowOutboxDispatched, "{}"
	f.store.outbox[key] = e

	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-exh-claim", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-1", DurablePhase: "review_launch_claimed", PayloadVersion: "v1",
		RetryState: `{"idempotencyKey":"` + key + `"}`, CreatedAt: f.clk.Now(),
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
			ID: "wfc-exh-" + strconv.Itoa(attempt), WorkflowRunID: f.runID,
			WorkflowStepID: &stepID, ProjectID: "proj-1",
			DurablePhase:   "review_launch_attempt",
			PayloadVersion: "v1",
			RetryState: `{"idempotencyKey":"` + key + `","cycle":1,"epoch":1,"attempt":` +
				strconv.Itoa(attempt) + `}`,
			CreatedAt: f.clk.Now(),
		}); err != nil {
			t.Fatalf("seed attempt %d: %v", attempt, err)
		}
	}

	// The gate must refuse the release outright.
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	var reviewStep domain.WorkflowStep
	for _, st := range steps {
		if st.ID == stepID {
			reviewStep = st
		}
	}
	if _, err := f.c.ReleaseReviewDispatchClaimForTest(f.ctx, run, reviewStep, f.store.outbox[key],
		"a recovery that would hand this claim back"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := f.store.outbox[key].Status; got == domain.WorkflowOutboxPending {
		t.Fatal("an exhausted claim was advertised as pending; nothing will ever retry it")
	}
}

// ---- the budget state machine has no gaps --------------------------------

// crashAfterClaimBeforeAllocation is the window Blocker 1 named: a durable claim
// with no attempt behind it. Allocation now runs FIRST, so producing that state
// means failing the claim write after the attempt landed — the inverse — and the
// attempt must still be spent.
func (f *budgetFixture) crashAfterClaim() {
	f.t.Helper()
	f.store.checkpointWriteErr = func(cp domain.WorkflowCheckpoint) error {
		if cp.DurablePhase != "review_launch_claimed" {
			return nil
		}
		f.store.checkpointWriteErr = nil
		return errors.New("crash writing the claim")
	}
	_, _ = f.c.ContinueRun(f.ctx, f.runID)
	f.store.checkpointWriteErr = nil
}

// 1. A crash around the claim boundary never yields a free retry.
func TestBudgetMachine_ClaimBoundaryNeverYieldsAFreeRetry(t *testing.T) {
	f := newBudgetFixture(t)
	f.crashAfterClaim()

	if got := f.countPhase("review_launch_attempt"); got != 1 {
		t.Fatalf("attempt allocations = %d, want 1 — the attempt precedes the claim", got)
	}
	if got := f.countPhase("review_launch_claimed"); got != 0 {
		t.Fatalf("claim records = %d, want 0 — the claim write was made to fail", got)
	}
	if !f.hasAttemptRecorded(1) {
		t.Fatal("the attempt was returned to the pool by a crash at the claim boundary")
	}
}

// 2. Repeating that crash cannot exceed the budget.
func TestBudgetMachine_RepeatedClaimBoundaryCrashesCannotExceedTheBudget(t *testing.T) {
	f := newBudgetFixture(t)
	for i := 0; i < 8; i++ {
		f.crashAfterClaim()
		_ = f.c.Reconcile(f.ctx)
	}
	for attempt := 4; attempt <= 9; attempt++ {
		if f.hasAttemptRecorded(attempt) {
			t.Fatalf("attempt %d was consumed; the budget is 3", attempt)
		}
	}
}

// 3. Attempt numbers are unique and monotonic across a cycle.
func TestBudgetMachine_AttemptNumbersAreUniqueAndMonotonic(t *testing.T) {
	f := newBudgetFixture(t)
	for i := 0; i < 3; i++ {
		f.failLaunch()
	}
	seen := map[int]int{}
	order := []int{}
	for _, rs := range f.attemptRecords() {
		var rec struct {
			Cycle   int `json:"cycle"`
			Attempt int `json:"attempt"`
		}
		if err := json.Unmarshal([]byte(rs), &rec); err != nil {
			t.Fatalf("attempt record is not decodable: %s", rs)
		}
		seen[rec.Attempt]++
		order = append(order, rec.Attempt)
	}
	for attempt, n := range seen {
		if n > 1 {
			t.Fatalf("attempt %d was allocated %d times; every dispatch must get a unique identity",
				attempt, n)
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("attempt numbers are not monotonic: %v", order)
		}
	}
	if len(order) != 3 {
		t.Fatalf("attempts = %v, want exactly 3", order)
	}
}

// 4. An attempt identity is never reused even when the recorded numbers have
// gaps — the case "count + 1" gets wrong.
func TestBudgetMachine_AttemptIsNeverReusedOverAGappedHistory(t *testing.T) {
	f := newBudgetFixture(t)
	steps, _ := f.store.ListWorkflowSteps(f.ctx, f.runID)
	var stepID string
	for _, st := range steps {
		if st.Kind == domain.WorkflowStepReview {
			stepID = st.ID
		}
	}
	key := "workflow-step-review:" + stepID + ":cycle1:codex"

	// A history with a GAP: attempt 2 recorded, attempt 1 never was. Counting
	// gives 1, so "count + 1" would hand out 2 — already spent.
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-gap", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-1", DurablePhase: "review_launch_attempt", PayloadVersion: "v1",
		RetryState: `{"idempotencyKey":"` + key + `","cycle":1,"epoch":1,"attempt":2}`,
		CreatedAt:  f.clk.Now(),
	}); err != nil {
		t.Fatalf("seed gapped history: %v", err)
	}

	f.failLaunch()

	if f.countAttempt(2) > 1 {
		t.Fatal("attempt 2 was allocated a second time over a gapped history")
	}
	if !f.hasAttemptRecorded(3) {
		t.Fatalf("the next attempt was not 3; records = %v", f.attemptRecords())
	}
}

// 5 + 6. Negative values are corruption, not absence.
//
// The record is seeded so that SKIPPING it changes the answer: two valid
// attempts plus a corrupt third. Treated as absent, the cycle looks like it has
// budget left; rejected, the gate refuses. A test where the corrupt record does
// not affect the count proves nothing.
func TestBudgetMachine_NegativeBudgetFieldsFailClosed(t *testing.T) {
	cases := []struct{ name, phase, payload string }{
		{"negative cycle", "review_launch_attempt", `{"idempotencyKey":"%s","cycle":-1,"attempt":3}`},
		{"negative attempt", "review_launch_attempt", `{"idempotencyKey":"%s","cycle":1,"attempt":-3}`},
		{"negative failure cycle", "reviewer_launch_error", `{"cycle":-1,"attempt":3}`},
		{"negative failure attempt", "reviewer_launch_error", `{"cycle":1,"attempt":-3}`},
		{"negative abandon attempt", "review_launch_abandoned", `{"idempotencyKey":"%s","cycle":1,"attempt":-3}`},
		{"negative abandon cycle", "review_launch_abandoned", `{"idempotencyKey":"%s","cycle":-1,"attempt":3}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBudgetFixture(t)
			steps, _ := f.store.ListWorkflowSteps(f.ctx, f.runID)
			var stepID string
			var reviewStep domain.WorkflowStep
			for _, st := range steps {
				if st.Kind == domain.WorkflowStepReview {
					stepID, reviewStep = st.ID, st
				}
			}
			key := "workflow-step-review:" + stepID + ":cycle1:codex"

			// Two attempts genuinely spent...
			for attempt := 1; attempt <= 2; attempt++ {
				if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
					ID: "wfc-ok-" + strconv.Itoa(attempt), WorkflowRunID: f.runID,
					WorkflowStepID: &stepID, ProjectID: "proj-1",
					DurablePhase:   "review_launch_attempt",
					PayloadVersion: "v1",
					RetryState: `{"idempotencyKey":"` + key + `","cycle":1,"epoch":1,"attempt":` +
						strconv.Itoa(attempt) + `}`,
					CreatedAt: f.clk.Now(),
				}); err != nil {
					t.Fatalf("seed attempt %d: %v", attempt, err)
				}
			}
			// ...and a third that is corrupt. Skipping it makes the cycle look
			// like it still has budget.
			payload := tc.payload
			if strings.Contains(payload, "%s") {
				payload = fmt.Sprintf(payload, key)
			}
			if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
				ID: "wfc-neg", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
				ProjectID: "proj-1", DurablePhase: tc.phase, PayloadVersion: "v1",
				RetryState: payload, CreatedAt: f.clk.Now(),
			}); err != nil {
				t.Fatalf("seed corrupt: %v", err)
			}

			entry := f.store.outbox[key]
			entry.ID, entry.IdempotencyKey = "wfo-neg", key
			run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)

			ok, _, err := f.c.ReviewLaunchBudgetRemainsForTest(f.ctx, run, reviewStep, entry)
			if err == nil && ok {
				t.Fatal("a corrupt attempt record was treated as absent; the cycle looked like it " +
					"had budget left and another launch would have been authorised")
			}
		})
	}
}

// 7 + 8. A human resume whose budget reset cannot be persisted must leave the
// entry failed, so the person can simply try again.
func TestBudgetMachine_HumanResumeLeavesEntryFailedWhenTheResetCannotPersist(t *testing.T) {
	f := newBudgetFixture(t)
	for i := 0; i < 3; i++ {
		f.failLaunch()
	}
	key := f.reviewOutboxKey()
	if got := f.store.outbox[key].Status; got != domain.WorkflowOutboxFailed {
		t.Fatalf("precondition: outbox = %q, want failed after the budget is spent", got)
	}

	// The reset write fails.
	f.store.checkpointWriteErr = func(cp domain.WorkflowCheckpoint) error {
		if cp.DurablePhase == "reviewer_launch_human_retry" {
			return errors.New("ledger unavailable")
		}
		return nil
	}
	_, _ = f.c.ContinueRun(f.ctx, f.runID)

	if got := f.store.outbox[key].Status; got != domain.WorkflowOutboxFailed {
		t.Fatalf("outbox = %q after a failed reset, want it left failed so the human can retry", got)
	}

	// 8. Retrying the resume once the ledger works must succeed.
	f.store.checkpointWriteErr = nil
	launches := f.launcher.launchCalls
	if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if f.launcher.launchCalls == launches {
		t.Fatal("the retried human resume did not restart the reviewer launch")
	}
}

// reviewOutboxKey is the review step's own dispatch claim key.
func (f *budgetFixture) reviewOutboxKey() string {
	f.t.Helper()
	steps, _ := f.store.ListWorkflowSteps(f.ctx, f.runID)
	for _, st := range steps {
		if st.Kind == domain.WorkflowStepReview {
			return "workflow-step-review:" + st.ID + ":cycle1:codex"
		}
	}
	return ""
}

// 9. A crash after the reset is persisted but before the outbox reopens must be
// finished by the next resume, not blocked by it.
func TestBudgetMachine_ResetPersistedThenCrashIsCompletedByTheNextResume(t *testing.T) {
	f := newBudgetFixture(t)
	for i := 0; i < 3; i++ {
		f.failLaunch()
	}
	key := f.reviewOutboxKey()

	// The reset lands; the reopen does not.
	f.store.beforeCheckpointInsert = func(cp domain.WorkflowCheckpoint) {
		if cp.DurablePhase != "reviewer_launch_human_retry" {
			return
		}
		f.store.outboxCASErr = errors.New("crash before reopen")
	}
	_, _ = f.c.ContinueRun(f.ctx, f.runID)
	f.store.beforeCheckpointInsert = nil
	f.store.outboxCASErr = nil

	if got := f.store.outbox[key].Status; got != domain.WorkflowOutboxFailed {
		t.Fatalf("precondition: outbox = %q, want still failed", got)
	}

	launches := f.launcher.launchCalls
	if _, err := f.c.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("resume after crash: %v", err)
	}
	if f.launcher.launchCalls == launches {
		t.Fatal("a resume whose reset was already persisted did not complete the reopen")
	}
}

// 10. Duplicate resumes are idempotent: one reset, not one per call.
func TestBudgetMachine_DuplicateResumeIsIdempotent(t *testing.T) {
	f := newBudgetFixture(t)
	for i := 0; i < 3; i++ {
		f.failLaunch()
	}
	for i := 0; i < 4; i++ {
		_, _ = f.c.ContinueRun(f.ctx, f.runID)
	}
	if got := f.countPhase("reviewer_launch_human_retry"); got > 1 {
		t.Fatalf("%d budget resets recorded for one claim, want 1", got)
	}
}

// ---- budget closure: gaps, epochs, and fail-closed resets ----------------

// seedAttempt writes one durable attempt record.
func (f *budgetFixture) seedAttempt(id, key string, cycle, epoch, attempt int) {
	f.t.Helper()
	steps, _ := f.store.ListWorkflowSteps(f.ctx, f.runID)
	var stepID string
	for _, st := range steps {
		if st.Kind == domain.WorkflowStepReview {
			stepID = st.ID
		}
	}
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: id, WorkflowRunID: f.runID, WorkflowStepID: &stepID, ProjectID: "proj-1",
		DurablePhase: "review_launch_attempt", PayloadVersion: "v1",
		RetryState: fmt.Sprintf(`{"idempotencyKey":%q,"cycle":%d,"epoch":%d,"attempt":%d}`,
			key, cycle, epoch, attempt),
		CreatedAt: f.clk.Now(),
	}); err != nil {
		f.t.Fatalf("seed attempt: %v", err)
	}
}

// seedReset places a durable human-resume reset on the ledger. `generation` is
// the failed outbox generation the reset was won against — the reset's
// uniqueness key — so a test states it explicitly rather than inheriting
// whatever the epoch happened to be.
func (f *budgetFixture) seedReset(id, key string, cycle, epoch int, generation string) {
	f.t.Helper()
	step := f.reviewStepValue()
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: id, WorkflowRunID: f.runID, WorkflowStepID: &step.ID, ProjectID: "proj-1",
		DurablePhase: "reviewer_launch_human_retry", PayloadVersion: "v1",
		HeadSHA: "review-launch-reset-gen-" + generation,
		RetryState: fmt.Sprintf(`{"idempotencyKey":%q,"cycle":%d,"epoch":%d,"failedGeneration":%q}`,
			key, cycle, epoch, generation),
		CreatedAt: f.clk.Now(),
	}); err != nil {
		f.t.Fatalf("seed reset: %v", err)
	}
}

func (f *budgetFixture) reviewStepValue() domain.WorkflowStep {
	f.t.Helper()
	steps, _ := f.store.ListWorkflowSteps(f.ctx, f.runID)
	for _, st := range steps {
		if st.Kind == domain.WorkflowStepReview {
			return st
		}
	}
	return domain.WorkflowStep{}
}

// 1 + 2. A GAPPED history whose highest attempt already reached the limit is
// exhausted, even though fewer than the limit of records survive. Counting
// records would report budget left and hand out attempt 4.
func TestBudgetClosure_GappedHistoryAtTheLimitIsExhausted(t *testing.T) {
	f := newBudgetFixture(t)
	step := f.reviewStepValue()
	key := "workflow-step-review:" + step.ID + ":cycle1:codex"

	// Only ONE record survives, and it says attempt 3. Counting gives 1.
	f.seedAttempt("wfc-gap-3", key, 1, 1, 3)

	entry := f.store.outbox[key]
	entry.ID, entry.IdempotencyKey = "wfo-gap", key
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)

	ok, reason, err := f.c.ReviewLaunchBudgetRemainsForTest(f.ctx, run, step, entry)
	if err != nil {
		t.Fatalf("budget gate: %v", err)
	}
	if ok {
		t.Fatal("a cycle whose highest attempt already reached the limit reported budget left; " +
			"counting surviving records instead of the highest number hands out attempt 4")
	}
	if !strings.Contains(reason, "launch attempts") {
		t.Fatalf("refusal reason = %q, want it to name the spent budget", reason)
	}

	// And no fourth attempt is ever allocated.
	launches := f.launcher.launchCalls
	for i := 0; i < 3; i++ {
		_ = f.c.Reconcile(f.ctx)
	}
	if f.hasAttemptRecorded(4) {
		t.Fatal("attempt 4 was allocated over a gapped, exhausted history")
	}
	if got := f.launcher.launchCalls - launches; got != 0 {
		t.Fatalf("%d launches over an exhausted budget, want 0", got)
	}
}

// 3. A reset-ledger read failure must leave the entry failed.
func TestBudgetClosure_ResetLedgerReadFailureLeavesEntryFailed(t *testing.T) {
	f := newBudgetFixture(t)
	for i := 0; i < 3; i++ {
		f.failLaunch()
	}
	key := f.reviewOutboxKey()
	if got := f.store.outbox[key].Status; got != domain.WorkflowOutboxFailed {
		t.Fatalf("precondition: outbox = %q, want failed", got)
	}

	// The failure is placed on the resume's OWN history read: its first read
	// (the launch record) succeeds, the second (the retry history) fails. A
	// blanket outage would be caught by the earlier read instead, which is
	// correct but proves nothing about this one.
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	step := f.reviewStepValue()
	entry := f.store.outbox[key]
	f.store.checkpointListCalls = 0
	f.store.checkpointListErrAfter = 1
	f.store.checkpointListErr = errors.New("checkpoint ledger unavailable")
	resumed := f.c.ResumeReviewLaunchAfterFailureForTest(f.ctx, run, step, entry)
	f.store.checkpointListErr = nil

	if resumed {
		t.Fatal("the resume reported success on a retry history it could not read")
	}
	if got := f.store.outbox[key].Status; got != domain.WorkflowOutboxFailed {
		t.Fatalf("outbox = %q after an unreadable reset ledger, want it left failed", got)
	}
	if got := f.countPhase("reviewer_launch_human_retry"); got != 0 {
		t.Fatalf("%d resets written on an unreadable ledger, want 0", got)
	}
}

// 4 + 8. Concurrent and repeated resumes open exactly one epoch.
func TestBudgetClosure_ConcurrentResumesOpenExactlyOneEpoch(t *testing.T) {
	f := newBudgetFixture(t)
	for i := 0; i < 3; i++ {
		f.failLaunch()
	}

	// A second resume runs inside the first, after it has read the history and
	// decided its epoch but before it writes — the window a duplicate falls in.
	f.store.beforeCheckpointInsert = func(cp domain.WorkflowCheckpoint) {
		if cp.DurablePhase != "reviewer_launch_human_retry" {
			return
		}
		_, _ = f.c.ContinueRun(f.ctx, f.runID)
	}
	_, _ = f.c.ContinueRun(f.ctx, f.runID)
	f.store.beforeCheckpointInsert = nil

	if got := f.countPhase("reviewer_launch_human_retry"); got != 1 {
		t.Fatalf("%d reset epochs opened by concurrent resumes, want exactly 1", got)
	}

	// Repeating afterwards stays idempotent.
	for i := 0; i < 3; i++ {
		_, _ = f.c.ContinueRun(f.ctx, f.runID)
	}
	if got := f.countPhase("reviewer_launch_human_retry"); got > 2 {
		t.Fatalf("%d reset epochs after repeated resumes; each resume opened its own", got)
	}
}

// 5. A delayed second reset must not erase attempts a NEWER epoch already spent.
func TestBudgetClosure_DelayedResetCannotEraseANewerEpochsAttempts(t *testing.T) {
	f := newBudgetFixture(t)
	step := f.reviewStepValue()
	key := "workflow-step-review:" + step.ID + ":cycle1:codex"

	// Epoch 1 spent, a reset opened epoch 2, and epoch 2 has spent everything.
	f.seedAttempt("wfc-e1", key, 1, 1, 1)
	f.seedReset("wfc-reset-2", key, 1, 2, key+"|wfc-launch-error-1")
	for attempt := 1; attempt <= 3; attempt++ {
		f.seedAttempt("wfc-e2-"+strconv.Itoa(attempt), key, 1, 2, attempt)
	}

	entry := f.store.outbox[key]
	entry.ID, entry.IdempotencyKey = "wfo-epoch", key
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)

	ok, _, err := f.c.ReviewLaunchBudgetRemainsForTest(f.ctx, run, step, entry)
	if err != nil {
		t.Fatalf("budget gate: %v", err)
	}
	if ok {
		t.Fatal("epoch 2's spent attempts were erased; the budget was handed back twice")
	}
}

// 5b. The mirror of the delayed-reset case, and the one that actually costs a
// person their resume: a dispatch that read the OLD epoch, then wrote its
// attempt record after the reset had already landed.
//
// The record is durably real and it is ordered after the reset, so a reader that
// only clears history at the reset and then counts everything following it will
// charge those attempts to the fresh epoch — and a human who just fixed the
// cause finds the budget they were handed already spent, by launches that
// belonged to the epoch their resume superseded. The epoch stamped on each
// attempt is what settles it, not where the record sits in the ledger.
func TestBudgetClosure_StaleEpochAttemptsDoNotSpendTheFreshBudget(t *testing.T) {
	f := newBudgetFixture(t)
	step := f.reviewStepValue()
	key := "workflow-step-review:" + step.ID + ":cycle1:codex"

	// A human resume opened epoch 2...
	f.seedReset("wfc-reset-2", key, 1, 2, key+"|wfc-launch-error-1")
	// ...and three in-flight dispatches from epoch 1 land their records after
	// it, carrying every attempt number the budget allows.
	for attempt := 1; attempt <= workflowcore.MaxReviewerLaunchAttemptsForTest; attempt++ {
		f.seedAttempt("wfc-stale-"+strconv.Itoa(attempt), key, 1, 1, attempt)
	}

	entry := f.store.outbox[key]
	entry.ID, entry.IdempotencyKey = "wfo-stale-epoch", key
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)

	ok, reason, err := f.c.ReviewLaunchBudgetRemainsForTest(f.ctx, run, step, entry)
	if err != nil {
		t.Fatalf("budget gate: %v", err)
	}
	if !ok {
		t.Fatalf("a superseded epoch's attempts spent the fresh epoch's budget (%s); "+
			"the human resume bought nothing", reason)
	}

	// And the attempt the fresh epoch allocates starts the new epoch over
	// rather than continuing the superseded one's numbering.
	attempt, aerr := f.c.AllocateReviewLaunchAttemptForTest(f.ctx, run, step, entry, 1)
	if aerr != nil {
		t.Fatalf("allocate in the fresh epoch: %v", aerr)
	}
	if attempt != 1 {
		t.Fatalf("fresh epoch allocated attempt %d, want 1", attempt)
	}
}

// ---- one failed generation opens at most one reset epoch ------------------
//
// The hole these close: the reset used to be keyed by the EPOCH NUMBER it was
// about to open. Two Continues on one failed entry therefore need not collide —
// the winner opens epoch 2, and a resume arriving afterwards reads epoch 2,
// computes epoch 3, and writes a key nobody holds. Only then does it discover it
// lost the stale failed->pending swap. Its reopen did nothing; its reset is
// durable and newer, so it hides every attempt epoch 2 spent, and the budget is
// handed back a second time by a resume that resumed nothing.

// latestLaunchErrorID is the id of the newest launch-failure checkpoint, which
// IS the identity of the current failed outbox generation.
func (f *budgetFixture) latestLaunchErrorID() string {
	f.t.Helper()
	id := ""
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase == "reviewer_launch_error" {
			id = cp.ID
		}
	}
	if id == "" {
		f.t.Fatal("no launch-failure record on the ledger")
	}
	return id
}

// resetGenerations lists the failed generation each durable reset was won
// against, which is the reset's uniqueness key.
func (f *budgetFixture) resetGenerations() []string {
	f.t.Helper()
	var out []string
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase != "reviewer_launch_human_retry" {
			continue
		}
		var rec struct {
			FailedGeneration string `json:"failedGeneration"`
		}
		if err := json.Unmarshal([]byte(cp.RetryState), &rec); err != nil {
			f.t.Fatalf("reset record %s is undecodable: %v", cp.ID, err)
		}
		out = append(out, rec.FailedGeneration)
	}
	return out
}

// failedEntry drives the launch to a durable failure and returns the snapshot a
// human resume would be holding — the exact failed generation.
func (f *budgetFixture) failedEntry() domain.WorkflowOutboxEntry {
	f.t.Helper()
	for i := 0; i < workflowcore.MaxReviewerLaunchAttemptsForTest; i++ {
		f.failLaunch()
	}
	entry := f.store.outbox[f.reviewOutboxKey()]
	if entry.Status != domain.WorkflowOutboxFailed {
		f.t.Fatalf("precondition: outbox = %q, want failed", entry.Status)
	}
	return entry
}

// 1. Two concurrent ContinueRun calls from the same failed generation.
func TestResumeOwnership_ConcurrentContinuesOpenOneEpochAndOneReopen(t *testing.T) {
	f := newBudgetFixture(t)
	_ = f.failedEntry()

	// A second Continue runs inside the first, in the window between the
	// winner deciding to claim and the claim taking effect.
	reopens := 0
	f.store.beforeCheckpointInsert = func(cp domain.WorkflowCheckpoint) {
		if cp.DurablePhase != "reviewer_launch_human_retry" {
			return
		}
		_, _ = f.c.ContinueRun(f.ctx, f.runID)
	}
	_, _ = f.c.ContinueRun(f.ctx, f.runID)
	f.store.beforeCheckpointInsert = nil

	gens := f.resetGenerations()
	if len(gens) != 1 {
		t.Fatalf("%d reset epochs opened by concurrent resumes (generations %v), want exactly 1", len(gens), gens)
	}
	if gens[0] == "" {
		t.Fatal("the surviving reset names no failed generation, so nothing proves it won one")
	}
	// Exactly one reopen: the entry left `failed` once.
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase == "reviewer_launch_human_retry" {
			reopens++
		}
	}
	if reopens != 1 {
		t.Fatalf("%d reopens of one failed generation, want 1", reopens)
	}
}

// 2. THE BLOCKER. The winner reopens and spends an attempt; the delayed loser —
// arriving with the same stale failed snapshot, reading a history that already
// contains epoch 2 — must not open epoch 3.
func TestResumeOwnership_DelayedDuplicateCannotOpenANewEpoch(t *testing.T) {
	f := newBudgetFixture(t)
	stale := f.failedEntry()
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	step := f.reviewStepValue()

	// The winner: claims the generation, opens epoch 2, reopens the entry.
	if !f.c.ResumeReviewLaunchAfterFailureForTest(f.ctx, run, step, stale) {
		t.Fatal("the first resume did not reopen the failed launch")
	}
	// ...and spends an attempt in the epoch it was given.
	entry := f.store.outbox[f.reviewOutboxKey()]
	spent, err := f.c.AllocateReviewLaunchAttemptForTest(f.ctx, run, step, entry, 1)
	if err != nil {
		t.Fatalf("the winner could not spend its fresh budget: %v", err)
	}
	if spent != 1 {
		t.Fatalf("winner allocated attempt %d in a fresh epoch, want 1", spent)
	}

	// The delayed loser, still holding the failed snapshot. It reads epoch 2 and
	// would compute epoch 3 — the case an epoch-keyed claim cannot refuse.
	if f.c.ResumeReviewLaunchAfterFailureForTest(f.ctx, run, step, stale) {
		t.Fatal("a delayed duplicate resume reported that it reopened the launch")
	}

	gens := f.resetGenerations()
	if len(gens) != 1 {
		t.Fatalf("%d reset epochs for one failed generation (%v), want exactly 1", len(gens), gens)
	}

	// And the winner's attempt is still charged: no epoch hid it.
	remaining, reason, berr := f.c.ReviewLaunchBudgetRemainsForTest(f.ctx, run, step, entry)
	if berr != nil {
		t.Fatalf("budget gate: %v", berr)
	}
	if !remaining {
		t.Fatalf("the winner's own fresh epoch reported no budget: %s", reason)
	}
	next, err := f.c.AllocateReviewLaunchAttemptForTest(f.ctx, run, step, entry, 1)
	if err != nil {
		t.Fatalf("allocate after the duplicate: %v", err)
	}
	if next != 2 {
		t.Fatalf("next attempt = %d, want 2 — the duplicate resume returned the winner's "+
			"spent attempt to the pool", next)
	}
}

// 3. Crash after winning the resume claim, before anything else could follow.
func TestResumeOwnership_CrashAfterTheClaimConvergesOnRestart(t *testing.T) {
	f := newBudgetFixture(t)
	stale := f.failedEntry()
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	step := f.reviewStepValue()

	// The claim lands and the process dies immediately after: the reset is
	// durable, the entry is still failed.
	f.store.checkpointWriteErr = func(cp domain.WorkflowCheckpoint) error { return nil }
	crashed := false
	f.store.beforeCheckpointInsert = func(cp domain.WorkflowCheckpoint) {
		if cp.DurablePhase == "reviewer_launch_human_retry" {
			crashed = true
		}
	}
	_ = f.c.ResumeReviewLaunchAfterFailureForTest(f.ctx, run, step, stale)
	f.store.beforeCheckpointInsert = nil
	f.store.checkpointWriteErr = nil
	if !crashed {
		t.Fatal("no reset claim was attempted")
	}

	// Restart: the resume runs again from the same failed generation. It must
	// see the claim already held, open NO second epoch, and converge.
	before := len(f.resetGenerations())
	for i := 0; i < 3; i++ {
		_, _ = f.c.ContinueRun(f.ctx, f.runID)
	}
	if got := len(f.resetGenerations()); got != before {
		t.Fatalf("%d reset epochs after restart, want the %d already claimed", got, before)
	}
}

// 4. Crash after the reset checkpoint but BEFORE the reopen. The next resume
// must finish the same reset rather than open a second epoch.
func TestResumeOwnership_CrashBeforeReopenIsCompletedWithoutASecondEpoch(t *testing.T) {
	f := newBudgetFixture(t)
	stale := f.failedEntry()
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	step := f.reviewStepValue()

	// The reset is written by hand — exactly the durable state a crash between
	// the claim and the swap leaves behind.
	rec := f.latestLaunchErrorID()
	generation := stale.IdempotencyKey + "|" + rec
	f.seedReset("wfc-crashed-reset", stale.IdempotencyKey, 1, 2, generation)
	if got := f.store.outbox[f.reviewOutboxKey()].Status; got != domain.WorkflowOutboxFailed {
		t.Fatalf("precondition: outbox = %q, want still failed", got)
	}

	// The next resume completes the reopen and opens nothing new.
	if !f.c.ResumeReviewLaunchAfterFailureForTest(f.ctx, run, step, stale) {
		t.Fatal("the resume did not finish the interrupted reopen")
	}
	if got := f.store.outbox[f.reviewOutboxKey()].Status; got != domain.WorkflowOutboxPending {
		t.Fatalf("outbox = %q after the completing resume, want pending", got)
	}
	gens := f.resetGenerations()
	if len(gens) != 1 || gens[0] != generation {
		t.Fatalf("reset generations = %v, want exactly [%s]", gens, generation)
	}
}

// 5. A duplicate resume after a restart is a no-op on the same epoch.
func TestResumeOwnership_DuplicateResumeAfterRestartIsANoOp(t *testing.T) {
	f := newBudgetFixture(t)
	stale := f.failedEntry()
	run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)
	step := f.reviewStepValue()

	if !f.c.ResumeReviewLaunchAfterFailureForTest(f.ctx, run, step, stale) {
		t.Fatal("the first resume did not reopen the failed launch")
	}
	first := f.resetGenerations()
	if len(first) != 1 {
		t.Fatalf("%d resets after the first resume, want 1", len(first))
	}

	// "Restart": repeated resumes, each re-reading the ledger from scratch.
	for i := 0; i < 4; i++ {
		if f.c.ResumeReviewLaunchAfterFailureForTest(f.ctx, run, step, stale) {
			t.Fatalf("duplicate resume %d reported a reopen", i)
		}
	}
	if got := f.resetGenerations(); len(got) != 1 || got[0] != first[0] {
		t.Fatalf("reset generations = %v after repeated resumes, want the original [%s]", got, first[0])
	}
}

// 6 + 7. A malformed or negative human-retry record must not reset the budget.
func TestBudgetClosure_CorruptResetRecordFailsClosed(t *testing.T) {
	cases := []struct{ name, payload string }{
		{"not json", `{not json at all`},
		{"empty object", `{}`},
		{"missing claim", `{"cycle":1,"epoch":2}`},
		{"negative cycle", `{"idempotencyKey":"k","cycle":-1,"epoch":2}`},
		{"negative epoch", `{"idempotencyKey":"k","cycle":1,"epoch":-2}`},
		{"zero epoch", `{"idempotencyKey":"k","cycle":1,"epoch":0}`},
		// A reset that cannot name the failed generation it was won against
		// cannot be shown to have won one, and a reset nobody won is exactly
		// the record that must never hand budget back.
		{"missing failed generation", `{"idempotencyKey":"k","cycle":1,"epoch":2}`},
		{"empty failed generation", `{"idempotencyKey":"k","cycle":1,"epoch":2,"failedGeneration":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBudgetFixture(t)
			step := f.reviewStepValue()
			key := "workflow-step-review:" + step.ID + ":cycle1:codex"

			// A fully spent cycle...
			for attempt := 1; attempt <= 3; attempt++ {
				f.seedAttempt("wfc-spent-"+strconv.Itoa(attempt), key, 1, 1, attempt)
			}
			// ...and a corrupt reset that would clear it.
			if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
				ID: "wfc-bad-reset", WorkflowRunID: f.runID, WorkflowStepID: &step.ID,
				ProjectID: "proj-1", DurablePhase: "reviewer_launch_human_retry",
				PayloadVersion: "v1", HeadSHA: "review-launch-reset-epoch-bad",
				RetryState: tc.payload, CreatedAt: f.clk.Now(),
			}); err != nil {
				t.Fatalf("seed corrupt reset: %v", err)
			}

			entry := f.store.outbox[key]
			entry.ID, entry.IdempotencyKey = "wfo-bad-reset", key
			run, _, _ := f.store.GetWorkflowRun(f.ctx, f.runID)

			ok, _, err := f.c.ReviewLaunchBudgetRemainsForTest(f.ctx, run, step, entry)
			if err == nil && ok {
				t.Fatal("a corrupt reset record handed the budget back; a malformed record is the " +
					"most dangerous one to honour, not the least")
			}
		})
	}
}
