package domain

// P9 — worker runtime ownership.
//
// A worker session row says what AO RECORDED about a launch. It does not say
// that the runtime answering today is that launch: a session id survives the
// process behind it, a tmux session NAME can be taken by anything, and a PID is
// reused by the operating system. Ownership of a live worker is therefore only
// ever proven by reading identity back FROM THE RUNTIME and finding that it is
// equal, link by link, to what the row recorded:
//
//	runtime incarnation ($N)      == sessions.runtime_instance_id
//	runtime AO_SESSION_OWNER      == sessions.runtime_owner_token
//	                              == SessionRuntimeOwnerToken(session, runtime_launch_id)
//	runtime AO_INSTALLATION_ID    == this installation (when the runtime is stamped)
//
// WorkerRuntimeProof is the closed vocabulary of what that read-back found. It
// deliberately separates the answers that are FACTS (owned, owned but exited,
// absent) from every answer that is only an inability to tell, because
// collapsing the two is how a reconciler adopts a stranger or relaunches over a
// worker it merely could not see.

// WorkerRuntimeProof classifies one read-back of a worker session's runtime.
type WorkerRuntimeProof string

const (
	// WorkerRuntimeOwned: every identity link holds and the workload is alive
	// (or its liveness is unreadable while the exact incarnation still exists).
	WorkerRuntimeOwned WorkerRuntimeProof = "owned"
	// WorkerRuntimeOwnedExited: every identity link holds and the workload
	// process is provably gone, while the pane is still there.
	WorkerRuntimeOwnedExited WorkerRuntimeProof = "owned_exited"
	// WorkerRuntimeAbsent: the runtime answered that neither the recorded
	// incarnation nor any session under the recorded name exists.
	WorkerRuntimeAbsent WorkerRuntimeProof = "absent"
	// WorkerRuntimeInstanceMismatch: something answers under the recorded
	// name, but it is a different incarnation than the one AO recorded.
	WorkerRuntimeInstanceMismatch WorkerRuntimeProof = "instance_mismatch"
	// WorkerRuntimeOwnerMismatch: the incarnation exists and carries an
	// ownership token that is not the one this session's launch recorded.
	WorkerRuntimeOwnerMismatch WorkerRuntimeProof = "owner_mismatch"
	// WorkerRuntimeInstallationMismatch: the runtime is stamped by a different
	// AO installation than the one reading it.
	WorkerRuntimeInstallationMismatch WorkerRuntimeProof = "installation_mismatch"
	// WorkerRuntimeProvenanceMissing: the row (or the runtime) carries no
	// ownership provenance to compare — a legacy session, or a launch whose
	// runtime identity was never made durable. Never inferred, never adopted.
	WorkerRuntimeProvenanceMissing WorkerRuntimeProof = "provenance_missing"
	// WorkerRuntimeUnsupported: the runtime adapter cannot read identity back
	// (conpty/Windows today). Recovery fails closed on it.
	WorkerRuntimeUnsupported WorkerRuntimeProof = "unsupported"
	// WorkerRuntimeUnavailable: the read itself failed. Nothing is concluded.
	WorkerRuntimeUnavailable WorkerRuntimeProof = "unavailable"
)

// Valid reports membership in the closed vocabulary.
func (p WorkerRuntimeProof) Valid() bool {
	switch p {
	case WorkerRuntimeOwned, WorkerRuntimeOwnedExited, WorkerRuntimeAbsent,
		WorkerRuntimeInstanceMismatch, WorkerRuntimeOwnerMismatch, WorkerRuntimeInstallationMismatch,
		WorkerRuntimeProvenanceMissing, WorkerRuntimeUnsupported, WorkerRuntimeUnavailable:
		return true
	}
	return false
}

// OwnershipProven reports that the runtime is provably the one AO launched for
// this session, whether or not its workload is still running.
func (p WorkerRuntimeProof) OwnershipProven() bool {
	return p == WorkerRuntimeOwned || p == WorkerRuntimeOwnedExited
}

// ExecutionProvenGone reports positive evidence that the execution AO launched
// for this session is not running any more. Only two answers qualify: the
// runtime said nothing exists, or it proved the incarnation is ours and its
// workload has exited. A mismatch is NOT gone — something is there.
func (p WorkerRuntimeProof) ExecutionProvenGone() bool {
	return p == WorkerRuntimeAbsent || p == WorkerRuntimeOwnedExited
}

// Contradicted reports an answer that names a DISAGREEMENT with the durable
// record (as opposed to an inability to read one).
func (p WorkerRuntimeProof) Contradicted() bool {
	switch p {
	case WorkerRuntimeInstanceMismatch, WorkerRuntimeOwnerMismatch, WorkerRuntimeInstallationMismatch:
		return true
	}
	return false
}

// WorkerRuntimeObservation is one read-back, identities only. It never carries
// a prompt, a command, an environment, pane contents or a credential: the
// ownership token is reported as a match/mismatch, not echoed.
type WorkerRuntimeObservation struct {
	SessionID SessionID
	Proof     WorkerRuntimeProof

	// Recorded identity, straight off the session row.
	RuntimeHandleID   string
	RuntimeInstanceID string
	RuntimeLaunchID   string

	// ObservedInstanceID is the incarnation the runtime answered with, when it
	// answered with one. It differs from RuntimeInstanceID only on a mismatch.
	ObservedInstanceID string
	// ObservedInstallationID is the installation stamp read back, when present.
	ObservedInstallationID string
	// WorkloadKnown / WorkloadAlive are the runtime's own process answer.
	WorkloadKnown bool
	WorkloadAlive bool

	// Detail is a bounded, identity-only sentence for the durable record.
	Detail string
}

// WorkerOwnershipStatus is the operator-facing summary of a proof (readback).
type WorkerOwnershipStatus string

const (
	WorkerOwnershipProven        WorkerOwnershipStatus = "proven"
	WorkerOwnershipUnproven      WorkerOwnershipStatus = "unproven"
	WorkerOwnershipLegacyUnknown WorkerOwnershipStatus = "legacy_unknown"
	WorkerOwnershipNotApplicable WorkerOwnershipStatus = "not_applicable"
)

// OwnershipStatus folds the proof into the three answers a person reads.
func (p WorkerRuntimeProof) OwnershipStatus() WorkerOwnershipStatus {
	switch {
	case p.OwnershipProven():
		return WorkerOwnershipProven
	case p == WorkerRuntimeProvenanceMissing:
		return WorkerOwnershipLegacyUnknown
	case p == "":
		return WorkerOwnershipNotApplicable
	default:
		return WorkerOwnershipUnproven
	}
}
