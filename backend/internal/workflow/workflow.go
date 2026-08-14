// Package workflow is the durable foundation for Checkpoint 8A (Workflow
// Durable Foundation): creating a workflow run and its initial steps,
// reading/listing/cancelling runs, and reconciling (read-mostly) on daemon
// boot. It deliberately does not launch any agent, execute any step, call
// Session Manager or Review Engine, or auto-advance anything — that is all
// out of scope for this checkpoint.
package workflow

import (
	stdctx "context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrInvalid and ErrNotFound let the transport layer map failures to 422/404.
var (
	ErrInvalid  = errors.New("workflow: invalid input")
	ErrNotFound = errors.New("workflow: not found")
	// ErrAlreadyTerminal means a cancel was requested on a run that is already
	// completed, failed, or cancelled. Cancelling a terminal run is a no-op
	// error, not a silent success: it must never flip a completed run back
	// toward running.
	ErrAlreadyTerminal = errors.New("workflow: run is already terminal")
)

// workflowStepPolicyV1 is the fixed Checkpoint 8A seeding policy: a strict
// linear chain of six steps. Later checkpoints may introduce other policy
// versions; this one is the only one implemented here.
var workflowStepPolicyV1 = []domain.WorkflowStepKind{
	domain.WorkflowStepPlan,
	domain.WorkflowStepWork,
	domain.WorkflowStepReview,
	domain.WorkflowStepFix,
	domain.WorkflowStepVerify,
	domain.WorkflowStepAdvance,
}

const policyVersionV1 = "v1"

// Store is the persistence surface the coordinator needs. *sqlite.Store
// satisfies it in production; tests use a fake.
type Store interface {
	CreateWorkflowRun(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) (domain.WorkflowRun, []domain.WorkflowStep, error)
	GetWorkflowRun(ctx stdctx.Context, id string) (domain.WorkflowRun, bool, error)
	ListWorkflowRuns(ctx stdctx.Context, projectID string) ([]domain.WorkflowRun, error)
	ListNonTerminalWorkflowRuns(ctx stdctx.Context) ([]domain.WorkflowRun, error)
	UpdateWorkflowRunState(ctx stdctx.Context, id string, expected, next domain.WorkflowRunState, now time.Time) (bool, error)
	ListWorkflowSteps(ctx stdctx.Context, runID string) ([]domain.WorkflowStep, error)
	UpdateWorkflowStepState(ctx stdctx.Context, id string, expected, next domain.WorkflowStepState, now time.Time) (bool, error)
	ListWorkflowAttempts(ctx stdctx.Context, stepID string) ([]domain.WorkflowAttempt, error)

	// The methods below back Checkpoint 8B's work-step dispatch/observation.
	UpdateWorkflowStepArtifact(ctx stdctx.Context, stepID, artifactJSON string, now time.Time) (bool, error)
	UpdateWorkflowStepSession(ctx stdctx.Context, stepID, sessionID string, now time.Time) (bool, error)
	CreateWorkflowAttempt(ctx stdctx.Context, id, stepID, harness, model string, startedAt time.Time) (domain.WorkflowAttempt, error)
	GetLatestWorkflowAttempt(ctx stdctx.Context, stepID string) (domain.WorkflowAttempt, bool, error)
	UpdateWorkflowAttemptOutcome(ctx stdctx.Context, attemptID string, finishedAt time.Time, outcome domain.WorkflowAttemptOutcome, errorClass domain.WorkflowErrorClass) error
	CreateWorkflowCheckpoint(ctx stdctx.Context, cp domain.WorkflowCheckpoint) (domain.WorkflowCheckpoint, error)
	ListWorkflowCheckpoints(ctx stdctx.Context, runID string) ([]domain.WorkflowCheckpoint, error)
	GetLatestWorkflowCheckpointByStep(ctx stdctx.Context, stepID string) (domain.WorkflowCheckpoint, bool, error)
	EnqueueWorkflowOutboxEntry(ctx stdctx.Context, entry domain.WorkflowOutboxEntry) (domain.WorkflowOutboxEntry, bool, error)
	UpdateWorkflowOutboxStatus(ctx stdctx.Context, id string, expected, next domain.WorkflowOutboxStatus, now time.Time, errorClass string) (bool, error)
}

// Projects resolves the project a run belongs to.
type Projects interface {
	GetProject(ctx stdctx.Context, id string) (domain.ProjectRecord, bool, error)
}

// Deps wires the coordinator.
type Deps struct {
	Store    Store
	Projects Projects

	// Sessions and ReviewRuns back Reconcile's best-effort integrity check
	// (see recovery.go). Both optional: a nil dependency simply skips its check.
	Sessions   Sessions
	ReviewRuns ReviewRuns
	// Logger receives recovery diagnostics. Optional.
	Logger *slog.Logger

	// Spawner, SessionFacts, and WorkspaceFacts back Checkpoint 8B's work-step
	// dispatch/observation (see dispatch.go, worker_progress.go). All optional:
	// a nil Spawner means dispatchWorkStep is a no-op (useful for tests that
	// only exercise the durable foundation); nil SessionFacts/WorkspaceFacts
	// similarly skip observation.
	Spawner        Spawner
	SessionFacts   SessionFacts
	WorkspaceFacts WorkspaceFacts

	// Clock and NewID are injectable for deterministic tests.
	Clock func() time.Time
	NewID func() string
}

// Coordinator is the core workflow durable-foundation engine.
type Coordinator struct {
	store    Store
	projects Projects
	clock    func() time.Time
	newID    func() string

	// sessions, reviewRuns, and log back Reconcile's best-effort integrity
	// check (see recovery.go). All optional.
	sessions   Sessions
	reviewRuns ReviewRuns
	log        *slog.Logger

	// spawner, sessionFacts, and workspaceFacts back Checkpoint 8B's work-step
	// dispatch/observation. All optional.
	spawner        Spawner
	sessionFacts   SessionFacts
	workspaceFacts WorkspaceFacts
}

// New wires a Coordinator from its dependencies, defaulting the clock and id source.
func New(d Deps) *Coordinator {
	clock := d.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	newID := d.NewID
	if newID == nil {
		newID = uuid.NewString
	}
	return &Coordinator{
		store:          d.Store,
		projects:       d.Projects,
		sessions:       d.Sessions,
		reviewRuns:     d.ReviewRuns,
		log:            d.Logger,
		spawner:        d.Spawner,
		sessionFacts:   d.SessionFacts,
		workspaceFacts: d.WorkspaceFacts,
		clock:          clock,
		newID:          newID,
	}
}

// RunDetail is a workflow run plus its steps and each step's attempts.
type RunDetail struct {
	Run   domain.WorkflowRun
	Steps []StepDetail
	// NextAction mirrors the run's most recent checkpoint's NextAction across
	// all its steps (e.g. "start_review" once the work step completes).
	// Empty when no checkpoint has recorded one yet.
	NextAction string
}

// StepDetail is one workflow step plus its recorded attempts and latest
// checkpoint, if any.
type StepDetail struct {
	Step             domain.WorkflowStep
	Attempts         []domain.WorkflowAttempt
	LatestCheckpoint *domain.WorkflowCheckpoint
}

// CreateRun creates a new workflow run in state pending and seeds its initial
// steps under the fixed v1 policy: a strict linear chain of six steps
// (plan, work, review, fix, verify, advance). Every step starts pending
// except the first, which starts ready since nothing blocks it — no
// automatic execution happens; a future checkpoint decides when to actually
// run it.
func (c *Coordinator) CreateRun(ctx stdctx.Context, projectID, objective string) (RunDetail, error) {
	if projectID == "" {
		return RunDetail{}, fmt.Errorf("%w: project id is required", ErrInvalid)
	}
	if objective == "" {
		return RunDetail{}, fmt.Errorf("%w: objective is required", ErrInvalid)
	}
	if c.projects != nil {
		if _, ok, err := c.projects.GetProject(ctx, projectID); err != nil {
			return RunDetail{}, err
		} else if !ok {
			return RunDetail{}, fmt.Errorf("%w: project %q", ErrNotFound, projectID)
		}
	}

	now := c.clock()
	runID := "wf-" + c.newID()
	run := domain.WorkflowRun{
		ID:             runID,
		ProjectID:      projectID,
		Objective:      objective,
		State:          domain.WorkflowRunPending,
		PolicyVersion:  policyVersionV1,
		PolicySnapshot: "{}",
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	steps := make([]domain.WorkflowStep, 0, len(workflowStepPolicyV1))
	var prevID *string
	for i, kind := range workflowStepPolicyV1 {
		state := domain.WorkflowStepPending
		if i == 0 {
			state = domain.WorkflowStepReady
		}
		stepID := "wfs-" + c.newID()
		steps = append(steps, domain.WorkflowStep{
			ID:              stepID,
			WorkflowRunID:   runID,
			Kind:            kind,
			Ordinal:         int64(i + 1),
			DependsOnStepID: prevID,
			State:           state,
			CreatedAt:       now,
			UpdatedAt:       now,
		})
		id := stepID
		prevID = &id
	}

	createdRun, createdSteps, err := c.store.CreateWorkflowRun(ctx, run, steps)
	if err != nil {
		return RunDetail{}, err
	}
	detail := RunDetail{Run: createdRun}
	for _, step := range createdSteps {
		detail.Steps = append(detail.Steps, StepDetail{Step: step})
	}
	return detail, nil
}

// GetRun reads one workflow run with its steps and each step's attempts.
// While the run is non-terminal and a work step is running, it opportunistically
// observes that step's real progress (session + throttled workspace facts) and
// persists any resulting transition before building the response — this is
// this codebase's existing "derive status at read time" philosophy (compare
// service/session/status.go's deriveStatus), not a background scheduler.
func (c *Coordinator) GetRun(ctx stdctx.Context, runID string) (RunDetail, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}

	detail := RunDetail{Run: run}
	for i, step := range steps {
		if !run.State.Terminal() && step.Kind == domain.WorkflowStepWork && step.State == domain.WorkflowStepRunning {
			updated, err := c.observeWorkStep(ctx, run, step)
			if err != nil {
				return RunDetail{}, err
			}
			steps[i] = updated
			step = updated
			if refreshed, ok2, rerr := c.store.GetWorkflowRun(ctx, runID); rerr == nil && ok2 {
				run = refreshed
				detail.Run = run
			}
		}
		attempts, err := c.store.ListWorkflowAttempts(ctx, step.ID)
		if err != nil {
			return RunDetail{}, err
		}
		var cpPtr *domain.WorkflowCheckpoint
		if cp, hasCP, cperr := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID); cperr == nil && hasCP {
			cp := cp
			cpPtr = &cp
		}
		detail.Steps = append(detail.Steps, StepDetail{Step: step, Attempts: attempts, LatestCheckpoint: cpPtr})
	}

	if checkpoints, cperr := c.store.ListWorkflowCheckpoints(ctx, runID); cperr == nil {
		for _, cp := range checkpoints {
			if cp.NextAction != "" {
				detail.NextAction = cp.NextAction
			}
		}
	}
	return detail, nil
}

