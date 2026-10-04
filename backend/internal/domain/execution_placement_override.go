package domain

import "time"

// execution_placement_override.go — P1-E §B/§C: the per-task placement
// OVERRIDE request, and the explicit generation TRANSITION.
//
// P1-D froze the placement and shipped every route over it read-only, recording
// why: a write that re-points a placement is a write that can aim a running
// agent at a different checkout. This file is the model that write needed, and
// it keeps the two halves apart because they are different facts.
//
// # A request is not a placement
//
// An override says what somebody WANTS. The frozen ExecutionPlacement says what
// AO is DOING. Before the freeze the request is an input to selection, consumed
// once; after it, the request changes nothing on its own — the frozen record
// still wins, and moving it requires a transition. That asymmetry is §B.3: a
// standing override must never silently re-point live work.
//
// # A transition is not a retry
//
// A transition mints a NEW placement generation and retires the old one. It is
// the only way a frozen placement's type or location changes, and it is
// deliberately not on the failover path: a provider hop leaves the placement
// exactly where it is (§I). Every one of the three generations keeps its own
// meaning — lifecycle for a retried obligation, placement for a replaced
// physical location, provider attempt for neither.

// PlacementOverrideRequest is what an operator asked for.
//
// `auto` is a real value rather than the absence of one: an operator who has
// changed their mind needs to be able to say "withdraw my override and let
// selection policy decide", and expressing that as "delete the row" would make
// a withdrawal indistinguishable from a request that was never made.
type PlacementOverrideRequest string

const (
	// PlacementOverrideAuto defers to selection policy — project kind, project
	// execution mode, and the planner's per-task downgrade.
	PlacementOverrideAuto PlacementOverrideRequest = "auto"
	// PlacementOverrideDirectBranch asks for work in the registered repository
	// on its configured branch, protected by the durable branch lock.
	PlacementOverrideDirectBranch PlacementOverrideRequest = "direct_branch"
	// PlacementOverrideIsolatedWorktree asks for an AO-owned git worktree on a
	// generated ao/* branch.
	PlacementOverrideIsolatedWorktree PlacementOverrideRequest = "isolated_worktree"
)

// IsKnown reports whether the value is one this build understands. An unknown
// request is never coerced to auto: a request AO cannot read is refused, because
// silently substituting the default would give an operator a placement they did
// not ask for and no signal that it happened.
func (r PlacementOverrideRequest) IsKnown() bool {
	switch r {
	case PlacementOverrideAuto, PlacementOverrideDirectBranch, PlacementOverrideIsolatedWorktree:
		return true
	default:
		return false
	}
}

// Explicit reports whether the request names a placement rather than deferring.
func (r PlacementOverrideRequest) Explicit() bool {
	return r == PlacementOverrideDirectBranch || r == PlacementOverrideIsolatedWorktree
}

// PlacementType maps an explicit request onto the placement it asks for. Only
// meaningful when Explicit reports true.
func (r PlacementOverrideRequest) PlacementType() ExecutionPlacementType {
	if r == PlacementOverrideDirectBranch {
		return PlacementDirectBranch
	}
	return PlacementIsolatedWorktree
}

// PlacementOverrideState is where a request is in its life.
type PlacementOverrideState string

const (
	// PlacementOverrideRequested is outstanding: the next freeze or transition
	// for this obligation will consume it. At most one per obligation, by index.
	PlacementOverrideRequested PlacementOverrideState = "requested"
	// PlacementOverrideApplied means a placement generation was frozen from it.
	// The generation is named on the row, so "which placement did this request
	// produce" is answerable rather than inferred from timestamps.
	PlacementOverrideApplied PlacementOverrideState = "applied"
	// PlacementOverrideSuperseded means a later request replaced it before it
	// was consumed.
	PlacementOverrideSuperseded PlacementOverrideState = "superseded"
	// PlacementOverrideRefused means AO would not honour it, and the detail
	// says why. Retained: a refusal an operator cannot read afterwards is a
	// refusal they will make again.
	PlacementOverrideRefused PlacementOverrideState = "refused"
)

// IsKnown reports whether the value is one this build understands.
func (s PlacementOverrideState) IsKnown() bool {
	switch s {
	case PlacementOverrideRequested, PlacementOverrideApplied,
		PlacementOverrideSuperseded, PlacementOverrideRefused:
		return true
	default:
		return false
	}
}

