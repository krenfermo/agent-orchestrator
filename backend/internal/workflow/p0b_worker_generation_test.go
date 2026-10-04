package workflow

// P0-B regression: the worker launch path's identity model, and the master
// path's park/resume ABA.
//
// The invariant every test here defends:
//
//	No durable transition off a launch may be taken by a pass that does not
//	hold the generation that launch was made under.
//
// That invariant held for the reviewer and for nothing on the worker path.
// Every transition off `dispatched` was `id + expected_status` -- a predicate
// ANY concurrent pass satisfies -- over a launch whose generation nothing
// recorded at all.

import (
	stdctx "context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// ---------------------------------------------------------------------------
// The outbox claim
// ---------------------------------------------------------------------------

func outboxFixture(t *testing.T) (*sqlite.Store, stdctx.Context, domain.WorkflowOutboxEntry) {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	run := domain.WorkflowRun{ID: "wf-gen", ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now}
	step := domain.WorkflowStep{ID: "wfs-work", WorkflowRunID: run.ID, Kind: domain.WorkflowStepWork,
		Ordinal: 1, State: domain.WorkflowStepReady, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := st.CreateWorkflowRun(ctx, run, []domain.WorkflowStep{step}); err != nil {
		t.Fatal(err)
	}
	stepID := step.ID
	entry, _, err := st.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID: "wfo-1", WorkflowRunID: run.ID, WorkflowStepID: &stepID,
		IdempotencyKey: workStepOutboxIdempotencyKey(step.ID),
		CommandType:    domain.WorkflowOutboxSpawnWorkerSession,
		Payload:        "{}", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, ctx, entry
}

// The acknowledge is the durable arbiter of PHASE 4. A pass that lost its claim
// must change nothing: without this, RUNNING could be licensed by a
// confirmation that belongs to a different launch of the same step.
func TestOnlyTheDispatchHoldingTheClaimMayAcknowledgeIt(t *testing.T) {
	st, ctx, entry := outboxFixture(t)
	now := time.Now().UTC()
	if ok, err := st.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "gen-A"); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	// The pass holding gen-B never held this row.
	if ok, err := st.AcknowledgeWorkflowOutboxDispatch(ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "gen-B"); err != nil || ok {
		t.Fatalf("acknowledge by a foreign generation: ok=%v err=%v, want ok=false", ok, err)
	}
	// And an UNCLAIMED acknowledge must not slip through either, which is what
	// the plain status compare-and-swap used to permit.
	if ok, err := st.AcknowledgeWorkflowOutboxDispatch(ctx, entry.ID, domain.WorkflowOutboxDispatched, now, ""); err != nil || ok {
		t.Fatalf("acknowledge with no generation: ok=%v err=%v, want ok=false", ok, err)
	}
	if ok, err := st.AcknowledgeWorkflowOutboxDispatch(ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "gen-A"); err != nil || !ok {
		t.Fatalf("acknowledge by the holder: ok=%v err=%v, want ok=true", ok, err)
	}
}

// A pass that paused after recording its launch error, and woke to find the row
// released and reclaimed by a second dispatch, must not stamp its failure on
// the live launch -- nor release the live launch's claim back to the pool.
func TestAStaleDispatchCanNeitherFailNorReleaseALiveLaunch(t *testing.T) {
	st, ctx, entry := outboxFixture(t)
	now := time.Now().UTC()
	if ok, _ := st.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "gen-A"); !ok {
		t.Fatal("claim A")
	}
	if ok, err := st.ReleaseDispatchedWorkflowOutboxGeneration(ctx, entry.ID, "transient", "gen-A"); err != nil || !ok {
		t.Fatalf("release A: ok=%v err=%v", ok, err)
	}
	if ok, _ := st.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "gen-B"); !ok {
		t.Fatal("claim B")
	}
	// A, now stale, tries to conclude what it thinks is still its own launch.
	if ok, err := st.FailWorkflowOutboxWithGeneration(ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "runtime_failed", "wfc-A", "gen-A"); err != nil || ok {
		t.Fatalf("stale fail: ok=%v err=%v, want ok=false", ok, err)
	}
	if ok, err := st.ReleaseDispatchedWorkflowOutboxGeneration(ctx, entry.ID, "transient", "gen-A"); err != nil || ok {
		t.Fatalf("stale release: ok=%v err=%v, want ok=false", ok, err)
	}
	entries, _ := st.ListWorkflowOutboxByRun(ctx, "wf-gen")
	if entries[0].Status != domain.WorkflowOutboxDispatched || entries[0].DispatchGeneration != "gen-B" {
		t.Fatalf("entry = %+v, want B's live claim untouched", entries[0])
	}
}

