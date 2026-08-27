package workflow_test

// Regression coverage for the false ambiguous_worker_state on a legitimately
// read-only task.
//
// The production shape: a plan step that said, in as many words, "Verify
// current repository state (build, tests, vet, git status)", with acceptance
// criteria requiring no source/test/documentation edit, requiring the known
// dirty baseline to be preserved, and requiring that no additional modified or
// untracked file appear. The worker ran the checks and went idle. AO recorded:
//
//	ambiguous_worker_state
//	worker idle with no verifiable change — needs human review
//
// Everything here runs against a real *sqlite.Store through the real GetRun
// observation path, including the real dispatch-provenance records the baseline
// fingerprint is read from. The fakes are the session facts, the worktree
// observation, and the clock.
//
// The invariant these tests hold, in both directions:
//
//   - a task the PLAN declared read-only, whose worktree AO has git-verified to
//     be unchanged since dispatch, completes and goes to review;
//   - a task that declared nothing — every legacy plan, every standalone
//     objective — is classified exactly as it was before, ambiguity included.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// readOnlyFixture is one run whose work step is dispatched, confirmed and
// running, with a durable dispatch-time workspace fingerprint under it — which
// is the fact the whole read-only rule is a comparison against.
type readOnlyFixture struct {
	t        *testing.T
	ctx      context.Context
	store    *sqlite.Store
	facts    *fakeSessionFacts
	ws       *mutableWorkspaceFacts
	clk      *fakeClock
	coord    *workflowcore.Coordinator
	runID    string
	stepID   string
	planID   string
	sessID   domain.SessionID
	worktree string
}

// newReadOnlyFixture seeds a run whose plan artifact carries `intent`, and
// whose worktree at dispatch is `baseline` — including, where the test asks for
// it, a pre-existing uncommitted file the plan permits.
func newReadOnlyFixture(t *testing.T, intent domain.WorkflowWriteIntent, baseline ports.WorkspaceObservation) *readOnlyFixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	worktree := t.TempDir()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "p", Path: worktree, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	baseline.Path = worktree
	f := &readOnlyFixture{
		t: t, ctx: ctx, store: store, worktree: worktree,
		facts: newFakeSessionFacts(),
		ws:    &mutableWorkspaceFacts{obs: baseline},
		clk:   &fakeClock{t: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)},
	}
	f.build()
	f.seed(intent, baseline)
	return f
}

func (f *readOnlyFixture) build() {
	f.coord = workflowcore.New(workflowcore.Deps{
		Store:          f.store,
		Projects:       f.store,
		Spawner:        neverSpawner{f.t},
		SessionFacts:   f.facts,
		WorkspaceFacts: f.ws,
		QuestionsStore: f.store,
		Clock:          f.clk.Now,
	})
}

// restart rebuilds the Coordinator over the same store — what a daemon restart
// actually is. Nothing about the read-only verdict may live in memory.
func (f *readOnlyFixture) restart() { f.build() }

