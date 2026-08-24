package workflow

// verifyTargetAfterFix decides which fingerprint a verification is about, and
// getting it wrong does not fail loudly — it makes the run go quiet. The
// override hands maybeVerify a target key; if that key matches the spent
// attempt's, maybeVerify reads the question as already answered, opens no new
// attempt, and the run sits at waiting with an approved review and a
// verification that never runs.

import (
	stdctx "context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func newTargetFixture(t *testing.T) (*Coordinator, *sqlite.Store, stdctx.Context, string) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	run := domain.WorkflowRun{ID: "wf-target", ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, run, nil); err != nil {
		t.Fatal(err)
	}
	coord := New(Deps{Store: store, Projects: store, Clock: func() time.Time { return time.Now().UTC() }})
	return coord, store, ctx, run.ID
}

func seedPhase(t *testing.T, ctx stdctx.Context, store *sqlite.Store, runID, id, phase, fingerprint string, at time.Time) {
	t.Helper()
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: id, WorkflowRunID: runID, ProjectID: "p", FingerprintAfter: fingerprint,
		DurablePhase: phase, PayloadVersion: "v1", RetryState: "{}", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// The override's own case, unchanged: a verify-driven fix changed the worktree
// after the approval, so the fix's fingerprint is what must be verified.
func TestFixDeliveredAfterReentryBecomesTheVerifyTarget(t *testing.T) {
	coord, store, ctx, runID := newTargetFixture(t)
	base := time.Now().UTC()
	seedPhase(t, ctx, store, runID, "cp-reentry", ReasonVerifyFixReentry, "", base)
	seedPhase(t, ctx, store, runID, "cp-fix", "fix_observed_waiting", "fp-after-fix", base.Add(time.Minute))

	got, ok := coord.verifyTargetAfterFix(ctx, runID)
	if !ok || got != "fp-after-fix" {
		t.Fatalf("target = (%q, %v), want the fix's delivered fingerprint", got, ok)
	}
}

// The regression: a review dispatched AFTER the fix delivery outranks it. Its
// target is the newer statement of what has to be verified, and letting the
// fix's fingerprint win here is what made wf-04e8309d go silent — approved
// review, spent attempt, no new verification, forever.
func TestAReviewAfterTheFixOutranksTheFixTarget(t *testing.T) {
	coord, store, ctx, runID := newTargetFixture(t)
	base := time.Now().UTC()
	seedPhase(t, ctx, store, runID, "cp-reentry", ReasonVerifyFixReentry, "", base)
	seedPhase(t, ctx, store, runID, "cp-fix", "fix_observed_waiting", "fp-after-fix", base.Add(time.Minute))
	// AO asked a reviewer again — an integration fresh review, a stale-approval
	// recovery, an amended criterion. All three land here.
	seedPhase(t, ctx, store, runID, "cp-review", reviewDispatchedDurablePhase, "", base.Add(2*time.Minute))

	if got, ok := coord.verifyTargetAfterFix(ctx, runID); ok {
		t.Fatalf("target = %q, want the override to stand down for the newer review", got)
	}
}

// A review that predates the fix does not outrank it: that is precisely the
// approval the override exists to supersede.
func TestAReviewBeforeTheFixDoesNotOutrankIt(t *testing.T) {
	coord, store, ctx, runID := newTargetFixture(t)
	base := time.Now().UTC()
	seedPhase(t, ctx, store, runID, "cp-review", reviewDispatchedDurablePhase, "", base)
	seedPhase(t, ctx, store, runID, "cp-reentry", ReasonVerifyFixReentry, "", base.Add(time.Minute))
	seedPhase(t, ctx, store, runID, "cp-fix", "fix_observed_waiting", "fp-after-fix", base.Add(2*time.Minute))

	got, ok := coord.verifyTargetAfterFix(ctx, runID)
	if !ok || got != "fp-after-fix" {
		t.Fatalf("target = (%q, %v), want the fix's fingerprint to still win", got, ok)
	}
}

// A target change authorized by a review AO itself asked for is not ambiguity.
// Without this the verification refuses the very re-verification AO requested,
// with "verify target changed after an attempt was created".
func TestATargetAdvancedByAReviewIsAuthorized(t *testing.T) {
	coord, store, ctx, runID := newTargetFixture(t)
	started := time.Now().UTC()
	attempt := domain.WorkflowAttempt{StartedAt: started}

	if coord.verifyTargetAdvancedByReview(ctx, runID, attempt) {
		t.Fatal("no review dispatched yet, so nothing authorizes a target change")
	}
	seedPhase(t, ctx, store, runID, "cp-review", reviewDispatchedDurablePhase, "", started.Add(time.Minute))
	if !coord.verifyTargetAdvancedByReview(ctx, runID, attempt) {
		t.Fatal("a review dispatched after the attempt started must authorize the new target")
	}
}

// A review that predates the attempt authorizes nothing: that is the approval
// the attempt was already made against.
func TestATargetIsNotAuthorizedByAnOlderReview(t *testing.T) {
	coord, store, ctx, runID := newTargetFixture(t)
	started := time.Now().UTC()
	seedPhase(t, ctx, store, runID, "cp-review", reviewDispatchedDurablePhase, "", started.Add(-time.Minute))
	if coord.verifyTargetAdvancedByReview(ctx, runID, domain.WorkflowAttempt{StartedAt: started}) {
		t.Fatal("a review older than the attempt must not authorize a target change")
	}
}

// A verification that cannot execute records ONE attempt for the condition,
// not one per poll. maybeVerify runs on every GetRun, and a standing condition
// re-observed is the same condition: wf-04e8309d minted 35 attempt rows in
// three minutes, each one becoming the "latest" that every later guard then
// reasoned against.
func TestAnUnexecutableVerifyRecordsOneAttemptPerCondition(t *testing.T) {
	coord, store, ctx, runID := newTargetFixture(t)
	now := time.Now().UTC()
	step := domain.WorkflowStep{ID: "wfs-v", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify,
		Ordinal: 1, State: domain.WorkflowStepPending, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now}
	run := domain.WorkflowRun{ID: runID + "-x", ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now}
	created, steps, err := store.CreateWorkflowRun(ctx, run, []domain.WorkflowStep{step})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		if _, _, err := coord.failVerifyWithoutExecution(ctx, created, steps[0],
			domain.WorkflowErrorVerifyEnvironment, "verify worktree/session facts are missing"); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	attempts, err := store.ListWorkflowAttempts(ctx, steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt rows = %d after 5 identical passes, want exactly 1", len(attempts))
	}
}

// No verify-driven fix cycle at all: nothing to override.
func TestNoReentryMeansNoOverride(t *testing.T) {
	coord, store, ctx, runID := newTargetFixture(t)
	seedPhase(t, ctx, store, runID, "cp-fix", "fix_observed_waiting", "fp", time.Now().UTC())
	if got, ok := coord.verifyTargetAfterFix(ctx, runID); ok {
		t.Fatalf("target = %q, want no override without a verify-driven re-entry", got)
	}
}
