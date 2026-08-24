package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

func (c *Coordinator) CreateObjectiveRun(ctx stdctx.Context, projectID, objective string, mode domain.WorkflowPlanApprovalMode) (RunDetail, error) {
	if c.planStore == nil || c.planner == nil || c.plannerContextBuilder == nil {
		return RunDetail{}, fmt.Errorf("%w: planner is unavailable", ErrInvalid)
	}
	if mode == "" {
		mode = domain.WorkflowPlanApprovalManual
	}
	if mode != domain.WorkflowPlanApprovalManual && mode != domain.WorkflowPlanApprovalAuto {
		return RunDetail{}, fmt.Errorf("%w: invalid plan approval mode", ErrInvalid)
	}
	if projectID == "" || strings.TrimSpace(objective) == "" {
		return RunDetail{}, fmt.Errorf("%w: project and objective are required", ErrInvalid)
	}
	if _, ok, err := c.projects.GetProject(ctx, projectID); err != nil {
		return RunDetail{}, err
	} else if !ok {
		return RunDetail{}, fmt.Errorf("%w: project %q", ErrNotFound, projectID)
	}
	now := c.clock()
	runID := "wf-" + c.newID()
	snapshot, _ := json.Marshal(domain.DefaultWorkflowPolicy())
	run := domain.WorkflowRun{ID: runID, ProjectID: projectID, Objective: strings.TrimSpace(objective), State: domain.WorkflowRunPending, PolicyVersion: policyVersionV1, PolicySnapshot: string(snapshot), CreatedAt: now, UpdatedAt: now}
	step := domain.WorkflowStep{ID: "wfs-" + c.newID(), WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepReady, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := c.store.CreateWorkflowRun(ctx, run, []domain.WorkflowStep{step}); err != nil {
		return RunDetail{}, err
	}
	if _, err := c.planStore.CreateWorkflowPlan(ctx, runID, mode, PlannerContextVersion, now); err != nil {
		return RunDetail{}, err
	}
	return c.GetRun(ctx, runID)
}

func (c *Coordinator) GeneratePlan(ctx stdctx.Context, runID string) (RunDetail, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	plan, ok, err := c.planStore.GetWorkflowPlan(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, fmt.Errorf("%w: run is not a master objective", ErrInvalid)
	}
	switch plan.Status {
	case domain.WorkflowPlanValidated, domain.WorkflowPlanInvalid:
		return c.GetRun(ctx, runID)
	case domain.WorkflowPlanApproved, domain.WorkflowPlanRejected:
		return RunDetail{}, ErrPlanLocked
	case domain.WorkflowPlanRunning:
		if plan.CommandStatus == domain.WorkflowPlanCommandResponded {
			return c.finalizeGeneratedPlan(ctx, run, plan)
		}
		return RunDetail{}, fmt.Errorf("%w: planner command already running", ErrPlanLocked)
	}
	project, found, err := c.projects.GetProject(ctx, run.ProjectID)
	if err != nil {
		return RunDetail{}, err
	}
	if !found {
		return RunDetail{}, fmt.Errorf("%w: project", ErrNotFound)
	}
	contextValue, err := c.plannerContextBuilder.Build(ctx, project)
	if err != nil {
		return c.failPlan(ctx, run, "planner_start_failed", err)
	}
	manifest := contextValue
	for i := range manifest.Documents {
		manifest.Documents[i].Content = ""
	}
	manifestJSON, _ := json.Marshal(manifest)
	provider, model := "unknown", "unknown"
	if d, ok := c.planner.(PlannerDescriptor); ok {
		provider, model = d.Descriptor()
	}
	started, err := c.planStore.StartWorkflowPlanCommand(ctx, runID, provider, model, string(manifestJSON), c.clock())
	if err != nil {
		return RunDetail{}, err
	}
	if !started {
		return RunDetail{}, ErrPlanLocked
	}
	steps, _ := c.store.ListWorkflowSteps(ctx, runID)
	if len(steps) > 0 && steps[0].State == domain.WorkflowStepReady {
		_, _ = c.store.UpdateWorkflowStepState(ctx, steps[0].ID, domain.WorkflowStepReady, domain.WorkflowStepRunning, c.clock())
	}
	// Checkpoint 8P-B.1: the planner is always Claude Code today (see
	// command.Planner.Descriptor's hardcoded "anthropic" provider) --
	// resolve against that harness rather than inventing a per-planner
	// provider-selection mechanism 8P-C hasn't built yet.
	runtimeEnv, _, _, err := c.resolveRuntimeEnv(ctx, run.ID, domain.HarnessClaudeCode)
	if err != nil {
		return c.failPlan(ctx, run, "planner_start_failed", err)
	}
	response, err := c.planner.Generate(ctx, PlannerRequest{Objective: run.Objective, Project: project, Context: contextValue, MaxSteps: MaxPlanSteps, RuntimeEnv: runtimeEnv})
	if err != nil {
		// Checkpoint 8N.1: a capacity/rate-limit-shaped planner failure must
		// never be treated the same as a real permanent failure (parse
		// error, planner binary missing, etc.) — reusing the SAME
		// provider-neutral classifier every worker/reviewer dispatch already
		// uses (failure_classifier.go), rather than re-deriving planner-
		// specific substring rules that could drift from it.
		cls := classifyProviderFailure(err)
		if cls.Eligible && (cls.Class == domain.WorkflowErrorRateLimited || cls.Class == domain.WorkflowErrorCapacityExhausted || cls.Class == domain.WorkflowErrorTransient) {
			return c.parkPlanForCapacity(ctx, run, err)
		}
		// Checkpoint 8P-E.10: classify by the adapter's typed sentinels
		// (errors.Is), not by substring-matching err.Error() -- the prior
		// "timeout"/"parse" text search could misfire if an objective's own
		// text happened to contain either word, and silently swallowed any
		// provider plain-text error the adapter had appended for
		// diagnostics (now covered above by classifyProviderFailure, since
		// the adapter embeds that text in the wrapped error message).
		//
		// Checkpoint 8P-E.13 Phase 3: a timeout and a malformed response are
		// both retryable facts about one attempt, not verdicts about the
		// objective — and neither is anything a human can repair by answering a
		// question. They now go through retryPlanOrFail, which retries a bounded
		// number of times and only then stops. planner_start_failed keeps
		// failing immediately: it means the planner never ran at all (bad auth,
		// missing binary), which retrying cannot change.
		switch {
		case errors.Is(err, ports.ErrPlannerTimeout):
			return c.retryPlanOrFail(ctx, run, "planner_timeout", err)
		case errors.Is(err, ports.ErrPlannerOutputMalformed):
			return c.retryPlanOrFail(ctx, run, "planner_parse_failed", err)
		}
		return c.failPlan(ctx, run, ReasonPlannerStartFailed, err)
	}
	if response.Provider != "" {
		provider = response.Provider
	}
	if response.Model != "" {
		model = response.Model
	}
	raw, err := json.Marshal(response.Plan)
	if err != nil {
		return c.failPlan(ctx, run, "planner_parse_failed", err)
	}
	if moved, err := c.planStore.PersistWorkflowPlanResponse(ctx, runID, string(raw), c.clock()); err != nil {
		return RunDetail{}, err
	} else if !moved {
		return RunDetail{}, ErrPlanLocked
	}
	plan, _, _ = c.planStore.GetWorkflowPlan(ctx, runID)
	plan.Provider = provider
	plan.Model = model
	return c.finalizeGeneratedPlan(ctx, run, plan)
}

func (c *Coordinator) finalizeGeneratedPlan(ctx stdctx.Context, run domain.WorkflowRun, record domain.WorkflowPlanRecord) (RunDetail, error) {
	var generated MasterPlan
	if err := json.Unmarshal([]byte(record.GeneratedPlanJSON), &generated); err != nil {
		return c.failPlan(ctx, run, "planner_parse_failed", err)
	}
	generated, validation, hash := NormalizeAndValidatePlan(generated, run.Objective, MaxPlanSteps)
	validationJSON, _ := json.Marshal(validation)
	normalizedJSON, _ := json.Marshal(generated)
	if string(normalizedJSON) != record.GeneratedPlanJSON {
		_, _ = c.planStore.PersistWorkflowPlanResponse(ctx, run.ID, string(normalizedJSON), c.clock())
	}
	if !validation.Valid {
		_, _ = c.planStore.FinishWorkflowPlan(ctx, run.ID, domain.WorkflowPlanInvalid, domain.WorkflowPlanCommandFailed, string(validationJSON), hash, "planner_policy_violation", c.clock())
		if run.State == domain.WorkflowRunPending {
			_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, c.clock())
		}
		c.stopPlanningStep(ctx, run.ID)
		return c.GetRun(ctx, run.ID)
	}
	idByPlan := map[string]string{}
	for _, s := range generated.Steps {
		idByPlan[s.ID] = "wft-" + c.newID()
	}
	tasks := make([]domain.WorkflowTask, 0, len(generated.Steps))
	now := c.clock()
	for i, s := range generated.Steps {
		criteria, _ := json.Marshal(s.AcceptanceCriteria)
		verify, _ := json.Marshal(s.Verify)
		deps := make([]string, 0, len(s.Dependencies))
		for _, d := range s.Dependencies {
			deps = append(deps, idByPlan[d])
		}
		state := domain.WorkflowTaskBlocked
		if len(deps) == 0 && i == 0 {
			state = domain.WorkflowTaskEligible
		}
		tasks = append(tasks, domain.WorkflowTask{ID: idByPlan[s.ID], WorkflowRunID: run.ID, PlanStepID: s.ID, Ordinal: int64(i + 1), Title: s.Title, Description: s.Description, AcceptanceCriteriaJSON: string(criteria), VerifyJSON: string(verify), State: state, Dependencies: deps, CreatedAt: now, UpdatedAt: now})
	}
	// Classify the task DAG before the tasks are written, so a task row never
	// exists without the scope estimate that belongs to it. The pair verdicts
	// go in straight after, once the rows they reference exist.
	graph := ClassifyTaskGraph(TaskGraphInput{
		WorkflowRunID: run.ID,
		Objective:     run.Objective,
		RepoRoots:     repoRootsFromContextManifest(record.ContextManifestJSON),
		Tasks:         taskScopeInputs(generated, tasks, idByPlan, nil),
	})
	// Then pick each task's execution strategy from the project's setting and
	// the verdicts just classified. For every project except a smart-parallel
	// one this is a no-op that touches no scope, so an existing plan's stored
	// scope is byte-for-byte what it was before the selector existed.
	graph = ApplyTaskExecutionStrategies(c.projectExecutionMode(ctx, run.ProjectID), graph)
	for i := range tasks {
		scope, ok := graph.Scopes[tasks[i].ID]
		if !ok {
			continue
		}
		raw, err := MarshalTaskScope(scope)
		if err != nil {
			return RunDetail{}, err
		}
		tasks[i].ScopeJSON = raw
	}
	if err := c.planStore.InsertWorkflowTasks(ctx, tasks); err != nil {
		return RunDetail{}, err
	}
	for i := range graph.Relationships {
		graph.Relationships[i].CreatedAt = now
	}
	if err := c.planStore.ReplaceWorkflowTaskRelationships(ctx, graph.Relationships); err != nil {
		return RunDetail{}, err
	}
	if _, err := c.planStore.FinishWorkflowPlan(ctx, run.ID, domain.WorkflowPlanValidated, domain.WorkflowPlanCommandCompleted, string(validationJSON), hash, "", now); err != nil {
		return RunDetail{}, err
	}
	// Checkpoint 8P-D: a valid plan auto-approves either because the client
	// explicitly requested approval_mode=auto (pre-existing behavior,
	// unchanged) OR because the run's frozen execution policy snapshot says
	// AutonomousMode=true. The safety gate is identical either way --
	// NormalizeAndValidatePlan already ran unconditionally above and an
	// invalid plan already returned early -- only the trigger differs. When
	// policy (not the client) is what decided it, persist that explicitly so
	// plan.approvalMode=="auto" is an honest, inspectable approval source
	// distinguishable from a human/manual approval, rather than silently
	// approving while leaving the record saying "manual".
	autonomous := policyForRun(run).Execution.AutonomousMode
	if record.ApprovalMode == domain.WorkflowPlanApprovalAuto || autonomous {
		if autonomous && record.ApprovalMode != domain.WorkflowPlanApprovalAuto {
			_, _ = c.planStore.SetWorkflowPlanApprovalMode(ctx, run.ID, domain.WorkflowPlanApprovalAuto, now)
		}
		return c.ApprovePlan(ctx, run.ID)
	}
	if run.State == domain.WorkflowRunPending {
		_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunWaiting, now)
	}
	steps, _ := c.store.ListWorkflowSteps(ctx, run.ID)
	if len(steps) > 0 && steps[0].State == domain.WorkflowStepRunning {
		_, _ = c.store.UpdateWorkflowStepState(ctx, steps[0].ID, domain.WorkflowStepRunning, domain.WorkflowStepWaiting, now)
	}
	return c.GetRun(ctx, run.ID)
}

