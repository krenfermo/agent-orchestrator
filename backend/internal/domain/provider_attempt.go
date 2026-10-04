package domain

import "time"

// provider_attempt.go — P1-D §F/§G/§H: the durable provider-attempt ledger.
//
// The rule this file exists to hold:
//
//	A PROVIDER ATTEMPT IS NOT A TASK GENERATION.
//
// The workflow run, the step, the task and the lifecycle generation are the
// OBLIGATION, and a failover never changes them. The provider attempt is how AO
// is currently trying to discharge it. Conflating the two makes every failover
// look like new work -- a fresh placement, a fresh review, a fresh capacity
// claim -- which is both wasteful and wrong, and it is the reason a Codex
// failure used to be indistinguishable from a retry.
//
// P1-D's predecessor had the vocabulary (FailoverSafety) as a read-time
// projection. A projection cannot answer "which provider is authoritative right
// now, and what did the last one prove before it stopped" after a restart, so
// this is a real table.

// FailoverSafety is what AO can prove about whether a provider attempt touched
// anything before it failed. It is durable: the classification is taken once,
// at the moment the evidence exists, and read back afterwards rather than
// recomputed from a world that has since moved.
type FailoverSafety string

const (
	// FailoverSafeBeforeExecution is a launch that failed with a classified
	// error and created nothing. The provider never ran, so nothing it could
	// have mutated exists, and another provider may take the same obligation.
	FailoverSafeBeforeExecution FailoverSafety = "safe_before_execution"
	// FailoverSafeAfterProvenNoMutation is a launch that got further -- a
	// runtime existed -- but whose workspace AO can POSITIVELY prove is
	// unchanged. Failover is allowed, and the same frozen placement stays
	// authoritative: the authority over the worktree did not move, only the
	// provider attempt did.
	FailoverSafeAfterProvenNoMutation FailoverSafety = "safe_after_proven_no_mutation"
	// FailoverAmbiguousExecution is the one that matters. AO cannot prove
	// whether the provider ran, or what it touched. Starting a second provider
	// on the same placement would be starting it over a state nobody can
	// describe, so nothing fails over: the run stops for evidence instead.
	FailoverAmbiguousExecution FailoverSafety = "ambiguous_execution"
	// FailoverCompletedExecution is a provider that finished its work. There is
	// no obligation left to route anywhere.
	FailoverCompletedExecution FailoverSafety = "completed_execution"
)

// PermitsFailover reports whether this safety state allows routing the same
// durable obligation to another provider.
//
// Only the two PROVEN states do. Ambiguity is never converted into permission,
// which is the whole point: the failure this prevents is provider B starting on
// a worktree provider A may have written.
func (s FailoverSafety) PermitsFailover() bool {
	return s == FailoverSafeBeforeExecution || s == FailoverSafeAfterProvenNoMutation
}

// IsKnown reports whether the value is one this build understands. An unknown
// safety class never permits failover.
func (s FailoverSafety) IsKnown() bool {
	switch s {
	case FailoverSafeBeforeExecution, FailoverSafeAfterProvenNoMutation,
		FailoverAmbiguousExecution, FailoverCompletedExecution:
		return true
	default:
		return false
	}
}

// ProviderAttemptState is the durable life of one provider attempt.
type ProviderAttemptState string

const (
	// ProviderAttemptPlanned is written before admission. It is the record that
	// a provider was chosen, so a crash before the launch leaves evidence of an
	// intent rather than nothing.
	ProviderAttemptPlanned ProviderAttemptState = "planned"
	// ProviderAttemptAdmitted means the unified admission gate passed for this
	// attempt: capacity, placement, branch authority and lifecycle all agreed.
	ProviderAttemptAdmitted ProviderAttemptState = "admitted"
	// ProviderAttemptLaunching is the crash window around the runtime launch
	// itself. An attempt found here after a restart has ambiguous execution
	// until evidence resolves it.
	ProviderAttemptLaunching ProviderAttemptState = "launching"
	// ProviderAttemptRunning means a runtime provably exists and is this
	// attempt's.
	ProviderAttemptRunning ProviderAttemptState = "running"
	// ProviderAttemptCompleted means the provider discharged the obligation.
	ProviderAttemptCompleted ProviderAttemptState = "completed"
	// ProviderAttemptFailedSafe means the attempt failed and AO can prove it
	// mutated nothing. This is the only failure state a failover may follow.
	ProviderAttemptFailedSafe ProviderAttemptState = "failed_safe"
	// ProviderAttemptFailedAmbiguous means the attempt failed and AO cannot
	// prove what it touched. Terminal, and it never fails over.
	ProviderAttemptFailedAmbiguous ProviderAttemptState = "failed_ambiguous"
	// ProviderAttemptSuperseded means a newer attempt is authoritative for this
	// obligation. A superseded attempt has no authority over anything.
	ProviderAttemptSuperseded ProviderAttemptState = "superseded"
	// ProviderAttemptAbandoned means the obligation itself went away (the run
	// was cancelled, the step reached a terminal state) while the attempt was
	// outstanding.
	ProviderAttemptAbandoned ProviderAttemptState = "abandoned"
)

