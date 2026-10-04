package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// admission.go — P1-D §C/§D/§E: ONE admission decision.
//
// Before this file, a launch had to satisfy three authorities that did not know
// about each other:
//
//	routeWorkerDispatch  is a provider usable
//	ensureBranchLock     is the repository safe to write
//	acquireCapacity      is there room on this machine
//
// Each was correct. Together they could not answer "why is this run not
// running" with anything an operator could act on, because whichever gate
// happened to refuse first parked the run under its own vocabulary and the
// other two were never consulted. A run queued behind a branch and a run whose
// machine was full looked identical.
//
// Admit is that one decision. It is deliberately a GATE, not a scheduler:
//
//   - it owns no queue. P1-C's durable capacity queue is the queue, and
//     admission calls into it rather than duplicating it.
//   - it owns no wake. The existing wake reasons are reused, one per waiting
//     reason, so a woken run resumes through the path it already had.
//   - it grants nothing of its own. Every authority it reports is one some
//     other component issued: a capacity claim, a branch lock, a frozen
//     placement, a provider attempt.
//
// What it adds is ORDER and a NAME. The order is fixed so the reported reason
// is stable rather than a race between three gates; the name is the closed
// domain.AdmissionWaitReason vocabulary, so `placement_wait` and `branch_wait`
// are finally different things.

// AdmissionRequest is one intended launch, in the admission gate's vocabulary.
type AdmissionRequest struct {
	Run  domain.WorkflowRun
	Step domain.WorkflowStep
	// Harness and Profile are the provider routing selected for this launch.
	// An empty Harness means routing has not produced one, which is a
	// provider_wait rather than a failure.
	Harness domain.AgentHarness
	Profile domain.ProviderProfileID
	// ProviderWaiting carries the routing decision's own verdict, so admission
	// reports provider_wait for the same reason routing did rather than
	// re-deriving provider health a second time and possibly disagreeing.
	ProviderWaiting bool
	// DependenciesReady reports whether this task's dependencies permit
	// execution. The task graph owns that question; admission only classifies
	// the answer.
	DependenciesReady bool
	// StrategyPermits reports whether the run's execution strategy allows this
	// launch at all.
	StrategyPermits bool
	// Capacity is the capacity request this launch would take. Built by the
	// caller because only the caller knows which kind of runtime it is
	// launching and under which intent key.
	Capacity capacityRequest
}