// The human resume reopens the failure the person actually SAW. The outbox row
// is reused across every retry, so `status = 'failed'` is satisfied by any
// failure of it -- and reopening a newer one grants a launch and a fresh budget
// epoch nobody asked for.
func TestAHumanResumeReopensTheFailureItObservedAndNoOther(t *testing.T) {
	st, ctx, entry := outboxFixture(t)
	now := time.Now().UTC()
	// Failure F1, observed by a person.
	if ok, _ := st.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "gen-A"); !ok {
		t.Fatal("claim A")
	}
	if ok, err := st.FailWorkflowOutboxWithGeneration(ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "auth", "F1", "gen-A"); err != nil || !ok {
		t.Fatalf("fail F1: ok=%v err=%v", ok, err)
	}
	// F1 is resumed, redispatched and fails again as F2 before the person's
	// own resume arrives.
	if ok, _ := st.ReopenFailedWorkflowOutboxGeneration(ctx, entry.ID, "auth", "F1"); !ok {
		t.Fatal("reopen F1")
	}
	if ok, _ := st.ClaimWorkflowOutboxDispatch(ctx, entry.ID, now, "gen-B"); !ok {
		t.Fatal("claim B")
	}
	if ok, _ := st.FailWorkflowOutboxWithGeneration(ctx, entry.ID, domain.WorkflowOutboxDispatched, now, "auth", "F2", "gen-B"); !ok {
		t.Fatal("fail F2")
	}
	// The late resume, still holding F1.
	if ok, err := st.ReopenFailedWorkflowOutboxGeneration(ctx, entry.ID, "auth", "F1"); err != nil || ok {
		t.Fatalf("stale reopen: ok=%v err=%v, want ok=false", ok, err)
	}
	entries, _ := st.ListWorkflowOutboxByRun(ctx, "wf-gen")
	if entries[0].Status != domain.WorkflowOutboxFailed || entries[0].FailureGeneration != "F2" {
		t.Fatalf("entry = %+v, want F2 still failed", entries[0])
	}
}

// ---------------------------------------------------------------------------
// The open attempt
// ---------------------------------------------------------------------------

// "The open attempt" used to be positional and computed outside any
// transaction, so two concurrent passes could both create one -- and an attempt
// row is what claims that work is in flight.
func TestExactlyOneConcurrentPassOpensAnAttempt(t *testing.T) {
	st, ctx, _ := outboxFixture(t)
	const passes = 8
	var wg sync.WaitGroup
	created := make([]bool, passes)
	ids := make([]string, passes)
	wg.Add(passes)
	for i := 0; i < passes; i++ {
		go func(i int) {
			defer wg.Done()
			att, isNew, err := st.ClaimOpenWorkflowAttempt(ctx,
				"wfa-"+strings.Repeat("x", i+1), "wfs-work", "claude-code", "", time.Now().UTC())
			if err != nil {
				t.Error(err)
				return
			}
			created[i], ids[i] = isNew, att.ID
		}(i)
	}
	wg.Wait()
	creators, first := 0, ""
	for i := range created {
		if created[i] {
			creators++
		}
		if first == "" {
			first = ids[i]
		}
		if ids[i] != first {
			t.Fatalf("two passes hold different open attempts: %q and %q", first, ids[i])
		}
	}
	if creators != 1 {
		t.Fatalf("creators = %d, want exactly 1", creators)
	}
	attempts, _ := st.ListWorkflowAttempts(ctx, "wfs-work")
	if len(attempts) != 1 {
		t.Fatalf("attempt rows = %d, want 1: an attempt row claims work is in flight", len(attempts))
	}
}

