package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// workflow_outbox_generation_test.go — the failed->pending transition is
// conditioned on the EXACT failed generation, in SQL.
//
// The hole: the reopen used to be `WHERE id = ? AND status = 'failed'`. One
// outbox row is reused across retries, so that predicate is satisfied by ANY
// failure of it. A human resume that observed failure F1 and was delayed — while
// somebody else resumed F1, redispatched, and failed again as F2 — still matched
// it, and reopened F2: a launch and a fresh budget epoch from a decision nobody
// made about F2. No amount of re-reading in Go closes that, because a read
// followed by a write is exactly the window.

func seedFailableOutbox(t *testing.T, s interface {
	CreateWorkflowRun(context.Context, domain.WorkflowRun, []domain.WorkflowStep) (domain.WorkflowRun, []domain.WorkflowStep, error)
	EnqueueWorkflowOutboxEntry(context.Context, domain.WorkflowOutboxEntry) (domain.WorkflowOutboxEntry, bool, error)
}, runID string, now time.Time) domain.WorkflowOutboxEntry {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-gen", runID, now), sampleWorkflowSteps(runID, now)); err != nil {
		t.Fatalf("create run: %v", err)
	}
	entry, created, err := s.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID: "wfo-gen-1", WorkflowRunID: runID, IdempotencyKey: "review-" + runID,
		CommandType: domain.WorkflowOutboxTriggerReview, Payload: "{}", CreatedAt: now,
	})
	if err != nil || !created {
		t.Fatalf("enqueue: created=%v err=%v", created, err)
	}
	return entry
}

