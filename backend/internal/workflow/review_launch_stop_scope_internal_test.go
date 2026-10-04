package workflow

// P0-C SECTION A, the residual: the ONE reviewer launch/recovery mutation that
// is not keyed by a reviewer generation.
//
// Every other mutation in review_launch_recovery.go names the exact generation
// it belongs to, in SQL:
//
//	claim       ClaimWorkflowOutboxDispatch                 stamps dispatch_generation
//	fail        FailWorkflowOutboxWithGeneration            CAS on dispatch_generation
//	release     ReleaseDispatchedWorkflowOutboxGeneration   CAS on dispatch_generation
//	reopen      ReopenFailedWorkflowOutboxGeneration        CAS on failure_generation
//	reset epoch checkpoint keyed head_sha = the generation  UNIQUE (migration 0136)
//	review_run  UpdateReviewRunResult                       CAS on status='running'
//
// clearReviewLaunchStop is the exception. It un-parks a RUN, and a run has no
// reviewer generation to CAS against — it is called from the one site that has
// just PROVEN a reviewer is attached (recordReviewDispatchSuccess), and it
// decides what to clear from the ledger's newest canonical stop reason.
//
// That makes the invariant it has to satisfy a scoping one rather than a
// generational one, and it is the last clause of section A's list: a reviewer
// launch succeeding must never UNBLOCK A NEWER LIFECYCLE GENERATION. Concretely:
// it may clear a stop this file wrote and that is still the operative one, and
// it must leave alone
//
//   - any stop written by another mechanism, and
//   - a reviewer-launch stop that a NEWER, unrelated stop has since superseded.
//
// These tests are that proof.

import (
	stdctx "context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// stopScopeFixture is a run parked in needs_attention with a ledger the test
// writes by hand, so the exact ordering of stops is stated rather than driven.
type stopScopeFixture struct {
	t     *testing.T
	ctx   stdctx.Context
	c     *Coordinator
	store *sqlite.Store
	run   domain.WorkflowRun
	step  domain.WorkflowStep
	base  time.Time
	n     int
}

func newStopScopeFixture(t *testing.T) *stopScopeFixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: base}); err != nil {
		t.Fatal(err)
	}
	run := domain.WorkflowRun{
		ID: "wf-stopscope", ProjectID: "p", Objective: "prove the stop scope",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: base, UpdatedAt: base,
	}
	seed := []domain.WorkflowStep{
		{ID: "wfs-work", WorkflowRunID: run.ID, Kind: domain.WorkflowStepWork, Ordinal: 1,
			State: domain.WorkflowStepCompleted, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
		{ID: "wfs-review", WorkflowRunID: run.ID, Kind: domain.WorkflowStepReview, Ordinal: 2,
			State: domain.WorkflowStepWaiting, ArtifactJSON: "{}", CreatedAt: base, UpdatedAt: base},
	}
	if _, _, err := store.CreateWorkflowRun(ctx, run, seed); err != nil {
		t.Fatal(err)
	}
	// Park it, exactly as a reviewer-launch failure does.
	if _, err := store.UpdateWorkflowRunState(ctx, run.ID,
		domain.WorkflowRunRunning, domain.WorkflowRunNeedsAttention, base); err != nil {
		t.Fatal(err)
	}
	run.State = domain.WorkflowRunNeedsAttention
	f := &stopScopeFixture{
		t: t, ctx: ctx, store: store, run: run, step: seed[1], base: base,
		c: New(Deps{Store: store, Projects: store, Clock: func() time.Time { return base.Add(time.Hour) }}),
	}
	return f
}

// stop appends a canonical attention reason to the ledger, newest last.
func (f *stopScopeFixture) stop(reason string) {
	f.t.Helper()
	f.n++
	step := f.step.ID
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID:             "cp-" + reason + "-" + string(rune('a'+f.n)),
		WorkflowRunID:  f.run.ID,
		WorkflowStepID: &step,
		ProjectID:      "p",
		NextAction:     reason,
		DurablePhase:   reason,
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      f.base.Add(time.Duration(f.n) * time.Minute),
	}); err != nil {
		f.t.Fatalf("seed stop %s: %v", reason, err)
	}
}

