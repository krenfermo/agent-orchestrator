// Package workflow is the durable foundation for Checkpoint 8A (Workflow
// Durable Foundation): creating a workflow run and its initial steps,
// reading/listing/cancelling runs, and reconciling (read-mostly) on daemon
// boot. It deliberately does not launch any agent, execute any step, call
// Session Manager or Review Engine, or auto-advance anything — that is all
// out of scope for this checkpoint.
package workflow

import (
	stdctx "context"
	"encoding/json"
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
	ErrPlanLocked      = errors.New("workflow: plan is already accepted or generation is in progress")
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

	// SetWorkflowStepReviewRun backs Checkpoint 8C/8D's review-step dispatch
	// (review_dispatch.go). Checkpoint 8D redefines workflow_steps.
	// review_run_id as "current/most-recent" (mutable across review cycles),
	// so this is an unconditional set, not write-once.
	SetWorkflowStepReviewRun(ctx stdctx.Context, stepID, reviewRunID string, now time.Time) (bool, error)

	// RecordAgentHealthEvent and GetAgentHealth back Checkpoint 8H's minimal
	// durable agent health (failure_classifier.go, health.go, failover.go).
	// Append-only: RecordAgentHealthEvent never updates a prior row;
	// GetAgentHealth derives current health from the latest one.
	RecordAgentHealthEvent(ctx stdctx.Context, ev domain.AgentHealthEvent) (domain.AgentHealthEvent, error)
	GetAgentHealth(ctx stdctx.Context, harness domain.AgentHarness) (domain.AgentHealthEvent, bool, error)
}

