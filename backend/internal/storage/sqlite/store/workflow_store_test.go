package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func sampleWorkflowRun(projectID, id string, now time.Time) domain.WorkflowRun {
	return domain.WorkflowRun{
		ID:             id,
		ProjectID:      projectID,
		Objective:      "ship the thing",
		State:          domain.WorkflowRunPending,
		PolicyVersion:  "v1",
		PolicySnapshot: "{}",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func sampleWorkflowSteps(runID string, now time.Time) []domain.WorkflowStep {
	kinds := []domain.WorkflowStepKind{
		domain.WorkflowStepPlan, domain.WorkflowStepWork, domain.WorkflowStepReview,
		domain.WorkflowStepFix, domain.WorkflowStepVerify, domain.WorkflowStepAdvance,
	}
	steps := make([]domain.WorkflowStep, 0, len(kinds))
	var prev *string
	for i, kind := range kinds {
		state := domain.WorkflowStepPending
		if i == 0 {
			state = domain.WorkflowStepReady
		}
		id := runID + "-step-" + string(rune('1'+i))
		steps = append(steps, domain.WorkflowStep{
			ID: id, WorkflowRunID: runID, Kind: kind, Ordinal: int64(i + 1),
			DependsOnStepID: prev, State: state, CreatedAt: now, UpdatedAt: now,
		})
		copied := id
		prev = &copied
	}
	return steps
}

func TestCreateWorkflowRunSeedsStepsInOneTransaction(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj")
	now := time.Now().UTC().Truncate(time.Second)

	run := sampleWorkflowRun("proj", "wf-1", now)
	steps := sampleWorkflowSteps("wf-1", now)

	createdRun, createdSteps, err := s.CreateWorkflowRun(ctx, run, steps)
	if err != nil {
		t.Fatalf("create workflow run: %v", err)
	}
	if createdRun.State != domain.WorkflowRunPending {
		t.Fatalf("run state = %q, want pending", createdRun.State)
	}
	if len(createdSteps) != 6 {
		t.Fatalf("created steps = %d, want 6", len(createdSteps))
	}
	if createdSteps[0].State != domain.WorkflowStepReady {
		t.Fatalf("first step state = %q, want ready", createdSteps[0].State)
	}
	for i, step := range createdSteps[1:] {
		if step.State != domain.WorkflowStepPending {
			t.Fatalf("step %d state = %q, want pending", i+1, step.State)
		}
	}
	if createdSteps[0].DependsOnStepID != nil {
		t.Fatalf("first step depends_on = %v, want nil", createdSteps[0].DependsOnStepID)
	}
	for i := 1; i < len(createdSteps); i++ {
		if createdSteps[i].DependsOnStepID == nil || *createdSteps[i].DependsOnStepID != createdSteps[i-1].ID {
			t.Fatalf("step %d depends_on = %v, want %q", i, createdSteps[i].DependsOnStepID, createdSteps[i-1].ID)
		}
	}

	fetched, ok, err := s.GetWorkflowRun(ctx, "wf-1")
	if err != nil || !ok {
		t.Fatalf("get workflow run: ok=%v err=%v", ok, err)
	}
	if fetched.Objective != run.Objective {
		t.Fatalf("fetched objective = %q, want %q", fetched.Objective, run.Objective)
	}

	listed, err := s.ListWorkflowSteps(ctx, "wf-1")
	if err != nil || len(listed) != 6 {
		t.Fatalf("list workflow steps: %d, err=%v", len(listed), err)
	}
}

func TestWorkflowRunAndStepFKConstraints(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// A run referencing a nonexistent project must fail.
	_, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("missing-project", "wf-bad", now), nil)
	if err == nil {
		t.Fatal("create workflow run with missing project: want error, got nil")
	}

	seedProject(t, s, "proj2")
	// A step referencing a nonexistent workflow_run_id must fail.
	badStep := domain.WorkflowStep{
		ID: "wfs-bad", WorkflowRunID: "no-such-run", Kind: domain.WorkflowStepPlan,
		Ordinal: 1, State: domain.WorkflowStepReady, CreatedAt: now, UpdatedAt: now,
	}
	_, _, err = s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj2", "wf-2", now), []domain.WorkflowStep{badStep})
	if err == nil {
		t.Fatal("create workflow step with dangling run id inside a mismatched run: want error, got nil")
	}
}

func TestUpdateWorkflowRunStateCAS(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj3")
	now := time.Now().UTC().Truncate(time.Second)
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj3", "wf-3", now), sampleWorkflowSteps("wf-3", now)); err != nil {
		t.Fatalf("create run: %v", err)
	}

	moved, err := s.UpdateWorkflowRunState(ctx, "wf-3", domain.WorkflowRunPending, domain.WorkflowRunRunning, now.Add(time.Second))
	if err != nil || !moved {
		t.Fatalf("advance to running: moved=%v err=%v", moved, err)
	}
	stale, err := s.UpdateWorkflowRunState(ctx, "wf-3", domain.WorkflowRunPending, domain.WorkflowRunFailed, now.Add(2*time.Second))
	if err != nil || stale {
		t.Fatalf("stale CAS: moved=%v err=%v", stale, err)
	}
	completed, err := s.UpdateWorkflowRunState(ctx, "wf-3", domain.WorkflowRunRunning, domain.WorkflowRunCompleted, now.Add(3*time.Second))
	if err != nil || !completed {
		t.Fatalf("complete: moved=%v err=%v", completed, err)
	}
	// The store's CAS primitive only compares the expected/actual state column
	// (like AdvanceSessionInterfaceTransition); it does not itself enforce
	// domain.ValidWorkflowRunTransition — that is the coordinator's job (see
	// internal/workflow's CancelRun and the domain-level transition tests).
	// A second completion CAS with the same expected state is therefore still
	// a legitimate no-op "metadata amendment" at the storage layer.
	sameState, err := s.UpdateWorkflowRunState(ctx, "wf-3", domain.WorkflowRunCompleted, domain.WorkflowRunCompleted, now.Add(4*time.Second))
	if err != nil || !sameState {
		t.Fatalf("completed -> completed (same-state CAS): moved=%v err=%v", sameState, err)
	}

	got, ok, err := s.GetWorkflowRun(ctx, "wf-3")
	if err != nil || !ok || got.State != domain.WorkflowRunCompleted || got.CompletedAt == nil {
		t.Fatalf("final run state = %+v, ok=%v err=%v", got, ok, err)
	}
}

