package workflow

import (
	stdctx "context"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// placement_observability.go — P1-D §O: enough state to inspect the new
// authority, and nothing more.
//
// The rule applied throughout: identity and state are exposed, secrets and
// tokens are not. A placement's owner token names a daemon incarnation and is
// AO's own local identifier, but it is still an ownership credential in shape,
// and an operator never needs it to answer "why has this not launched" — so it
// does not appear in any view here. Repository paths, branches, generations,
// states and waiting reasons do, because those are exactly what makes a stuck
// run explainable.

// ProviderAttemptView is one durable provider attempt, as an operator sees it.
type ProviderAttemptView struct {
	ID                  string
	Ordinal             int64
	Provider            domain.AgentHarness
	Profile             domain.ProviderProfileID
	State               domain.ProviderAttemptState
	Safety              domain.FailoverSafety
	FailureClass        domain.WorkflowErrorClass
	FailureReason       string
	LifecycleGeneration int64
	PlacementGeneration int64
	WorkflowStepID      string
	// RuntimeSessionID and CapacityClaimID are AO's own local names for the
	// runtime the attempt paid for and the slot that authorized it. They are
	// what let a held slot be correlated with something an operator can see.
	RuntimeSessionID string
	CapacityClaimID  string
	// MutationEvidence is the digest that PROVED a safe_after_proven_no_mutation
	// classification. It names what was compared, so a reader can tell which
	// fingerprint the claim rested on instead of trusting the word "proven".
	MutationEvidence     string
	PredecessorAttemptID string
	SuccessorAttemptID   string
	// Authoritative reports whether this attempt is the one currently entitled
	// to act. Exactly one attempt per obligation may be, and a chain where none
	// is means the obligation's provider budget is spent.
	Authoritative bool
}

// ListProviderAttempts returns a run's whole provider-attempt history, oldest
// first. The chain reads as ONE obligation moving between providers rather than
// as unrelated attempts, which is the §F distinction made visible.
func (c *Coordinator) ListProviderAttempts(ctx stdctx.Context, runID string) ([]ProviderAttemptView, error) {
	if !c.providerAttemptsEnabled() {
		return nil, nil
	}
	attempts, err := c.providerAttempts.ListProviderAttemptsForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderAttemptView, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, ProviderAttemptView{
			ID: a.ID, Ordinal: a.Ordinal, Provider: a.Provider, Profile: a.Profile,
			State: a.State, Safety: a.Safety, FailureClass: a.FailureClass,
			FailureReason: a.FailureReason, LifecycleGeneration: a.LifecycleGeneration,
			PlacementGeneration: a.PlacementGeneration, WorkflowStepID: a.WorkflowStepID,
			RuntimeSessionID: a.RuntimeSessionID, CapacityClaimID: a.CapacityClaimID,
			MutationEvidence:     a.MutationEvidenceDigest,
			PredecessorAttemptID: a.PredecessorAttemptID, SuccessorAttemptID: a.SuccessorAttemptID,
			Authoritative: a.State.Authoritative(),
		})
	}
	return out, nil
}

// AdmissionStateView is why a run has not launched, as one answer.
//
// It is derived at read time from durable rows and never estimates: a field AO
// cannot read stays empty rather than being guessed at.
type AdmissionStateView struct {
	// WaitingReason is the closed vocabulary from domain.AdmissionWaitReason,
	// empty when the run is not waiting on admission.
	WaitingReason domain.AdmissionWaitReason
	Detail        string
	// AutoResume reports whether this wait clears without anyone doing
	// anything -- the difference between "AO is queuing" and "somebody has to
	// decide something".
	AutoResume bool
	// SpendsRetryBudget is always false and is surfaced deliberately: §C's
	// guarantee that waiting is free should be checkable from the API, not
	// only from a comment.
	SpendsRetryBudget bool
	// PlacementReady and PlacementState report what the placement authority
	// said; CapacityClaim names the slot, when one was granted.
	PlacementReady      bool
	PlacementState      domain.ExecutionPlacementState
	PlacementGeneration int64
	CapacityClaimID     string
	// CurrentAttemptID names the provider attempt currently authoritative.
	CurrentAttemptID string
}

