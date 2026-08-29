package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// plan_reuse.go — P1-B §C/§D.
//
// A run that stopped after planning already holds a perfectly good plan. Until
// now the only way back was to plan again, which spent the planner, produced a
// different decomposition, and orphaned whatever the first one had already
// executed. The plan was reusable; nothing could say so.
//
// What "reusable" has to mean, though, is narrow. Two separate things must
// both hold, and they are checked separately because they fail separately:
//
//   - IDENTITY. The plan on disk must be the plan its hash says it is. If the
//     bytes and the hash disagree, AO does not know what it is holding and
//     reuse is refused outright -- there is no revalidation that fixes not
//     knowing.
//   - COMPATIBILITY. The project context the plan was generated against must
//     still describe the project. If it has moved, the plan may well still be
//     right, but AO cannot say so, and a stale plan must never silently run.
//     That is `stale_but_revalidatable`: a real plan, and an explicit decision
//     owed before any of it executes.
//
// Reuse never touches review or verification. It decides which plan executes,
// and every task it dispatches goes through the identical review/fix/verify
// path it would have gone through the first time.

// PlanReuseAssessment is the full answer about one run's plan.
type PlanReuseAssessment struct {
	Reusability domain.PlanReusability
	// Revision is the plan's current durable revision.
	Revision int64
	// PlanHash is the plan's recorded structural identity, when it has one.
	PlanHash string
	// TaskCount is how many tasks the current revision holds.
	TaskCount int
	// Reason is AO's own sentence about the classification.
	Reason string
	// ContextDrift names the manifest comparison's verdict for a stale plan.
	ContextDrift string
}

// maxPlanRevisions bounds how many times ONE objective may be re-planned.
//
// It is the same argument maxAmbiguousPlanReopens makes: a plan that was wrong
// is worth replacing, a second replacement is worth having, and an objective
// on its fourth decomposition has a problem no further decomposition will
// find. Without a bound, "regenerate" is an unbounded planner-spend loop that
// a poll or an impatient operator can drive forever.
const maxPlanRevisions = 3

// planRegeneratedPhase records one deliberate regeneration, with the
// superseded revision's identity, BEFORE the plan row moves. Ordered
// reason-first for the same reason CP30/CP31/CP32 are: the explanation must
// survive a crash that loses the state change, never the other way round.
const planRegeneratedPhase = "plan_regenerated"

// planReusedPhase records a deliberate reuse of an existing plan revision.
const planReusedPhase = "plan_reused"

// planRevisionRegenerator is the optional store capability regeneration needs,
// asserted at the call site (mirroring ambiguousPlanReopener) so a store or
// test double without it refuses with a readable error rather than failing to
// compile.
type planRevisionRegenerator interface {
	RegenerateWorkflowPlan(ctx stdctx.Context, runID string, expectedRevision int64, now time.Time) (bool, error)
}

// assessPlanReuse classifies a run's durable plan. It reads only; the
// classification is re-derived on demand precisely so it can never be stale.
func (c *Coordinator) assessPlanReuse(ctx stdctx.Context, d RunDetail, strategy domain.ExecutionStrategySelection) PlanReuseAssessment {
	if d.Plan == nil || !strategy.Effective.Planned() {
		return PlanReuseAssessment{Reusability: domain.PlanReuseNotApplicable, Revision: 1,
			Reason: "This run has no plan of its own, so there is nothing to reuse."}
	}
	record := *d.Plan
	out := PlanReuseAssessment{
		Revision:  planRevisionOf(record),
		PlanHash:  record.PlanHash,
		TaskCount: len(d.Tasks),
	}

	switch record.Status {
	case domain.WorkflowPlanValidated, domain.WorkflowPlanApproved:
	default:
		out.Reusability = domain.PlanReuseNotReusable
		out.Reason = fmt.Sprintf("This objective's plan is %s; there is no accepted plan to reuse.", record.Status)
		return out
	}

	// Identity. Recomputing the hash from the stored bytes is the only way to
	// know the row still describes the plan it claims to. A mismatch is not a
	// staleness problem and must not be offered as one.
	var generated MasterPlan
	if err := json.Unmarshal([]byte(record.GeneratedPlanJSON), &generated); err != nil {
		out.Reusability = domain.PlanReuseNotReusable
		out.Reason = "AO cannot read this objective's stored plan, so it cannot prove what would be reused."
		return out
	}
	_, validation, hash := NormalizeAndValidatePlan(generated, d.Run.Objective, MaxPlanSteps)
	if !validation.Valid {
		out.Reusability = domain.PlanReuseNotReusable
		out.Reason = "The stored plan no longer passes AO's plan policy, so it cannot be reused as it stands."
		return out
	}
	if record.PlanHash != "" && hash != record.PlanHash {
		out.Reusability = domain.PlanReuseNotReusable
		out.Reason = "The stored plan does not match the hash it was approved under, so AO cannot prove which plan reuse would execute."
		return out
	}
	if len(d.Tasks) == 0 {
		out.Reusability = domain.PlanReuseNotReusable
		out.Reason = "This objective's plan has no tasks at its current revision, so there is nothing to execute."
		return out
	}

	// Compatibility. The manifest is content-free (paths and hashes, never
	// bodies -- see GeneratePlan), so comparing it says whether the project
	// AO would plan against today is the project it planned against then,
	// without re-reading a single document body.
	drift, known := c.plannerContextDrift(ctx, d.Run, record)
	if !known {
		// §M: reconstruct only from durable proof. AO cannot show the context
		// still holds, so it does not claim it does -- and it does not throw
		// the plan away either. A person decides.
		out.Reusability = domain.PlanReuseStaleRevalidatable
		out.ContextDrift = "unverifiable"
		out.Reason = "A validated plan exists, but AO cannot re-derive the project context it was generated from, so it cannot prove the plan still applies."
		return out
	}
	if drift {
		out.Reusability = domain.PlanReuseStaleRevalidatable
		out.ContextDrift = "context_changed"
		out.Reason = "A validated plan exists, but the project context it was generated from has changed since. Revalidate it or regenerate the plan before any of it executes."
		return out
	}
	out.Reusability = domain.PlanReuseExact
	out.Reason = "The stored plan matches its recorded identity and the project context it was generated from. It can be executed as it stands."
	return out
}

