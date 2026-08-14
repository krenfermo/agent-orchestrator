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
}

func newFakeStore() *fakeStore {
	return &fakeStore{runs: map[string]domain.WorkflowRun{}, steps: map[string][]domain.WorkflowStep{}}
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

func (f *fakeStore) ListWorkflowAttempts(_ context.Context, _ string) ([]domain.WorkflowAttempt, error) {
	return nil, nil
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