func TestWorkflowAttemptsIncrementAndUnique(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj4")
	now := time.Now().UTC().Truncate(time.Second)
	_, steps, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj4", "wf-4", now), sampleWorkflowSteps("wf-4", now))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stepID := steps[0].ID

	a1, err := s.CreateWorkflowAttempt(ctx, "wfa-1", stepID, "claude-code", "sonnet", now)
	if err != nil {
		t.Fatalf("create attempt 1: %v", err)
	}
	if a1.AttemptNumber != 1 {
		t.Fatalf("attempt 1 number = %d, want 1", a1.AttemptNumber)
	}
	a2, err := s.CreateWorkflowAttempt(ctx, "wfa-2", stepID, "claude-code", "sonnet", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("create attempt 2: %v", err)
	}
	if a2.AttemptNumber != 2 {
		t.Fatalf("attempt 2 number = %d, want 2", a2.AttemptNumber)
	}

	attempts, err := s.ListWorkflowAttempts(ctx, stepID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("list attempts: %d, err=%v", len(attempts), err)
	}
	latest, ok, err := s.GetLatestWorkflowAttempt(ctx, stepID)
	if err != nil || !ok || latest.ID != a2.ID {
		t.Fatalf("latest attempt = %+v, ok=%v err=%v", latest, ok, err)
	}
}

func TestWorkflowCheckpointsAppendOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj5")
	now := time.Now().UTC().Truncate(time.Second)
	_, steps, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj5", "wf-5", now), sampleWorkflowSteps("wf-5", now))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stepID := steps[0].ID

	cp1, err := s.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-1", WorkflowRunID: "wf-5", WorkflowStepID: &stepID, ProjectID: "proj5",
		RetryState: "{}", DurablePhase: "started", PayloadVersion: "v1", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("create checkpoint 1: %v", err)
	}
	cp2, err := s.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-2", WorkflowRunID: "wf-5", WorkflowStepID: &stepID, ProjectID: "proj5",
		RetryState: "{}", DurablePhase: "advanced", PayloadVersion: "v1", CreatedAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("create checkpoint 2: %v", err)
	}
	if cp1.ID == cp2.ID {
		t.Fatal("checkpoints must be distinct rows, never updated in place")
	}

	list, err := s.ListWorkflowCheckpoints(ctx, "wf-5")
	if err != nil || len(list) != 2 {
		t.Fatalf("list checkpoints: %d, err=%v", len(list), err)
	}
	if list[0].DurablePhase != "started" || list[1].DurablePhase != "advanced" {
		t.Fatalf("checkpoint order = %+v", list)
	}
}

