package workflow

import (
	"context"

	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// incident.go — Checkpoint 8P-E.18's service surface for the Incident Advisor.
//
// It is an OPTIONAL capability (type-asserted by the controller, mirroring
// PlannerManager and ExecutionPolicyApplier) rather than an addition to
// Manager, so every existing Manager implementation and test double keeps
// compiling unchanged and a deployment without an incident agent launcher
// degrades to "the endpoints answer 501" instead of failing to build.
//
// Every method here is a thin pass-through. That is deliberate: the decisions —
// what evidence travels, what an agent is allowed to propose, what needs an
// approval, what is stale — all live in internal/workflow, next to the durable
// state they are derived from. A service layer that re-implemented any of them
// would be a second place for the rules to disagree.

// IncidentAdvisor is the optional service capability a Manager may also
// implement to expose the Incident Advisor. Controllers type-assert for it and
// answer 501 when it is absent; see the file comment above for why it is
// optional and why every method is a pass-through.
type IncidentAdvisor interface {
	// OpenIncident derives the run's current stop and returns the incident for
	// it, recording one if this stop has not been seen before.
	OpenIncident(ctx context.Context, runID string) (workflowcore.Incident, error)
	// IncidentPackFor returns the incident together with the bounded evidence
	// pack a diagnosis would be taken against.
	IncidentPackFor(ctx context.Context, runID string) (workflowcore.Incident, workflowcore.IncidentContextPack, error)
	// RequestIncidentDiagnosis launches the isolated Diagnostic Agent.
	RequestIncidentDiagnosis(ctx context.Context, runID string) (workflowcore.Incident, workflowcore.IncidentContextPack, error)
	// SubmitIncidentDiagnosis records an agent's validated answer. It cannot
	// execute anything.
	SubmitIncidentDiagnosis(ctx context.Context, runID string, sub workflowcore.IncidentDiagnosisSubmission) (workflowcore.Incident, error)
	// ExecuteIncidentAction authorizes and carries out the proposed action.
	// approvedBy is empty only for an action AO may take by itself.
	ExecuteIncidentAction(ctx context.Context, runID, incidentID, approvedBy string) (workflowcore.Incident, error)
	// LoadIncident folds one incident, marking it stale when the run's current
	// stop no longer matches it.
	LoadIncident(ctx context.Context, runID, incidentID string) (workflowcore.Incident, error)
	// DeriveIncidentStatus projects an incident onto the modal's vocabulary.
	DeriveIncidentStatus(ctx context.Context, inc workflowcore.Incident) workflowcore.IncidentStatus
	// RepairOriginFor reports whether a run is an Incident Advisor repair.
	RepairOriginFor(ctx context.Context, runID string) (workflowcore.IncidentRepairOrigin, bool)
}

// OpenIncident derives the run's current incident. Pass-through; see the file
// comment for why the logic lives in internal/workflow.
func (s *Service) OpenIncident(ctx context.Context, runID string) (workflowcore.Incident, error) {
	return s.coordinator.OpenIncident(ctx, runID)
}

// IncidentPackFor returns the run's incident together with the evidence pack
// assembled for it.
func (s *Service) IncidentPackFor(ctx context.Context, runID string) (workflowcore.Incident, workflowcore.IncidentContextPack, error) {
	return s.coordinator.IncidentPackFor(ctx, runID)
}

// RequestIncidentDiagnosis asks the incident agent to diagnose the run and
// returns the incident with the pack the request was built from.
func (s *Service) RequestIncidentDiagnosis(ctx context.Context, runID string) (workflowcore.Incident, workflowcore.IncidentContextPack, error) {
	return s.coordinator.RequestIncidentDiagnosis(ctx, runID)
}

// SubmitIncidentDiagnosis records an agent's diagnosis against the run's
// incident.
func (s *Service) SubmitIncidentDiagnosis(ctx context.Context, runID string, sub workflowcore.IncidentDiagnosisSubmission) (workflowcore.Incident, error) {
	return s.coordinator.SubmitIncidentDiagnosis(ctx, runID, sub)
}

// ExecuteIncidentAction runs the incident's proposed action under the named
// approver.
func (s *Service) ExecuteIncidentAction(ctx context.Context, runID, incidentID, approvedBy string) (workflowcore.Incident, error) {
	return s.coordinator.ExecuteIncidentAction(ctx, runID, incidentID, approvedBy)
}

// LoadIncident reads one incident of a run by id.
func (s *Service) LoadIncident(ctx context.Context, runID, incidentID string) (workflowcore.Incident, error) {
	return s.coordinator.LoadIncident(ctx, runID, incidentID)
}

// DeriveIncidentStatus computes an incident's current status from the durable
// state, rather than reading a stored field that could be stale.
func (s *Service) DeriveIncidentStatus(ctx context.Context, inc workflowcore.Incident) workflowcore.IncidentStatus {
	return s.coordinator.DeriveIncidentStatus(ctx, inc)
}

// RepairOriginFor returns the origin a repair run was launched from, when the
// run is one.
func (s *Service) RepairOriginFor(ctx context.Context, runID string) (workflowcore.IncidentRepairOrigin, bool) {
	return s.coordinator.RepairOriginFor(ctx, runID)
}