// parkPlanForCapacity is Checkpoint 8N.1's planner counterpart to
// dispatch.go's markRunWaitingForCapacity: a capacity/rate-limit-shaped
// planner failure must never permanently invalidate the plan (failPlan's
// WorkflowPlanInvalid is terminal — GeneratePlan's own status switch treats
// it as a no-op forever after). Instead the plan command is reset to
// WorkflowPlanPending (the same "nothing attempted yet" state GeneratePlan's
// switch falls through to actual generation on), so the exact same
// GeneratePlan call this function was itself invoked from is what a later
// retry re-enters — no second, parallel "resume planning" code path. The run
// itself is deliberately left in WorkflowRunPending (untouched): several
// other places in this file gate on run.State == Pending
// (finalizeGeneratedPlan, failPlan), and flipping it to Waiting here would
// desync those checks from a state this checkpoint did not audit closely
// enough to change safely. Observability instead comes from the durable wake
// itself (NextWakeAt/WaitReason, surfaced by GetRun once wired — see
// scheduleWake) and this checkpoint, not from run.State.
func (c *Coordinator) parkPlanForCapacity(ctx stdctx.Context, run domain.WorkflowRun, cause error) (RunDetail, error) {
	now := c.clock()
	validation, _ := json.Marshal(PlanValidation{Valid: false, Errors: []string{"waiting_for_capacity: " + cause.Error()}})
	// command_status must reset to idle (not failed): StartWorkflowPlanCommand
	// — the exact call the next GeneratePlan retry makes — only CAS-arms from
	// status=pending AND command_status IN (idle, pending); leaving it at
	// failed would permanently ErrPlanLocked every retry.
	_, _ = c.planStore.FinishWorkflowPlan(ctx, run.ID, domain.WorkflowPlanPending, domain.WorkflowPlanCommandIdle, string(validation), "", "planner_capacity", now)
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		ProjectID:      run.ProjectID,
		NextAction:     "waiting_for_capacity: planner unavailable (" + cause.Error() + ") — will retry automatically once capacity recovers",
		DurablePhase:   "planner_capacity_wait",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	})
	c.scheduleWake(ctx, run, nil, wake.ReasonPlannerCapacity, "")
	return c.GetRun(ctx, run.ID)
}

