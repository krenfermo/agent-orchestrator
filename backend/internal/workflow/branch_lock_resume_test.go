package workflow_test

// Checkpoint 8P-E.13A.2 regression fixture: what happens AFTER a branch-lock
// wait is remediated.
//
// Reconstructed synthetically from ~/.ao/data on 2026-08-21 (the real database
// is never read or written here):
//
//	wf-507d9a93  child of master wf-40209d5f. Blocked by a direct-branch lock,
//	             then correctly recovered: the lock was released, the branch_lock
//	             wake revived, agent-orchestrator-11 dispatched, the work step
//	             COMPLETED. Its latest checkpoint said
//	             durable_phase=worker_observed_worker_result_available,
//	             next_action=start_review — and then nothing ever happened.
//	             plan=completed work=completed review=pending, run=needs_attention.
//
// The run row was still parked in needs_attention from the branch wait, and
// needs_attention is a state the forward transitions cannot leave
// (ValidWorkflowRunTransition allows needs_attention -> running only). So the
// work step's own completion transition (-> waiting) was dropped as an invalid
// transition, and every downstream guard that asks for a running/waiting run —
// including the master's "work done, review pending, call ContinueRun" branch,
// the only path that unblocks cycle 1's review — declined to act.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// newBranchResumeCoordinator is newQueuedBranchCoordinator plus the two facts a
// resume test needs to be able to observe: the session facts (so a worker can
// be made to finish) and a workspace that actually reports a change (so the
// finish is git-verified evidence rather than an ambiguous idle).
func newBranchResumeCoordinator(t *testing.T, spawner *fakeSpawner, locks *fakeBranchLocks, wakeSched *fakeWakeScheduler) (*workflowcore.Coordinator, *fakeStore, *fakeSessionFacts, *fakeClock) {
	t.Helper()
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	facts := newFakeSessionFacts()
	spawner.facts = facts
	locks.targets["proj"] = []branchTarget{{repoPath: contendedRepo, branch: contendedBranch}}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Projects:         fakeProjects{"proj": directProject("proj", contendedRepo, contendedBranch)},
		Spawner:          spawner,
		SessionFacts:     facts,
		WorkspaceFacts:   &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{Dirty: true, HeadSHA: "head-1"}},
		BranchLocks:      locks,
		WakeScheduler:    wakeSched,
		ReviewRuns:       newFakeReviewRuns(),
		ReviewerLauncher: &fakeReviewerLauncher{},
		Clock:            clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, facts, clk
}

// parkRunInNeedsAttention writes the legacy/pre-fix durable shape: the run row
// itself in needs_attention, carrying the given stop phase. This is exactly how
// the rows that produced this checkpoint are on disk — a branch wait misfiled
// as a stop.
func parkRunInNeedsAttention(t *testing.T, store *fakeStore, runID, projectID, phase, detail string) {
	t.Helper()
	ctx := context.Background()
	run, ok, err := store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): %v (found=%v)", runID, err, ok)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		if _, err := store.UpdateWorkflowRunState(ctx, runID, run.State, domain.WorkflowRunNeedsAttention, time.Now().UTC()); err != nil {
			t.Fatalf("park %s in needs_attention: %v", runID, err)
		}
	}
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-park-" + runID, WorkflowRunID: runID, ProjectID: projectID,
		NextAction: detail, DurablePhase: phase,
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record stop for %s: %v", runID, err)
	}
}

// finishWorker makes every session this run's steps reference terminated, which
// — together with the dirty workspace the fixture reports — is the fact-only
// evidence evaluateWorkStepProgress accepts as "the work is done".
func finishWorker(t *testing.T, store *fakeStore, facts *fakeSessionFacts, runID string) {
	t.Helper()
	steps, err := store.ListWorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.SessionID == nil {
			continue
		}
		rec, ok, _ := facts.GetSession(context.Background(), domain.SessionID(*s.SessionID))
		if !ok {
			continue
		}
		rec.IsTerminated = true
		rec.Activity = domain.Activity{State: domain.ActivityExited}
		facts.put(rec)
	}
}

// THE BUG, end to end: a run parked in needs_attention by a branch wait, whose
// branch then frees, must run its worker, observe its completion, and dispatch
// its review — without a single human action anywhere in the sequence.
func TestBranchQueuedRunParkedInNeedsAttentionResumesThroughToReview(t *testing.T) {
	locks := newFakeBranchLocks()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: contendedBranch, WorkspacePath: contendedRepo}}}
	c, store, facts, clk := newBranchResumeCoordinator(t, spawner, locks, newFakeWakeScheduler())
	ctx := context.Background()

	// A holder that wrote to the branch keeps it, so the queued run parks.
	holderID := seedStoppedHolder(t, c, store, spawner, workflowcore.ReasonFixBudgetExhausted, true)
	queued, err := c.CreateRun(ctx, "proj", "the queued child")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, queued.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if got := stepState(t, store, queued.Run.ID, domain.WorkflowStepWork); got != domain.WorkflowStepReady {
		t.Fatalf("work step = %q, want ready (the branch is taken)", got)
	}
	// The legacy misfiling this checkpoint has to survive: the branch wait
	// durably recorded as a stop rather than as a wait.
	parkRunInNeedsAttention(t, store, queued.Run.ID, "proj", workflowcore.ReasonBranchQueued,
		"waiting_for_branch: held by "+holderID)

	// The branch frees, and the branch_lock wake fires. ContinueRun is the
	// wakepoller's only entry point (wakepoller.Resumer).
	locks.staleRuns[holderID] = true
	if _, err := c.ContinueRun(ctx, queued.Run.ID); err != nil {
		t.Fatalf("ContinueRun (branch freed): %v", err)
	}

	if spawner.calls != 2 {
		t.Fatalf("spawner calls = %d, want the queued run's worker dispatched once the branch came free", spawner.calls)
	}
	after, _, err := store.GetWorkflowRun(ctx, queued.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if after.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("the run kept its resolved branch stop after its worker was dispatched")
	}
	if !hasCheckpointPhase(t, store, queued.Run.ID, "attention_cleared") {
		t.Fatal("the resume was not recorded: a state change nobody can account for afterwards")
	}

	// The worker finishes. One more wake-driven resume is all AO gets.
	clk.Advance(30 * time.Second)
	finishWorker(t, store, facts, queued.Run.ID)
	if _, err := c.ContinueRun(ctx, queued.Run.ID); err != nil {
		t.Fatalf("ContinueRun (worker finished): %v", err)
	}

	if got := stepState(t, store, queued.Run.ID, domain.WorkflowStepWork); got != domain.WorkflowStepCompleted {
		t.Fatalf("work step = %q, want completed", got)
	}
	if got := stepState(t, store, queued.Run.ID, domain.WorkflowStepReview); got != domain.WorkflowStepRunning {
		t.Fatalf("review step = %q, want running: work completed and next_action was start_review", got)
	}
	detail, err := c.GetRun(ctx, queued.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if detail.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q while its review is running", detail.Run.State)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	if life.Phase != workflowcore.PhaseReviewing {
		t.Fatalf("phase = %q, want reviewing", life.Phase)
	}
	if life.Attention == workflowcore.AttentionHuman {
		t.Fatalf("a run that resumed on its own asked for a human decision: %#v", life)
	}
}