// clear runs the production call and reports the run state it left behind, read
// back from the database rather than from the returned value.
func (f *stopScopeFixture) clear() domain.WorkflowRunState {
	f.t.Helper()
	f.c.clearReviewLaunchStop(f.ctx, f.run)
	got, found, err := f.store.GetWorkflowRun(f.ctx, f.run.ID)
	if err != nil || !found {
		f.t.Fatalf("re-read run: %v (found=%v)", err, found)
	}
	return got.State
}

// The ordinary case, and the reason this function exists: a run parked because
// the reviewer would not launch is released once one demonstrably has. Without
// it the run reports needs_attention forever, and — since needs_attention only
// transitions forward to running — the review step's own completion is later
// dropped as an invalid transition.
func TestClearReviewLaunchStop_ReleasesItsOwnOperativeStop(t *testing.T) {
	for reason := range reviewLaunchStopReasons {
		t.Run(reason, func(t *testing.T) {
			f := newStopScopeFixture(t)
			f.stop(reason)
			if got := f.clear(); got != domain.WorkflowRunRunning {
				t.Fatalf("run = %q after a proven reviewer launch, want running", got)
			}
		})
	}
}

// A stop written by ANOTHER mechanism is not this file's to clear. Clearing it
// would be a reviewer launch silently answering a question nobody asked it —
// the run would leave needs_attention with its actual blocker untouched.
func TestClearReviewLaunchStop_LeavesAForeignStopAlone(t *testing.T) {
	for _, reason := range []string{
		ReasonVerifyWorkspaceUnattributable,
		ReasonVerifyApprovedHeadUnprovable,
	} {
		t.Run(reason, func(t *testing.T) {
			if _, ok := attentionDispositions[reason]; !ok {
				t.Fatalf("%q is not a canonical attention reason; this test is misconfigured", reason)
			}
			f := newStopScopeFixture(t)
			f.stop(reason)
			if got := f.clear(); got != domain.WorkflowRunNeedsAttention {
				t.Fatalf("run = %q, want it still parked: a reviewer launch cleared a stop it did not create", got)
			}
		})
	}
}

// THE SECTION A CLAUSE, in full: a stale reviewer-launch stop must not unblock a
// NEWER lifecycle generation.
//
// The run was parked on a reviewer-launch failure, and then something newer and
// unrelated parked it again. A reviewer launching now resolves the OLD stop and
// nothing else, so the run must stay parked on the newer one. Releasing it here
// is precisely "a stale reviewer generation unblocking a newer lifecycle
// generation", and it cannot happen: the clear is driven by the ledger's NEWEST
// canonical reason, not by the presence of a reviewer-launch reason anywhere in
// history.
func TestClearReviewLaunchStop_NeverUnblocksANewerStop(t *testing.T) {
	f := newStopScopeFixture(t)
	f.stop(ReasonReviewerLaunchRetry)           // generation N: the launch failed
	f.stop(ReasonVerifyWorkspaceUnattributable) // newer, and about something else entirely

	if got := f.clear(); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want it still parked on the newer stop: a reviewer launch "+
			"unblocked a lifecycle generation it knows nothing about", got)
	}
}

// The mirror, which is what makes the test above about ORDER rather than about
// the mere presence of a foreign reason: once the newer stop has been resolved,
// the reviewer-launch stop underneath it is operative again and IS cleared.
func TestClearReviewLaunchStop_ClearsOnceTheNewerStopIsResolved(t *testing.T) {
	f := newStopScopeFixture(t)
	f.stop(ReasonVerifyWorkspaceUnattributable)
	f.stop(attentionClearedPhase) // a resume cleared everything before it
	f.stop(ReasonReviewerLaunchRetry)

	if got := f.clear(); got != domain.WorkflowRunRunning {
		t.Fatalf("run = %q, want running: the operative stop was this file's own", got)
	}
}

// A run that is not parked at all is untouched — a reviewer launching is not a
// licence to move a running run anywhere.
func TestClearReviewLaunchStop_IgnoresARunThatIsNotParked(t *testing.T) {
	f := newStopScopeFixture(t)
	if _, err := f.store.UpdateWorkflowRunState(f.ctx, f.run.ID,
		domain.WorkflowRunNeedsAttention, domain.WorkflowRunRunning, f.base); err != nil {
		t.Fatal(err)
	}
	f.run.State = domain.WorkflowRunRunning
	f.stop(ReasonReviewerLaunchRetry)
	if got := f.clear(); got != domain.WorkflowRunRunning {
		t.Fatalf("run = %q, want running (unchanged)", got)
	}
}