// ExecutionPlacementOverride is one durable placement request.
type ExecutionPlacementOverride struct {
	ID             string
	WorkflowRunID  string
	TaskID         string
	WorkflowStepID string
	ProjectID      string

	Requested PlacementOverrideRequest
	// RequestedBy names the operator. A request with nobody's name on it is
	// refused by the coordinator rather than stored: an unattributed
	// re-pointing of a running obligation is what this whole model exists to
	// make impossible.
	RequestedBy string
	Reason      string

	State PlacementOverrideState
	// AppliedGeneration is the placement generation that consumed this request,
	// zero while it is outstanding.
	AppliedGeneration int64
	Detail            string

	CreatedAt  time.Time
	UpdatedAt  time.Time
	ResolvedAt *time.Time
}

// Valid reports whether the record is internally consistent enough to store.
func (o ExecutionPlacementOverride) Valid() bool {
	return o.ID != "" && o.WorkflowRunID != "" &&
		o.Requested.IsKnown() && o.State.IsKnown() && o.RequestedBy != ""
}

// PlacementTransitionState is where a transition is in its life.
type PlacementTransitionState string

const (
	// PlacementTransitionRequested is written before the replacement it
	// authorizes, so a crash leaves an explanation for a move that may not have
	// happened rather than a move nobody can account for.
	PlacementTransitionRequested PlacementTransitionState = "requested"
	// PlacementTransitionApplied means the replacement generation exists and is
	// named on the row.
	PlacementTransitionApplied PlacementTransitionState = "applied"
	// PlacementTransitionRefused means AO would not move the placement, and
	// RefusalReason names which authority said no. Refused rows are excluded
	// from the surviving-transition index: a "not yet" must not become a
	// permanent no.
	PlacementTransitionRefused PlacementTransitionState = "refused"
)

// IsKnown reports whether the value is one this build understands.
func (s PlacementTransitionState) IsKnown() bool {
	switch s {
	case PlacementTransitionRequested, PlacementTransitionApplied, PlacementTransitionRefused:
		return true
	default:
		return false
	}
}

// PlacementTransitionRefusal is the closed vocabulary of reasons AO declines to
// move a frozen placement.
//
// Closed, and every value names an AUTHORITY rather than a symptom, because the
// operator's next action differs per authority: a held capacity claim clears on
// its own, a live runtime needs stopping, a drifted expected state means the
// request describes a world that no longer exists and has to be re-read.
type PlacementTransitionRefusal string

const (
	// PlacementTransitionNoAuthority — nobody's name on the request.
	PlacementTransitionNoAuthority PlacementTransitionRefusal = "no_operator_authority"
	// PlacementTransitionUnknownRequest — a placement value this build cannot
	// read. Never coerced to a default.
	PlacementTransitionUnknownRequest PlacementTransitionRefusal = "unknown_placement_request"
	// PlacementTransitionNoPlacement — nothing is frozen yet, so there is
	// nothing to transition. The operator wants an override, not a transition.
	PlacementTransitionNoPlacement PlacementTransitionRefusal = "no_frozen_placement"
	// PlacementTransitionNotCurrent — the generation named is not the newest.
	PlacementTransitionNotCurrent PlacementTransitionRefusal = "placement_not_current"
	// PlacementTransitionStateDrifted — the placement is not in the state the
	// requester asserted. A refusal rather than a correction: the request
	// describes a situation that no longer holds.
	PlacementTransitionStateDrifted PlacementTransitionRefusal = "lifecycle_state_drifted"
	// PlacementTransitionProviderActive — an authoritative provider attempt
	// still holds the obligation.
	PlacementTransitionProviderActive PlacementTransitionRefusal = "active_provider_attempt"
	// PlacementTransitionCapacityHeld — a capacity claim is still outstanding,
	// which means a runtime is paid for and may exist.
	PlacementTransitionCapacityHeld PlacementTransitionRefusal = "held_capacity_claim"
	// PlacementTransitionRuntimeLive — a worker, reviewer, fix or repair
	// runtime still owns the old placement (§B.6).
	PlacementTransitionRuntimeLive PlacementTransitionRefusal = "live_runtime"
	// PlacementTransitionBranchHeld — the run still holds branch authority over
	// the old placement.
	PlacementTransitionBranchHeld PlacementTransitionRefusal = "held_branch_authority"
	// PlacementTransitionIntegrating — integration authority is outstanding on
	// the old placement (§B.7). Moving underneath a merge in progress is how a
	// commit gets lost.
	PlacementTransitionIntegrating PlacementTransitionRefusal = "outstanding_integration"
	// PlacementTransitionRunTerminal — the run is over. Nothing will launch
	// into a replacement, so minting one would only orphan a second checkout.
	PlacementTransitionRunTerminal PlacementTransitionRefusal = "run_is_terminal"
	// PlacementTransitionUnreadable — an authority could not be read. Fail
	// closed: an unreadable authority is never evidence of quiescence.
	PlacementTransitionUnreadable PlacementTransitionRefusal = "authority_unreadable"
)

