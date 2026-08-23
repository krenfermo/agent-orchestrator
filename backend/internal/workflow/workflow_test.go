package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type fakeStore struct {
	runs  map[string]domain.WorkflowRun
	steps map[string][]domain.WorkflowStep

	// attempts, checkpoints, and outbox back Checkpoint 8B's dispatch/
	// observation methods. Keyed the same way the real store's queries are.
	attempts    map[string][]domain.WorkflowAttempt    // by workflow_step_id
	checkpoints map[string][]domain.WorkflowCheckpoint // by workflow_run_id, oldest first
	outbox      map[string]domain.WorkflowOutboxEntry  // by idempotency_key

	// healthEvents backs Checkpoint 8H's minimal agent health, append-only
	// per harness, oldest first (mirrors agent_health_events).
	healthEvents map[string][]domain.AgentHealthEvent
	// scopedHealthEvents backs Checkpoint 8P-C's per-(user,profile) health,
	// keyed by "harness|userID|profileID".
	scopedHealthEvents map[string][]domain.AgentHealthEvent
	// owners backs Checkpoint 8P-C's runOwner lookup (mirrors
	// GetWorkflowRunOwner). nil/missing entry means unowned.
	owners map[string]domain.UserID

	// listStepsErr injects a storage failure into the step lookup, so a test
	// can prove what still happens when the bookkeeping AFTER a durable state
	// transition fails (Checkpoint 8P-E13A.1).
	listStepsErr error

	seq int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		runs:               map[string]domain.WorkflowRun{},
		steps:              map[string][]domain.WorkflowStep{},
		attempts:           map[string][]domain.WorkflowAttempt{},
		checkpoints:        map[string][]domain.WorkflowCheckpoint{},
		outbox:             map[string]domain.WorkflowOutboxEntry{},
		healthEvents:       map[string][]domain.AgentHealthEvent{},
		scopedHealthEvents: map[string][]domain.AgentHealthEvent{},
		owners:             map[string]domain.UserID{},
	}
}

func (f *fakeStore) RecordAgentHealthEvent(_ context.Context, ev domain.AgentHealthEvent) (domain.AgentHealthEvent, error) {
	key := string(ev.Harness)
	f.healthEvents[key] = append(f.healthEvents[key], ev)
	if ev.UserID != "" && ev.ProviderProfileID != "" {
		scopedKey := string(ev.Harness) + "|" + string(ev.UserID) + "|" + string(ev.ProviderProfileID)
		f.scopedHealthEvents[scopedKey] = append(f.scopedHealthEvents[scopedKey], ev)
	}
	return ev, nil
}

func (f *fakeStore) GetAgentHealth(_ context.Context, harness domain.AgentHarness) (domain.AgentHealthEvent, bool, error) {
	list := f.healthEvents[string(harness)]
	if len(list) == 0 {
		return domain.AgentHealthEvent{}, false, nil
	}
	return list[len(list)-1], true, nil
}

func (f *fakeStore) GetAgentHealthScoped(_ context.Context, harness domain.AgentHarness, userID domain.UserID, profileID domain.ProviderProfileID) (domain.AgentHealthEvent, bool, error) {
	key := string(harness) + "|" + string(userID) + "|" + string(profileID)
	list := f.scopedHealthEvents[key]
	if len(list) == 0 {
		return domain.AgentHealthEvent{}, false, nil
	}
	return list[len(list)-1], true, nil
}

func (f *fakeStore) GetWorkflowRunOwner(_ context.Context, id string) (*domain.UserID, error) {
	owner, ok := f.owners[id]
	if !ok {
		return nil, nil
	}
	return &owner, nil
}

func (f *fakeStore) SetWorkflowRunOwner(_ context.Context, id string, owner domain.UserID) (bool, error) {
	if _, ok := f.runs[id]; !ok {
		return false, nil
	}
	f.owners[id] = owner
	return true, nil
}

func (f *fakeStore) UpdateWorkflowRunPolicySnapshot(_ context.Context, id, policySnapshot string, now time.Time) (bool, error) {
	run, ok := f.runs[id]
	if !ok {
		return false, nil
	}
	run.PolicySnapshot = policySnapshot
	run.UpdatedAt = now
	f.runs[id] = run
	return true, nil
}

func (f *fakeStore) CreateWorkflowRun(_ context.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) (domain.WorkflowRun, []domain.WorkflowStep, error) {
	f.runs[run.ID] = run
	f.steps[run.ID] = append([]domain.WorkflowStep{}, steps...)
	return run, steps, nil
}