// IsKnown reports whether the value is one this build understands.
func (s ProviderAttemptState) IsKnown() bool {
	switch s {
	case ProviderAttemptPlanned, ProviderAttemptAdmitted, ProviderAttemptLaunching,
		ProviderAttemptRunning, ProviderAttemptCompleted, ProviderAttemptFailedSafe,
		ProviderAttemptFailedAmbiguous, ProviderAttemptSuperseded, ProviderAttemptAbandoned:
		return true
	default:
		return false
	}
}

// Terminal reports whether this attempt will never move again.
func (s ProviderAttemptState) Terminal() bool {
	switch s {
	case ProviderAttemptCompleted, ProviderAttemptFailedSafe, ProviderAttemptFailedAmbiguous,
		ProviderAttemptSuperseded, ProviderAttemptAbandoned:
		return true
	default:
		return false
	}
}

// Authoritative reports whether an attempt in this state is the one currently
// entitled to act for its obligation: to launch, to mutate, to write a
// completion, to release its own capacity or branch authority.
//
// Every terminal state answers false, which is what makes "a stale provider
// cannot regain authority" a property of the state machine rather than of a
// caller remembering to check.
func (s ProviderAttemptState) Authoritative() bool {
	switch s {
	case ProviderAttemptPlanned, ProviderAttemptAdmitted, ProviderAttemptLaunching, ProviderAttemptRunning:
		return true
	default:
		return false
	}
}

// ProviderAttempt is one durable attempt to discharge one obligation with one
// provider.
type ProviderAttempt struct {
	// ID is unique and durable. It is minted before the attempt is written and
	// never reused, so "which attempt launched this runtime" has one answer
	// forever.
	ID string

	// The OBLIGATION. None of these change across a failover.
	WorkflowRunID       string
	WorkflowStepID      string
	TaskID              string
	ProjectID           string
	LifecycleGeneration int64

	// PlacementGeneration binds the attempt to the frozen placement it is
	// entitled to touch. An attempt naming a superseded placement generation is
	// refused everywhere, which is what stops a stale provider writing into a
	// replacement worktree.
	PlacementGeneration int64

	// Ordinal is how many hops from the preferred provider this attempt is:
	// 1 for the preferred provider, 2 for its first fallback, and so on. It is
	// what the failover budget counts, and it is unique per obligation.
	Ordinal int64
	// Provider is the harness; Profile names the specific provider profile when
	// one is resolved.
	Provider AgentHarness
	Profile  ProviderProfileID

	State ProviderAttemptState
	// FailureReason is the human-readable cause; FailureClass is the
	// classification the routing/health policy already uses.
	FailureReason string
	FailureClass  WorkflowErrorClass
	// Safety is the durable failover classification, taken at the moment the
	// evidence existed.
	Safety FailoverSafety
	// MutationEvidenceDigest is the workspace fingerprint that PROVES a
	// safe_after_proven_no_mutation classification. Empty for every other
	// class, and its absence is what makes that class unclaimable without
	// proof.
	MutationEvidenceDigest string

	// RuntimeSessionID names the runtime this attempt launched, if it got that
	// far. CapacityClaimID names the slot that authorized the launch.
	RuntimeSessionID string
	CapacityClaimID  string

	// PredecessorAttemptID and SuccessorAttemptID chain the failover, so the
	// ledger reads as one obligation moving rather than as unrelated attempts.
	PredecessorAttemptID string
	SuccessorAttemptID   string

	CreatedAt  time.Time
	UpdatedAt  time.Time
	TerminalAt *time.Time
}

// ProviderFailoverBudget is the durable bound on how far one obligation may be
// routed. It is persisted with the run's policy rather than held in memory, so
// a restart cannot reset it -- which is what stops an A->B->A->B loop across
// daemon boots.
type ProviderFailoverBudget struct {
	// Preferred is the first provider the obligation is offered to.
	Preferred AgentHarness
	// FallbackOrder is the remaining providers in policy order.
	FallbackOrder []AgentHarness
	// MaxFailovers is how many HOPS are permitted, so the maximum ordinal is
	// MaxFailovers+1.
	MaxFailovers int
	// CurrentOrdinal is the highest ordinal recorded for this obligation.
	CurrentOrdinal int
}

// Exhausted reports whether the budget forbids another hop.
//
// The maximum ordinal is MaxFailovers+1 -- the preferred provider is ordinal 1
// and is not itself a failover -- so the budget is spent once the current
// ordinal has passed MaxFailovers.
func (b ProviderFailoverBudget) Exhausted() bool {
	return b.CurrentOrdinal > b.MaxFailovers
}

// NextOrdinal is the ordinal a new attempt would take.
func (b ProviderFailoverBudget) NextOrdinal() int { return b.CurrentOrdinal + 1 }