// AdmissionState answers "why has this run not launched" from durable records.
//
// The waiting reason is read from the run's own ledger rather than recomputed
// by running the admission gate again. Re-running it would take capacity claims
// and branch locks as a side effect of a read, and would report the situation
// as it is NOW rather than the one the run is actually parked on.
func (c *Coordinator) AdmissionState(ctx stdctx.Context, runID string) (AdmissionStateView, error) {
	out := AdmissionStateView{}
	run, found, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return out, err
	}
	if !found {
		return out, nil
	}
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return out, err
	}
	// The ledger first, because a branch or placement wait is the most
	// specific thing AO knows and it is written down. Newest-first, and the
	// first match wins: an older wait describes a situation that has resolved.
	for i := len(checkpoints) - 1; i >= 0 && out.WaitingReason == domain.AdmissionWaitNone; i-- {
		cp := checkpoints[i]
		switch cp.DurablePhase {
		case placementWaitPhase:
			var wait PlacementWait
			if json.Unmarshal([]byte(cp.RetryState), &wait) == nil {
				out.WaitingReason = domain.AdmissionPlacementWait
				out.Detail = wait.Detail
				out.AutoResume = wait.AutoResume
				out.PlacementGeneration = wait.PlacementGeneration
				out.PlacementState = domain.ExecutionPlacementState(wait.PlacementState)
			}
		case branchWaitPhase:
			out.WaitingReason = domain.AdmissionBranchWait
			out.Detail = cp.NextAction
			out.AutoResume = true
		}
	}
	// A run that is not parked reports no reason at all. Reporting the last
	// wait it ever had would describe a situation that has since resolved.
	if !runIsWaiting(run) {
		out = AdmissionStateView{}
	} else if out.WaitingReason == domain.AdmissionWaitNone {
		// Neither branch nor placement. The two remaining causes are
		// distinguished by which authority is actually withholding: a QUEUED
		// capacity claim is the machine being full, and a waiting routing
		// decision is the provider being unusable. Both are read from durable
		// rows the respective authority already wrote -- admission is not
		// re-run here, because re-running it would take claims and locks as a
		// side effect of a read.
		if c.capacityEnabled() {
			if claims, cerr := c.capacity.ListCapacityClaimsForRun(ctx, runID); cerr == nil {
				for _, claim := range claims {
					if claim.State == domain.CapacityClaimQueued {
						out.WaitingReason = domain.AdmissionCapacityWait
						out.Detail = "no runtime execution slot is currently free on this machine"
						out.AutoResume = true
					}
				}
			}
		}
		if out.WaitingReason == domain.AdmissionWaitNone {
			if _, waiting := c.waitingRoutingDecision(ctx, runID); waiting {
				out.WaitingReason = domain.AdmissionProviderWait
				out.Detail = "no eligible provider is currently usable for this role"
			}
		}
	}
	out.SpendsRetryBudget = out.WaitingReason.SpendsRetryBudget()

	if c.placementEnabled() {
		scope := placementScopeFor(run)
		if placement, ok, perr := c.placements.GetLiveExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID); perr == nil && ok {
			out.PlacementState = placement.State
			out.PlacementGeneration = placement.PlacementGeneration
			out.PlacementReady = placement.State.PermitsLaunch() && placement.Valid()
		}
	}
	if c.capacityEnabled() {
		if claims, cerr := c.capacity.ListCapacityClaimsForRun(ctx, runID); cerr == nil {
			for _, claim := range claims {
				if claim.State == domain.CapacityClaimHeld {
					out.CapacityClaimID = claim.ID
				}
			}
		}
	}
	if c.providerAttemptsEnabled() {
		if attempts, aerr := c.providerAttempts.ListProviderAttemptsForRun(ctx, runID); aerr == nil {
			for _, a := range attempts {
				if a.State.Authoritative() {
					out.CurrentAttemptID = a.ID
				}
			}
		}
	}
	return out, nil
}

// runIsWaiting reports whether a run is currently parked, which is the only
// state in which a recorded waiting reason describes the present.
func runIsWaiting(run domain.WorkflowRun) bool {
	return run.State == domain.WorkflowRunWaiting || run.State == domain.WorkflowRunNeedsAttention
}
