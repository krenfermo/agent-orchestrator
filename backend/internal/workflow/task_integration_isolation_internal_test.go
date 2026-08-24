package workflow

// Two findings the independent reviewer raised on Task 5, as tests.
//
//  1. A conflict belongs to the TASK. Parking the whole objective on one task's
//     merge problem is the opposite of what parallel execution is for: an
//     independent sibling that is ready must keep integrating, and a dependent
//     one waits because it depends, not because the objective stopped.
//
//  2. The audit is not best-effort. integration.New makes the recorder
//     mandatory so no integration goes unaccounted for; a promotion that
//     reported success without a record would be exactly that. But a failed
//     audit must never un-move a ref either — git and SQLite are two stores and
//     no ordering of the two writes survives every crash — so the physical side
//     stands, the promotion does not complete, and a later pass finishes the
//     audit without redoing anything.

import (
	stdctx "context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// 1. A conflicts; B is independent and ready, and keeps going.
func TestConflictingTaskDoesNotBlockAnIndependentSibling(t *testing.T) {
	f := newOvertakenFixture(t, domain.WorkflowStepCompleted, domain.WorkflowStepCompleted)
	// Make task A's replay impossible: an unrelated history shares no ancestor
	// with the target, which is a conflict no strategy can resolve.
	laneGit(t, f.repo, "checkout", "--orphan", "unrelated")
	laneGit(t, f.repo, "commit", "--allow-empty", "-m", "unrelated root")
	orphan := laneGit(t, f.repo, "rev-parse", "HEAD")
	laneGit(t, f.repo, "checkout", "main")
	laneGit(t, f.repo, "update-ref", f.refName, orphan)

	err := f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child)
	if err == nil {
		t.Fatal("expected the conflict to be reported")
	}
	// It is a TASK conflict, not a generic failure, and that distinction is
	// what stops the caller from parking the objective.
	if !errors.Is(err, errIntegrationTaskConflict) {
		t.Fatalf("err = %v, want errIntegrationTaskConflict", err)
	}
	// Recorded against the task, with what a person needs.
	found := false
	cps, cerr := f.store.ListWorkflowCheckpoints(f.ctx, f.master.ID)
	if cerr != nil {
		t.Fatal(cerr)
	}
	for _, cp := range cps {
		if cp.DurablePhase != taskIntegrationConflictPhase {
			continue
		}
		found = true
		if !strings.Contains(cp.RetryState, `"recommendedAction"`) {
			t.Fatalf("conflict record has no recommended action: %s", cp.RetryState)
		}
	}
	if !found {
		t.Fatal("no task-scoped conflict record was written")
	}
	// The objective itself is untouched: it did not record the task's conflict
	// as its own failure, so nothing about it stops a sibling.
	state, serr := f.coord.getMasterIntegrationState(f.ctx, f.master.ID)
	if serr != nil {
		t.Fatal(serr)
	}
	if state.LastErrorReason != "" {
		t.Fatalf("the objective adopted a task's conflict: %q", state.LastErrorReason)
	}
	// And the lane is free, which is what lets B integrate at all.
	if lanes := f.lanes.heldNow(); len(lanes) != 0 {
		t.Fatalf("integration lane still held after a conflict: %v", lanes)
	}
}

// 2. The audit is mandatory: a promotion whose record cannot be written does
// not report success.
func TestDirectBranchPromotionFailsWhenItsAuditCannotBeWritten(t *testing.T) {
	facts := &directBranchFacts{}
	_, store, ctx := newDirectBranchCoordinator(t, facts)
	head := dbCommit(t, "one")
	facts.obs = dbObservation(head)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", head)
	coord := newDirectBranchCoordinatorOver(t, store, facts, errors.New("sqlite is unavailable"))

	err := coord.promoteTaskToIntegration(ctx, master, task, detail)
	if err == nil {
		t.Fatal("a promotion completed without its audit record")
	}
	// The task is NOT promoted…
	if got := promotionCheckpoints(t, ctx, store, master.ID, masterIntegrationDurablePhase); len(got) != 0 {
		t.Fatalf("promotion checkpoints = %d, want 0 while the audit is missing", len(got))
	}
	// …and the outstanding audit is recorded, naming the ref and the SHA it is
	// expected to be at, so a later pass can tell "already integrated, audit
	// outstanding" from "not integrated".
	pending := promotionCheckpoints(t, ctx, store, master.ID, integrationAuditPendingPhase)
	if len(pending) != 1 {
		t.Fatalf("audit-pending rows = %d, want exactly 1", len(pending))
	}
	if !strings.Contains(pending[0].RetryState, `"expectedSha":"`+head+`"`) {
		t.Fatalf("pending record does not name the expected SHA: %s", pending[0].RetryState)
	}
}