// maxPlannerRetries bounds how many times a timed-out or malformed planner
// response is retried before AO stops and says so. Deliberately small: a
// planner that has failed three times in a row is failing for a reason a
// fourth identical attempt will not discover, and every attempt costs real
// provider budget.
//
// The counter is derived, not stored: retryPlanOrFail counts the run's own
// durable planner_retry_scheduled checkpoints. That makes the budget survive
// daemon restarts, Mac sleeps and renderer closes for free, with no new column
// and no in-memory state that a crash could reset to zero.
const maxPlannerRetries = 3

// retryPlanOrFail is Checkpoint 8P-E.13 Phase 3's truthful planner-failure
// semantics.
//
// The old behavior called failPlan for every planner_timeout, which marked the
// plan permanently WorkflowPlanInvalid (GeneratePlan's own status switch treats
// that as a no-op forever after) and moved the run to needs_attention. The
// Board then rendered "Te necesita" for a condition no human decision can
// repair — the MEDUSA misreport this checkpoint exists to end.
//
// Within budget it instead reuses parkPlanForCapacity's proven mechanism: reset
// the plan command to Pending/Idle (the exact state GeneratePlan falls through
// to real generation on, so the retry re-enters the same code path rather than
// a parallel one), record a canonical planner_retry_scheduled stop, and let the
// durable wake bring it back. The run's phase derives to "retrying" and its
// attention to ao_internal, so nothing interrupts the user.
//
// Past budget it hands over to failPlan with ReasonPlannerExhausted — which
// DOES carry a real human action ("retry planning, simplify the objective, or
// switch the planner provider"), so the resulting human_decision satisfies the
// Phase 2 invariant honestly rather than by synthesis.
func (c *Coordinator) retryPlanOrFail(ctx stdctx.Context, run domain.WorkflowRun, class string, cause error) (RunDetail, error) {
	attempts := c.plannerRetryCount(ctx, run.ID)
	if attempts >= maxPlannerRetries {
		return c.failPlan(ctx, run, ReasonPlannerExhausted, fmt.Errorf("%s after %d retries: %w", class, attempts, cause))
	}

	now := c.clock()
	validation, _ := json.Marshal(PlanValidation{Valid: false, Errors: []string{class + ": " + cause.Error()}})
	// Same CAS-safety reasoning as parkPlanForCapacity: command_status must
	// reset to idle, not failed, or StartWorkflowPlanCommand permanently
	// ErrPlanLocks every retry.
	_, _ = c.planStore.FinishWorkflowPlan(ctx, run.ID, domain.WorkflowPlanPending, domain.WorkflowPlanCommandIdle, string(validation), "", class, now)
	c.stopPlanningStep(ctx, run.ID)
	c.recordAttentionStop(ctx, run, nil, ReasonPlannerRetryScheduled, fmt.Sprintf(
		"%s (retry %d of %d): %s — AO will retry planning automatically, no action needed",
		class, attempts+1, maxPlannerRetries, cause.Error(),
	))
	// transient_retry is the existing wake reason for exactly this shape of
	// wait (a bounded retry of something that failed once), and its backoff
	// grows per attempt off the wake row's own attempt_count. No new reason,
	// no migration.
	c.scheduleWake(ctx, run, nil, wake.ReasonTransientRetry, "")
	return c.GetRun(ctx, run.ID)
}

// plannerRetryCount counts this run's durable planner_retry_scheduled
// checkpoints — the restart-safe retry budget counter.
func (c *Coordinator) plannerRetryCount(ctx stdctx.Context, runID string) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// Cannot read the budget: treat it as exhausted rather than risk an
		// unbounded retry loop. Conservative in the direction that stops.
		return maxPlannerRetries
	}
	n := 0
	for _, cp := range cps {
		if cp.DurablePhase == ReasonPlannerRetryScheduled {
			n++
		}
	}
	return n
}

