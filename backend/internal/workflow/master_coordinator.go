package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
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
	runtimeEnv, _, err := c.resolveRuntimeEnv(ctx, run.ID, domain.HarnessClaudeCode)
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
		class := "planner_start_failed"
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "timeout") {
			class = "planner_timeout"
		} else if strings.Contains(lower, "parse") {
			class = "planner_parse_failed"
		}
		return c.failPlan(ctx, run, class, err)
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
	if err := c.planStore.InsertWorkflowTasks(ctx, tasks); err != nil {
		return RunDetail{}, err
	}
	if _, err := c.planStore.FinishWorkflowPlan(ctx, run.ID, domain.WorkflowPlanValidated, domain.WorkflowPlanCommandCompleted, string(validationJSON), hash, "", now); err != nil {
		return RunDetail{}, err
	}
	if record.ApprovalMode == domain.WorkflowPlanApprovalAuto {
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

func (c *Coordinator) failPlan(ctx stdctx.Context, run domain.WorkflowRun, class string, cause error) (RunDetail, error) {
	validation, _ := json.Marshal(PlanValidation{Valid: false, Errors: []string{cause.Error()}})
	_, _ = c.planStore.FinishWorkflowPlan(ctx, run.ID, domain.WorkflowPlanInvalid, domain.WorkflowPlanCommandFailed, string(validation), "", class, c.clock())
	if run.State == domain.WorkflowRunPending {
		_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, c.clock())
	}
	c.stopPlanningStep(ctx, run.ID)
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
	if c.wakeScheduler != nil {
		if next, werr := c.wakeScheduler.NextForRun(ctx, domain.WorkflowRunID(run.ID)); werr == nil && next != nil {
			at := next.ScheduledAt
			detail.NextWakeAt = &at
			detail.WaitReason = string(next.Reason)
		}
	}
	return detail, nil
}

func (c *Coordinator) reconcileMasterTasks(ctx stdctx.Context, run domain.WorkflowRun) error {
	tasks, err := c.planStore.ListWorkflowTasks(ctx, run.ID)
	if err != nil {
		return err
	}
	completed := map[string]bool{}
	active := false
	for i := range tasks {
		task := &tasks[i]
		if task.State == domain.WorkflowTaskCompleted {
			completed[task.ID] = true
			continue
		}
		if task.State != domain.WorkflowTaskRunning {
			continue
		}
		active = true
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
		if child.Run.State == domain.WorkflowRunRunning || child.Run.State == domain.WorkflowRunWaiting {
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
		if child.Run.State == domain.WorkflowRunCompleted {
			// Checkpoint 8M.1: materialize this task's verified worktree
			// content into the master run's integration ref BEFORE marking
			// it completed, so a crash between the two never lets a task
			// count as "done" without its code actually being propagated.
			if err := c.promoteTaskToIntegration(ctx, run, *task, child); err != nil {
				if run.State == domain.WorkflowRunRunning {
					_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, c.clock())
				}
				return nil
			}
			_, _ = c.planStore.UpdateWorkflowTaskState(ctx, task.ID, domain.WorkflowTaskRunning, domain.WorkflowTaskCompleted, c.clock())
			completed[task.ID] = true
			active = false
		} else if child.Run.State == domain.WorkflowRunNeedsAttention || child.Run.State == domain.WorkflowRunFailed {
			if run.State == domain.WorkflowRunRunning {
				_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, c.clock())
			}
			return nil
		}
	}
	if len(tasks) > 0 && len(completed) == len(tasks) {
		if run.State == domain.WorkflowRunRunning {
			_, _ = c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunCompleted, c.clock())
		}
		return nil
	}
	if active {
		return nil
	}
	for i := range tasks {
		task := &tasks[i]
		if task.State == domain.WorkflowTaskCompleted || task.State == domain.WorkflowTaskCancelled || task.ExecutionRunID != nil {
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
			continue
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
		return c.dispatchMasterTask(ctx, run, *task)
	}
	return nil
}

func (c *Coordinator) dispatchMasterTask(ctx stdctx.Context, parent domain.WorkflowRun, task domain.WorkflowTask) error {
	if id, ok, err := c.planStore.FindWorkflowRunByPlannedTask(ctx, task.ID); err != nil {
		return err
	} else if ok {
		_, _ = c.planStore.SetWorkflowTaskExecutionRun(ctx, task.ID, id, c.clock())
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
	_, err = c.StartRun(ctx, child.Run.ID)
	return err
}