// A concluded attempt is never reused: Checkpoint 8H's rule that a prior
// provider's failed attempt is not overwritten by its fallback's is unchanged.
func TestAConcludedAttemptIsNeverReopenedAsTheOpenOne(t *testing.T) {
	st, ctx, _ := outboxFixture(t)
	now := time.Now().UTC()
	first, isNew, err := st.ClaimOpenWorkflowAttempt(ctx, "wfa-1", "wfs-work", "codex", "", now)
	if err != nil || !isNew {
		t.Fatalf("first: isNew=%v err=%v", isNew, err)
	}
	if ok, err := st.ClaimWorkflowAttemptOutcome(ctx, first.ID, now, domain.WorkflowAttemptFailed, "runtime_failed"); err != nil || !ok {
		t.Fatalf("conclude: ok=%v err=%v", ok, err)
	}
	second, isNew, err := st.ClaimOpenWorkflowAttempt(ctx, "wfa-2", "wfs-work", "claude-code", "", now)
	if err != nil || !isNew || second.ID == first.ID {
		t.Fatalf("second: isNew=%v id=%q err=%v, want a NEW attempt", isNew, second.ID, err)
	}
	// And only one caller may ever conclude an attempt.
	if ok, err := st.ClaimWorkflowAttemptOutcome(ctx, first.ID, now, domain.WorkflowAttemptSucceeded, ""); err != nil || ok {
		t.Fatalf("second conclude: ok=%v err=%v, want ok=false — last-writer-wins is what turned a succeeded attempt into a failed one", ok, err)
	}
}

// ---------------------------------------------------------------------------
// The master task park/resume ABA
// ---------------------------------------------------------------------------

func taskABAFixture(t *testing.T) (*sqlite.Store, stdctx.Context, string) {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	master := domain.WorkflowRun{ID: "wf-aba", ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now}
	if _, _, err := st.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatal(err)
	}
	task := domain.WorkflowTask{ID: "task-aba", WorkflowRunID: master.ID, PlanStepID: "s1", Ordinal: 1,
		Title: "t", Description: "d", AcceptanceCriteriaJSON: "[]", VerifyJSON: "{}", ScopeJSON: "{}",
		State: domain.WorkflowTaskRunning, CreatedAt: now, UpdatedAt: now}
	if err := st.InsertWorkflowTasks(ctx, []domain.WorkflowTask{task}); err != nil {
		t.Fatal(err)
	}
	return st, ctx, task.ID
}