func (c *Coordinator) failPlan(ctx stdctx.Context, run domain.WorkflowRun, class string, cause error) (RunDetail, error) {
	validation, _ := json.Marshal(PlanValidation{Valid: false, Errors: []string{cause.Error()}})
	_, _ = c.planStore.FinishWorkflowPlan(ctx, run.ID, domain.WorkflowPlanInvalid, domain.WorkflowPlanCommandFailed, string(validation), "", class, c.clock())
	if run.State == domain.WorkflowRunPending {
		_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, c.clock())
	}
	c.stopPlanningStep(ctx, run.ID)
	// Checkpoint 8P-E.13: a failed plan records WHY in the canonical
	// vocabulary, so the Board can name the stop and the action instead of
	// falling through to an unnamed needs_attention.
	c.recordAttentionStop(ctx, run, nil, class, cause.Error())
	detail, getErr := c.GetRun(ctx, run.ID)
	if getErr != nil {
		return RunDetail{}, getErr
	}
	return detail, nil
}

func (c *Coordinator) stopPlanningStep(ctx stdctx.Context, runID string) {
	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err == nil && len(steps) > 0 && steps[0].State == domain.WorkflowStepRunning {
		_, _ = c.store.UpdateWorkflowStepState(ctx, steps[0].ID, domain.WorkflowStepRunning, domain.WorkflowStepWaiting, c.clock())
	}
}

func (c *Coordinator) ApprovePlan(ctx stdctx.Context, runID string) (RunDetail, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, ErrNotFound
	}
	plan, ok, err := c.planStore.GetWorkflowPlan(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, ErrInvalid
	}
	if plan.Status == domain.WorkflowPlanApproved {
		return c.GetRun(ctx, runID)
	}
	if plan.Status != domain.WorkflowPlanValidated {
		return RunDetail{}, fmt.Errorf("%w: plan is not valid", ErrInvalid)
	}
	now := c.clock()
	moved, err := c.planStore.ApproveWorkflowPlan(ctx, runID, now)
	if err != nil {
		return RunDetail{}, err
	}
	if !moved {
		return c.GetRun(ctx, runID)
	}
	steps, _ := c.store.ListWorkflowSteps(ctx, runID)
	if len(steps) > 0 {
		if steps[0].State == domain.WorkflowStepWaiting {
			_, _ = c.store.UpdateWorkflowStepState(ctx, steps[0].ID, domain.WorkflowStepWaiting, domain.WorkflowStepRunning, now)
		}
		_, _ = c.store.UpdateWorkflowStepState(ctx, steps[0].ID, domain.WorkflowStepRunning, domain.WorkflowStepCompleted, now)
	}
	if run.State == domain.WorkflowRunWaiting || run.State == domain.WorkflowRunPending {
		_, _ = c.store.UpdateWorkflowRunState(ctx, runID, run.State, domain.WorkflowRunRunning, now)
	}
	return c.GetRun(ctx, runID)
}

func (c *Coordinator) RejectPlan(ctx stdctx.Context, runID string) (RunDetail, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if !ok {
		return RunDetail{}, ErrNotFound
	}
	if run.State.Terminal() {
		return RunDetail{}, ErrAlreadyTerminal
	}
	_, _ = c.planStore.RejectWorkflowPlan(ctx, runID, c.clock())
	return c.CancelRun(ctx, runID)
}

func (c *Coordinator) getMasterRun(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep, plan domain.WorkflowPlanRecord) (RunDetail, error) {
	if plan.Status == domain.WorkflowPlanApproved && !run.State.Terminal() {
		if err := c.reconcileMasterTasks(ctx, run); err != nil {
			return RunDetail{}, err
		}
		run, _, _ = c.store.GetWorkflowRun(ctx, run.ID)
	}
	tasks, err := c.planStore.ListWorkflowTasks(ctx, run.ID)
	if err != nil {
		return RunDetail{}, err
	}
	detail := RunDetail{Run: run, Plan: &plan, Tasks: tasks}
	for _, step := range steps {
		attempts, _ := c.store.ListWorkflowAttempts(ctx, step.ID)
		detail.Steps = append(detail.Steps, StepDetail{Step: step, Attempts: attempts})
	}
	if summary, err := c.buildIntegrationSummary(ctx, run.ID); err == nil {
		detail.IntegrationState = summary
	}
	// Checkpoint 8P-E.13: a master run needs the same durable attention
	// carriers a single-task run has had since 8P-E.12. Without these three
	// reads, DeriveLifecycle saw an empty LatestCheckpointPhase and an empty
	// question list for every objective run, so ClassifyAttention could never
	// name a master stop — which is precisely why a planner failure surfaced as
	// an unexplained "needs attention" on the Board.
	if cps, cerr := c.store.ListWorkflowCheckpoints(ctx, run.ID); cerr == nil {
		for _, cp := range cps {
			if cp.NextAction != "" {
				detail.NextAction = cp.NextAction
			}
			if !cp.CreatedAt.Before(detail.LatestCheckpointAt) {
				detail.LatestCheckpointPhase = cp.DurablePhase
				detail.LatestCheckpointAt = cp.CreatedAt
			}
		}
	}
	if c.questionsStore != nil {
		if qs, qerr := c.questionsStore.ListWorkflowQuestionsByRun(ctx, run.ID); qerr == nil {
			detail.Questions = qs
		}
	}
	if c.wakeScheduler != nil {
		if next, werr := c.wakeScheduler.NextForRun(ctx, domain.WorkflowRunID(run.ID)); werr == nil && next != nil {
			at := next.ScheduledAt
			detail.NextWakeAt = &at
			detail.WaitReason = string(next.Reason)
		}
	}
	return detail, nil
}

// reconcileMasterTasks is the read-time master-task reconcile loop (called
// from getMasterRun on every GetRun, and from Reconcile at boot). Checkpoint
// 8P-D wraps the actual reconcile pass so that, regardless of which branch it
// took, a still-progressing autonomous run always leaves behind a fresh
// headless-progression wake -- see maybeScheduleAutonomousHeartbeat.
func (c *Coordinator) reconcileMasterTasks(ctx stdctx.Context, run domain.WorkflowRun) error {
	err := c.reconcileMasterTasksOnce(ctx, run)
	c.maybeScheduleAutonomousHeartbeat(ctx, run.ID)
	return err
}

