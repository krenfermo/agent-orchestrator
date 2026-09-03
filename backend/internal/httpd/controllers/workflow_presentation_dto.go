package controllers

import (
	"context"
	"time"

	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// workflow_presentation_dto.go — P3-A §24: the compact presentation projection.
//
// The daemon already owned every fact this carries. What it did not own was the
// PRESENTATION of them, so each surface re-derived one and the three could
// disagree — a run reading "Reviewing" on the board, "needs_attention" on its
// own page, and a bare step kind on its task card. This view is that derivation
// crossing the wire once.
//
// It is deliberately not a second lifecycle. Nothing here is stored, nothing
// here is authoritative over the durable fields it sits beside, and every
// technical value it replaces at the top of a screen is still present under
// `technical`. The frontend renders backend semantics; it does not compute
// them, and in particular it runs no git logic of its own.

// WorkflowPresentationView is the human status projection of one run.
type WorkflowPresentationView struct {
	// Stage is the human status vocabulary. It is a projection of `phase`, and
	// the mapping lives in the daemon (workflow.StageForPhase) so two surfaces
	// cannot disagree about it.
	Stage string `json:"stage" enum:"preparing,planning,working,reviewing,correcting,verifying,integrating,waiting,needs_attention,completed,cancelled,failed"`
	// RequiresHuman is the single flag a surface uses to decide whether to
	// interrupt somebody. It is never the run state: a run durably parked in
	// needs_attention that AO is still handling itself reports false.
	RequiresHuman bool `json:"requiresHuman"`
	// AutomaticActionActive reports AO is acting on a problem right now — a
	// repair in flight, a scheduled retry, an admission wait that clears by
	// itself. While it is true the UI must offer no duplicate remedy.
	AutomaticActionActive bool `json:"automaticActionActive"`
	// SummaryCode is the stable key the UI renders its sentence from: the
	// canonical attention reason for a stop, the wait reason for a wait, the
	// stage otherwise. A code the UI has no copy for still names something
	// real, and `technical.attentionDetail` is the daemon's own fallback prose.
	SummaryCode string `json:"summaryCode"`
	// RecommendedAction is the one thing AO suggests, empty when the honest
	// answer is that nothing is required of anyone.
	RecommendedAction string                         `json:"recommendedAction,omitempty"`
	Actions           []WorkflowPresentationAction   `json:"actions,omitempty"`
	Progress          []WorkflowPresentationStage    `json:"progress,omitempty"`
	Placement         *WorkflowPresentationPlacement `json:"placement,omitempty"`
	Timeline          []WorkflowPresentationEvent    `json:"timeline,omitempty"`
	Technical         WorkflowPresentationTechnical  `json:"technical"`
}

// WorkflowPresentationAction is one offer and its own answer to "may I press
// this". A disabled action is present WITH its reason rather than absent: "why
// is this greyed out" is answerable, "where did the button go" is not.
type WorkflowPresentationAction struct {
	ID      string `json:"id" enum:"continue,cancel,repair,commit_and_continue,view_changes,view_blocking_workflow,wait,authenticate,revalidate_plan,regenerate_plan,open_session,integrate,use_isolated_worktree"`
	Primary bool   `json:"primary,omitempty"`
	Enabled bool   `json:"enabled"`
	// DisabledReason is a stable code, not prose: repair_active,
	// repair_exhausted, not_recoverable, placement_explicit.
	DisabledReason string `json:"disabledReason,omitempty"`
}

// WorkflowPresentationStage is one entry of the visible progression.
//
// There is no percentage anywhere in this view, on purpose: a bounded set of
// stages with a known current one is a fact, and a number derived from it would
// be a fabrication.
type WorkflowPresentationStage struct {
	Stage string `json:"stage" enum:"preparing,planning,working,reviewing,correcting,verifying,integrating,waiting,needs_attention,completed,cancelled,failed"`
	State string `json:"state" enum:"completed,current,future,blocked,skipped"`
	// Optional marks a stage only some runs have (correcting, integrating), so
	// "this may not happen" renders differently from "this has not happened".
	Optional bool `json:"optional,omitempty"`
}

// WorkflowPresentationPlacement is "where is AO working", in the terms the user
// chose it in.
type WorkflowPresentationPlacement struct {
	Type  string `json:"type" enum:"direct_branch,isolated_worktree"`
	State string `json:"state,omitempty" enum:"selected,waiting,preparing,ready,active,reviewing,integrating,integrated,conflict,preserved,terminal"`
	// ChosenBy answers "did I ask for this or did AO pick it". `unknown` is a
	// real value for a run with no placement record — never coerced to
	// automatic, because claiming AO chose something it has no record of
	// choosing is a fabrication.
	ChosenBy string `json:"chosenBy" enum:"user,automatic,unknown"`
	// ChoiceReason is the stable code behind ChosenBy: the applied override's
	// recorded reason, or the placement's own provenance.
	ChoiceReason    string `json:"choiceReason,omitempty"`
	RepoPath        string `json:"repoPath,omitempty"`
	ExecutionBranch string `json:"executionBranch,omitempty"`
	BaseBranch      string `json:"baseBranch,omitempty"`
	MergeTarget     string `json:"mergeTarget,omitempty"`
	WorktreePath    string `json:"worktreePath,omitempty"`
	IntegratedSHA   string `json:"integratedSha,omitempty"`
	// IntegrationRequired is FALSE for every direct-branch placement. It is the
	// field that makes "never ask a direct-branch run whether to merge" a
	// property of the projection rather than of each screen remembering it.
	IntegrationRequired bool `json:"integrationRequired"`
	// Integration is the same answer with its five distinguishable values
	// (P3-B §15), so a surface can say "nothing to integrate" and "integration
	// failed" instead of one generic "merge pending". It is derived together
	// with IntegrationRequired and can never disagree with it.
	Integration string `json:"integration" enum:"not_required,pending,in_progress,integrated,failed"`
	Generation  int64  `json:"generation,omitempty"`
}

// WorkflowPresentationEvent is one line of the bounded activity timeline.
// Heartbeats, wake retries and reconcile passes are deliberately absent.
type WorkflowPresentationEvent struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind" enum:"started,planned,worker_launched,work_completed,review_started,review_verdict,fix_started,repair_started,verified,integrated,provider_failed,stopped,completed,cancelled,failed"`
	// Detail is the technical qualifier — a harness, a verdict, an error class,
	// a SHA — so the line stays short without losing specificity.
	Detail string `json:"detail,omitempty"`
}

