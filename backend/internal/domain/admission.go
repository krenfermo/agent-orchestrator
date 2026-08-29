package domain

import "time"

// admission.go — P1-D §C/§D: ONE admission decision.
//
// Before this, safety was distributed. The capacity scheduler decided whether
// the machine had room. The branch-lock gate decided whether the repository was
// safe to write. The worktree lifecycle decided whether a checkout existed.
// Each was correct on its own, and together they could not answer the question
// an operator actually asks -- "why is this not running" -- with anything
// better than "waiting".
//
// AdmissionDecision is that answer, and it is a GATE over the obligations those
// three already own, never a second scheduler. It does not queue anything, does
// not grant anything, and does not own a wake. It reads the durable facts each
// authority already publishes and says, in one place, whether a launch is
// permitted and -- when it is not -- exactly which authority is withholding it.

// AdmissionWaitReason is the closed vocabulary of why a launch was withheld.
//
// A generic "waiting" is deliberately absent. AO knows the cause in every case
// it withholds a launch, and reporting a cause it knows as an unknown is how a
// run that is queued behind a branch ends up looking identical to one whose
// provider is rate-limited.
type AdmissionWaitReason string

const (
	// AdmissionWaitNone is the zero value, used by an admitted decision.
	AdmissionWaitNone AdmissionWaitReason = ""
	// AdmissionCapacityWait means the machine is full. P1-C's durable capacity
	// queue holds the obligation and a release wakes it.
	AdmissionCapacityWait AdmissionWaitReason = "capacity_wait"
	// AdmissionBranchWait means another run holds the repository+branch this one
	// must write. Distinct from placement_wait: the placement is frozen and
	// correct, the branch authority is simply somebody else's right now.
	AdmissionBranchWait AdmissionWaitReason = "branch_wait"
	// AdmissionPlacementWait means the frozen placement is not ready -- not yet
	// frozen, still being prepared, or superseded by a newer generation. The
	// distinction from branch_wait is the whole point of §C: a run blocked
	// because its worktree does not exist yet is not blocked on another run.
	AdmissionPlacementWait AdmissionWaitReason = "placement_wait"
	// AdmissionProviderWait means no provider is eligible -- unauthenticated, in
	// cooldown, or none configured for the role.
	AdmissionProviderWait AdmissionWaitReason = "provider_wait"
	// AdmissionDependencyWait means the task's dependencies have not completed.
	AdmissionDependencyWait AdmissionWaitReason = "dependency_wait"
	// AdmissionLifecycleSuperseded means this pass is carrying a generation the
	// lifecycle has moved past. Not a wait a person can clear and not one that
	// resolves by itself -- the pass simply has no authority, and stops.
	AdmissionLifecycleSuperseded AdmissionWaitReason = "lifecycle_superseded"
	// AdmissionStrategyRefused means the run's execution strategy does not permit
	// this launch at all.
	AdmissionStrategyRefused AdmissionWaitReason = "strategy_refused"
)

// IsKnown reports whether the value is one this build understands.
func (r AdmissionWaitReason) IsKnown() bool {
	switch r {
	case AdmissionWaitNone, AdmissionCapacityWait, AdmissionBranchWait, AdmissionPlacementWait,
		AdmissionProviderWait, AdmissionDependencyWait, AdmissionLifecycleSuperseded,
		AdmissionStrategyRefused:
		return true
	default:
		return false
	}
}

// SpendsRetryBudget reports whether being withheld for this reason should count
// against the obligation's retry budget.
//
// Every reason answers false, and that is not an oversight -- it is the §C
// requirement stated as code. None of these is a failed attempt: the launch did
// not happen, nothing was consumed, and nothing went wrong. Burning a retry for
// "the machine was full" is how a run that waited politely for ten minutes ends
// up out of attempts.
//
// The method exists rather than the constant `false` so that a future reason
// which IS a failure has one place to say so, and so a test can assert the
// property over the whole vocabulary instead of over a comment.
func (r AdmissionWaitReason) SpendsRetryBudget() bool { return false }

// ConsumesCapacity reports whether an obligation withheld for this reason
// should be holding a runtime slot. Every reason answers false: a run that is
// not launching is not running, and a slot held by a wait is a slot no other
// run can use.
func (r AdmissionWaitReason) ConsumesCapacity() bool { return false }

// SelfResolving reports whether this wait is expected to clear on its own.
//
// It is what separates "AO is queuing, sit tight" from "nothing will change
// until somebody does something", which is the difference between a spinner
// that is honest and one that is not.
func (r AdmissionWaitReason) SelfResolving() bool {
	switch r {
	case AdmissionCapacityWait, AdmissionBranchWait, AdmissionPlacementWait, AdmissionDependencyWait:
		return true
	default:
		// provider_wait may need auth; lifecycle_superseded and
		// strategy_refused are not waits at all.
		return false
	}
}

// AdmissionDecision is the single answer to "may this obligation launch now".
//
// A decision is Admitted only when every authority agreed, and it then names
// the exact tuple the launch is valid for: (lifecycle generation, placement
// generation, provider attempt, capacity claim, branch authority). §K's rule is
// that a launch is valid only for that tuple, and a stale component invalidates
// it -- so the tuple travels with the decision rather than being re-read later
// from a world that may have moved.
type AdmissionDecision struct {
	Admitted bool
	// Reason is set when Admitted is false, and is never AdmissionWaitNone in
	// that case.
	Reason AdmissionWaitReason
	// Detail is a human-readable explanation. Never the sole carrier of the
	// cause -- Reason is what code branches on.
	Detail string

	// The authoritative tuple. All five are populated on an admitted decision;
	// a refused decision populates whatever it managed to establish, which is
	// what makes the refusal explainable.
	LifecycleGeneration int64
	PlacementGeneration int64
	ProviderAttemptID   string
	CapacityClaimID     string
	CapacityDispatchKey string
	// BranchAuthority names the branch lock ids this launch is entitled under,
	// or is empty for an isolated placement, which needs none.
	BranchAuthority []string

	// PlacementReady and PlacementState report what the placement authority
	// said, so an observer can distinguish "no placement" from "a placement
	// that is not ready yet".
	PlacementReady bool
	PlacementState ExecutionPlacementState

	DecidedAt time.Time
}

// Withheld reports whether the decision refused the launch for a reason that
// will clear by itself.
func (d AdmissionDecision) Withheld() bool {
	return !d.Admitted && d.Reason.SelfResolving()
}