// maybeScheduleAutonomousHeartbeat re-schedules Checkpoint 8P-D's
// ReasonAutonomousProgress wake after a reconcile pass, so the daemon poller
// alone keeps calling ContinueRun/GetRun on this master run until it either
// reaches a terminal state, needs human attention, or is genuinely blocked on
// a HUMAN_REQUIRED question -- at which point it deliberately stops
// rescheduling itself (no busy loop, and no point polling a run that is
// waiting on a human; the existing answer-question path is what re-enters
// reconcileMasterTasks and therefore reschedules this heartbeat again once
// unblocked). Best-effort: reads fresh state itself rather than trusting the
// caller's possibly-stale run value, and never fails/propagates an error --
// matches scheduleCapacityWake's own "observers don't invent failures"
// convention.
func (c *Coordinator) maybeScheduleAutonomousHeartbeat(ctx stdctx.Context, runID string) {
	if c.wakeScheduler == nil {
		return
	}
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		return
	}
	if run.State.Terminal() {
		return
	}
	// Checkpoint 8P-E.13 Phase 7: needs_attention used to end the heartbeat
	// unconditionally, which is how every AO-remediable stop became permanent.
	// A planner waiting on its own scheduled retry, a review queued behind a
	// branch lock, a verify-driven fix cycle — all of them park the run in
	// needs_attention and all of them are things AO finishes by itself, but
	// with no wake left behind nothing ever called back. Only a stop AO cannot
	// remediate stops the heartbeat now.
	// Checkpoint 8P-E.13A.2 widens Phase 7's rule from "self-remediable" to
	// "not the user's problem". The third kind of stop — one AO stopped on but
	// could not name — used to end the heartbeat as hard as an exhausted fix
	// budget, which is how a run AO had already recovered from stayed silent:
	// nothing was waiting on a person, and nothing was going to call back.
	//
	// One human-owned reason is deliberately exempt: the child mirror. It is not
	// a decision addressed to this run at all (the person acts on the CHILD),
	// and ending the parent's heartbeat on it is what made the mirror permanent
	// — the heartbeat is the only thing that re-enters reconcileMasterTasks, so
	// with it gone nothing was left to notice the child had recovered. The
	// parent keeps watching; reconcileMirroredChildStop decides, from the
	// child's current state, whether the mirror is still true.
	if run.State == domain.WorkflowRunNeedsAttention && c.stopIsHumanOwned(ctx, run) &&
		!c.stopIsMirroredChildStop(ctx, run) {
		return
	}
	if !policyForRun(run).Execution.AutonomousMode {
		return
	}
	if c.questionsStore != nil {
		questions, qerr := c.questionsStore.ListWorkflowQuestionsByRun(ctx, runID)
		if qerr == nil {
			for _, q := range questions {
				if q.State == domain.QuestionStateHumanRequired {
					return
				}
			}
		}
	}
	// Checkpoint 8P-E.12: never add a heartbeat on top of a more specific open
	// wake. NextForRun surfaces the soonest one, and the heartbeat's short
	// interval would usually win it — hiding "waiting for reviewer capacity,
	// next retry 14:02" behind a generic "autonomous progress". The specific
	// wake already resumes the run, so the heartbeat would buy nothing anyway.
	if next, err := c.wakeScheduler.NextForRun(ctx, domain.WorkflowRunID(runID)); err == nil && next != nil && next.Reason != wake.ReasonAutonomousProgress {
		return
	}
	c.scheduleWake(ctx, run, nil, wake.ReasonAutonomousProgress, "")
}