// PlacementQuiescence is the proof that no authority still owns the old
// placement, and it is an AND over durable facts — never an inference from what
// is on the filesystem (§C).
//
// A checkout that looks idle proves nothing about whether a provider is
// mid-write, a merge is half-applied, or a repair holds a ceded lock. Each field
// here is a question put to the component that owns the answer.
type PlacementQuiescence struct {
	// RunActive reports the run is not terminal.
	RunActive bool
	// NoProviderAttempt reports no authoritative provider attempt remains.
	NoProviderAttempt bool
	// NoCapacityClaim reports no capacity claim is outstanding for the run.
	NoCapacityClaim bool
	// NoLiveRuntime reports no worker/reviewer/fix/repair runtime is bound to
	// the obligation.
	NoLiveRuntime bool
	// NoBranchAuthority reports the run holds no branch lock.
	NoBranchAuthority bool
	// NoIntegrationAuthority reports the placement is not reviewing,
	// integrating, or already integrated.
	NoIntegrationAuthority bool
	// Digest is the recorded form of the above, stored on the transition so
	// "it was safe" is inspectable afterwards rather than recomputed against a
	// world that has moved.
	Digest string
}

// Quiesced reports whether every authority answered. It is an AND on purpose:
// a partial proof is not a proof, and there is no field here whose absence is
// tolerable.
func (q PlacementQuiescence) Quiesced() bool {
	return q.RunActive && q.NoProviderAttempt && q.NoCapacityClaim &&
		q.NoLiveRuntime && q.NoBranchAuthority && q.NoIntegrationAuthority
}

// Refusal names the first authority that refused, in the order an operator can
// act on: what they must stop, then what clears on its own, then what needs a
// re-read. An empty result means quiesced.
func (q PlacementQuiescence) Refusal() PlacementTransitionRefusal {
	switch {
	case !q.RunActive:
		return PlacementTransitionRunTerminal
	case !q.NoIntegrationAuthority:
		return PlacementTransitionIntegrating
	case !q.NoProviderAttempt:
		return PlacementTransitionProviderActive
	case !q.NoLiveRuntime:
		return PlacementTransitionRuntimeLive
	case !q.NoBranchAuthority:
		return PlacementTransitionBranchHeld
	case !q.NoCapacityClaim:
		return PlacementTransitionCapacityHeld
	default:
		return ""
	}
}

// ExecutionPlacementTransition is the durable account of one placement
// generation being replaced by another.
type ExecutionPlacementTransition struct {
	ID             string
	WorkflowRunID  string
	TaskID         string
	WorkflowStepID string
	ProjectID      string

	FromGeneration int64
	ToGeneration   int64

	// The source provenance, copied from the old placement at request time so
	// this row still describes what was replaced after the placement's own
	// state has moved on.
	FromType            ExecutionPlacementType
	FromRepoPath        string
	FromExecutionBranch string
	FromWorktreePath    string
	FromBaseSHA         string

	Requested PlacementOverrideRequest
	ToType    ExecutionPlacementType

	RequestedBy string
	Reason      string
	// ExpectedState is the placement state the requester asserted.
	ExpectedState ExecutionPlacementState

	QuiescenceDigest string
	State            PlacementTransitionState
	RefusalReason    PlacementTransitionRefusal
	Detail           string

	CreatedAt time.Time
	UpdatedAt time.Time
}