// StartRun transitions a pending run to running: executes the deterministic
// plan step synchronously to completion, unblocks the work step (a one-off
// hardcoded "plan just finished, unblock work" edge — not a generic
// dependency-resolution engine), and dispatches its Codex worker spawn
// idempotently. Calling it again on a non-pending, non-terminal run is a
// belt-and-suspenders idempotent no-op that just returns the current detail;
// dispatchWorkStep's own outbox-level guards are the deeper idempotency layer.
func (c *Coordinator) StartRun(ctx stdctx.Context, runID string) (RunDetail, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State.Terminal() {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is already %s", ErrAlreadyTerminal, runID, run.State)
	}
	if run.State != domain.WorkflowRunPending {
		return c.GetRun(ctx, runID)
	}

	now := c.clock()
	if _, err := c.store.UpdateWorkflowRunState(ctx, runID, domain.WorkflowRunPending, domain.WorkflowRunRunning, now); err != nil {
		return RunDetail{}, err
	}
	run.State = domain.WorkflowRunRunning

	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	var planStep, workStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepPlan:
			planStep = &steps[i]
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		}
	}
	if planStep == nil || workStep == nil {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is missing its plan/work step", ErrInvalid, runID)
	}

	artifact := BuildPlanArtifact(run.ProjectID, run.Objective, run.PolicyVersion)
	artifactJSON, err := MarshalPlanArtifact(artifact)
	if err != nil {
		return RunDetail{}, err
	}

	if planStep.State == domain.WorkflowStepReady {
		if _, err := c.store.UpdateWorkflowStepState(ctx, planStep.ID, domain.WorkflowStepReady, domain.WorkflowStepRunning, now); err != nil {
			return RunDetail{}, err
		}
	}
	if _, err := c.store.UpdateWorkflowStepArtifact(ctx, planStep.ID, artifactJSON, now); err != nil {
		return RunDetail{}, err
	}
	if _, err := c.store.UpdateWorkflowStepState(ctx, planStep.ID, domain.WorkflowStepRunning, domain.WorkflowStepCompleted, now); err != nil {
		return RunDetail{}, err
	}

	if workStep.State == domain.WorkflowStepPending {
		if _, err := c.store.UpdateWorkflowStepState(ctx, workStep.ID, domain.WorkflowStepPending, domain.WorkflowStepReady, now); err != nil {
			return RunDetail{}, err
		}
		workStep.State = domain.WorkflowStepReady
	}

	prompt := BuildWorkStepPrompt(artifact)
	if _, err := c.dispatchWorkStep(ctx, run, *workStep, prompt); err != nil {
		return RunDetail{}, err
	}

	return c.GetRun(ctx, runID)
}