type masterPlanStore interface {
	CreateWorkflowPlan(ctx stdctx.Context, runID string, mode domain.WorkflowPlanApprovalMode, contextVersion string, now time.Time) (domain.WorkflowPlanRecord, error)
	GetWorkflowPlan(ctx stdctx.Context, runID string) (domain.WorkflowPlanRecord, bool, error)
	StartWorkflowPlanCommand(ctx stdctx.Context, runID, provider, model, manifest string, now time.Time) (bool, error)
	PersistWorkflowPlanResponse(ctx stdctx.Context, runID, planJSON string, now time.Time) (bool, error)
	FinishWorkflowPlan(ctx stdctx.Context, runID string, status domain.WorkflowPlanStatus, command domain.WorkflowPlanCommandStatus, validationJSON, hash, errorClass string, now time.Time) (bool, error)
	InsertWorkflowTasks(ctx stdctx.Context, tasks []domain.WorkflowTask) error
	ListWorkflowTasks(ctx stdctx.Context, runID string) ([]domain.WorkflowTask, error)
	UpdateWorkflowTaskState(ctx stdctx.Context, id string, expected, next domain.WorkflowTaskState, now time.Time) (bool, error)
	SetWorkflowTaskExecutionRun(ctx stdctx.Context, taskID, executionRunID string, now time.Time) (bool, error)
	FindWorkflowRunByPlannedTask(ctx stdctx.Context, taskID string) (string, bool, error)
	ApproveWorkflowPlan(ctx stdctx.Context, runID string, now time.Time) (bool, error)
	RejectWorkflowPlan(ctx stdctx.Context, runID string, now time.Time) (bool, error)
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

	// ReviewerLauncher backs Checkpoint 8C's review-step dispatch
	// (review_dispatch.go). Optional: a nil ReviewerLauncher means
	// dispatchReviewStep is a no-op, same convention as a nil Spawner for
	// dispatchWorkStep. ReviewRuns (above) doubles as 8C's review read/write
	// path in addition to its original 8A recovery-integrity-check role.
	ReviewerLauncher ReviewerLauncher

	// MessageSender backs Checkpoint 8D's fix-step dispatch (fix_dispatch.go):
	// delivering fix findings to the SAME worker session, never a new Spawn.
	// Optional: a nil MessageSender means dispatchFixStep is a no-op.
	MessageSender         MessageSender
	Verifier              VerifyRunner
	Planner               Planner
	PlannerContextBuilder PlannerContextBuilder

	// Switcher backs Checkpoint 8H's live-session Codex->Claude failover
	// (failover.go): the durable, generation-fenced session_manager agent-
	// switching saga, reused unmodified — workflow only decides WHEN to call
	// it. Optional: a nil Switcher means a live-session provider failure can
	// still be classified/recorded (RecordAgentHealthEvent) but cannot
	// durably switch the session, and is surfaced as needs_attention instead.
	Switcher AgentSwitcher

	// QuestionsStore and PaneReader back Checkpoint 8K-A's durable question
	// detection/classification/policy-resolution (questions_wiring.go).
	// Both optional: a nil QuestionsStore means detection, delivery,
	// dispatch guards, and cancel-cascade are all no-ops (same convention
	// as every other optional dependency here); a nil PaneReader with a
	// non-nil QuestionsStore still delivers already-answered questions
	// (restart recovery) but never attempts new detection.
	QuestionsStore QuestionsStore
	PaneReader     PaneReader

	// DecisionResolverLauncher backs Checkpoint 8K-B pass 2's cross-provider
	// Decision Resolver dispatch (decision_resolver_wiring.go). Optional: a
	// nil launcher means dispatchDecisionResolver always reports
	// "waiting_for_capacity: resolver unavailable" and never launches a
	// session — same nil-safe-optional convention as every other dependency
	// here (e.g. ReviewerLauncher).
	DecisionResolverLauncher DecisionResolverLauncher

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

	// reviewerLauncher backs Checkpoint 8C's review-step dispatch. Optional.
	reviewerLauncher ReviewerLauncher

	// messageSender backs Checkpoint 8D's fix-step dispatch. Optional.
	messageSender         MessageSender
	verifier              VerifyRunner
	planStore             masterPlanStore
	planner               Planner
	plannerContextBuilder PlannerContextBuilder

	// switcher backs Checkpoint 8H's live-session failover. Optional.
	switcher AgentSwitcher

	// questionsStore and paneReader back Checkpoint 8K-A's durable question
	// handling. Both optional.
	questionsStore QuestionsStore
	paneReader     PaneReader

	// decisionResolverLauncher backs Checkpoint 8K-B pass 2's resolver
	// dispatch. Optional.
	decisionResolverLauncher DecisionResolverLauncher
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
		store:                    d.Store,
		projects:                 d.Projects,
		sessions:                 d.Sessions,
		reviewRuns:               d.ReviewRuns,
		log:                      d.Logger,
		spawner:                  d.Spawner,
		sessionFacts:             d.SessionFacts,
		workspaceFacts:           d.WorkspaceFacts,
		reviewerLauncher:         d.ReviewerLauncher,
		messageSender:            d.MessageSender,
		verifier:                 d.Verifier,
		planStore:                func() masterPlanStore { s, _ := d.Store.(masterPlanStore); return s }(),
		planner:                  d.Planner,
		plannerContextBuilder:    d.PlannerContextBuilder,
		switcher:                 d.Switcher,
		questionsStore:           d.QuestionsStore,
		paneReader:               d.PaneReader,
		decisionResolverLauncher: d.DecisionResolverLauncher,
		clock:                    clock,
		newID:                    newID,
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
	Plan       *domain.WorkflowPlanRecord
	Tasks      []domain.WorkflowTask
	// SessionLifecycle is Checkpoint 8M's audit trail of every
	// session_lifecycle_decision checkpoint recorded for this run (task-
	// boundary/provider-switch/fix-compact decisions), in the same order
	// ListWorkflowCheckpoints returns them. Run-level, not per-step — see
	// session_context_pack.go's persistSessionLifecycleDecision doc comment
	// for why these are deliberately not tied to a WorkflowStepID.
	SessionLifecycle []SessionLifecycleAuditEntry
}

// SessionLifecycleAuditEntry is one durable session-lifecycle decision plus
// the context pack it produced (if any) and when it was recorded.
type SessionLifecycleAuditEntry struct {
	Decision    domain.SessionLifecycleDecision
	ContextPack *domain.SessionContextPack
	CreatedAt   time.Time
}

// StepDetail is one workflow step plus its recorded attempts and latest
// checkpoint, if any.
type StepDetail struct {
	Step             domain.WorkflowStep
	Attempts         []domain.WorkflowAttempt
	LatestCheckpoint *domain.WorkflowCheckpoint
	// Review is populated for a review step whose ReviewRunID is set,
	// fetched live from ReviewRuns.GetReviewRun (Checkpoint 8C). Nil when
	// not applicable or the review run could not be read. The findings body
	// is truncated here, not persisted: workflow_checkpoints only ever store
	// the review_run_id reference, never a copy of the body.
	Review *ReviewSummary
	// ReviewPolicy is populated for a review step whose review_policy_decision
	// checkpoint exists (Checkpoint 8I) — for REQUIRED decisions this sits
	// alongside Review; for SKIPPED decisions Review stays nil (no
	// review_run was ever created) and this is the only durable record of
	// why. Read live at GetRun time, mirroring Review's own read pattern.
	ReviewPolicy *ReviewPolicyDecision
	// Routing is Checkpoint 8L's ExecutionRouter decision for this step's
	// role (worker/reviewer/planner), populated whenever a routing_decision
	// checkpoint exists for it — read live, mirroring ReviewPolicy's own
	// pattern.
	Routing *domain.RoutingDecision
}