func (f *readOnlyFixture) seed(intent domain.WorkflowWriteIntent, baseline ports.WorkspaceObservation) {
	f.t.Helper()
	rec, err := f.store.CreateSession(f.ctx, domain.SessionRecord{
		ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		f.t.Fatalf("seed session: %v", err)
	}
	f.sessID = rec.ID

	created, err := f.coord.CreateRun(f.ctx, "p", "Verify current repository state (build, tests, vet, git status)")
	if err != nil {
		f.t.Fatalf("CreateRun: %v", err)
	}
	f.runID = created.Run.ID
	for _, s := range created.Steps {
		switch s.Step.Kind {
		case domain.WorkflowStepWork:
			f.stepID = s.Step.ID
		case domain.WorkflowStepPlan:
			f.planID = s.Step.ID
		}
	}
	now := f.clk.Now()

	// The durable declaration, exactly where dispatchMasterTask writes it for a
	// planned task: the execution run's own plan step artifact.
	artifact := workflowcore.BuildPlanArtifact("p", "Verify current repository state", "v1")
	artifact.WriteIntent = intent
	raw, err := workflowcore.MarshalPlanArtifact(artifact)
	if err != nil {
		f.t.Fatalf("marshal plan artifact: %v", err)
	}
	if _, err := f.store.UpdateWorkflowStepArtifact(f.ctx, f.planID, raw, now); err != nil {
		f.t.Fatalf("persist plan artifact: %v", err)
	}

	mustStep := func(from, to domain.WorkflowStepState) {
		if _, err := f.store.UpdateWorkflowStepState(f.ctx, f.stepID, from, to, now); err != nil {
			f.t.Fatalf("step %s -> %s: %v", from, to, err)
		}
	}
	mustStep(domain.WorkflowStepPending, domain.WorkflowStepReady)
	mustStep(domain.WorkflowStepReady, domain.WorkflowStepRunning)
	if _, err := f.store.UpdateWorkflowStepSession(f.ctx, f.stepID, string(f.sessID), now); err != nil {
		f.t.Fatalf("attach session: %v", err)
	}
	if _, err := f.store.UpdateWorkflowRunState(f.ctx, f.runID, domain.WorkflowRunPending, domain.WorkflowRunRunning, now); err != nil {
		f.t.Fatalf("run -> running: %v", err)
	}
	stepID := f.stepID
	sessionID := string(f.sessID)
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-dispatched", WorkflowRunID: f.runID, WorkflowStepID: &stepID, ProjectID: "p",
		SessionID: &sessionID, Branch: "work", WorktreePath: f.worktree, BaseSHA: baseline.HeadSHA,
		DurablePhase: "worker_dispatched", PayloadVersion: "v1", RetryState: "{}", CreatedAt: now,
	}); err != nil {
		f.t.Fatalf("seed checkpoint: %v", err)
	}
	// The dispatch-confirmation record, carrying the fingerprint of the tree as
	// the worker was handed it. confirmWorkerDispatch writes exactly this.
	if _, err := f.store.CreateWorkflowDispatchCheckpoint(f.ctx, domain.WorkflowDispatchCheckpoint{
		ID: "wfd-confirmed", WorkflowRunID: f.runID, WorkflowStepID: &stepID, SessionID: &sessionID,
		Phase: domain.DispatchPhaseWorkerDispatched, IdempotencyKey: "idem-1",
		Harness: string(domain.HarnessClaudeCode), LaunchStage: domain.LaunchStageConfirm,
		LaunchOutcome: domain.LaunchOutcomeDispatched,
		Branch:        "work", WorktreePath: f.worktree, BaseSHA: baseline.HeadSHA,
		WorkspaceFingerprint: workflowcore.WorkspaceFingerprint(baseline),
		CreatedAt:            now,
	}); err != nil {
		f.t.Fatalf("seed dispatch checkpoint: %v", err)
	}
	f.setIdle()
}

// reseedDispatchBaseline replaces the dispatch-time fingerprint after the test
// has changed what the worktree looked like when the worker was handed it.
func (f *readOnlyFixture) reseedDispatchBaseline(baseline ports.WorkspaceObservation) {
	f.t.Helper()
	stepID := f.stepID
	sessionID := string(f.sessID)
	if _, err := f.store.CreateWorkflowDispatchCheckpoint(f.ctx, domain.WorkflowDispatchCheckpoint{
		ID: "wfd-confirmed-2", WorkflowRunID: f.runID, WorkflowStepID: &stepID, SessionID: &sessionID,
		Phase: domain.DispatchPhaseWorkerDispatched, IdempotencyKey: "idem-1",
		Harness: string(domain.HarnessClaudeCode), LaunchStage: domain.LaunchStageConfirm,
		LaunchOutcome: domain.LaunchOutcomeDispatched,
		Branch:        "work", WorktreePath: f.worktree, BaseSHA: baseline.HeadSHA,
		WorkspaceFingerprint: workflowcore.WorkspaceFingerprint(baseline),
		CreatedAt:            f.clk.Now(),
	}); err != nil {
		f.t.Fatalf("reseed dispatch checkpoint: %v", err)
	}
}

// setIdle publishes the session facts of a worker that started, worked and
// ended its turn — the exact shape the incident's worker was in.
func (f *readOnlyFixture) setIdle() {
	f.facts.put(domain.SessionRecord{
		ID: f.sessID, ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
		Activity:      domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.clk.Now()},
		FirstSignalAt: f.clk.Now().Add(-5 * time.Minute),
		Metadata:      domain.SessionMetadata{WorkspacePath: f.worktree, Branch: "work"},
	})
}