func (c *Coordinator) reconcileMasterTasksOnce(ctx stdctx.Context, run domain.WorkflowRun) error {
	tasks, err := c.planStore.ListWorkflowTasks(ctx, run.ID)
	if err != nil {
		return err
	}
	// Before anything is decided about this plan: re-derive the one piece of the
	// parent's state that is not the parent's own, from the children's CURRENT
	// durable states. It runs first so every branch below — promotion, task
	// completion, the plan's own completeRun, the next task's dispatch — sees a
	// parent whose attention reflects what is true now rather than what was true
	// when some child stopped hours ago.
	run = c.reconcileMirroredChildStop(ctx, run, tasks)
	completed := map[string]bool{}
	activeTasks := map[string]bool{}
	parkedTasks := map[string]bool{}
	for i := range tasks {
		task := &tasks[i]
		if task.State == domain.WorkflowTaskCompleted {
			completed[task.ID] = true
			continue
		}
		if task.State.Parked() {
			// The task is stopped on something only a person can decide, and
			// this pass must do NOTHING about it. Skipping it here is the whole
			// remedy for the retry storm the reviewer found: before the parked
			// state existed, a task whose integration had conflicted stayed at
			// "running", so every poll re-entered the branch below, re-rebased
			// the same worktree onto the same target, hit the same conflict and
			// wrote another checkpoint and another notification.
			//
			// It is deliberately NOT counted as active. An independent sibling
			// must keep running and integrating — a parked task holds nothing
			// and blocks nobody except its own dependents, who are blocked
			// because they depend on it, not because the objective stopped.
			parkedTasks[task.ID] = true
			continue
		}
		if task.State != domain.WorkflowTaskRunning {
			continue
		}
		activeTasks[task.ID] = true
		if task.ExecutionRunID == nil {
			if id, ok, err := c.planStore.FindWorkflowRunByPlannedTask(ctx, task.ID); err != nil {
				return err
			} else if ok {
				_, _ = c.planStore.SetWorkflowTaskExecutionRun(ctx, task.ID, id, c.clock())
				task.ExecutionRunID = &id
			} else {
				return fmt.Errorf("task %s running without execution run", task.ID)
			}
		}
		child, err := c.GetRun(ctx, *task.ExecutionRunID)
		if err != nil {
			return err
		}
		// Checkpoint 8P-E.13A.2: needs_attention joins running/waiting here when
		// the child's stop is not a human decision. A child recovered from a
		// branch queue can have a completed work step and a still-pending review
		// step underneath a stale stop, and cycle 1's review unblock only ever
		// happens through ContinueRun (advanceReviewFixCycle's
		// includeCycle1Unblock) — so refusing to call it for a needs_attention
		// child is what left the run permanently one transition short of Review.
		// A child genuinely waiting on a person is excluded, unchanged.
		childCanAdvance := child.Run.State == domain.WorkflowRunRunning ||
			child.Run.State == domain.WorkflowRunWaiting ||
			(child.Run.State == domain.WorkflowRunNeedsAttention && !c.stopIsHumanOwned(ctx, child.Run))
		if childCanAdvance {
			// The parent's mirror of this child was already re-derived above,
			// from the children's current states, for every branch of this pass
			// — not just this one.
			var workDone, reviewPending bool
			for _, s := range child.Steps {
				if s.Step.Kind == domain.WorkflowStepWork && s.Step.State == domain.WorkflowStepCompleted {
					workDone = true
				}
				if s.Step.Kind == domain.WorkflowStepReview && (s.Step.State == domain.WorkflowStepPending || s.Step.State == domain.WorkflowStepReady) {
					reviewPending = true
				}
			}
			if workDone && reviewPending {
				child, err = c.ContinueRun(ctx, child.Run.ID)
				if err != nil {
					return err
				}
			}
		}
		// Checkpoint 8P-E.13 Phase 6: every child outcome now has an explicit
		// answer for what happens to its task row. Previously only the
		// completed case did, so a child that failed, was cancelled, or stopped
		// for attention left its task at "running" indefinitely — the stale
		// "Task 1 of 7 running" that outlived its child by hours.
		switch child.Run.State {
		case domain.WorkflowRunCompleted:
			// Checkpoint 8M.1: materialize this task's verified worktree
			// content into the master run's integration ref BEFORE marking
			// it completed, so a crash between the two never lets a task
			// count as "done" without its code actually being propagated.
			if err := c.promoteTaskToIntegration(ctx, run, *task, child); err != nil {
				if errors.Is(err, errIntegrationBusy) {
					// Another task of this run owns the integration lane right
					// now. Nothing is wrong: the task stays running and this
					// reconcile (which re-runs on every GetRun) retries. Parking
					// the run in needs_attention here would turn the single-lane
					// property itself into a stop for a human.
					return nil
				}
				if errors.Is(err, errIntegrationWaitingOnDependency) || errors.Is(err, errIntegrationFreshReview) {
					// Two different reasons for the same shape of answer: this
					// task cannot integrate in THIS pass, nothing is wrong, and
					// nothing about it stops a sibling.
					//
					//   - waiting on a dependency: a task it requires has not
					//     landed yet, so it is early rather than blocked. The
					//     pass that follows that dependency's integration finds
					//     it ready.
					//   - waiting on a fresh review: its rebase changed what it
					//     contributes, so its child run has been re-opened for
					//     one more review cycle and is running again.
					//
					// `continue` rather than `return nil`, which is the whole
					// difference from the busy lane: a lane that is busy is busy
					// for every task of this run, and one task waiting on its own
					// dependency must not stop the pass that would integrate the
					// dependency itself. The task stays active and stays running.
					continue
				}
				if errors.Is(err, errIntegrationTaskConflict) {
					// The conflict belongs to THIS task. It has already been
					// recorded against it, with the files and all three SHAs,
					// and the integration lane is back. Every independent
					// sibling must still be allowed to run and to integrate —
					// parking the objective on one task's merge problem is the
					// opposite of what parallel execution is for. The objective
					// stops only when nothing is left that can move, which the
					// end of this pass decides on its own — so this pass's own
					// accounting has to know the task just stopped being
					// active, or the objective would read "a task is still
					// working" from a task that has in fact parked.
					delete(activeTasks, task.ID)
					parkedTasks[task.ID] = true
					continue
				}
				if run.State == domain.WorkflowRunRunning {
					_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, c.clock())
					run.State = domain.WorkflowRunNeedsAttention
				}
				// Recorded once per distinct failure, not once per poll: this
				// path runs on every GetRun, and an integration failure is
				// normally a permanent condition (Checkpoint 8P-E.13B).
				c.recordAttentionStopOnce(ctx, run, nil, masterIntegrationFailureDurablePhase,
					fmt.Sprintf("task %d (%s) passed review and verification but could not be integrated: %v", task.Ordinal, task.Title, err))
				return nil
			}
			// A promotion that succeeds after an earlier failure resolves the
			// stop it caused. Without this the master would stay parked in
			// needs_attention on a condition that no longer holds — and, because
			// an integration failure is a human-owned reason, with no heartbeat
			// left to dispatch the next task either.
			run = c.clearIntegrationStop(ctx, run)
			_, _ = c.planStore.UpdateWorkflowTaskState(ctx, task.ID, domain.WorkflowTaskRunning, domain.WorkflowTaskCompleted, c.clock())
			// The task is done, so its write set is no longer an estimate.
			// Replace it with what the child run actually wrote and re-classify
			// the remaining pairs against that evidence, so the tasks still to
			// be scheduled are ordered by a fact rather than by a guess. After
			// the state transition on purpose: refreshing a scope must never be
			// what decides whether a completed task counts as completed.
			c.recordObservedTaskWriteSet(ctx, *task, child.Run.ID)
			completed[task.ID] = true
			delete(activeTasks, task.ID)

		case domain.WorkflowRunFailed:
			_, _ = c.planStore.UpdateWorkflowTaskState(ctx, task.ID, domain.WorkflowTaskRunning, domain.WorkflowTaskFailed, c.clock())
			// The task's own terminal failure, reported once for the task and
			// then again — as the parent's stop — for the workflow it blocks.
			// This reconcile re-runs on every poll and after every restart; the
			// notification's dedupe key is what makes that one notification
			// rather than one per pass.
			c.notifyRunFailed(ctx, child.Run, fmt.Sprintf(
				"Task %d (%s) ended without completing.", task.Ordinal, task.Title))
			if run.State == domain.WorkflowRunRunning {
				_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, c.clock())
			}
			c.recordAttentionStop(ctx, run, nil, ReasonChildFailed,
				fmt.Sprintf("task %d (%s) failed — run %s", task.Ordinal, task.Title, child.Run.ID))
			return nil

		case domain.WorkflowRunCancelled:
			// A cancelled child is a decision already taken, not a new one to
			// ask about: mirror it onto the task and let the parent's own
			// completion accounting below decide what that means for the run.
			_, _ = c.planStore.UpdateWorkflowTaskState(ctx, task.ID, domain.WorkflowTaskRunning, domain.WorkflowTaskCancelled, c.clock())
			delete(activeTasks, task.ID)

		case domain.WorkflowRunNeedsAttention:
			// The task stays "running": its child is genuinely non-terminal and
			// may still resume (a self-remediable stop will; a human decision
			// may). What must not happen is the parent going silent, so mirror
			// the stop onto the parent only when the child's own stop is one AO
			// cannot handle itself — otherwise the parent stays running and its
			// heartbeat keeps driving the child forward.
			// Checkpoint 8P-E.13A.2: the test is "is this the user's problem?",
			// not "is it on a scheduled retry?". A child stopped for a reason AO
			// could not even name is not a decision anyone can be asked to make
			// (ClassifyAttention says the same), and mirroring it onto the
			// parent both showed the user an unactionable card and killed the
			// parent's heartbeat — the only thing left that could drive the
			// child out of that state.
			if !c.stopIsHumanOwned(ctx, child.Run) {
				return nil
			}
			if run.State == domain.WorkflowRunRunning {
				_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, c.clock())
			}
			// Once per distinct occurrence, not once per poll: this branch is
			// re-derived on every reconcile pass for as long as the child stays
			// stopped, and the parent keeps its heartbeat through the mirror
			// (see maybeScheduleAutonomousHeartbeat), so there are now many more
			// passes over the same unchanged condition than there used to be.
			c.recordAttentionStopOnce(ctx, run, nil, ReasonChildNeedsAttention,
				fmt.Sprintf("task %d (%s) stopped and needs a decision — run %s", task.Ordinal, task.Title, child.Run.ID))
			return nil
		}
	}
	if len(tasks) > 0 && len(completed) == len(tasks) {
		if run.State == domain.WorkflowRunRunning {
			if _, err := c.completeRun(ctx, run, run.State); err != nil {
				return err
			}
		}
		return nil
	}
	// Direct-branch mode intentionally retains its historical single-writer
	// dispatch. Worktree modes use the durable DAG/conflict snapshot below and
	// may fill every currently safe lane in one reconciliation pass.
	parallelWorktrees := c.projectExecutionMode(ctx, run.ProjectID).SmartParallel()
	if len(activeTasks) > 0 && !parallelWorktrees {
		return nil
	}
	graph := TaskGraphSnapshot{}
	if parallelWorktrees {
		var graphErr error
		graph, graphErr = c.LoadTaskGraph(ctx, run.ID)
		if graphErr != nil {
			return graphErr
		}
	}
	dispatched := false
	for i := range tasks {
		task := &tasks[i]
		// A parked task is skipped here as well as above. It is not terminal —
		// a person will very likely release it — but it is emphatically not
		// dispatchable, and the dispatch loop's own test ("not terminal, no
		// execution run yet") would otherwise send a task that has already run
		// and conflicted back to a worker.
		if task.State.Terminal() || task.State.Parked() || task.ExecutionRunID != nil {
			continue
		}
		eligible := true
		for _, d := range task.Dependencies {
			if !completed[d] {
				eligible = false
				break
			}
		}
		if !eligible {
			if err := c.persistTaskWaitingReason(ctx, task, domain.WorkflowTaskWaitingDependency); err != nil {
				return err
			}
			continue
		}
		if parallelWorktrees {
			conflict := false
			for _, other := range graph.ConflictsFor(task.ID) {
				if activeTasks[other] {
					conflict = true
					break
				}
			}
			if conflict {
				if err := c.persistTaskWaitingReason(ctx, task, domain.WorkflowTaskWaitingConflict); err != nil {
					return err
				}
				if task.State == domain.WorkflowTaskEligible {
					_, _ = c.planStore.UpdateWorkflowTaskState(ctx, task.ID, domain.WorkflowTaskEligible, domain.WorkflowTaskBlocked, c.clock())
					task.State = domain.WorkflowTaskBlocked
				}
				continue
			}
		}
		if err := c.persistTaskWaitingReason(ctx, task, ""); err != nil {
			return err
		}
		if task.State == domain.WorkflowTaskBlocked {
			_, _ = c.planStore.UpdateWorkflowTaskState(ctx, task.ID, domain.WorkflowTaskBlocked, domain.WorkflowTaskEligible, c.clock())
			task.State = domain.WorkflowTaskEligible
		}
		// Checkpoint 8K-A: never dispatch the next master task while the
		// parent run itself has an unresolved question open.
		if open, err := c.hasOpenQuestion(ctx, run.ID, nil); err != nil {
			return err
		} else if open {
			return nil
		}
		if err := c.dispatchMasterTask(ctx, run, *task); err != nil {
			return err
		}
		dispatched = true
		activeTasks[task.ID] = true
		if !parallelWorktrees {
			return nil
		}
	}
	if dispatched {
		return nil
	}

	// A parked task stops the OBJECTIVE only when it has stopped everything
	// else too.
	//
	// This is the second half of "a conflict belongs to the task": the master
	// keeps going while any sibling can still move, and reflects the stop only
	// once nothing in the DAG can. Reaching here already means nothing was
	// dispatched and the plan did not complete; the extra condition is that
	// nothing is in flight either, because a running sibling is progress and a
	// run that reported attention while a task was still working would be
	// asking a person to decide something that may resolve itself.
	if len(activeTasks) == 0 && len(parkedTasks) > 0 {
		if run.State == domain.WorkflowRunRunning {
			_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, c.clock())
			run.State = domain.WorkflowRunNeedsAttention
		}
		// Once per distinct occurrence, not once per poll: this condition is
		// re-derived on every reconcile pass for as long as the task stays
		// parked, and the parked task itself already carries the detail.
		c.recordAttentionStopOnce(ctx, run, nil, ReasonTaskParked,
			fmt.Sprintf("%s, and no other task can make progress until it is resolved",
				describeParkedTasks(tasks)))
		return nil
	}

	// Checkpoint 8P-E.13 Phase 6: nothing is running, nothing completed the
	// plan, and no task is eligible to dispatch. That is only reachable when a
	// task ended unsuccessfully and its dependents can therefore never unblock.
	// Saying so is the whole point — before this, the master run simply sat at
	// "running" with every remaining task "blocked" and no explanation.
	if blockedByUnsuccessfulTask(tasks) {
		if run.State == domain.WorkflowRunRunning {
			_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, c.clock())
		}
		c.recordAttentionStop(ctx, run, nil, ReasonChildFailed,
			"a task ended without completing, so the tasks depending on it can never become eligible")
	}
	return nil
}