// ReviewSummary is a read-time-only projection of a review step's review_run
// facts, for the API layer to surface without importing review internals.
type ReviewSummary struct {
	Harness         domain.ReviewerHarness
	Verdict         domain.ReviewVerdict
	Target          string
	FindingsSummary string
}

// reviewFindingsSummaryMaxLen bounds the truncated findings preview surfaced
// in GetRun's response (Checkpoint 8C §7).
const reviewFindingsSummaryMaxLen = 500

// CreateRun creates a new workflow run in state pending and seeds its initial
// steps under the fixed v1 policy: a strict linear chain of six steps
// (plan, work, review, fix, verify, advance). Every step starts pending
// except the first, which starts ready since nothing blocks it — no
// automatic execution happens; a future checkpoint decides when to actually
// run it.
func (c *Coordinator) CreateRun(ctx stdctx.Context, projectID, objective string, verification ...VerificationPlan) (RunDetail, error) {
	return c.createSingleTaskRun(ctx, projectID, objective, nil, nil, verification...)
}

func (c *Coordinator) createSingleTaskRun(ctx stdctx.Context, projectID, objective string, parentWorkflowID, plannedTaskID *string, verification ...VerificationPlan) (RunDetail, error) {
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
	policySnapshot, err := json.Marshal(domain.DefaultWorkflowPolicy())
	if err != nil {
		return RunDetail{}, fmt.Errorf("marshal default workflow policy: %w", err)
	}
	run := domain.WorkflowRun{
		ID:               runID,
		ProjectID:        projectID,
		Objective:        objective,
		State:            domain.WorkflowRunPending,
		PolicyVersion:    policyVersionV1,
		PolicySnapshot:   string(policySnapshot),
		CreatedAt:        now,
		UpdatedAt:        now,
		ParentWorkflowID: parentWorkflowID,
		PlannedTaskID:    plannedTaskID,
	}

	steps := make([]domain.WorkflowStep, 0, len(workflowStepPolicyV1))
	planArtifact := BuildPlanArtifact(projectID, objective, policyVersionV1, verification...)
	planArtifactJSON, err := MarshalPlanArtifact(planArtifact)
	if err != nil {
		return RunDetail{}, err
	}
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
		if kind == domain.WorkflowStepPlan {
			steps[len(steps)-1].ArtifactJSON = planArtifactJSON
		}
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
	if c.planStore != nil {
		if plan, isMaster, planErr := c.planStore.GetWorkflowPlan(ctx, runID); planErr != nil {
			return RunDetail{}, planErr
		} else if isMaster {
			return c.getMasterRun(ctx, run, steps, plan)
		}
	}

	detail := RunDetail{Run: run}
	for i, step := range steps {
		if !run.State.Terminal() && step.Kind == domain.WorkflowStepWork && step.State == domain.WorkflowStepRunning {
			updated, err := c.observeWorkStep(ctx, run, step)
			if err != nil {
				return RunDetail{}, err
			}
			steps[i] = updated
			if refreshed, ok2, rerr := c.store.GetWorkflowRun(ctx, runID); rerr == nil && ok2 {
				run = refreshed
				detail.Run = run
			}
		}
	}

	// Checkpoint 8K-A: durable question detection + restart-recovery
	// delivery sweep, read-time-derived exactly like observeWorkStep above —
	// runs before the review<->fix cascade so a freshly detected/answered
	// question's dispatch guards (dispatch.go, fix_dispatch.go,
	// review_dispatch.go) see up-to-date state within this same call.
	waitingForCapacity, err := c.reconcileQuestions(ctx, run, steps)
	if err != nil {
		return RunDetail{}, err
	}

	// Checkpoint 8D: the automatic review<->fix progression cascade —
	// opportunistically observes a running review/fix step and dispatches
	// the next eligible cycle within this same call (see advanceReviewFixCycle's
	// doc comment). includeCycle1Unblock=false: the very first review
	// dispatch (work-just-completed, review step still "pending") stays
	// ContinueRun's explicit job, exactly as Checkpoint 8C originally scoped
	// it (see TestReviewStepUntouchedAfterWorkCompletion).
	if updatedRun, err := c.advanceReviewFixCycle(ctx, run, steps, false); err != nil {
		return RunDetail{}, err
	} else {
		run = updatedRun
		detail.Run = run
	}

	for _, step := range steps {
		attempts, err := c.store.ListWorkflowAttempts(ctx, step.ID)
		if err != nil {
			return RunDetail{}, err
		}
		var cpPtr *domain.WorkflowCheckpoint
		if cp, hasCP, cperr := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID); cperr == nil && hasCP {
			cp := cp
			cpPtr = &cp
		}
		var reviewSummary *ReviewSummary
		var reviewPolicy *ReviewPolicyDecision
		if step.Kind == domain.WorkflowStepReview {
			if step.ReviewRunID != nil && c.reviewRuns != nil {
				if rr, found, rrErr := c.reviewRuns.GetReviewRun(ctx, *step.ReviewRunID); rrErr == nil && found {
					body := rr.Body
					if len(body) > reviewFindingsSummaryMaxLen {
						body = body[:reviewFindingsSummaryMaxLen]
					}
					reviewSummary = &ReviewSummary{
						Harness:         rr.Harness,
						Verdict:         rr.Verdict,
						Target:          rr.TargetSHA,
						FindingsSummary: body,
					}
				}
			}
			if decision, ok := c.reviewPolicyDecisionForStep(ctx, runID, step.ID); ok {
				reviewPolicy = &decision
			}
		}
		var routing *domain.RoutingDecision
		if decision, ok := c.routingDecisionForStep(ctx, runID, step.ID); ok {
			routing = &decision
		}
		detail.Steps = append(detail.Steps, StepDetail{Step: step, Attempts: attempts, LatestCheckpoint: cpPtr, Review: reviewSummary, ReviewPolicy: reviewPolicy, Routing: routing})
	}

	if checkpoints, cperr := c.store.ListWorkflowCheckpoints(ctx, runID); cperr == nil {
		for _, cp := range checkpoints {
			if cp.NextAction != "" {
				detail.NextAction = cp.NextAction
			}
			if cp.DurablePhase == sessionLifecycleDurablePhase {
				if rec, ok := decodeSessionLifecycleRecord(cp.RetryState); ok {
					detail.SessionLifecycle = append(detail.SessionLifecycle, SessionLifecycleAuditEntry{
						Decision: rec.Decision, ContextPack: rec.ContextPack, CreatedAt: cp.CreatedAt,
					})
				}
			}
		}
	}

	// Checkpoint 8K-A: an open question always wins over any checkpoint's
	// recorded NextAction — the run is not truly progressing toward that
	// next_action until the question is resolved. Derived at read time
	// only, never persisted: the moment the question is answered+delivered
	// it drops out of ListOpenWorkflowQuestionsByRun and this override
	// disappears on its own on the very next GetRun call, no second manual
	// step required.
	if c.questionsStore != nil {
		if open, oerr := c.questionsStore.ListOpenWorkflowQuestionsByRun(ctx, runID); oerr == nil && len(open) > 0 {
			detail.NextAction = nextActionForOpenQuestion(open[0])
		} else if waitingForCapacity != "" {
			// Checkpoint 8K-B: a resolving question stuck on provider
			// capacity only wins the NextAction slot when no higher-priority
			// pending/human_required question override already applies —
			// "an open question always wins" (8K-A's own framing) extends
			// naturally to "an open question always wins over a capacity
			// wait" here.
			detail.NextAction = waitingForCapacity
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

	artifact, err := UnmarshalPlanArtifact(planStep.ArtifactJSON)
	if err != nil {
		return RunDetail{}, err
	}
	if artifact.Objective == "" {
		artifact = BuildPlanArtifact(run.ProjectID, run.Objective, run.PolicyVersion)
	}
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

// ContinueRun is Checkpoint 8C's explicit unblock-and-dispatch entry point,
// named generically (not "startReview") so it stays reusable without a
// breaking API change. Checkpoint 8D widens what it drives (the full
// review<->fix cascade, see advanceReviewFixCycle) but its contract is
// unchanged: any state with nothing eligible to dispatch is an idempotent
// no-op that just returns the current detail, mirroring StartRun's shape.
//
// 8C->8D root-cause fix (double-continue bug): manual E2E testing of 8C
// found that the FIRST POST /continue call sometimes left the review step
// "pending" with reviewRunId: null, requiring a second identical call to
// actually dispatch. Root cause: ContinueRun read the work step's state
// straight from storage without ever opportunistically observing it first
// (unlike GetRun, which always does before deciding anything) — so if the
// real Codex worker had already finished but no prior GetRun poll had yet
// observed that fact, the work step was still durably "running" in the DB,
// dispatchReviewStep's own `workStep.State != Completed` guard correctly
// no-op'd, and the review step stayed "pending" until some later GetRun call
// happened to observe completion first. The fix: ContinueRun now calls
// observeWorkStep itself, within this same call, before evaluating anything
// downstream — exactly mirroring GetRun's existing pattern — so a single
// call reliably dispatches whenever the underlying state is genuinely
// eligible, without depending on interleaving with the frontend's separate
// 2s poll. See TestContinueRunSingleCallDispatchesReviewFromCompletedWork
// for the regression test.
func (c *Coordinator) ContinueRun(ctx stdctx.Context, runID string) (RunDetail, error) {
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

	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	var workStep, reviewStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		case domain.WorkflowStepReview:
			reviewStep = &steps[i]
		}
	}
	if workStep == nil || reviewStep == nil {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q is missing its work/review step", ErrInvalid, runID)
	}

	if workStep.State == domain.WorkflowStepRunning {
		updated, err := c.observeWorkStep(ctx, run, *workStep)
		if err != nil {
			return RunDetail{}, err
		}
		*workStep = updated
		if refreshed, ok2, rerr := c.store.GetWorkflowRun(ctx, runID); rerr == nil && ok2 {
			run = refreshed
		}
	}

	if _, err := c.advanceReviewFixCycle(ctx, run, steps, true); err != nil {
		return RunDetail{}, err
	}

	return c.GetRun(ctx, runID)
}