func (f *fakeStore) GetWorkflowRun(_ context.Context, id string) (domain.WorkflowRun, bool, error) {
	run, ok := f.runs[id]
	return run, ok, nil
}

func (f *fakeStore) ListWorkflowRuns(_ context.Context, projectID string) ([]domain.WorkflowRun, error) {
	var out []domain.WorkflowRun
	for _, run := range f.runs {
		if projectID == "" || run.ProjectID == projectID {
			out = append(out, run)
		}
	}
	return out, nil
}

func (f *fakeStore) ListNonTerminalWorkflowRuns(_ context.Context) ([]domain.WorkflowRun, error) {
	var out []domain.WorkflowRun
	for _, run := range f.runs {
		if !run.State.Terminal() {
			out = append(out, run)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateWorkflowRunState(_ context.Context, id string, expected, next domain.WorkflowRunState, now time.Time) (bool, error) {
	run, ok := f.runs[id]
	if !ok || run.State != expected || !domain.ValidWorkflowRunTransition(expected, next) {
		return false, nil
	}
	run.State = next
	run.UpdatedAt = now
	if next == domain.WorkflowRunCompleted {
		run.CompletedAt = &now
	}
	if next == domain.WorkflowRunCancelled {
		run.CancelledAt = &now
	}
	f.runs[id] = run
	return true, nil
}

func (f *fakeStore) ListWorkflowSteps(_ context.Context, runID string) ([]domain.WorkflowStep, error) {
	if f.listStepsErr != nil {
		return nil, f.listStepsErr
	}
	return append([]domain.WorkflowStep{}, f.steps[runID]...), nil
}

func (f *fakeStore) UpdateWorkflowStepState(_ context.Context, id string, expected, next domain.WorkflowStepState, now time.Time) (bool, error) {
	for runID, steps := range f.steps {
		for i, step := range steps {
			if step.ID != id {
				continue
			}
			if step.State != expected || !domain.ValidWorkflowStepTransition(expected, next) {
				return false, nil
			}
			step.State = next
			step.UpdatedAt = now
			if next.Terminal() {
				step.CompletedAt = &now
			}
			f.steps[runID][i] = step
			return true, nil
		}
	}
	return false, nil
}

// ReopenFailedWorkflowStep mirrors the real store's compare-and-swap: expected
// state pinned to `failed`, completed_at cleared, false when no row matched.
func (f *fakeStore) ReopenFailedWorkflowStep(_ context.Context, stepID string, now time.Time) (bool, error) {
	for runID, steps := range f.steps {
		for i, step := range steps {
			if step.ID != stepID {
				continue
			}
			if step.State != domain.WorkflowStepFailed {
				return false, nil
			}
			step.State = domain.WorkflowStepReady
			step.UpdatedAt = now
			step.CompletedAt = nil
			f.steps[runID][i] = step
			return true, nil
		}
	}
	return false, nil
}

// ReopenCompletedWorkflowStep mirrors the real store's compare-and-swap:
// expected state pinned to `completed`, next state `waiting`, completed_at
// cleared, false when no row matched.
func (f *fakeStore) ReopenCompletedWorkflowStep(_ context.Context, stepID string, now time.Time) (bool, error) {
	for runID, steps := range f.steps {
		for i, step := range steps {
			if step.ID != stepID {
				continue
			}
			if step.State != domain.WorkflowStepCompleted {
				return false, nil
			}
			step.State = domain.WorkflowStepWaiting
			step.UpdatedAt = now
			step.CompletedAt = nil
			f.steps[runID][i] = step
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) ListWorkflowAttempts(_ context.Context, stepID string) ([]domain.WorkflowAttempt, error) {
	return append([]domain.WorkflowAttempt{}, f.attempts[stepID]...), nil
}

func (f *fakeStore) UpdateWorkflowStepArtifact(_ context.Context, stepID, artifactJSON string, now time.Time) (bool, error) {
	for runID, steps := range f.steps {
		for i, step := range steps {
			if step.ID != stepID {
				continue
			}
			step.ArtifactJSON = artifactJSON
			step.UpdatedAt = now
			f.steps[runID][i] = step
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) SetWorkflowStepReviewRun(_ context.Context, stepID, reviewRunID string, now time.Time) (bool, error) {
	for runID, steps := range f.steps {
		for i, step := range steps {
			if step.ID != stepID {
				continue
			}
			rid := reviewRunID
			step.ReviewRunID = &rid
			step.UpdatedAt = now
			f.steps[runID][i] = step
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) UpdateWorkflowStepSession(_ context.Context, stepID, sessionID string, now time.Time) (bool, error) {
	for runID, steps := range f.steps {
		for i, step := range steps {
			if step.ID != stepID {
				continue
			}
			if step.SessionID != nil {
				return false, nil
			}
			sid := sessionID
			step.SessionID = &sid
			step.UpdatedAt = now
			f.steps[runID][i] = step
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) CreateWorkflowAttempt(_ context.Context, id, stepID, harness, model string, startedAt time.Time) (domain.WorkflowAttempt, error) {
	f.seq++
	attempt := domain.WorkflowAttempt{
		ID:             id,
		WorkflowStepID: stepID,
		AttemptNumber:  int64(len(f.attempts[stepID]) + 1),
		Harness:        harness,
		Model:          model,
		StartedAt:      startedAt,
	}
	f.attempts[stepID] = append(f.attempts[stepID], attempt)
	return attempt, nil
}

func (f *fakeStore) GetLatestWorkflowAttempt(_ context.Context, stepID string) (domain.WorkflowAttempt, bool, error) {
	list := f.attempts[stepID]
	if len(list) == 0 {
		return domain.WorkflowAttempt{}, false, nil
	}
	return list[len(list)-1], true, nil
}

func (f *fakeStore) UpdateWorkflowAttemptOutcome(_ context.Context, attemptID string, finishedAt time.Time, outcome domain.WorkflowAttemptOutcome, errorClass domain.WorkflowErrorClass) error {
	for stepID, list := range f.attempts {
		for i, a := range list {
			if a.ID != attemptID {
				continue
			}
			if !finishedAt.IsZero() {
				t := finishedAt
				a.FinishedAt = &t
			} else {
				a.FinishedAt = nil
			}
			a.Outcome = outcome
			a.ErrorClass = errorClass
			f.attempts[stepID][i] = a
			return nil
		}
	}
	return nil
}

func (f *fakeStore) CreateWorkflowCheckpoint(_ context.Context, cp domain.WorkflowCheckpoint) (domain.WorkflowCheckpoint, error) {
	f.checkpoints[cp.WorkflowRunID] = append(f.checkpoints[cp.WorkflowRunID], cp)
	return cp, nil
}

func (f *fakeStore) ListWorkflowCheckpoints(_ context.Context, runID string) ([]domain.WorkflowCheckpoint, error) {
	return append([]domain.WorkflowCheckpoint{}, f.checkpoints[runID]...), nil
}

func (f *fakeStore) GetLatestWorkflowCheckpointByStep(_ context.Context, stepID string) (domain.WorkflowCheckpoint, bool, error) {
	var latest domain.WorkflowCheckpoint
	found := false
	for _, list := range f.checkpoints {
		for _, cp := range list {
			if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
				continue
			}
			if !found || cp.CreatedAt.After(latest.CreatedAt) {
				latest = cp
				found = true
			}
		}
	}
	return latest, found, nil
}

func (f *fakeStore) EnqueueWorkflowOutboxEntry(_ context.Context, entry domain.WorkflowOutboxEntry) (domain.WorkflowOutboxEntry, bool, error) {
	if existing, ok := f.outbox[entry.IdempotencyKey]; ok {
		return existing, false, nil
	}
	entry.Status = domain.WorkflowOutboxPending
	f.outbox[entry.IdempotencyKey] = entry
	return entry, true, nil
}

func (f *fakeStore) UpdateWorkflowOutboxStatus(_ context.Context, id string, expected, next domain.WorkflowOutboxStatus, now time.Time, errorClass string) (bool, error) {
	for key, entry := range f.outbox {
		if entry.ID != id {
			continue
		}
		if entry.Status != expected {
			return false, nil
		}
		entry.Status = next
		switch next {
		case domain.WorkflowOutboxDispatched:
			t := now
			entry.DispatchedAt = &t
		case domain.WorkflowOutboxAcknowledged:
			t := now
			entry.AcknowledgedAt = &t
		case domain.WorkflowOutboxFailed:
			t := now
			entry.FailedAt = &t
		}
		entry.ErrorClass = errorClass
		f.outbox[key] = entry
		return true, nil
	}
	return false, nil
}

func newCoordinator() (*workflowcore.Coordinator, *fakeStore) {
	store := newFakeStore()
	clock := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: store,
		Clock: func() time.Time { return clock },
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store
}

func TestCreateRunSeedsSixLinearSteps(t *testing.T) {
	c, _ := newCoordinator()
	ctx := context.Background()

	detail, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if detail.Run.State != domain.WorkflowRunPending {
		t.Fatalf("run state = %q, want pending", detail.Run.State)
	}
	if len(detail.Steps) != 6 {
		t.Fatalf("steps = %d, want 6", len(detail.Steps))
	}
	wantKinds := []domain.WorkflowStepKind{
		domain.WorkflowStepPlan, domain.WorkflowStepWork, domain.WorkflowStepReview,
		domain.WorkflowStepFix, domain.WorkflowStepVerify, domain.WorkflowStepAdvance,
	}
	for i, sd := range detail.Steps {
		if sd.Step.Kind != wantKinds[i] {
			t.Errorf("step %d kind = %q, want %q", i, sd.Step.Kind, wantKinds[i])
		}
		if sd.Step.Ordinal != int64(i+1) {
			t.Errorf("step %d ordinal = %d, want %d", i, sd.Step.Ordinal, i+1)
		}
		if i == 0 {
			if sd.Step.State != domain.WorkflowStepReady {
				t.Errorf("first step state = %q, want ready", sd.Step.State)
			}
			if sd.Step.DependsOnStepID != nil {
				t.Errorf("first step depends_on = %v, want nil", sd.Step.DependsOnStepID)
			}
			continue
		}
		if sd.Step.State != domain.WorkflowStepPending {
			t.Errorf("step %d state = %q, want pending", i, sd.Step.State)
		}
		if sd.Step.DependsOnStepID == nil || *sd.Step.DependsOnStepID != detail.Steps[i-1].Step.ID {
			t.Errorf("step %d depends_on = %v, want %q", i, sd.Step.DependsOnStepID, detail.Steps[i-1].Step.ID)
		}
	}
}

func TestCreateRunRejectsEmptyObjective(t *testing.T) {
	c, _ := newCoordinator()
	if _, err := c.CreateRun(context.Background(), "proj-1", ""); !errors.Is(err, workflowcore.ErrInvalid) {
		t.Fatalf("CreateRun with empty objective: err=%v, want ErrInvalid", err)
	}
}

func TestCancelRunCascadesToNonTerminalSteps(t *testing.T) {
	c, store := newCoordinator()
	ctx := context.Background()
	detail, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := detail.Run.ID

	cancelled, err := c.CancelRun(ctx, runID)
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if cancelled.Run.State != domain.WorkflowRunCancelled {
		t.Fatalf("run state = %q, want cancelled", cancelled.Run.State)
	}
	for _, sd := range cancelled.Steps {
		if sd.Step.State != domain.WorkflowStepCancelled {
			t.Errorf("step %q state = %q, want cancelled", sd.Step.ID, sd.Step.State)
		}
	}
	_ = store
}

func TestCancelRunOnAlreadyTerminalRunIsRejectedNotSilent(t *testing.T) {
	c, store := newCoordinator()
	ctx := context.Background()
	detail, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := detail.Run.ID

	// Force the run straight to completed (bypassing the coordinator, as a
	// completed run would be in production once execution exists).
	if _, err := store.UpdateWorkflowRunState(ctx, runID, domain.WorkflowRunPending, domain.WorkflowRunRunning, time.Now()); err != nil {
		t.Fatalf("force running: %v", err)
	}
	if _, err := store.UpdateWorkflowRunState(ctx, runID, domain.WorkflowRunRunning, domain.WorkflowRunCompleted, time.Now()); err != nil {
		t.Fatalf("force completed: %v", err)
	}

	if _, err := c.CancelRun(ctx, runID); !errors.Is(err, workflowcore.ErrAlreadyTerminal) {
		t.Fatalf("CancelRun on completed run: err=%v, want ErrAlreadyTerminal", err)
	}
	// The run must not have been mutated back toward running.
	got, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state after rejected cancel = %q, want completed (unchanged)", got.Run.State)
	}
}

func TestListRunsFiltersByProject(t *testing.T) {
	c, _ := newCoordinator()
	ctx := context.Background()
	if _, err := c.CreateRun(ctx, "proj-a", "a"); err != nil {
		t.Fatalf("create a: %v", err)
	}

	runs, err := c.ListRuns(ctx, "proj-a")
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ProjectID != "proj-a" {
		t.Fatalf("runs = %+v", runs)
	}

	empty, err := c.ListRuns(ctx, "proj-nonexistent")
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("runs for unrelated project = %+v, want empty", empty)
	}
}