// Admit is the single gate every metered launch passes through.
//
// The nine conditions of §C, evaluated in a fixed order, cheapest and most
// fundamental first:
//
//  1. lifecycle generation is current      -> lifecycle_superseded
//  2. strategy permits execution           -> strategy_refused
//  3. dependencies permit execution        -> dependency_wait
//  4. provider is eligible                 -> provider_wait
//  5. placement is frozen and ready        -> placement_wait
//  6. branch/worktree authority is ready   -> branch_wait
//  7. mutation exclusivity is proven       -> branch_wait
//  8. no stale placement generation active -> placement_wait
//  9. capacity exists                      -> capacity_wait
//
// Capacity is LAST on purpose, and this is the one ordering decision worth
// arguing about. A capacity claim is the only authority in the list that
// consumes a shared, bounded resource: taking one for a launch that a branch or
// a placement was going to refuse anyway occupies a slot no other run can use,
// for as long as it takes the next reconcile to release it. Every other check
// is free to fail. So AO establishes that the launch is otherwise legal, and
// only then asks for the room to perform it.
//
// A refused decision is never an error. The caller parks the run under the
// wake that matches the reason, and — because AdmissionWaitReason.
// SpendsRetryBudget() is false for every value — nothing is charged for
// waiting.
func (c *Coordinator) Admit(ctx stdctx.Context, req AdmissionRequest) (domain.AdmissionDecision, error) {
	decision := domain.AdmissionDecision{
		LifecycleGeneration: req.Capacity.Generation,
		DecidedAt:           c.clock(),
	}

	// 1. Lifecycle generation. A pass carrying a generation the step has moved
	// past speaks for nothing, and must not be allowed to take a claim, a lock
	// or a placement on behalf of an obligation that has already been retried.
	current := c.stepDispatchGeneration(ctx, req.Step.ID)
	if req.Capacity.Generation > 0 && req.Capacity.Generation < current {
		return refuse(decision, domain.AdmissionLifecycleSuperseded,
			fmt.Sprintf("this pass carries dispatch generation %d; the step is at %d", req.Capacity.Generation, current)), nil
	}

	// 2. Strategy.
	if !req.StrategyPermits {
		return refuse(decision, domain.AdmissionStrategyRefused,
			"the run's execution strategy does not permit this launch"), nil
	}

	// 3. Dependencies.
	if !req.DependenciesReady {
		return refuse(decision, domain.AdmissionDependencyWait,
			"this task's dependencies have not completed"), nil
	}

	// 4. Provider eligibility. Routing already decided this; admission reports
	// its verdict rather than forming a second opinion that could disagree.
	if req.ProviderWaiting || req.Harness == "" {
		return refuse(decision, domain.AdmissionProviderWait,
			"no eligible provider is currently usable for this role"), nil
	}

	// 5. Placement. Frozen, current, and physically ready.
	placement, placementOK, err := c.EnsureExecutionPlacement(ctx, req.Run, req.Step)
	if err != nil {
		if isPlacementUnprovable(err) {
			// A legacy run whose placement cannot be established from durable
			// facts. Fail-closed and NOT self-resolving: nothing about waiting
			// produces the missing evidence, so the reason says placement and
			// the detail says a person is needed.
			return refuse(decision, domain.AdmissionPlacementWait,
				"this run executed before placements were durable and its placement cannot be proven from durable facts"), nil
		}
		return decision, err
	}
	if c.placementEnabled() {
		if !placementOK {
			return refuse(decision, domain.AdmissionPlacementWait,
				"no execution placement is frozen for this obligation yet"), nil
		}
		decision.PlacementGeneration = placement.PlacementGeneration
		decision.PlacementState = placement.State
		// 8, evaluated here because it is the same read: a stale placement
		// generation is refused before it can take any authority.
		if !c.PlacementIsCurrent(ctx, placementScopeFor(req.Run), placement.PlacementGeneration) {
			return refuse(decision, domain.AdmissionPlacementWait,
				fmt.Sprintf("placement generation %d has been superseded", placement.PlacementGeneration)), nil
		}
		if !placement.Valid() {
			// A stored record that does not hold together is not an authority.
			// Refusing is the only safe reading: the alternative is launching
			// against a placement AO cannot describe.
			return refuse(decision, domain.AdmissionPlacementWait,
				"the frozen placement record is not internally consistent"), nil
		}
		if !placement.State.PermitsLaunch() {
			return refuse(decision, domain.AdmissionPlacementWait,
				"the frozen placement is not ready to be launched into ("+string(placement.State)+")"), nil
		}
		decision.PlacementReady = true
	}

	// 6/7. Branch authority and mutation exclusivity.
	//
	// For a direct-branch placement these are the same fact: the durable
	// per-(repository, branch) lock IS the exclusivity proof, enforced by a
	// partial unique index rather than by agreement. For an isolated placement
	// the exclusivity is physical -- one worktree, one task -- and no lock is
	// needed, which is why BranchAuthority is legitimately empty there.
	if c.branchLocks != nil && (!c.placementEnabled() || placement.Type == domain.PlacementDirectBranch) {
		held, ok, err := c.acquireBranchAuthority(ctx, req.Run, req.Step)
		if err != nil {
			return decision, err
		}
		if !ok {
			return refuse(decision, domain.AdmissionBranchWait,
				"another run holds this repository and branch"), nil
		}
		decision.BranchAuthority = held
	}

	// 9. Capacity, last: the only bounded shared resource in the list.
	if c.capacityEnabled() {
		admitted, cerr := c.acquireCapacity(ctx, req.Capacity)
		if cerr != nil {
			return decision, cerr
		}
		decision.CapacityDispatchKey = req.Capacity.dispatchKey()
		if !admitted {
			return refuse(decision, domain.AdmissionCapacityWait,
				"no runtime execution slot is currently free on this machine"), nil
		}
		if claim, found, gerr := c.capacity.GetCapacityClaim(ctx, req.Capacity.dispatchKey()); gerr == nil && found {
			decision.CapacityClaimID = claim.ID
		}
	}

	// Everything agreed. Record the provider attempt this tuple authorizes,
	// so §K's (G, P, A, C, B) is reconstructable from durable rows after a
	// restart rather than only from this call's stack.
	if attempt, found, aerr := c.EnsureProviderAttempt(ctx, req.Run, req.Step, placement, req.Harness, req.Profile); aerr != nil {
		return decision, aerr
	} else if found {
		decision.ProviderAttemptID = attempt.ID
		c.advanceProviderAttempt(ctx, attempt, domain.ProviderAttemptAdmitted, "", "", "", "")
		c.bindProviderAttemptRuntime(ctx, attempt, attempt.RuntimeSessionID, decision.CapacityClaimID)
	} else if c.providerAttemptsEnabled() {
		// The ledger is wired and has no attempt to offer: the obligation's
		// provider attempts are spent. Refusing here rather than launching
		// anyway is what stops a reconcile from quietly starting an
		// unbudgeted attempt.
		//
		// The capacity claim taken above is released, because a refused launch
		// must not occupy a slot -- see AdmissionWaitReason.ConsumesCapacity.
		c.releaseCapacity(ctx, req.Capacity, "admission refused after the provider budget was spent")
		return refuse(decision, domain.AdmissionProviderWait,
			"every provider attempt this obligation is budgeted for has been spent"), nil
	}

	decision.Admitted = true
	decision.Reason = domain.AdmissionWaitNone
	return decision, nil
}