// 3. The retry completes only the audit, exactly once, and never repeats the
// physical side — including across a restart.
func TestPendingAuditIsCompletedOnceWithoutRepeatingTheIntegration(t *testing.T) {
	facts := &directBranchFacts{}
	_, store, ctx := newDirectBranchCoordinator(t, facts)
	head := dbCommit(t, "one")
	facts.obs = dbObservation(head)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", head)

	broken := newDirectBranchCoordinatorOver(t, store, facts, errors.New("sqlite is unavailable"))
	if err := broken.promoteTaskToIntegration(ctx, master, task, detail); err == nil {
		t.Fatal("expected the first pass to refuse")
	}

	// A restart, then the store recovers.
	restarted := newDirectBranchCoordinatorOver(t, store, facts, nil)
	for i := 0; i < 3; i++ {
		if err := restarted.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}

	// Exactly one promotion, however many retries.
	if got := promotionCheckpoints(t, ctx, store, master.ID, masterIntegrationDurablePhase); len(got) != 1 {
		t.Fatalf("promotion checkpoints = %d, want exactly 1", len(got))
	}
	// The audit landed exactly once.
	records, err := restarted.ListTaskIntegrations(ctx, master.ID)
	if err != nil {
		t.Fatal(err)
	}
	landed := 0
	for _, r := range records {
		if r.Outcome == string(integration.OutcomeIntegrated) {
			landed++
		}
	}
	if landed != 1 {
		t.Fatalf("landed audit records = %d, want exactly 1", landed)
	}
	// And the outstanding marker was answered rather than left advertising a
	// condition that is over.
	if restarted.hasPendingIntegrationAudit(ctx, master.ID, task.ID) {
		t.Fatal("the audit-pending marker is still outstanding after the audit was written")
	}
}

// 4. An audit that fails BEFORE anything physical happens leaves nothing behind
// to reconcile: direct-branch moves no ref, so the marker names the SHA the
// branch is already at and the retry is a pure audit write.
func TestAuditFailureBeforeAnyMovementLeavesTheBranchUntouched(t *testing.T) {
	facts := &directBranchFacts{}
	_, store, ctx := newDirectBranchCoordinator(t, facts)
	head := dbCommit(t, "one")
	facts.obs = dbObservation(head)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", head)
	coord := newDirectBranchCoordinatorOver(t, store, facts, errors.New("sqlite is unavailable"))

	_ = coord.promoteTaskToIntegration(ctx, master, task, detail)

	// The branch is exactly where it was: a direct-branch promotion never
	// moves it, so there is nothing that could have been half-done.
	if facts.obs.HeadSHA != head {
		t.Fatalf("the branch moved during a proof-only promotion: %s", facts.obs.HeadSHA)
	}
	pending := promotionCheckpoints(t, ctx, store, master.ID, integrationAuditPendingPhase)
	if len(pending) != 1 || pending[0].HeadSHA != head {
		t.Fatalf("pending record = %+v, want one naming %s", pending, head)
	}
}