// ListRuns lists workflow run summaries, optionally filtered by project id.
func (c *Coordinator) ListRuns(ctx stdctx.Context, projectID string) ([]domain.WorkflowRun, error) {
	return c.store.ListWorkflowRuns(ctx, projectID)
}

// CancelRun transitions a run to cancelled and cascades cancellation to every
// non-terminal step. Cancelling an already-terminal run is a no-op error
// (ErrAlreadyTerminal), never a silent success, and never mutates the run.
func (c *Coordinator) CancelRun(ctx stdctx.Context, runID string) (RunDetail, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State.Terminal() {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is already %s", ErrAlreadyTerminal, runID, run.State)
	}

	now := c.clock()
	moved, err := c.store.UpdateWorkflowRunState(ctx, runID, run.State, domain.WorkflowRunCancelled, now)
	if err != nil {
		return RunDetail{}, err
	}
	if !moved {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q changed while cancelling", ErrInvalid, runID)
	}

	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	for _, step := range steps {
		if step.State.Terminal() {
			continue
		}
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, domain.WorkflowStepCancelled, now); err != nil {
			return RunDetail{}, err
		}
		// Checkpoint 8B semantic: cancelling a run never stops a worker
		// session. No kill/stop port is wired here by construction. The left-
		// running session is only recorded as an informational trail so a
		// human knows to stop it manually if it should not continue.
		if step.Kind == domain.WorkflowStepWork && step.SessionID != nil {
			stepID := step.ID
			sessionID := *step.SessionID
			if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
				ID:             "wfc-" + c.newID(),
				WorkflowRunID:  runID,
				WorkflowStepID: &stepID,
				ProjectID:      run.ProjectID,
				SessionID:      &sessionID,
				NextAction: fmt.Sprintf(
					"worker session %s left running — AO does not auto-stop it on workflow cancellation; stop it manually (e.g. via the session Kill action) if it should not continue",
					sessionID,
				),
				DurablePhase:   "worker_left_running_on_cancel",
				PayloadVersion: "v1",
				RetryState:     "{}",
				CreatedAt:      now,
			}); err != nil {
				return RunDetail{}, err
			}
		}
	}

	return c.GetRun(ctx, runID)
}