// `running` recurs across the park/resume cycle without bound, so
// `state = 'running'` is satisfied by EVERY generation of it. A pass that
// observed attempt N's conflict and then paused must not park attempt N+1 for
// it -- overwriting the new attempt's attention with stale SHAs.
func TestAParkComputedForOneAttemptCannotLandOnTheNext(t *testing.T) {
	st, ctx, taskID := taskABAFixture(t)
	now := time.Now().UTC()

	// Attempt 1 conflicts and parks.
	if ok, err := st.ParkWorkflowTaskForAttention(ctx, taskID, domain.WorkflowTaskRunning, 0,
		"integration_conflict", domain.WorkflowTaskAttention{Reason: "integration_conflict", Attempt: 1, SourceSHA: "old"}, now); err != nil || !ok {
		t.Fatalf("park 1: ok=%v err=%v", ok, err)
	}
	// A person resumes it into attempt 2.
	if ok, err := st.ResumeWorkflowTaskFromAttention(ctx, taskID, domain.WorkflowTaskRunning, 1, now); err != nil || !ok {
		t.Fatalf("resume 1: ok=%v err=%v", ok, err)
	}
	// The paused pass, still holding attempt 1's conflict, lands now.
	if ok, err := st.ParkWorkflowTaskForAttention(ctx, taskID, domain.WorkflowTaskRunning, 0,
		"integration_conflict", domain.WorkflowTaskAttention{Reason: "integration_conflict", Attempt: 1, SourceSHA: "stale"}, now); err != nil || ok {
		t.Fatalf("stale park: ok=%v err=%v, want ok=false", ok, err)
	}
	tasks, _ := st.ListWorkflowTasks(ctx, "wf-aba")
	if tasks[0].State != domain.WorkflowTaskRunning {
		t.Fatalf("task = %q, want still running: attempt 2 was parked for attempt 1's conflict", tasks[0].State)
	}
	if tasks[0].Attention.SourceSHA != "old" {
		t.Fatalf("attention SHA = %q, want the resumed attempt's history left alone", tasks[0].Attention.SourceSHA)
	}
	// The pass that DOES hold attempt 1's post-resume state parks correctly.
	if ok, err := st.ParkWorkflowTaskForAttention(ctx, taskID, domain.WorkflowTaskRunning, 1,
		"integration_conflict", domain.WorkflowTaskAttention{Reason: "integration_conflict", Attempt: 2, SourceSHA: "new"}, now); err != nil || !ok {
		t.Fatalf("park 2: ok=%v err=%v", ok, err)
	}
}

// The symmetric half: resume the stop the person read, not whichever is current.
func TestAResumeReleasesTheStopItObservedAndNoOther(t *testing.T) {
	st, ctx, taskID := taskABAFixture(t)
	now := time.Now().UTC()
	if ok, _ := st.ParkWorkflowTaskForAttention(ctx, taskID, domain.WorkflowTaskRunning, 0,
		"integration_conflict", domain.WorkflowTaskAttention{Attempt: 1}, now); !ok {
		t.Fatal("park 1")
	}
	if ok, _ := st.ResumeWorkflowTaskFromAttention(ctx, taskID, domain.WorkflowTaskRunning, 1, now); !ok {
		t.Fatal("resume 1")
	}
	if ok, _ := st.ParkWorkflowTaskForAttention(ctx, taskID, domain.WorkflowTaskRunning, 1,
		"verification_failed", domain.WorkflowTaskAttention{Attempt: 2}, now); !ok {
		t.Fatal("park 2")
	}
	// A resume issued against the FIRST stop must not clear the second.
	if ok, err := st.ResumeWorkflowTaskFromAttention(ctx, taskID, domain.WorkflowTaskRunning, 1, now); err != nil || ok {
		t.Fatalf("stale resume: ok=%v err=%v, want ok=false", ok, err)
	}
	tasks, _ := st.ListWorkflowTasks(ctx, "wf-aba")
	if !tasks[0].State.Parked() {
		t.Fatalf("task = %q, want still parked on the stop nobody has read", tasks[0].State)
	}
	if ok, err := st.ResumeWorkflowTaskFromAttention(ctx, taskID, domain.WorkflowTaskRunning, 2, now); err != nil || !ok {
		t.Fatalf("current resume: ok=%v err=%v, want ok=true", ok, err)
	}
}

// A task whose attention_json has never carried an attempt reads as 0, so the
// rows already on disk keep exactly the fence they always had.
func TestTaskRowsWithNoRecordedAttemptStillParkAndResume(t *testing.T) {
	st, ctx, taskID := taskABAFixture(t)
	now := time.Now().UTC()
	if ok, err := st.ParkWorkflowTaskForAttention(ctx, taskID, domain.WorkflowTaskRunning, 0,
		"integration_conflict", domain.WorkflowTaskAttention{Reason: "integration_conflict"}, now); err != nil || !ok {
		t.Fatalf("park: ok=%v err=%v", ok, err)
	}
	if ok, err := st.ResumeWorkflowTaskFromAttention(ctx, taskID, domain.WorkflowTaskRunning, 0, now); err != nil || !ok {
		t.Fatalf("resume: ok=%v err=%v", ok, err)
	}
}