// refuse stamps a decision with its reason. It exists so no call site can
// produce a refusal with an empty reason, which is the "generic waiting" §C
// forbids.
func refuse(d domain.AdmissionDecision, reason domain.AdmissionWaitReason, detail string) domain.AdmissionDecision {
	d.Admitted = false
	d.Reason = reason
	d.Detail = detail
	return d
}

// isPlacementUnprovable identifies the one placement error that is a durable
// state rather than a fault: a legacy run AO refuses to guess a placement for.
// It is matched by sentinel rather than by message, so a wrapped or annotated
// error still classifies correctly.
func isPlacementUnprovable(err error) bool {
	return errors.Is(err, ErrPlacementUnprovable)
}

// acquireBranchAuthority takes the direct-branch locks a launch needs and
// reports the lock ids it now holds.
//
// It reuses ensureBranchLock, which already owns the three truthful outcomes
// (acquired / waiting / a human's dirty checkout) and their durable records.
// Admission classifies the answer; it does not re-implement the acquisition,
// because a second acquisition path is a second place two runs could both
// believe they own a branch.
func (c *Coordinator) acquireBranchAuthority(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) ([]string, bool, error) {
	ok, err := c.ensureBranchLock(ctx, run, step)
	if err != nil || !ok {
		return nil, false, err
	}
	held, herr := c.branchLocks.HeldByRun(ctx, run.ID)
	if herr != nil {
		// The locks were acquired; only the read-back for the decision's
		// evidence failed. That is worth reporting as an empty authority list
		// rather than as a refusal -- refusing here would release nothing and
		// stall a run over bookkeeping.
		if c.log != nil {
			c.log.Debug("workflow: branch authority read-back failed", "run", run.ID, "err", herr)
		}
		return nil, true, nil
	}
	ids := make([]string, 0, len(held))
	for _, lock := range held {
		ids = append(ids, lock.ID)
	}
	return ids, true, nil
}