// WorkflowPresentationTechnical is everything an operator diagnosing the run
// needs and a person reading it does not. Retained in full, rendered secondary.
type WorkflowPresentationTechnical struct {
	Phase           string `json:"phase,omitempty"`
	RunState        string `json:"runState,omitempty"`
	Attention       string `json:"attention,omitempty"`
	AttentionReason string `json:"attentionReason,omitempty"`
	// AttentionDetail is the daemon's own English sentence about the remedy —
	// the fallback a UI renders when it has no localized copy for the reason,
	// so a reason added in the backend is never a blank page.
	AttentionDetail     string     `json:"attentionDetail,omitempty"`
	WaitReason          string     `json:"waitReason,omitempty"`
	NextWakeAt          *time.Time `json:"nextWakeAt,omitempty"`
	PlacementGeneration int64      `json:"placementGeneration,omitempty"`
	LifecycleGeneration int64      `json:"lifecycleGeneration,omitempty"`
	ErrorClass          string     `json:"errorClass,omitempty"`
	RepairRunID         string     `json:"repairRunId,omitempty"`
	// The execution this status is about (P3-D §24). Identities and bounded
	// classifications only: no prompt text, no credentials, no pane contents.
	AttemptID      string     `json:"attemptId,omitempty"`
	AttemptNumber  int64      `json:"attemptNumber,omitempty"`
	Provider       string     `json:"provider,omitempty"`
	SessionID      string     `json:"sessionId,omitempty"`
	Authority      string     `json:"authority,omitempty" enum:"active,concluded,superseded,legacy_unproven"`
	DispatchedAt   *time.Time `json:"dispatchedAt,omitempty"`
	LastEventPhase string     `json:"lastEventPhase,omitempty"`
	LastEventAt    *time.Time `json:"lastEventAt,omitempty"`
}