// plannerContextDrift reports whether the project's planner context has moved
// since the plan was generated, and whether that question could be answered at
// all. It compares the CONTENT-FREE manifest, exactly the artifact
// GeneratePlan already persists for this purpose.
func (c *Coordinator) plannerContextDrift(ctx stdctx.Context, run domain.WorkflowRun, record domain.WorkflowPlanRecord) (drift, known bool) {
	if c.projects == nil || c.plannerContextBuilder == nil {
		return false, false
	}
	if record.ContextManifestJSON == "" || record.ContextManifestJSON == "{}" {
		// Nothing was recorded to compare against -- a plan generated before
		// the manifest existed, or one whose planner never started.
		return false, false
	}
	project, found, err := c.projects.GetProject(ctx, run.ProjectID)
	if err != nil || !found {
		return false, false
	}
	current, err := c.plannerContextBuilder.Build(ctx, project)
	if err != nil {
		return false, false
	}
	manifest := current
	manifest.Documents = make([]PlannerDocument, len(current.Documents))
	for i, doc := range current.Documents {
		doc.Content = ""
		manifest.Documents[i] = doc
	}
	currentJSON, err := json.Marshal(manifest)
	if err != nil {
		return false, false
	}
	return string(currentJSON) != record.ContextManifestJSON, true
}

// ReusePlan is the explicit "keep this plan" operation.
//
// It refuses anything but an `exact` classification, and that refusal is the
// whole feature: a stale plan that quietly executed is precisely the outcome
// §C forbids, and there is no flag here to force one.
//
// Its effect is deliberately small. It records the reuse on the ledger and
// re-enters the ordinary resume path, which dispatches the existing plan's
// existing tasks under their existing identities. It approves nothing that was
// not already approved, it re-plans nothing, and it creates no task rows -- so
// a repeated call converges on the same state rather than compounding.
func (c *Coordinator) ReusePlan(ctx stdctx.Context, runID string) (RunDetail, PlanReuseAssessment, error) {
	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		return RunDetail{}, PlanReuseAssessment{}, err
	}
	if detail.Run.State.Terminal() {
		return RunDetail{}, PlanReuseAssessment{}, fmt.Errorf("%w: workflow run %q is already %s", ErrAlreadyTerminal, runID, detail.Run.State)
	}
	strategy := c.strategyForRun(ctx, detail.Run)
	assessment := c.assessPlanReuse(ctx, detail, strategy)
	if assessment.Reusability != domain.PlanReuseExact {
		return detail, assessment, fmt.Errorf("%w: this objective's plan is %s and cannot be reused: %s",
			ErrInvalid, assessment.Reusability, assessment.Reason)
	}

	c.recordPlanRevisionEvent(ctx, detail.Run, planReusedPhase, assessment.Revision,
		fmt.Sprintf("plan revision %d reused as-is (hash %s, %d tasks)", assessment.Revision, assessment.PlanHash, assessment.TaskCount))

	// An approved plan just needs driving. A validated one still needs the
	// approval its own mode governs -- reuse is not a way around it.
	if detail.Plan.Status == domain.WorkflowPlanValidated {
		if detail.Plan.ApprovalMode == domain.WorkflowPlanApprovalAuto {
			if _, aerr := c.ApprovePlan(ctx, runID); aerr != nil {
				return detail, assessment, aerr
			}
		} else {
			refreshed, rerr := c.GetRun(ctx, runID)
			if rerr != nil {
				return detail, assessment, rerr
			}
			return refreshed, assessment, nil
		}
	}
	resumed, cerr := c.ContinueRun(ctx, runID)
	if cerr != nil {
		return detail, assessment, cerr
	}
	return resumed, assessment, nil
}