// recordAdmissionWait parks a run under the wake that matches its refusal.
//
// One function, one place, so the mapping from reason to wake is stated once
// and a new reason cannot silently get no wake at all. Every branch here leaves
// the step and the outbox entry untouched, which is what makes a wait
// non-duplicating: the next pass re-derives the same intent key, finds the same
// claim, and produces no second lock, worktree or wake.
func (c *Coordinator) recordAdmissionWait(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, decision domain.AdmissionDecision) (domain.WorkflowStep, error) {
	switch decision.Reason {
	case domain.AdmissionCapacityWait, domain.AdmissionProviderWait:
		// markRunWaitingForCapacity already owns the run-state move and the
		// capacity wake for both of these.
		return c.markRunWaitingForCapacity(ctx, run, step)
	case domain.AdmissionBranchWait:
		// ensureBranchLock already recorded the waiting_for_branch checkpoint
		// and scheduled the branch wake; nothing further to write.
		return step, nil
	case domain.AdmissionPlacementWait:
		return step, c.markRunWaitingForPlacement(ctx, run, step, decision)
	case domain.AdmissionDependencyWait, domain.AdmissionStrategyRefused, domain.AdmissionLifecycleSuperseded:
		// None of these is a launch AO is going to retry on a timer. The task
		// graph, the strategy and the lifecycle each drive their own
		// progression, and scheduling a wake here would be a second, competing
		// one.
		return step, nil
	default:
		return step, nil
	}
}

// placementWaitPhase is the durable phase of a waiting_for_placement
// checkpoint, mirroring branchWaitPhase.
const placementWaitPhase = "waiting_for_placement"

// markRunWaitingForPlacement records the truthful waiting_for_placement state.
//
// It mirrors markRunWaitingForBranch rather than markRunDirtyWorktree for the
// self-resolving cases, because a placement that is still being prepared does
// clear on its own. The one shape that does NOT is an unprovable legacy
// placement, and that gets no wake: nothing about waiting produces the missing
// evidence, and a run retrying forever looks identical to a hung one.
func (c *Coordinator) markRunWaitingForPlacement(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, decision domain.AdmissionDecision) error {
	now := c.clock()
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunPending {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunWaiting, now); err != nil {
			return err
		}
		run.State = domain.WorkflowRunWaiting
	}
	stepID := step.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction:     "waiting_for_placement: " + decision.Detail,
		DurablePhase:   placementWaitPhase,
		PayloadVersion: "v1",
		RetryState:     marshalPlacementWait(decision),
		CreatedAt:      now,
	}); err != nil {
		return err
	}
	if decision.PlacementGeneration > 0 {
		// A placement that exists but is not ready is being prepared by
		// somebody, so this run should come back and look again. One with no
		// generation at all is the unprovable legacy case; it waits for a
		// person, not for a timer.
		c.scheduleWake(ctx, run, stepIDPtr(step.ID), wake.ReasonBranchLock, "")
	}
	return nil
}

// PlacementWait is the structured waiting_for_placement state a run surfaces.
// Every field is a fact the admission decision established; nothing is
// estimated.
type PlacementWait struct {
	Reason              string `json:"reason"`
	Detail              string `json:"detail,omitempty"`
	PlacementGeneration int64  `json:"placementGeneration,omitempty"`
	PlacementState      string `json:"placementState,omitempty"`
	// AutoResume reports whether this wait clears without anyone doing
	// anything. False means the queue is behind a decision, and the person is
	// told so rather than left watching a spinner.
	AutoResume bool `json:"autoResume"`
}

func marshalPlacementWait(d domain.AdmissionDecision) string {
	wait := PlacementWait{
		Reason:              string(d.Reason),
		Detail:              d.Detail,
		PlacementGeneration: d.PlacementGeneration,
		PlacementState:      string(d.PlacementState),
		AutoResume:          d.PlacementGeneration > 0,
	}
	return marshalJSONOrEmptyObject(wait)
}

// marshalJSONOrEmptyObject keeps a checkpoint's RetryState valid JSON whatever
// happens: an unmarshalable payload becomes "{}" rather than an empty string,
// which every reader in this package already tolerates.
func marshalJSONOrEmptyObject(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