// ---------------------------------------------------------------------------
// T3 — two durable bindings that disagree
// ---------------------------------------------------------------------------

// Under the schema this is impossible. The point of checking is not that it is
// likely: it is that the previous code (`_, _ =`) was structurally incapable of
// telling anyone, and two tasks sharing one execution is the shape that puts two
// workers on one worktree.
func TestADisagreementBetweenTwoDurableBindingsStopsInsteadOfBeingIgnored(t *testing.T) {
	st, ctx, _ := taskABAFixture(t)
	c := New(Deps{Store: st, Projects: st, Clock: func() time.Time { return time.Now().UTC() }})
	other := "wf-somebody-elses-child"
	task := domain.WorkflowTask{ID: "task-aba", WorkflowRunID: "wf-aba", ExecutionRunID: &other}
	err := c.bindTaskToChildRun(ctx, task, "wf-this-passes-child")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "disagreeing bindings") {
		t.Fatalf("err = %v, want a readable refusal naming both bindings", err)
	}
}

// The ordinary recovery case -- already bound to THIS run -- is not a
// contradiction, because the binding is monotonic and unique.
func TestRebindingATaskToTheRunItAlreadyNamesIsNotAContradiction(t *testing.T) {
	st, ctx, taskID := taskABAFixture(t)
	now := time.Now().UTC()
	child := domain.WorkflowRun{ID: "wf-aba-child", ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now}
	if _, _, err := st.CreateWorkflowRun(ctx, child, nil); err != nil {
		t.Fatal(err)
	}
	c := New(Deps{Store: st, Projects: st, Clock: func() time.Time { return time.Now().UTC() }})
	task := domain.WorkflowTask{ID: taskID, WorkflowRunID: "wf-aba", ExecutionRunID: &child.ID}
	if err := c.bindTaskToChildRun(ctx, task, child.ID); err != nil {
		t.Fatalf("re-binding to the same run must be a no-op, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// RUNNING is licensed by the confirmation, not by order of execution
// ---------------------------------------------------------------------------

// `id = ? AND state = 'ready'` is satisfied by any pass that reaches the
// statement. Naming the session makes RUNNING licensed by the confirmation that
// wrote it -- so a step cannot enter RUNNING under a confirmation belonging to
// a different launch of the same step.
func TestAStepOnlyStartsForTheSessionItsOwnConfirmationWrote(t *testing.T) {
	st, ctx, _ := outboxFixture(t)
	now := time.Now().UTC()
	worker, err := st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
		Activity:  domain.Activity{State: domain.ActivityActive},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A step with no session at all cannot start.
	if ok, err := st.StartWorkflowStepForSession(ctx, "wfs-work", string(worker.ID), now); err != nil || ok {
		t.Fatalf("start with no session: ok=%v err=%v, want ok=false", ok, err)
	}
	if _, err := st.UpdateWorkflowStepSession(ctx, "wfs-work", string(worker.ID), now); err != nil {
		t.Fatal(err)
	}
	// A pass holding some OTHER session must not start it.
	if ok, err := st.StartWorkflowStepForSession(ctx, "wfs-work", "somebody-elses-session", now); err != nil || ok {
		t.Fatalf("start for a foreign session: ok=%v err=%v, want ok=false", ok, err)
	}
	steps, _ := st.ListWorkflowSteps(ctx, "wf-gen")
	if steps[0].State != domain.WorkflowStepReady {
		t.Fatalf("step = %q, want still ready", steps[0].State)
	}
	// The confirming pass starts it, once.
	if ok, err := st.StartWorkflowStepForSession(ctx, "wfs-work", string(worker.ID), now); err != nil || !ok {
		t.Fatalf("start by the confirming pass: ok=%v err=%v, want ok=true", ok, err)
	}
	if ok, err := st.StartWorkflowStepForSession(ctx, "wfs-work", string(worker.ID), now); err != nil || ok {
		t.Fatalf("second start: ok=%v err=%v, want ok=false", ok, err)
	}
}
