// Package workflow is the daemon's HTTP-facing workflow service boundary.
// The core coordinator lives in internal/workflow; this layer is the thin
// contract the API controller depends on.
package workflow

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ErrInvalid, ErrNotFound, and ErrAlreadyTerminal re-export the coordinator
// sentinels so the HTTP controller maps service failures to the right status
// without importing the core package.
var (
	ErrInvalid         = workflowcore.ErrInvalid
	ErrNotFound        = workflowcore.ErrNotFound
	ErrAlreadyTerminal = workflowcore.ErrAlreadyTerminal
	ErrPlanLocked      = workflowcore.ErrPlanLocked
	// ErrArchiveUnsupported surfaces a store that cannot archive.
	ErrArchiveUnsupported = workflowcore.ErrArchiveUnsupported
)

// ListFilter narrows ListRuns. An empty ProjectID means every project.
type ListFilter struct {
	ProjectID string
}

// RunSummary is a workflow run without its step/attempt fan-out, for list views.
type RunSummary = domain.WorkflowRun

// Manager is the workflows surface the HTTP controller depends on.
type Manager interface {
	CreateRun(ctx context.Context, projectID, objective string, verification workflowcore.VerificationPlan) (workflowcore.RunDetail, error)
	GetRun(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	ListRuns(ctx context.Context, filter ListFilter) ([]RunSummary, error)
	CancelRun(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	StartRun(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	// ContinueRun is Checkpoint 8C's generic unblock-and-dispatch entry
	// point. Today it dispatches the review step once the work step has
	// completed; a future checkpoint can extend it (fix/verify automation)
	// without a breaking API change.
	ContinueRun(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	// ResumeTask releases one task parked in needs_attention (migration 0130)
	// after a person has dealt with what parked it. Idempotent.
	ResumeTask(ctx context.Context, runID, taskID string) (workflowcore.RunDetail, error)
	// AuthorizeIntegrationFreshReviewException grants ONE additional
	// integration fresh review to a task whose ordinary budget is spent, on a
	// named person's explicit authority. See
	// workflow/task_integration_fresh_review_exception.go.
	AuthorizeIntegrationFreshReviewException(ctx context.Context, req workflowcore.IntegrationFreshReviewExceptionRequest) (workflowcore.IntegrationFreshReviewException, error)
	// AmendTaskCriterion is the Plan / Acceptance Criteria Amendment action
	// (migration 0132): a human-approved change to a criterion that stopped
	// describing reality, followed by a fresh independent review.
	AmendTaskCriterion(ctx context.Context, req TaskCriterionAmendment) (domain.WorkflowTaskCriterionAmendment, workflowcore.RunDetail, error)
	// ResumeAmendedTaskReview re-applies an existing amendment's consequences
	// when its fresh review never opened. It creates no second amendment.
	ResumeAmendedTaskReview(ctx context.Context, runID, taskID string) (workflowcore.RunDetail, error)
}

// PlannerManager is the optional capability a Manager may also implement when
// the deployment has a planner wired. Controllers type-assert for it, so a
// build without one degrades to an unsupported-operation response.
type PlannerManager interface {
	Manager
	CreateObjectiveRun(ctx context.Context, projectID, objective string, mode domain.WorkflowPlanApprovalMode) (workflowcore.RunDetail, error)
	GeneratePlan(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	ApprovePlan(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	RejectPlan(ctx context.Context, runID string) (workflowcore.RunDetail, error)
}

// OperatorRecoverer is the operator-only recovery surface: the two actions a
// person may take on a run AO has deliberately fail-closed on, and that nothing
// automatic is allowed to take on their behalf.
//
// It is an optional, type-asserted interface (mirroring ExecutionPolicyApplier)
// so a Manager implementation or test double that predates it keeps compiling
// unchanged, and a deployment without it answers 501 rather than doing
// something weaker.
//
// Both are bounded, both write their authorization durably before they move
// anything, and neither invents a fact AO could not prove:
//
//   - RecoverUnprovableApprovedHead discards an approval whose commit AO cannot
//     locate and asks for one fresh review of the live workspace. It never
//     attests a commit and never verifies unreviewed code.
//   - ReopenAmbiguousPlan returns an objective whose planner was interrupted by
//     a restart to ordinary, from-scratch planning. It adopts nothing from the
//     discarded planner.
type OperatorRecoverer interface {
	RecoverUnprovableApprovedHead(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	ReopenAmbiguousPlan(ctx context.Context, runID string, observedUpdatedAt time.Time) (workflowcore.RunDetail, error)
}

// ExecutionPolicyApplier is Checkpoint 8P-C's optional post-creation step:
// embed the caller's execution policy into a just-created run's policy
// snapshot. Optional (type-asserted by the controller, mirroring
// PlannerManager) so a Manager implementation/test double that predates
// 8P-C keeps compiling unchanged.
type ExecutionPolicyApplier interface {
	ApplyExecutionPolicySnapshot(ctx context.Context, runID string, userID domain.UserID, autonomousOverride *bool) error
}

// StrategyManager is P1-A's execution-strategy surface: creating a run under
// an explicitly chosen (or policy-selected) strategy, and reading back the
// strategy a run is durably executing under.
//
// Optional and type-asserted, exactly like PlannerManager/BoardReader, so a
// Manager implementation or test double that predates P1-A keeps compiling and
// simply degrades to the pre-P1-A create paths.
type StrategyManager interface {
	// CreateTaskRun creates a bounded TASK run: no objective planner, no
	// decomposition, ordinary review/verify.
	CreateTaskRun(ctx context.Context, req workflowcore.TaskRunRequest) (workflowcore.RunDetail, error)
	// CreateObjectiveRunWithStrategy creates an AUTONOMOUS or MASTER
	// objective, freezing the selection into the run at creation.
	CreateObjectiveRunWithStrategy(ctx context.Context, projectID, objective string, mode domain.WorkflowPlanApprovalMode, strategy domain.ExecutionStrategySelection) (workflowcore.RunDetail, error)
	// EffectiveStrategy answers "which strategy is this run using", mapping a
	// pre-P1-A run from its own durable facts rather than guessing.
	EffectiveStrategy(ctx context.Context, runID string) (domain.ExecutionStrategySelection, error)
}

// RecoveryManager is P1-B's recovery surface: assessing a stopped run,
// discharging its outstanding obligation, deciding what happens to its plan,
// and launching a bounded Repair Agent.
//
// Optional and type-asserted, exactly like PlannerManager/StrategyManager, so
// a Manager implementation or test double that predates P1-B keeps compiling
// and its deployment answers 501 rather than doing something weaker.
type RecoveryManager interface {
	// AssessRecovery is the deterministic "what should I do about this run".
	// It writes nothing.
	AssessRecovery(ctx context.Context, runID string) (workflowcore.RecoveryAssessment, error)
	// ResumeRun states the durable obligation and discharges only that one.
	ResumeRun(ctx context.Context, runID string) (workflowcore.RunDetail, workflowcore.ResumeReport, error)
	// ReusePlan executes an existing plan revision, refusing anything but an
	// exact match.
	ReusePlan(ctx context.Context, runID string) (workflowcore.RunDetail, workflowcore.PlanReuseAssessment, error)
	// RegeneratePlan mints a new durable plan revision.
	RegeneratePlan(ctx context.Context, runID string) (workflowcore.RunDetail, workflowcore.PlanReuseAssessment, error)
	// PlanRepair computes what a repair would do, without doing it.
	PlanRepair(ctx context.Context, runID string) (workflowcore.RepairPlan, error)
	// LaunchRepair creates the bounded repair run on a named authority.
	LaunchRepair(ctx context.Context, runID, authorizedBy string) (domain.RepairIntent, error)
	// ApplyRepairPolicy freezes a just-created run's auto-repair mode.
	ApplyRepairPolicy(ctx context.Context, runID string, mode domain.RepairMode) error
}

// BoardReader is Checkpoint 8P-E.12's project Board projection. Optional
// (type-asserted by the controller, mirroring PlannerManager) so a Manager
// implementation or test double that predates it keeps compiling unchanged.
type BoardReader interface {
	ProjectBoard(ctx context.Context, projectID string, retention time.Duration) ([]workflowcore.BoardEntry, error)
}

// RunArchiver is the cancel-and-archive surface. Optional (type-asserted by
// the controller, mirroring PlannerManager/BoardReader) so a Manager
// implementation or test double that predates archiving keeps compiling.
type RunArchiver interface {
	CancelAndArchiveRun(ctx context.Context, runID string) (workflowcore.RunDetail, error)
	ProjectBoardHistory(ctx context.Context, projectID string, limit int) ([]workflowcore.BoardEntry, error)
}

// BoardHistoryLimit caps one page of the archived view. Archiving is a manual,
// per-workflow action, so this is a sanity bound on the response size rather
// than a paging scheme.
const BoardHistoryLimit = 100

// BoardTerminalRetention is how long a finished run stays on the Board. A run
// that vanishes the instant it succeeds is indistinguishable from one that
// never ran, so completion is worth showing for a while.
const BoardTerminalRetention = 30 * time.Minute

// Service is the API-facing workflow service. It delegates to the core coordinator.
type Service struct {
	coordinator *workflowcore.Coordinator
}

var _ Manager = (*Service)(nil)

// New wraps a core workflow coordinator as the API-facing service.
func New(coordinator *workflowcore.Coordinator) *Service {
	return &Service{coordinator: coordinator}
}

// CreateRun creates a new workflow run and seeds its initial steps.
func (s *Service) CreateRun(ctx context.Context, projectID, objective string, verification workflowcore.VerificationPlan) (workflowcore.RunDetail, error) {
	return s.coordinator.CreateRun(ctx, projectID, objective, verification)
}

// CreateObjectiveRun creates a run for an objective under the given approval
// mode.
func (s *Service) CreateObjectiveRun(ctx context.Context, projectID, objective string, mode domain.WorkflowPlanApprovalMode) (workflowcore.RunDetail, error) {
	return s.coordinator.CreateObjectiveRun(ctx, projectID, objective, mode)
}

// ApplyExecutionPolicySnapshot implements ExecutionPolicyApplier.
func (s *Service) ApplyExecutionPolicySnapshot(ctx context.Context, runID string, userID domain.UserID, autonomousOverride *bool) error {
	return s.coordinator.ApplyExecutionPolicySnapshot(ctx, runID, userID, autonomousOverride)
}

// CreateTaskRun implements StrategyManager.
func (s *Service) CreateTaskRun(ctx context.Context, req workflowcore.TaskRunRequest) (workflowcore.RunDetail, error) {
	return s.coordinator.CreateTaskRun(ctx, req)
}

// PlacementManager is P1-D's observability surface: the frozen execution
// placement, the durable provider-attempt ledger, and the one admission answer
// to "why has this not launched".
//
// Optional and type-asserted, exactly like RecoveryManager above, so a Manager
// implementation or test double that predates P1-D keeps compiling and its
// deployment answers 501 rather than showing a placement it does not have.
//
// Read-only by construction. There is deliberately no method here that freezes,
// replaces or retires a placement: those happen on the dispatch path, where the
// generation and the capacity claim that authorize them exist. An API that
// could move a placement would be an API that could point a running agent at a
// different checkout.
type PlacementManager interface {
	// ListPlacements returns every placement recorded for a run, with the
	// current generation per obligation flagged.
	ListPlacements(ctx context.Context, runID string) ([]workflowcore.PlacementView, error)
	// ListProviderAttempts returns the run's provider-attempt chain.
	ListProviderAttempts(ctx context.Context, runID string) ([]workflowcore.ProviderAttemptView, error)
	// AdmissionState answers why the run has not launched.
	AdmissionState(ctx context.Context, runID string) (workflowcore.AdmissionStateView, error)
}

// CreateObjectiveRunWithStrategy implements StrategyManager.
func (s *Service) CreateObjectiveRunWithStrategy(ctx context.Context, projectID, objective string, mode domain.WorkflowPlanApprovalMode, strategy domain.ExecutionStrategySelection) (workflowcore.RunDetail, error) {
	return s.coordinator.CreateObjectiveRunWithStrategy(ctx, projectID, objective, mode, strategy)
}

// EffectiveStrategy implements StrategyManager.
func (s *Service) EffectiveStrategy(ctx context.Context, runID string) (domain.ExecutionStrategySelection, error) {
	return s.coordinator.EffectiveStrategy(ctx, runID)
}

// AssessRecovery implements RecoveryManager.
func (s *Service) AssessRecovery(ctx context.Context, runID string) (workflowcore.RecoveryAssessment, error) {
	return s.coordinator.AssessRecovery(ctx, runID)
}

// ResumeRun implements RecoveryManager.
func (s *Service) ResumeRun(ctx context.Context, runID string) (workflowcore.RunDetail, workflowcore.ResumeReport, error) {
	return s.coordinator.ResumeRun(ctx, runID)
}

// ReusePlan implements RecoveryManager.
func (s *Service) ReusePlan(ctx context.Context, runID string) (workflowcore.RunDetail, workflowcore.PlanReuseAssessment, error) {
	return s.coordinator.ReusePlan(ctx, runID)
}

// RegeneratePlan implements RecoveryManager.
func (s *Service) RegeneratePlan(ctx context.Context, runID string) (workflowcore.RunDetail, workflowcore.PlanReuseAssessment, error) {
	return s.coordinator.RegeneratePlan(ctx, runID)
}

// PlanRepair implements RecoveryManager.
func (s *Service) PlanRepair(ctx context.Context, runID string) (workflowcore.RepairPlan, error) {
	return s.coordinator.PlanRepair(ctx, runID)
}

// LaunchRepair implements RecoveryManager.
func (s *Service) LaunchRepair(ctx context.Context, runID, authorizedBy string) (domain.RepairIntent, error) {
	return s.coordinator.LaunchRepair(ctx, runID, authorizedBy)
}

// ApplyRepairPolicy implements RecoveryManager.
func (s *Service) ApplyRepairPolicy(ctx context.Context, runID string, mode domain.RepairMode) error {
	return s.coordinator.ApplyRepairPolicy(ctx, runID, mode)
}

// GeneratePlan invokes the planner for a run and persists the plan it returns.
func (s *Service) GeneratePlan(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.GeneratePlan(ctx, runID)
}

// ApprovePlan approves a run's validated plan so its tasks may be dispatched.
func (s *Service) ApprovePlan(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.ApprovePlan(ctx, runID)
}

// RejectPlan refuses a run's plan, ending it without dispatching any work.
func (s *Service) RejectPlan(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.RejectPlan(ctx, runID)
}

// GetRun returns one workflow run with its steps and attempts.
func (s *Service) GetRun(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.GetRun(ctx, runID)
}

// ListRuns lists workflow run summaries, optionally filtered by project id.
func (s *Service) ListRuns(ctx context.Context, filter ListFilter) ([]RunSummary, error) {
	return s.coordinator.ListRuns(ctx, filter.ProjectID)
}

// CancelRun cancels a workflow run and cascades cancellation to its non-terminal steps.
func (s *Service) CancelRun(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.CancelRun(ctx, runID)
}

// StartRun transitions a pending run to running, runs its plan step, and
// dispatches its work step's Codex worker (idempotently).
func (s *Service) StartRun(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.StartRun(ctx, runID)
}

// ProjectBoard implements BoardReader.
func (s *Service) ProjectBoard(ctx context.Context, projectID string, retention time.Duration) ([]workflowcore.BoardEntry, error) {
	return s.coordinator.ProjectBoard(ctx, projectID, retention)
}

// CancelAndArchiveRun implements RunArchiver: it cancels the run and its
// non-terminal children through the canonical cancellation lifecycle, then
// marks the run archived so the active Board stops showing it. Deletes nothing.
func (s *Service) CancelAndArchiveRun(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.CancelAndArchiveRun(ctx, runID)
}

// ProjectBoardHistory implements RunArchiver: the archived ("Mostrar
// archivados") projection of a project's workflows.
func (s *Service) ProjectBoardHistory(ctx context.Context, projectID string, limit int) ([]workflowcore.BoardEntry, error) {
	return s.coordinator.ProjectBoardHistory(ctx, projectID, limit)
}

// ContinueRun dispatches the review step's real Claude reviewer once the
// work step has completed (idempotently); a no-op otherwise.
func (s *Service) ContinueRun(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.ContinueRun(ctx, runID)
}

// RecoverUnprovableApprovedHead implements OperatorRecoverer: the explicit,
// human-only recovery for a run parked because AO cannot prove which commit its
// approved review target was read at.
func (s *Service) RecoverUnprovableApprovedHead(ctx context.Context, runID string) (workflowcore.RunDetail, error) {
	return s.coordinator.RecoverUnprovableApprovedHead(ctx, runID)
}

// ReopenAmbiguousPlan implements OperatorRecoverer: CP7's human-only, bounded,
// observed-version reopen of an objective whose planner was in flight across a
// daemon restart. observedUpdatedAt is the plan row version the caller's own
// view was rendered from, and a stale one is refused rather than accepted.
func (s *Service) ReopenAmbiguousPlan(ctx context.Context, runID string, observedUpdatedAt time.Time) (workflowcore.RunDetail, error) {
	return s.coordinator.ReopenAmbiguousPlan(ctx, runID, observedUpdatedAt)
}

// TaskCriterionAmendment is one human-approved amendment, at the service
// boundary. It mirrors workflowcore.TaskCriterionAmendmentRequest rather than
// importing that vocabulary into the transport layer.
type TaskCriterionAmendment struct {
	RunID             string
	TaskID            string
	CriterionIndex    int
	OriginalCriterion string
	AmendedCriterion  string
	Reason            string
	Evidence          []string
	ApprovedBy        string
}

// AmendTaskCriterion records the amendment, applies it, and returns the run
// with its review re-opened. It never approves the work.
func (s *Service) AmendTaskCriterion(ctx context.Context, req TaskCriterionAmendment) (domain.WorkflowTaskCriterionAmendment, workflowcore.RunDetail, error) {
	amendment, err := s.coordinator.AmendTaskAcceptanceCriterion(ctx, workflowcore.TaskCriterionAmendmentRequest{
		RunID: req.RunID, TaskID: req.TaskID, CriterionIndex: req.CriterionIndex,
		OriginalCriterion: req.OriginalCriterion, AmendedCriterion: req.AmendedCriterion,
		Reason: req.Reason, Evidence: req.Evidence, ApprovedBy: req.ApprovedBy,
	})
	if err != nil {
		return domain.WorkflowTaskCriterionAmendment{}, workflowcore.RunDetail{}, err
	}
	detail, err := s.coordinator.GetRun(ctx, req.RunID)
	return amendment, detail, err
}

// ResumeAmendedTaskReview finishes an amendment whose fresh review never got
// opened. Idempotent: a task already moving is left alone.
func (s *Service) ResumeAmendedTaskReview(ctx context.Context, runID, taskID string) (workflowcore.RunDetail, error) {
	if err := s.coordinator.ResumeAmendedTaskReview(ctx, runID, taskID); err != nil {
		return workflowcore.RunDetail{}, err
	}
	return s.coordinator.GetRun(ctx, runID)
}

// AuthorizeIntegrationFreshReviewException grants one additional integration
// fresh review for a parked task, on a named person's authority. The bound
// itself is unchanged; this widens it for exactly this task and exactly this
// workspace state.
func (s *Service) AuthorizeIntegrationFreshReviewException(ctx context.Context, req workflowcore.IntegrationFreshReviewExceptionRequest) (workflowcore.IntegrationFreshReviewException, error) {
	return s.coordinator.AuthorizeIntegrationFreshReviewException(ctx, req)
}

// ResumeTask releases one task parked in needs_attention after a person has
// dealt with what parked it, and returns the run as it then is.
//
// Idempotent: resuming a task that is not parked changes nothing and is not an
// error, so a repeated request cannot produce a second integration attempt.
func (s *Service) ResumeTask(ctx context.Context, runID, taskID string) (workflowcore.RunDetail, error) {
	if err := s.coordinator.ResumeTaskAfterAttention(ctx, runID, taskID); err != nil {
		return workflowcore.RunDetail{}, err
	}
	return s.coordinator.GetRun(ctx, runID)
}

// ListPlacements implements PlacementManager.
func (s *Service) ListPlacements(ctx context.Context, runID string) ([]workflowcore.PlacementView, error) {
	return s.coordinator.ListPlacements(ctx, runID)
}

// ListProviderAttempts implements PlacementManager.
func (s *Service) ListProviderAttempts(ctx context.Context, runID string) ([]workflowcore.ProviderAttemptView, error) {
	return s.coordinator.ListProviderAttempts(ctx, runID)
}

// AdmissionState implements PlacementManager.
func (s *Service) AdmissionState(ctx context.Context, runID string) (workflowcore.AdmissionStateView, error) {
	return s.coordinator.AdmissionState(ctx, runID)
}