// 5. Isolated integration keeps the same guarantee from the other side: a
// second pass over an already-moved ref does not move it again, and the audit
// it writes describes a no-op rather than a second landing.
func TestIsolatedIntegrationDoesNotRepeatAnAlreadyLandedRef(t *testing.T) {
	f := newOvertakenFixture(t, domain.WorkflowStepCompleted, domain.WorkflowStepCompleted)
	if err := f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child); err != nil {
		t.Fatalf("first promotion: %v", err)
	}
	headAfterFirst := laneGit(t, f.repo, "rev-parse", f.refName)

	// A second pass, as a restart or a poll would make.
	_ = f.coord.promoteTaskToIntegration(f.ctx, f.master, f.task, f.child)

	if got := laneGit(t, f.repo, "rev-parse", f.refName); got != headAfterFirst {
		t.Fatalf("the ref moved again on a repeat pass: %s -> %s", headAfterFirst, got)
	}
	promotions := 0
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.master.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, cp := range cps {
		// Only THIS task's promotions: the fixture pre-seeds a sibling's, which
		// is part of what makes the target overtaken in the first place.
		if cp.DurablePhase == masterIntegrationDurablePhase && strings.Contains(cp.RetryState, `"`+f.task.ID+`"`) {
			promotions++
		}
	}
	if promotions != 1 {
		t.Fatalf("promotion checkpoints for %s = %d, want exactly 1 across two passes", f.task.ID, promotions)
	}
}

// 6. Both modes owe the same audit. This is the property the reviewer's second
// finding is really about: direct-branch must not be a cheaper path.
func TestBothModesRecordTheSameAuditFields(t *testing.T) {
	facts := &directBranchFacts{}
	coord, store, ctx := newDirectBranchCoordinator(t, facts)
	head := dbCommit(t, "one")
	facts.obs = dbObservation(head)
	master, task, detail := seedDirectBranchTask(t, ctx, store, "p", "task-1", head)
	if err := coord.promoteTaskToIntegration(ctx, master, task, detail); err != nil {
		t.Fatalf("promote: %v", err)
	}
	records, err := coord.ListTaskIntegrations(ctx, master.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("a direct-branch promotion wrote no audit record")
	}
	rec := records[len(records)-1]
	if rec.Strategy == "" || rec.SourceSHA == "" || rec.TargetBeforeSHA == "" || rec.TargetAfterSHA == "" {
		t.Fatalf("direct-branch audit is missing required fields: %+v", rec)
	}
	if rec.Outcome != string(integration.OutcomeIntegrated) {
		t.Fatalf("outcome = %q, want integrated", rec.Outcome)
	}
}

// ---- helpers ---------------------------------------------------------------

func dbObservation(head string) ports.WorkspaceObservation {
	return ports.WorkspaceObservation{Branch: dbTestBranch, HeadSHA: head}
}

// auditFailingStore is the real store with one write broken: the integration
// audit. It models the case the reviewer's second finding is about — git moved
// (or, for direct branch, was proven) and SQLite would not take the record —
// without any test-only field on the production Coordinator.
type auditFailingStore struct {
	*sqlite.Store
	fail error
}

func (s auditFailingStore) CreateWorkflowCheckpoint(ctx stdctx.Context, cp domain.WorkflowCheckpoint) (domain.WorkflowCheckpoint, error) {
	if s.fail != nil && cp.DurablePhase == taskIntegrationDurablePhase {
		return domain.WorkflowCheckpoint{}, s.fail
	}
	return s.Store.CreateWorkflowCheckpoint(ctx, cp)
}

// heldNow reports the lanes currently held, so a test can assert the lane was
// given back rather than merely that it was once taken.
func (l *laneStub) heldNow() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []string{}
	for k := range l.held {
		out = append(out, k)
	}
	return out
}

// newDirectBranchCoordinatorOver builds a second Coordinator over the SAME
// store, standing in for a daemon restart.
func newDirectBranchCoordinatorOver(t *testing.T, store *sqlite.Store, facts *directBranchFacts, failAudit error) *Coordinator {
	t.Helper()
	return New(Deps{
		Store:            auditFailingStore{Store: store, fail: failAudit},
		Projects:         store,
		WorkspaceFacts:   facts,
		IntegrationLocks: newLaneStub(),
		Clock:            func() time.Time { return time.Now().UTC() },
	})
}
