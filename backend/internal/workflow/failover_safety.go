package workflow

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// failover_safety.go — the mutation-safety rule provider failover obeys, and
// (as of this pass) the mid-execution case that had no rule at all.
//
// Two thirds of this was already true before P1-D, structurally, in
// dispatch.go's launch path:
//
//   - the launcher either returns a session record, or returns an error having
//     created none. That is a documented pre-work property, and it is what
//     lets a failed launch be retried on another provider at all.
//   - a launcher that answers "fine" and names no session is the ONE ambiguous
//     launch case, and it does not fail over. It records an ambiguous-launch
//     boundary and stops; adoptOrMarkAmbiguous later resolves it from evidence,
//     adopting what is really there or escalating, and never launching a second
//     worker over the first.
//
// What was missing first was a NAME -- "AO does not fail over after an
// ambiguous launch" was true because of where a `return` sits, which no
// operator can read and no test can point at. What was missing after that was
// the other half of the problem: a provider that got PAST the launch, had a
// runtime, and then died. There was no rule for it, so every such failure
// stopped the run whether or not the provider had touched anything.
//
// ClassifyMidExecutionFailoverSafety is that rule, and it is deliberately the
// strictest one in the file: it fails over only on MutationProof.Proven(),
// which is an AND over five durable facts. A clean workspace probe is one of
// them and never all of them.

// FailoverSafety and its four values now live in `domain`, because P1-D made
// them DURABLE: the classification is written onto a provider-attempt row at
// the moment the evidence exists, and read back afterwards rather than
// recomputed against a world that has since moved. The aliases below keep this
// package's vocabulary unchanged for every existing caller and test.
type FailoverSafety = domain.FailoverSafety

const (
	// FailoverSafeBeforeExecution is a launch that failed with a classified
	// error and created nothing.
	FailoverSafeBeforeExecution = domain.FailoverSafeBeforeExecution
	// FailoverSafeAfterProvenNoMutation is a launch that got further but whose
	// workspace AO can POSITIVELY prove is unchanged. See ProveNoMutation: the
	// proof is five conditions, and a clean `git status` is one of them, never
	// all of them.
	FailoverSafeAfterProvenNoMutation = domain.FailoverSafeAfterProvenNoMutation
	// FailoverAmbiguousExecution is the one that matters: AO cannot prove
	// whether the provider ran, or what it touched, so nothing fails over.
	FailoverAmbiguousExecution = domain.FailoverAmbiguousExecution
	// FailoverCompletedExecution is a provider that finished its work.
	FailoverCompletedExecution = domain.FailoverCompletedExecution
)

// ClassifyFailoverSafety names what AO can prove about one launch outcome.
//
// The inputs are exactly the facts the launch path already has:
//
//	launchErr        the launcher's error, if any
//	namedSession     whether the launcher identified a session it created
//	workspaceProven  whether AO holds POSITIVE proof the workspace is unchanged
//	                 (MutationProof.Proven(), never a bare status probe)
//
// The ordering is the safety model. Ambiguity is checked BEFORE the error
// classification, because an ambiguous launch that also carries an eligible
// error class is still ambiguous -- and treating it as a clean failure is
// precisely how a second worker gets launched over a live one.
func ClassifyFailoverSafety(launchErr error, namedSession, workspaceProven bool) FailoverSafety {
	switch {
	case launchErr == nil && namedSession:
		// The launch worked. Whatever happens next is this provider's own
		// execution, not a failover question.
		return FailoverCompletedExecution
	case launchErr == nil && !namedSession:
		// The launcher answered "fine" and named nothing. This is
		// errLaunchWithoutEvidence's shape, and it is the ambiguous case.
		return FailoverAmbiguousExecution
	case workspaceProven:
		return FailoverSafeAfterProvenNoMutation
	default:
		// A classified launch error with no session created. The launcher's
		// contract -- a record or an error having created nothing -- is what
		// makes this provable rather than assumed.
		return FailoverSafeBeforeExecution
	}
}

// ClassifyMidExecutionFailoverSafety is P1-D §H: the classification for an
// attempt that got PAST the launch and then failed.
//
// This is the case P1-D's first pass did not implement at all. Before-execution
// failover was safe because the launcher's contract made it safe; there was no
// answer for a provider that had a runtime and then died, so every such failure
// stopped the run.
//
// The rule is one sentence, and the whole file exists to make it unambiguous:
//
//	a mid-execution failure fails over ONLY on positive proof of no mutation.
//
// Not on a clean `git status`, not on an absent commit, not on a plausible
// error class. MutationProof.Proven() is an AND over five durable facts, and
// anything short of all five lands here as ambiguous_execution -- which never
// fails over.
func ClassifyMidExecutionFailoverSafety(attempt domain.ProviderAttempt, proof MutationProof) FailoverSafety {
	if attempt.State == domain.ProviderAttemptCompleted {
		return FailoverCompletedExecution
	}
	if proof.Proven() {
		return FailoverSafeAfterProvenNoMutation
	}
	return FailoverAmbiguousExecution
}

// ProviderAttemptIdentity is P1-D §S: what changes, and what deliberately does
// not, when an obligation moves between providers.
//
// The distinction it exists to hold is that a provider attempt is NOT a task
// generation. The workflow, the task, the step and the lifecycle generation are
// the obligation, and they stay exactly as they were; the provider attempt is
// how AO is currently trying to discharge it. Conflating the two would mean
// every failover looked like new work -- a fresh placement, a fresh review, a
// fresh claim -- which is both wasteful and wrong.
//
// It remains a value type rather than becoming the durable row: the row
// (domain.ProviderAttempt) is the authority, and this is the shape a caller
// hands to a decision. NewProviderAttemptIdentity below builds one FROM the
// durable record, so the two cannot drift.
type ProviderAttemptIdentity struct {
	// WorkflowRunID, WorkflowStepID and LifecycleGeneration are the
	// OBLIGATION. Failover never changes them.
	WorkflowRunID       string
	WorkflowStepID      string
	LifecycleGeneration int64
	// AttemptNumber is the provider attempt within that obligation, and
	// FailoverOrdinal is how many hops from the preferred provider it is.
	AttemptNumber   int
	FailoverOrdinal int
	// From and To name the providers, and Reason and Safety say why the hop
	// was authorized. A hop with no recorded safety is not a hop AO took.
	From    domain.AgentHarness
	To      domain.AgentHarness
	Reason  domain.WorkflowErrorClass
	Safety  FailoverSafety
	Profile domain.ProviderProfileID
}

// Authorized reports whether this identity describes a hop AO was allowed to
// make. It is the record's own consistency check: a failover that names no
// destination, or one whose safety does not permit failover, was never
// authorized however it came to be written down.
func (p ProviderAttemptIdentity) Authorized() bool {
	return p.To != "" && p.To != p.From && p.Safety.PermitsFailover()
}

// NewProviderAttemptIdentity projects the durable ledger row into the decision
// shape above, so a hop's identity is read from what was written rather than
// reconstructed alongside it.
func NewProviderAttemptIdentity(from, to domain.ProviderAttempt) ProviderAttemptIdentity {
	return ProviderAttemptIdentity{
		WorkflowRunID:       to.WorkflowRunID,
		WorkflowStepID:      to.WorkflowStepID,
		LifecycleGeneration: to.LifecycleGeneration,
		AttemptNumber:       int(to.Ordinal),
		FailoverOrdinal:     int(to.Ordinal) - 1,
		From:                from.Provider,
		To:                  to.Provider,
		Reason:              from.FailureClass,
		Safety:              from.Safety,
		Profile:             to.Profile,
	}
}