func (f *readOnlyFixture) poll(n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		// Past the observation throttle, so every poll pays for a fresh
		// worktree reading rather than reusing the dispatch checkpoint's.
		f.clk.Advance(10 * time.Second)
		if _, err := f.coord.GetRun(f.ctx, f.runID); err != nil {
			f.t.Fatalf("GetRun %d: %v", i, err)
		}
	}
}

func (f *readOnlyFixture) run() domain.WorkflowRun {
	f.t.Helper()
	r, ok, err := f.store.GetWorkflowRun(f.ctx, f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v ok=%v", err, ok)
	}
	return r
}

func (f *readOnlyFixture) step() domain.WorkflowStep {
	f.t.Helper()
	steps, err := f.store.ListWorkflowSteps(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepWork {
			return s
		}
	}
	f.t.Fatal("no work step")
	return domain.WorkflowStep{}
}

func (f *readOnlyFixture) checkpoints() []domain.WorkflowCheckpoint {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	return cps
}

func (f *readOnlyFixture) countPhase(phase string) int {
	f.t.Helper()
	n := 0
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

func (f *readOnlyFixture) phases() []string {
	f.t.Helper()
	var out []string
	for _, cp := range f.checkpoints() {
		out = append(out, cp.DurablePhase)
	}
	return out
}

// errorClasses is every error class this run's attempts carry — the column the
// incident's ambiguous_worker_state actually landed in.
func (f *readOnlyFixture) errorClasses() []domain.WorkflowErrorClass {
	f.t.Helper()
	attempt, ok, err := f.store.GetLatestWorkflowAttempt(f.ctx, f.stepID)
	if err != nil {
		f.t.Fatalf("GetLatestWorkflowAttempt: %v", err)
	}
	if !ok {
		return nil
	}
	return []domain.WorkflowErrorClass{attempt.ErrorClass}
}

func (f *readOnlyFixture) assertNoAmbiguity(what string) {
	f.t.Helper()
	if n := f.countPhase(workflowcore.AmbiguousWorkerStateEvidencePhase); n != 0 {
		f.t.Fatalf("%s: %d ambiguous_worker_state evidence row(s); phases = %v", what, n, f.phases())
	}
	for _, class := range f.errorClasses() {
		if class == domain.WorkflowErrorAmbiguousWorkerState {
			f.t.Fatalf("%s: an attempt carries ambiguous_worker_state", what)
		}
	}
}

// dirtyBaseline is the "known dirty baseline" the incident's plan explicitly
// required to be preserved: a real uncommitted file, present before the task
// starts and present, byte-identical, after it.
func dirtyBaseline(t *testing.T, dir string) ports.WorkspaceObservation {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "postrunqa.txt"), []byte("pre-existing local work\n"), 0o600); err != nil {
		t.Fatalf("seed dirty baseline: %v", err)
	}
	return ports.WorkspaceObservation{
		Path: dir, HeadSHA: "base-sha", Dirty: true,
		Changes: []ports.WorkspaceChange{{Path: "postrunqa.txt", Status: "M"}},
	}
}

// ---------------------------------------------------------------------------
// (a) The incident itself: read-only + successful verification + no workspace
// change must COMPLETE, not stop the run.
// ---------------------------------------------------------------------------

func TestReadOnlyTaskWithUnchangedWorktreeCompletesInsteadOfAmbiguous(t *testing.T) {
	f := newReadOnlyFixture(t, domain.WorkflowWriteIntentReadOnly,
		ports.WorkspaceObservation{HeadSHA: "base-sha"})
	f.poll(3)

	f.assertNoAmbiguity("a declared read-only task whose worktree is unchanged")
	if got := f.step().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("work step = %q, want completed; phases = %v", got, f.phases())
	}
	if got := f.run().State; got != domain.WorkflowRunWaiting {
		t.Fatalf("run state = %q, want waiting (i.e. ready for review)", got)
	}
	if n := f.countPhase("worker_observed_worker_read_only_verified"); n != 1 {
		t.Fatalf("read-only verified checkpoints = %d, want exactly 1; phases = %v", n, f.phases())
	}
	// The verdict must be auditable: both fingerprints, on the ledger.
	found := false
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase != "read_only_completion_evidence" {
			continue
		}
		found = true
		if cp.RetryState == "" || cp.RetryState == "{}" {
			t.Fatalf("read-only evidence row carries no payload: %+v", cp)
		}
		if cp.WorktreePath != f.worktree {
			t.Fatalf("read-only evidence row dropped the step's worktree identity: %+v", cp)
		}
	}
	if !found {
		t.Fatalf("no read_only_completion_evidence row was recorded; phases = %v", f.phases())
	}
}