func outboxByID(t *testing.T, s interface {
	ListWorkflowOutboxByRun(context.Context, string) ([]domain.WorkflowOutboxEntry, error)
}, runID, id string) domain.WorkflowOutboxEntry {
	t.Helper()
	list, err := s.ListWorkflowOutboxByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	for _, e := range list {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("outbox %s not found", id)
	return domain.WorkflowOutboxEntry{}
}

// 1 + 5. A reopen naming generation F1 must not match a row that is now failed
// as F2 — nor one failed by any other generation.
func TestOutboxGenerationCAS_StaleGenerationCannotReopenALaterFailure(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-gen")
	now := time.Now().UTC().Truncate(time.Second)
	entry := seedFailableOutbox(t, s, "wf-gen-1", now)

	// D1 claims the row, then F1 fails it and stamps itself onto it.
	if ok, err := s.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "wfc-authz-1"); err != nil || !ok {
		t.Fatalf("claim G1: ok=%v err=%v", ok, err)
	}
	f1 := entry.IdempotencyKey + "|wfc-launch-error-1"
	ok, err := s.FailWorkflowOutboxWithGeneration(ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "transient", f1, "wfc-authz-1")
	if err != nil || !ok {
		t.Fatalf("fail with generation F1: ok=%v err=%v", ok, err)
	}
	if got := outboxByID(t, s, "wf-gen-1", entry.ID); got.FailureGeneration != f1 {
		t.Fatalf("row generation = %q, want %q", got.FailureGeneration, f1)
	}

	// Somebody resumes F1 and the launch fails again: F2 now owns the row.
	if ok, err := s.ReopenFailedWorkflowOutboxGeneration(ctx, entry.ID, "transient", f1); err != nil || !ok {
		t.Fatalf("reopen F1: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "wfc-authz-2"); err != nil || !ok {
		t.Fatalf("claim G2: ok=%v err=%v", ok, err)
	}
	f2 := entry.IdempotencyKey + "|wfc-launch-error-2"
	if ok, err := s.FailWorkflowOutboxWithGeneration(ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "transient", f2, "wfc-authz-2"); err != nil || !ok {
		t.Fatalf("fail with generation F2: ok=%v err=%v", ok, err)
	}

	// THE BLOCKER: the delayed F1 resume executes its swap. id + status still
	// match; the generation does not.
	reopened, err := s.ReopenFailedWorkflowOutboxGeneration(ctx, entry.ID, "transient", f1)
	if err != nil {
		t.Fatalf("stale reopen: %v", err)
	}
	if reopened {
		t.Fatal("a reopen naming F1 matched a row that is failed as F2")
	}
	got := outboxByID(t, s, "wf-gen-1", entry.ID)
	if got.Status != domain.WorkflowOutboxFailed {
		t.Fatalf("outbox = %q after the stale reopen, want it left failed", got.Status)
	}
	if got.FailureGeneration != f2 {
		t.Fatalf("row generation = %q after the stale reopen, want %q", got.FailureGeneration, f2)
	}

	// A generation from an unrelated failure record cannot satisfy it either.
	if reopened, err := s.ReopenFailedWorkflowOutboxGeneration(
		ctx, entry.ID, "transient", entry.IdempotencyKey+"|wfc-some-other-error"); err != nil || reopened {
		t.Fatalf("a foreign generation reopened the row: ok=%v err=%v", reopened, err)
	}
}

// 2 + 3. The matching generation reopens exactly once; repeating is a no-op.
func TestOutboxGenerationCAS_MatchingGenerationReopensExactlyOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-gen")
	now := time.Now().UTC().Truncate(time.Second)
	entry := seedFailableOutbox(t, s, "wf-gen-2", now)

	if ok, err := s.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "wfc-authz-1"); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	f1 := entry.IdempotencyKey + "|wfc-launch-error-1"
	if ok, err := s.FailWorkflowOutboxWithGeneration(ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "transient", f1, "wfc-authz-1"); err != nil || !ok {
		t.Fatalf("fail with generation: ok=%v err=%v", ok, err)
	}

	reopened, err := s.ReopenFailedWorkflowOutboxGeneration(ctx, entry.ID, "transient", f1)
	if err != nil || !reopened {
		t.Fatalf("the matching generation did not reopen the row: ok=%v err=%v", reopened, err)
	}
	got := outboxByID(t, s, "wf-gen-2", entry.ID)
	switch {
	case got.Status != domain.WorkflowOutboxPending:
		t.Fatalf("outbox = %q, want pending", got.Status)
	case got.FailedAt != nil:
		t.Fatal("the reopened row still carries a failure timestamp")
	case got.FailureGeneration != "":
		t.Fatalf("the reopened row still carries generation %q; it is no longer failed", got.FailureGeneration)
	}

	// Duplicates change nothing.
	for i := 0; i < 3; i++ {
		if reopened, err := s.ReopenFailedWorkflowOutboxGeneration(ctx, entry.ID, "transient", f1); err != nil || reopened {
			t.Fatalf("duplicate reopen %d: ok=%v err=%v", i, reopened, err)
		}
	}
}

// The ordinary status CAS clears the stamp, so no state can inherit a
// generation that does not describe it.
func TestOutboxGenerationCAS_OrdinaryTransitionsClearTheStamp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-gen")
	now := time.Now().UTC().Truncate(time.Second)
	entry := seedFailableOutbox(t, s, "wf-gen-3", now)

	if ok, err := s.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "wfc-authz-1"); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	f1 := entry.IdempotencyKey + "|wfc-launch-error-1"
	if ok, err := s.FailWorkflowOutboxWithGeneration(ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "transient", f1, "wfc-authz-1"); err != nil || !ok {
		t.Fatalf("fail with generation: ok=%v err=%v", ok, err)
	}
	if ok, err := s.UpdateWorkflowOutboxStatus(
		ctx, entry.ID, domain.WorkflowOutboxFailed, domain.WorkflowOutboxPending, now, ""); err != nil || !ok {
		t.Fatalf("ordinary transition: ok=%v err=%v", ok, err)
	}
	if got := outboxByID(t, s, "wf-gen-3", entry.ID); got.FailureGeneration != "" {
		t.Fatalf("generation %q survived an ordinary transition", got.FailureGeneration)
	}
}

// ---- the claim token: who owns a dispatched row -------------------------
//
// The hole one step earlier than the reopen: `dispatched` is not an owner. A
// dispatch that pauses after writing its launch-failure record can wake to find
// the row released by recovery and reclaimed by a second dispatch — still
// `dispatched`, to somebody else. A fail predicate of id + status matched that,
// failing a live generation and stamping F1 onto it, which a human resume of F1
// could then reopen into yet another dispatch.