func (c *Coordinator) persistTaskWaitingReason(ctx stdctx.Context, task *domain.WorkflowTask, reason domain.WorkflowTaskWaitingReason) error {
	scope, err := UnmarshalTaskScope(task.ScopeJSON)
	if err != nil {
		return err
	}
	if scope.WaitingReason == reason {
		return nil
	}
	scope.WaitingReason = reason
	raw, err := MarshalTaskScope(scope)
	if err != nil {
		return err
	}
	if _, err = c.planStore.UpdateWorkflowTaskScope(ctx, task.ID, raw, c.clock()); err != nil {
		return err
	}
	task.ScopeJSON = raw
	return nil
}

// describeParkedTasks names the parked tasks and what each is parked on, so the
// objective's stop points at the tasks a person has to act on instead of saying
// only that it stopped.
func describeParkedTasks(tasks []domain.WorkflowTask) string {
	var parts []string
	for _, t := range tasks {
		if !t.State.Parked() {
			continue
		}
		parts = append(parts, fmt.Sprintf("task %d (%s) is parked on %s", t.Ordinal, t.Title, t.AttentionReason))
	}
	if len(parts) == 0 {
		return "a task is parked"
	}
	return strings.Join(parts, "; ")
}

// blockedByUnsuccessfulTask reports whether this plan can no longer make
// progress on its own: at least one task ended unsuccessfully, and at least one
// task is still waiting for something that will never arrive.
func blockedByUnsuccessfulTask(tasks []domain.WorkflowTask) bool {
	unsuccessful, pending := false, false
	for _, t := range tasks {
		switch {
		case t.State == domain.WorkflowTaskFailed || t.State == domain.WorkflowTaskCancelled:
			unsuccessful = true
		case !t.State.Terminal():
			pending = true
		}
	}
	return unsuccessful && pending
}