// The other half of the rule, and the one that keeps it honest: a stop that
// genuinely belongs to a person is never cleared by any amount of automatic
// progress underneath it.
func TestGenuineHumanDecisionIsNeverAutoResumed(t *testing.T) {
	for _, stopPhase := range []string{
		workflowcore.ReasonFixBudgetExhausted,
		"dirty_worktree",
		workflowcore.ReasonWorkerBlocked,
		workflowcore.ReasonWorkerDispatchAmbiguous,
	} {
		t.Run(stopPhase, func(t *testing.T) {
			locks := newFakeBranchLocks()
			spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: contendedBranch, WorkspacePath: contendedRepo}}}
			c, store, facts, clk := newBranchResumeCoordinator(t, spawner, locks, newFakeWakeScheduler())
			ctx := context.Background()

			run, err := c.CreateRun(ctx, "proj", "a run that needs a person")
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			if _, err := c.StartRun(ctx, run.Run.ID); err != nil {
				t.Fatalf("StartRun: %v", err)
			}
			parkRunInNeedsAttention(t, store, run.Run.ID, "proj", stopPhase, "a person has to decide something")

			// Even with a worker that finished and real work in the tree, the
			// stop stands: the decision was never AO's to make.
			clk.Advance(30 * time.Second)
			finishWorker(t, store, facts, run.Run.ID)
			if _, err := c.ContinueRun(ctx, run.Run.ID); err != nil {
				t.Fatalf("ContinueRun: %v", err)
			}

			after, _, err := store.GetWorkflowRun(ctx, run.Run.ID)
			if err != nil {
				t.Fatalf("GetWorkflowRun: %v", err)
			}
			if after.State != domain.WorkflowRunNeedsAttention {
				t.Fatalf("run state = %q, want needs_attention: %q is a human decision", after.State, stopPhase)
			}
			if hasCheckpointPhase(t, store, run.Run.ID, "attention_cleared") {
				t.Fatalf("%q was auto-resumed", stopPhase)
			}
		})
	}
}

// The same rule for the other durable carrier. A provider auth failure is
// recorded as an attempt's error_class, not as a checkpoint phase, and the
// checkpoint on top of it is the generic observation phase that named the whole
// stranded-run problem — so this is the exact shape where a fix that keyed only
// on checkpoints would wrongly resume a run whose provider is not logged in.
func TestAuthFailureCarriedOnAnAttemptIsNeverAutoResumed(t *testing.T) {
	locks := newFakeBranchLocks()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: contendedBranch, WorkspacePath: contendedRepo}}}
	c, store, facts, clk := newBranchResumeCoordinator(t, spawner, locks, newFakeWakeScheduler())
	ctx := context.Background()

	run, err := c.CreateRun(ctx, "proj", "a run whose provider is not authenticated")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, run.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	steps, err := store.ListWorkflowSteps(ctx, run.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.Kind != domain.WorkflowStepWork {
			continue
		}
		attempt, ok, aerr := store.GetLatestWorkflowAttempt(ctx, s.ID)
		if aerr != nil || !ok {
			t.Fatalf("GetLatestWorkflowAttempt: %v (found=%v)", aerr, ok)
		}
		if err := store.UpdateWorkflowAttemptOutcome(ctx, attempt.ID, clk.Now(),
			domain.WorkflowAttemptFailed, domain.WorkflowErrorAuth); err != nil {
			t.Fatalf("record auth failure: %v", err)
		}
	}
	// Parked with the generic observation phase on top — no canonical reason in
	// the checkpoints at all. The attempt is the only thing that knows.
	parkRunInNeedsAttention(t, store, run.Run.ID, "proj",
		"worker_observed_worker_result_available", "start_review")

	clk.Advance(30 * time.Second)
	finishWorker(t, store, facts, run.Run.ID)
	if _, err := c.ContinueRun(ctx, run.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	after, _, err := store.GetWorkflowRun(ctx, run.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if after.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention: an auth failure is a human decision", after.State)
	}
	if hasCheckpointPhase(t, store, run.Run.ID, "attention_cleared") {
		t.Fatal("an auth failure was auto-resumed")
	}
}