// 1 + 4. A stale dispatch cannot fail a row that has been reclaimed.
func TestOutboxDispatchCAS_StaleDispatchCannotFailAReclaimedRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-gen")
	now := time.Now().UTC().Truncate(time.Second)
	entry := seedFailableOutbox(t, s, "wf-gen-4", now)

	// D1 claims the row.
	if ok, err := s.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "wfc-authz-d1"); err != nil || !ok {
		t.Fatalf("D1 claim: ok=%v err=%v", ok, err)
	}

	// D1 pauses. Recovery releases the claim, and D2 takes it.
	if ok, err := s.ReleaseDispatchedWorkflowOutboxGeneration(ctx, entry.ID, "", "wfc-authz-d1"); err != nil || !ok {
		t.Fatalf("recovery release: ok=%v err=%v", ok, err)
	}
	if ok, err := s.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "wfc-authz-d2"); err != nil || !ok {
		t.Fatalf("D2 claim: ok=%v err=%v", ok, err)
	}

	// THE BLOCKER: stale D1 fails the row with its own generation F1. The row
	// IS dispatched — to D2.
	f1 := entry.IdempotencyKey + "|wfc-launch-error-1"
	failed, err := s.FailWorkflowOutboxWithGeneration(
		ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "transient", f1, "wfc-authz-d1")
	if err != nil {
		t.Fatalf("stale fail: %v", err)
	}
	if failed {
		t.Fatal("a stale dispatch failed a row that had been reclaimed by another dispatch")
	}
	got := outboxByID(t, s, "wf-gen-4", entry.ID)
	switch {
	case got.Status != domain.WorkflowOutboxDispatched:
		t.Fatalf("outbox = %q after the stale fail, want D2 left dispatched", got.Status)
	case got.DispatchGeneration != "wfc-authz-d2":
		t.Fatalf("claim token = %q, want D2 still holding it", got.DispatchGeneration)
	case got.FailureGeneration != "":
		t.Fatalf("F1 was stamped onto D2: failure generation = %q", got.FailureGeneration)
	}

	// A stale RELEASE is refused for the same reason.
	if released, err := s.ReleaseDispatchedWorkflowOutboxGeneration(ctx, entry.ID, "", "wfc-authz-d1"); err != nil || released {
		t.Fatalf("a stale release gave away D2's claim: ok=%v err=%v", released, err)
	}
}

// 2 + 3. The owning dispatch fails the row exactly once; repeating is a no-op.
func TestOutboxDispatchCAS_OwningDispatchFailsExactlyOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-gen")
	now := time.Now().UTC().Truncate(time.Second)
	entry := seedFailableOutbox(t, s, "wf-gen-5", now)

	if ok, err := s.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "wfc-authz-d1"); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	f1 := entry.IdempotencyKey + "|wfc-launch-error-1"
	failed, err := s.FailWorkflowOutboxWithGeneration(
		ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "transient", f1, "wfc-authz-d1")
	if err != nil || !failed {
		t.Fatalf("the owning dispatch could not fail its own row: ok=%v err=%v", failed, err)
	}
	got := outboxByID(t, s, "wf-gen-5", entry.ID)
	switch {
	case got.Status != domain.WorkflowOutboxFailed:
		t.Fatalf("outbox = %q, want failed", got.Status)
	case got.FailureGeneration != f1:
		t.Fatalf("failure generation = %q, want %q", got.FailureGeneration, f1)
	case got.DispatchGeneration != "":
		t.Fatalf("the failed row still names a claim holder: %q", got.DispatchGeneration)
	}

	for i := 0; i < 3; i++ {
		if failed, err := s.FailWorkflowOutboxWithGeneration(
			ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "transient", f1, "wfc-authz-d1"); err != nil || failed {
			t.Fatalf("duplicate fail %d: ok=%v err=%v", i, failed, err)
		}
	}
}