func (c *Coordinator) dispatchMasterTask(ctx stdctx.Context, parent domain.WorkflowRun, task domain.WorkflowTask) error {
	if id, ok, err := c.planStore.FindWorkflowRunByPlannedTask(ctx, task.ID); err != nil {
		return err
	} else if ok {
		_, _ = c.planStore.SetWorkflowTaskExecutionRun(ctx, task.ID, id, c.clock())
		// Checkpoint 8P-C.1: re-entering this branch means either a normal
		// re-dispatch (StartRun is idempotent) or restart recovery after a
		// crash between the child's creation and its owner stamp below --
		// re-stamp unconditionally (idempotent) so a NULL-owned child from
		// that crash window is healed before StartRun is ever called again.
		if perr := c.stampChildOwnership(ctx, id, parent); perr != nil {
			return perr
		}
		if perr := c.requireChildOwnershipForDispatch(ctx, id, parent); perr != nil {
			return perr
		}
		_, err = c.StartRun(ctx, id)
		return err
	}
	var criteria []string
	var verify VerificationPlan
	_ = json.Unmarshal([]byte(task.AcceptanceCriteriaJSON), &criteria)
	_ = json.Unmarshal([]byte(task.VerifyJSON), &verify)
	objective := task.Title + "\n\n" + task.Description

	// Checkpoint 8M §13: task N+1 never inherits task N's session/
	// transcript — it gets a compact, fact-only recap of each already-
	// completed dependency task instead, threaded through the exact same
	// objective text that already flows into BuildPlanArtifact/
	// BuildWorkStepPrompt (no second prompt-assembly path).
	var depPack *domain.SessionContextPack
	if allTasks, tasksErr := c.planStore.ListWorkflowTasks(ctx, parent.ID); tasksErr == nil {
		if depBlock, pack, blockErr := c.priorTaskContextBlock(ctx, allTasks, task); blockErr == nil && depBlock != "" {
			objective = objective + "\n\n" + depBlock
			depPack = pack
		}
	}

	artifact := BuildPlanArtifact(parent.ProjectID, objective, policyVersionV1, verify)
	artifact.AcceptanceCriteria = criteria
	parentID, taskID := parent.ID, task.ID
	child, err := c.createSingleTaskRun(ctx, parent.ProjectID, objective, &parentID, &taskID, verify)
	if err != nil {
		return err
	}
	// Checkpoint 8P-C.1: stamp the child's durable owner from the parent's
	// own owner immediately after creation -- see stampChildOwnership's doc
	// comment for why a crash in the window between these two writes is
	// safely healed by the FindWorkflowRunByPlannedTask recovery branch
	// above, rather than needing a single atomic transaction.
	if perr := c.stampChildOwnership(ctx, child.Run.ID, parent); perr != nil {
		return perr
	}
	// Checkpoint 8P-C: a master task's child run inherits the SAME frozen
	// execution policy the parent objective was created with -- never
	// re-derived from the (possibly since-changed) live policy, matching
	// "no recalcules historia con policy futura" for Routing/Wake.
	if perr := c.inheritExecutionPolicySnapshot(ctx, child.Run.ID, parent); perr != nil {
		return perr
	}
	if len(task.Dependencies) > 0 {
		decision := domain.SessionLifecycleDecision{
			Action: domain.LifecycleNewSession, Role: domain.WorkflowRoleWorker,
			Reasons:       []domain.SessionLifecycleReason{domain.LifecycleReasonTaskBoundary},
			PolicyVersion: domain.SessionLifecyclePolicyVersion,
		}
		if depPack != nil {
			decision.ContextPackHash = depPack.ContentHash()
		}
		_ = c.persistSessionLifecycleDecision(ctx, child.Run, nil, decision, depPack)
	}
	// Replace the deterministic generic criteria with the planner's accepted criteria.
	for _, s := range child.Steps {
		if s.Step.Kind == domain.WorkflowStepPlan {
			raw, _ := MarshalPlanArtifact(artifact)
			_, _ = c.store.UpdateWorkflowStepArtifact(ctx, s.Step.ID, raw, c.clock())
		}
	}
	if _, err := c.planStore.SetWorkflowTaskExecutionRun(ctx, task.ID, child.Run.ID, c.clock()); err != nil {
		return err
	}
	if perr := c.requireChildOwnershipForDispatch(ctx, child.Run.ID, parent); perr != nil {
		return perr
	}
	_, err = c.StartRun(ctx, child.Run.ID)
	return err
}