func TestWorkflowOutboxIdempotency(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj6")
	now := time.Now().UTC().Truncate(time.Second)
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj6", "wf-6", now), sampleWorkflowSteps("wf-6", now)); err != nil {
		t.Fatalf("create run: %v", err)
	}

	entry := domain.WorkflowOutboxEntry{
		ID: "wfo-1", WorkflowRunID: "wf-6", IdempotencyKey: "spawn-wf-6-step-1",
		CommandType: domain.WorkflowOutboxSpawnWorkerSession, Payload: "{}", CreatedAt: now,
	}
	first, created, err := s.EnqueueWorkflowOutboxEntry(ctx, entry)
	if err != nil || !created || first.ID != "wfo-1" {
		t.Fatalf("first enqueue: created=%v err=%v first=%+v", created, err, first)
	}

	dup := entry
	dup.ID = "wfo-2" // a different id, same idempotency key
	second, created, err := s.EnqueueWorkflowOutboxEntry(ctx, dup)
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if created {
		t.Fatal("second enqueue with the same idempotency key must not create a new row")
	}
	if second.ID != "wfo-1" {
		t.Fatalf("second enqueue returned id %q, want the original %q", second.ID, "wfo-1")
	}

	list, err := s.ListWorkflowOutboxByRun(ctx, "wf-6")
	if err != nil || len(list) != 1 {
		t.Fatalf("list outbox: %d entries, err=%v", len(list), err)
	}
}

// TestWorkflowAttemptFKAgainstNonexistentStep is Checkpoint 8B integrity
// coverage: an attempt row can never dangle off a step that does not exist.
func TestWorkflowAttemptFKAgainstNonexistentStep(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := s.CreateWorkflowAttempt(ctx, "wfa-orphan", "no-such-step", "codex", "", now); err == nil {
		t.Fatal("create attempt against a nonexistent step: want error, got nil")
	}
}

// TestWorkflowStepArtifactJSONCheckRejectsInvalidJSON is Checkpoint 8B
// integrity coverage for the 0095 migration's json_valid(artifact_json) CHECK.
func TestWorkflowStepArtifactJSONCheckRejectsInvalidJSON(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj7")
	now := time.Now().UTC().Truncate(time.Second)
	_, steps, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj7", "wf-7", now), sampleWorkflowSteps("wf-7", now))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stepID := steps[0].ID

	if _, err := s.UpdateWorkflowStepArtifact(ctx, stepID, "not valid json", now); err == nil {
		t.Fatal("update artifact_json with invalid JSON: want error, got nil")
	}
	if _, err := s.UpdateWorkflowStepArtifact(ctx, stepID, `{"objective":"ship it"}`, now); err != nil {
		t.Fatalf("update artifact_json with valid JSON: %v", err)
	}
}