// ---------------------------------------------------------------------------
// (f) The known dirty baseline: present before, unchanged after, and explicitly
// permitted by the plan. It must be accepted — and it is precisely the case a
// dirty/untracked check alone gets wrong in BOTH directions.
// ---------------------------------------------------------------------------

func TestReadOnlyTaskPreservingAKnownDirtyBaselineCompletes(t *testing.T) {
	// The baseline is seeded into the fixture's own worktree, so the
	// fingerprint hashes real file content on both sides of the comparison —
	// which is what makes "unchanged" mean unchanged rather than merely
	// "the same paths are still listed".
	f := newReadOnlyFixture(t, domain.WorkflowWriteIntentReadOnly,
		ports.WorkspaceObservation{HeadSHA: "base-sha"})
	baseline := dirtyBaseline(t, f.worktree)
	f.ws.obs = baseline
	f.reseedDispatchBaseline(baseline)

	f.poll(3)

	f.assertNoAmbiguity("a read-only task that preserved the known dirty baseline")
	if got := f.step().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("work step = %q, want completed; phases = %v", got, f.phases())
	}
	if got := f.run().State; got != domain.WorkflowRunWaiting {
		t.Fatalf("run state = %q, want waiting", got)
	}
}

// ---------------------------------------------------------------------------
// (b) Read-only + an unexpected mutation must stop for a person — with the
// reason that names what actually happened, never the ambiguity.
// ---------------------------------------------------------------------------

func TestReadOnlyTaskThatMutatesTheWorktreeNeedsAttention(t *testing.T) {
	f := newReadOnlyFixture(t, domain.WorkflowWriteIntentReadOnly,
		ports.WorkspaceObservation{HeadSHA: "base-sha"})

	// The worker wrote a file it was told not to write.
	if err := os.WriteFile(filepath.Join(f.worktree, "edited.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.ws.obs = ports.WorkspaceObservation{
		Path: f.worktree, HeadSHA: "base-sha", Untracked: true,
		Changes: []ports.WorkspaceChange{{Path: "edited.go", Status: "??"}},
	}

	f.poll(3)

	f.assertNoAmbiguity("a read-only task that mutated its worktree")
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got)
	}
	if got := f.step().State; got != domain.WorkflowStepWaiting {
		t.Fatalf("work step = %q, want waiting", got)
	}
	if n := f.countPhase("worker_observed_worker_read_only_violated"); n != 1 {
		t.Fatalf("read-only violation checkpoints = %d, want exactly 1; phases = %v", n, f.phases())
	}
	// The canonical stop record: recordAttentionStop writes the reason as the
	// checkpoint's durable phase.
	if n := f.countPhase(workflowcore.ReasonReadOnlyWorkspaceMutated); n != 1 {
		t.Fatalf("%q attention stops = %d, want exactly 1; phases = %v",
			workflowcore.ReasonReadOnlyWorkspaceMutated, n, f.phases())
	}
	if n := f.countPhase(workflowcore.ReasonWorkerDispatchAmbiguous); n != 0 {
		t.Fatalf("the mutation was recorded as an ambiguity; phases = %v", f.phases())
	}
}

// ---------------------------------------------------------------------------
// (c) Fail-closed, unchanged: a task that declared nothing and produced nothing
// is still ambiguous. This is the test that must NEVER be relaxed.
// ---------------------------------------------------------------------------