// policyForRun decodes a run's durable policy_snapshot into a
// domain.WorkflowPolicy, falling back to domain.DefaultWorkflowPolicy() if
// the snapshot is empty/unparseable (defensive: every run created via
// CreateRun always has a valid snapshot; this only guards pre-8D or
// hand-edited rows). Checkpoint 8D's fix-budget enforcement (fix_dispatch.go)
// always reads the policy back through this function — never a bare
// constant scattered in dispatch code.
func policyForRun(run domain.WorkflowRun) domain.WorkflowPolicy {
	var p domain.WorkflowPolicy
	if run.PolicySnapshot != "" && run.PolicySnapshot != "{}" {
		if err := json.Unmarshal([]byte(run.PolicySnapshot), &p); err == nil && p.MaxFixCycles > 0 {
			return p
		}
	}
	return domain.DefaultWorkflowPolicy()
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

	// Checkpoint 8K-A: cancelling a run also cancels any open questions —
	// no resolver call, no delivery attempt follows. Cancelled is terminal
	// for a question just like for a step; the delivery sweep only ever
	// considers state=answered, so a cancelled question is naturally
	// excluded without any extra guard there.
	if c.questionsStore != nil {
		// Checkpoint 8K-B: also force any still-resolving question to
		// cancelled and stop trusting its in-flight resolution attempt —
		// CancelOpenWorkflowQuestionsByRun only ever touched
		// pending/human_required, so state=resolving needs its own pass
		// here. No resolver-launch, no delivery follows either transition.
		if all, err := c.questionsStore.ListWorkflowQuestionsByRun(ctx, runID); err != nil {
			return RunDetail{}, err
		} else {
			for _, q := range all {
				if q.State != domain.QuestionStateResolving {
					continue
				}
				if _, err := c.questionsStore.CancelRunningResolutionsByQuestion(ctx, string(q.ID), now); err != nil {
					return RunDetail{}, err
				}
				if _, err := c.questionsStore.TransitionWorkflowQuestionState(ctx, string(q.ID), domain.QuestionStateResolving, domain.QuestionStateCancelled, "workflow run cancelled", now); err != nil {
					return RunDetail{}, err
				}
			}
		}
		if _, err := c.questionsStore.CancelOpenWorkflowQuestionsByRun(ctx, runID); err != nil {
			return RunDetail{}, err
		}
	}

	return c.GetRun(ctx, runID)
}