// TestWorkflowAttemptErrorClassRebuiltCheckAcceptsOldAndNewValues is
// Checkpoint 8B store-level coverage for the 0096 migration's rebuilt
// workflow_attempts table: the six 8A error classes and the six new 8B
// classes must all still be accepted by the CHECK constraint, proving the
// rebuild preserved the old values while adding the new ones.
func TestWorkflowAttemptErrorClassRebuiltCheckAcceptsOldAndNewValues(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj8")
	now := time.Now().UTC().Truncate(time.Second)
	_, steps, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj8", "wf-8", now), sampleWorkflowSteps("wf-8", now))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stepID := steps[0].ID

	classes := []domain.WorkflowErrorClass{
		// 8A classes (must still be accepted after the 0096 rebuild).
		domain.WorkflowErrorRateLimited, domain.WorkflowErrorAuth, domain.WorkflowErrorTransient,
		domain.WorkflowErrorTool, domain.WorkflowErrorTestFailed, domain.WorkflowErrorReviewChangesRequested,
		// 8B classes (new).
		domain.WorkflowErrorSessionCreateFailed, domain.WorkflowErrorAgentStartFailed,
		domain.WorkflowErrorPromptDeliveryFailed, domain.WorkflowErrorRuntimeFailed,
		domain.WorkflowErrorWorkerTerminatedUnexpectedly, domain.WorkflowErrorAmbiguousWorkerState,
	}
	for i, class := range classes {
		attempt, err := s.CreateWorkflowAttempt(ctx, "wfa-class-"+string(rune('a'+i)), stepID, "codex", "", now)
		if err != nil {
			t.Fatalf("create attempt for class %q: %v", class, err)
		}
		if err := s.UpdateWorkflowAttemptOutcome(ctx, attempt.ID, now, domain.WorkflowAttemptFailed, class); err != nil {
			t.Fatalf("record error class %q: %v", class, err)
		}
	}
}

// TestWorkflowAttemptErrorClassAcceptsCheckpoint8HClasses covers the 0102
// rebuild: capacity_exhausted and binary_missing must be accepted by the
// real sqlite CHECK constraint, not just by domain.WorkflowErrorClass.Valid.
func TestWorkflowAttemptErrorClassAcceptsCheckpoint8HClasses(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj8h")
	now := time.Now().UTC().Truncate(time.Second)
	_, steps, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj8h", "wf-8h", now), sampleWorkflowSteps("wf-8h", now))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stepID := steps[0].ID

	for i, class := range []domain.WorkflowErrorClass{domain.WorkflowErrorCapacityExhausted, domain.WorkflowErrorBinaryMissing} {
		attempt, err := s.CreateWorkflowAttempt(ctx, "wfa-8h-"+string(rune('a'+i)), stepID, "codex", "", now)
		if err != nil {
			t.Fatalf("create attempt for class %q: %v", class, err)
		}
		if err := s.UpdateWorkflowAttemptOutcome(ctx, attempt.ID, now, domain.WorkflowAttemptFailed, class); err != nil {
			t.Fatalf("record error class %q: %v", class, err)
		}
	}
}

// TestAgentHealthEventAppendOnlyAndLatestWins covers Checkpoint 8H's minimal
// durable agent health: RecordAgentHealthEvent never updates a prior row, and
// GetAgentHealth always derives from the most recently recorded event.
func TestAgentHealthEventAppendOnlyAndLatestWins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, ok, err := s.GetAgentHealth(ctx, domain.HarnessCodex); err != nil || ok {
		t.Fatalf("GetAgentHealth before any event: ok=%v err=%v, want not found", ok, err)
	}

	first, err := s.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-1", Harness: domain.HarnessCodex, State: domain.AgentHealthCooldown,
		Reason: "rate_limited (inferred)", FailureClass: domain.WorkflowErrorRateLimited,
		ConsecutiveFailures: 1, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("RecordAgentHealthEvent 1: %v", err)
	}
	if first.State != domain.AgentHealthCooldown {
		t.Fatalf("first.State = %q, want cooldown", first.State)
	}

	later := now.Add(time.Minute)
	if _, err := s.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-2", Harness: domain.HarnessCodex, State: domain.AgentHealthAvailable,
		Reason: "dispatch succeeded", ConsecutiveFailures: 0, CreatedAt: later,
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent 2: %v", err)
	}

	latest, ok, err := s.GetAgentHealth(ctx, domain.HarnessCodex)
	if err != nil || !ok {
		t.Fatalf("GetAgentHealth: ok=%v err=%v", ok, err)
	}
	if latest.ID != "ahe-2" || latest.State != domain.AgentHealthAvailable {
		t.Fatalf("latest = %+v, want ahe-2/available (most recent event wins)", latest)
	}

	// A different harness must never see codex's events.
	if _, ok, err := s.GetAgentHealth(ctx, domain.HarnessClaudeCode); err != nil || ok {
		t.Fatalf("GetAgentHealth(claude-code): ok=%v err=%v, want not found (per-harness isolation)", ok, err)
	}
}