func TestUndeclaredTaskWithNoChangeIsStillAmbiguous(t *testing.T) {
	for _, intent := range []domain.WorkflowWriteIntent{
		domain.WorkflowWriteIntentUnspecified, // every legacy plan, every standalone objective
		domain.WorkflowWriteIntentMutating,
	} {
		t.Run(string("intent="+intent), func(t *testing.T) {
			f := newReadOnlyFixture(t, intent, ports.WorkspaceObservation{HeadSHA: "base-sha"})
			f.poll(3)

			if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
				t.Fatalf("run state = %q, want needs_attention", got)
			}
			if got := f.step().State; got != domain.WorkflowStepWaiting {
				t.Fatalf("work step = %q, want waiting", got)
			}
			if n := f.countPhase(workflowcore.AmbiguousWorkerStateEvidencePhase); n == 0 {
				t.Fatalf("no ambiguous_worker_state evidence was recorded; phases = %v", f.phases())
			}
			if n := f.countPhase("worker_observed_worker_idle"); n == 0 {
				t.Fatalf("the idle-with-no-change observation was not recorded; phases = %v", f.phases())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (d) The existing success path is untouched: a mutating task with a real
// change completes exactly as it always did.
// ---------------------------------------------------------------------------

func TestMutatingTaskWithRealChangeStillCompletes(t *testing.T) {
	f := newReadOnlyFixture(t, domain.WorkflowWriteIntentMutating,
		ports.WorkspaceObservation{HeadSHA: "base-sha"})
	if err := os.WriteFile(filepath.Join(f.worktree, "impl.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.ws.obs = ports.WorkspaceObservation{
		Path: f.worktree, HeadSHA: "base-sha", Untracked: true,
		Changes: []ports.WorkspaceChange{{Path: "impl.go", Status: "??"}},
	}

	f.poll(3)

	f.assertNoAmbiguity("a mutating task that produced a real change")
	if got := f.step().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("work step = %q, want completed; phases = %v", got, f.phases())
	}
	if n := f.countPhase("worker_observed_worker_result_available"); n != 1 {
		t.Fatalf("result-available checkpoints = %d, want exactly 1; phases = %v", n, f.phases())
	}
}

// ---------------------------------------------------------------------------
// (e) Restart/recovery convergence. The declaration and the baseline are both
// durable, so a Coordinator that has never seen this run before reaches the
// same verdict — which is the whole reason the intent rides on the plan
// artifact rather than in memory.
// ---------------------------------------------------------------------------

func TestReadOnlyCompletionConvergesAcrossARestart(t *testing.T) {
	f := newReadOnlyFixture(t, domain.WorkflowWriteIntentReadOnly,
		ports.WorkspaceObservation{HeadSHA: "base-sha"})

	// Crash before AO ever observed this worker: nothing about the completion
	// has been decided yet.
	f.restart()
	if err := f.coord.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.poll(3)

	f.assertNoAmbiguity("a read-only task observed for the first time after a restart")
	if got := f.step().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("work step = %q, want completed; phases = %v", got, f.phases())
	}

	// And a second restart over the settled run changes nothing: the verdict is
	// idempotent, not re-litigated.
	before := len(f.checkpoints())
	f.restart()
	f.poll(3)
	if got := f.step().State; got != domain.WorkflowStepCompleted {
		t.Fatalf("work step = %q after the second restart, want completed", got)
	}
	f.assertNoAmbiguity("a settled read-only run polled again after a restart")
	if after := len(f.checkpoints()); after != before {
		t.Fatalf("re-observing a completed read-only step wrote %d new checkpoint(s); phases = %v",
			after-before, f.phases())
	}
}

// ---------------------------------------------------------------------------
// (10) Parent<->child propagation: the child never enters needs_attention, so
// there is nothing for the master run to mirror as child_needs_attention.
// ---------------------------------------------------------------------------

func TestCompletedReadOnlyChildNeverParksItsParent(t *testing.T) {
	f := newReadOnlyFixture(t, domain.WorkflowWriteIntentReadOnly,
		ports.WorkspaceObservation{HeadSHA: "base-sha"})
	f.poll(5)

	// The mirror in reconcileMasterRun fires only on a child run sitting in
	// needs_attention. Proving the child never reaches that state is what
	// proves the parent is never parked on it.
	if got := f.run().State; got == domain.WorkflowRunNeedsAttention {
		t.Fatalf("child run state = %q; a parent would mirror this as child_needs_attention", got)
	}
	for _, reason := range []string{
		workflowcore.ReasonWorkerDispatchAmbiguous,
		workflowcore.ReasonWorkerBlocked,
		workflowcore.ReasonReadOnlyWorkspaceMutated,
	} {
		if n := f.countPhase(reason); n != 0 {
			t.Fatalf("a completed read-only child recorded a %q attention stop; phases = %v", reason, f.phases())
		}
	}
}