// RegeneratePlan mints a new plan revision for an objective whose plan cannot
// be reused.
//
// Four properties, each of which §D asks for by name:
//
//   - The old plan stays auditable. Its identity, hash and task count are
//     written to the ledger BEFORE the row moves, and its task rows are never
//     deleted -- they remain readable through ListWorkflowTasksAtRevision.
//   - The new plan gets a new durable identity. Every task it mints is
//     namespaced to the new revision, so it can never collide with, or be
//     mistaken for, a superseded one.
//   - Stale tasks cannot become authoritative. ListWorkflowTasks is scoped to
//     the plan's current revision, so a child bound to a superseded task is
//     structurally invisible to reconciliation.
//   - Repeated requests are bounded and idempotent. The store write is a
//     compare-and-set on the revision the caller observed, and the total
//     number of revisions is capped at maxPlanRevisions.
//
// It is operator-initiated only. Nothing in reconcileRun, no wake reason and
// no heartbeat reaches it, for exactly the reason ReopenAmbiguousPlan is
// human-only: restart -> regenerate -> plan -> restart is an unbounded
// provider spend with nobody watching.
func (c *Coordinator) RegeneratePlan(ctx stdctx.Context, runID string) (RunDetail, PlanReuseAssessment, error) {
	if c.planStore == nil {
		return RunDetail{}, PlanReuseAssessment{}, fmt.Errorf("%w: planner is unavailable", ErrInvalid)
	}
	regenerator, ok := c.planStore.(planRevisionRegenerator)
	if !ok {
		return RunDetail{}, PlanReuseAssessment{}, fmt.Errorf("%w: this store cannot regenerate a plan revision", ErrInvalid)
	}
	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		return RunDetail{}, PlanReuseAssessment{}, err
	}
	if detail.Run.State.Terminal() {
		return RunDetail{}, PlanReuseAssessment{}, fmt.Errorf("%w: workflow run %q is already %s", ErrAlreadyTerminal, runID, detail.Run.State)
	}
	if detail.Plan == nil {
		return RunDetail{}, PlanReuseAssessment{}, fmt.Errorf("%w: workflow run %q is not a planned objective", ErrInvalid, runID)
	}
	strategy := c.strategyForRun(ctx, detail.Run)
	if !strategy.Effective.Planned() {
		return RunDetail{}, PlanReuseAssessment{}, fmt.Errorf("%w: a %q run has no plan to regenerate", ErrInvalid, strategy.Effective)
	}
	assessment := c.assessPlanReuse(ctx, detail, strategy)
	observed := planRevisionOf(*detail.Plan)
	if observed >= maxPlanRevisions {
		return detail, assessment, fmt.Errorf(
			"%w: this objective has already been planned %d times; a further regeneration will not discover anything the last one did not. Read the plan, narrow the objective, or start a fresh run",
			ErrInvalid, observed)
	}

	// Reason first: the superseded revision is on the ledger before the row
	// that describes it changes, so a crash between the two loses the state
	// change and not the explanation.
	c.recordPlanRevisionEvent(ctx, detail.Run, planRegeneratedPhase, observed,
		fmt.Sprintf("plan revision %d superseded (status %s, hash %s, %d tasks, reuse %s): %s",
			observed, detail.Plan.Status, detail.Plan.PlanHash, len(detail.Tasks),
			assessment.Reusability, assessment.Reason))

	moved, err := regenerator.RegenerateWorkflowPlan(ctx, runID, observed, c.clock())
	if err != nil {
		return detail, assessment, err
	}
	if !moved {
		// Somebody else regenerated from the same view, or the plan is in a
		// status with nothing to regenerate. Either way this call must not
		// mint a second revision: report the state as it now is.
		refreshed, rerr := c.GetRun(ctx, runID)
		if rerr != nil {
			return detail, assessment, rerr
		}
		return refreshed, c.assessPlanReuse(ctx, refreshed, strategy),
			fmt.Errorf("%w: this objective's plan moved since you read it (revision %d); re-read it and decide again", ErrPlanLocked, observed)
	}
	refreshed, err := c.GetRun(ctx, runID)
	if err != nil {
		return detail, assessment, err
	}
	return refreshed, c.assessPlanReuse(ctx, refreshed, strategy), nil
}

// recordPlanRevisionEvent appends one plan-revision fact to the run's ledger.
func (c *Coordinator) recordPlanRevisionEvent(ctx stdctx.Context, run domain.WorkflowRun, phase string, revision int64, detail string) {
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		ProjectID:      run.ProjectID,
		DurablePhase:   phase,
		NextAction:     detail,
		PayloadVersion: "v1",
		RetryState:     fmt.Sprintf(`{"planRevision":%d}`, revision),
		CreatedAt:      c.clock(),
	})
}