func workflowPresentationView(p workflowcore.Presentation) WorkflowPresentationView {
	out := WorkflowPresentationView{
		Stage:                 string(p.Stage),
		RequiresHuman:         p.RequiresHuman,
		AutomaticActionActive: p.AutomaticActionActive,
		SummaryCode:           p.SummaryCode,
		RecommendedAction:     string(p.RecommendedAction),
		Technical: WorkflowPresentationTechnical{
			Phase:               string(p.Technical.Phase),
			RunState:            string(p.Technical.RunState),
			Attention:           string(p.Technical.Attention),
			AttentionReason:     p.Technical.AttentionReason,
			AttentionDetail:     p.Technical.AttentionDetail,
			WaitReason:          p.Technical.WaitReason,
			NextWakeAt:          p.Technical.NextWakeAt,
			PlacementGeneration: p.Technical.PlacementGeneration,
			LifecycleGeneration: p.Technical.LifecycleGeneration,
			ErrorClass:          string(p.Technical.ErrorClass),
			RepairRunID:         p.Technical.RepairRunID,
			AttemptID:           p.Technical.Execution.AttemptID,
			AttemptNumber:       p.Technical.Execution.AttemptNumber,
			Provider:            p.Technical.Execution.Provider,
			SessionID:           p.Technical.Execution.SessionID,
			Authority:           string(p.Technical.Execution.Authority),
			DispatchedAt:        timePtrOrNil(p.Technical.Execution.StartedAt),
			LastEventPhase:      p.Technical.Execution.LastEventPhase,
			LastEventAt:         timePtrOrNil(p.Technical.Execution.LastEventAt),
		},
	}
	for _, a := range p.Actions {
		out.Actions = append(out.Actions, WorkflowPresentationAction{
			ID: string(a.ID), Primary: a.Primary, Enabled: a.Enabled, DisabledReason: a.DisabledReason,
		})
	}
	for _, s := range p.Progress {
		out.Progress = append(out.Progress, WorkflowPresentationStage{
			Stage: string(s.Stage), State: string(s.State), Optional: s.Optional,
		})
	}
	for _, e := range p.Timeline {
		out.Timeline = append(out.Timeline, WorkflowPresentationEvent{At: e.At, Kind: string(e.Kind), Detail: e.Detail})
	}
	if p.Placement.Known {
		out.Placement = &WorkflowPresentationPlacement{
			Type: string(p.Placement.Type), State: string(p.Placement.State),
			ChosenBy: string(p.Placement.ChosenBy), ChoiceReason: p.Placement.ChoiceReason,
			RepoPath: p.Placement.RepoPath, ExecutionBranch: p.Placement.ExecutionBranch,
			BaseBranch: p.Placement.BaseBranch, MergeTarget: p.Placement.MergeTarget,
			WorktreePath: p.Placement.WorktreePath, IntegratedSHA: p.Placement.IntegratedSHA,
			IntegrationRequired: p.Placement.IntegrationRequired,
			Integration:         string(p.Placement.Integration),
			Generation:          p.Placement.Generation,
		}
	}
	return out
}

// presentationInputs reads the placement authority for one run.
//
// Optional by construction, exactly like every other placement consumer: a
// deployment with no PlacementManager wired gets a presentation with no
// placement rather than a failed request, and the projection reports
// `chosenBy: unknown` rather than inventing a placement it never read.
func (c *WorkflowsController) presentationInputs(ctx context.Context, runID string) (
	[]workflowcore.PlacementView, []workflowcore.PlacementOverrideView, workflowcore.AdmissionStateView,
) {
	var placements []workflowcore.PlacementView
	var overrides []workflowcore.PlacementOverrideView
	var admission workflowcore.AdmissionStateView
	if c.Svc == nil {
		return nil, nil, admission
	}
	if svc, ok := c.Svc.(workflowsvc.PlacementManager); ok {
		if got, err := svc.ListPlacements(ctx, runID); err == nil {
			placements = got
		}
		if got, err := svc.AdmissionState(ctx, runID); err == nil {
			admission = got
		}
	}
	if svc, ok := c.Svc.(workflowsvc.PlacementOverrideManager); ok {
		if got, err := svc.ListPlacementOverrides(ctx, runID); err == nil {
			overrides = got
		}
	}
	return placements, overrides, admission
}

// timePtrOrNil renders a zero time as an absent field rather than as the epoch,
// so "AO does not hold this fact" and "this happened in 1970" stay different
// answers on the wire.
func timePtrOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
