package projectmemory

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	pm "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// memory_state.go -- the derivation lifecycle P4-H asks for, derived rather
// than stored.
//
// P4-G established the vocabulary for the code graph (intelligence_state.go)
// and the reasoning there applies unchanged here: the durable row already
// carries a phase, a generation and an indexed commit, and the two states that
// are questions about the WORLD rather than about the row -- "nothing has been
// derived yet" and "the checkout has moved on since it was" -- are computed on
// every read. Persisting them would mean a background writer had to keep them
// true, and they would be wrong the moment somebody committed.
//
// So P4-H adds no column and no table for memory status either. What it adds
// is the SECOND application of the same derivation, to a second subsystem,
// with the same five names -- because an operator looking at one project
// should not have to learn two vocabularies for "is this ready".
//
// One difference from the graph, and it is deliberate. The graph is stale when
// its indexed commit is not the checkout's head. Memory is stale when that is
// true OR when facts inside it went stale individually: a repository whose
// memory is at the right commit but holds twelve invalidated facts is not
// "ready", because a reader asking it a question may get nothing where it used
// to get an answer. Item state is part of the subsystem's state here in a way
// it is not for the graph, which has no per-node validity.

// MemoryLifecycleState is what a person is shown for one repository's durable
// memory, and what the reconciler acts on.
type MemoryLifecycleState string

const (
	// MemoryPending means nothing has been derived yet. A project that was
	// just imported sits here until the reconciler reaches it.
	MemoryPending MemoryLifecycleState = "pending"
	// MemoryDeriving means a pass is in flight. Whatever was already derived
	// stays readable while it runs.
	MemoryDeriving MemoryLifecycleState = "deriving"
	// MemoryReady means the derived facts describe the checkout AO can see,
	// at a commit it can still vouch for, with nothing withheld.
	MemoryReady MemoryLifecycleState = "ready"
	// MemoryStale means there ARE facts and AO can prove they no longer fully
	// describe the checkout -- the commit moved, or individual facts were
	// invalidated by changes nothing has re-derived yet. Distinct from ready
	// because serving stale knowledge as current is the failure this
	// subsystem refuses to make.
	MemoryStale MemoryLifecycleState = "stale"
	// MemoryFailed means the last pass ended on an error. Anything previously
	// derived is still readable; what is broken is the attempt to move past
	// it.
	MemoryFailed MemoryLifecycleState = "failed"
)

// memoryState derives one repository's memory lifecycle state.
//
// The order is the order an operator would ask in: is something happening
// right now, did the last attempt break, has anything been derived at all, and
// can AO still vouch for what it has.
func memoryState(status domain.ProjectMemoryStatus, head string) MemoryLifecycleState {
	switch {
	case status.Index.Phase == domain.IndexPhaseFailed:
		return MemoryFailed
	case inFlight(status.Index.Phase):
		return MemoryDeriving
	case status.Index.IndexedCommit == "" || status.Counts.Total == 0:
		return MemoryPending
	case head != "" && status.Index.IndexedCommit != head:
		return MemoryStale
	case status.Counts.Valid == 0:
		// Facts exist and none of them may be served. That is a stale
		// repository whose facts happen to have been individually
		// invalidated, and calling it ready would mean an empty pack from a
		// subsystem reporting itself healthy.
		return MemoryStale
	default:
		return MemoryReady
	}
}

// inFlight reports whether a pass is running. It is a switch over the phases
// rather than "not idle and not failed" so a phase a newer build introduces
// does not silently read as in-flight forever and block the reconciler off
// the repository.
func inFlight(phase domain.ProjectMemoryIndexPhase) bool {
	switch phase {
	case domain.IndexPhaseScanning, domain.IndexPhaseSummarizing,
		domain.IndexPhaseLinking, domain.IndexPhaseFinalizing:
		return true
	default:
		return false
	}
}

// needsDerivation reports whether the reconciler should act on this state.
//
// Failed is excluded for the same reason the graph reconciler excludes it: a
// pass that failed will fail again for the same reason, and retrying it every
// tick turns one broken repository into a permanent busy loop. Recovering from
// failed is a deliberate act -- a manual sync or rebuild -- which is also the
// moment somebody is present to read the error.
func needsDerivation(s MemoryLifecycleState) bool {
	return s == MemoryPending || s == MemoryStale
}

// memoryLifecycle reads one repository's derived state, and reports whether
// memory is wired at all.
func (s *Service) memoryLifecycle(
	ctx context.Context, projectID domain.ProjectID, repoPath string,
) (domain.ProjectMemoryStatus, MemoryLifecycleState, bool) {
	if s.memory == nil {
		return domain.ProjectMemoryStatus{}, "", false
	}
	status, found, err := s.memory.Status(ctx, projectID, repoPath)
	if err != nil {
		return domain.ProjectMemoryStatus{}, "", false
	}
	if !found {
		// No row at all is the honest "pending": the repository exists and
		// nothing has been derived from it.
		return domain.ProjectMemoryStatus{}, MemoryPending, true
	}
	head, _ := pm.HeadOf(ctx, repoPath)
	return status, memoryState(status, head), true
}

// NeedsDerivationForTest exposes the reconciler's scheduling predicate to the
// package's external test.
//
// It is exported for tests rather than the predicate itself because the
// predicate is not a decision any caller outside this package should be
// making: what the reconciler acts on is its own business, and a second caller
// applying it would be a second scheduler. What a test must be able to check
// is that a FAILED repository is never among them — a reconciler that retries
// a permanently broken project every tick starves every other project of the
// per-tick budget.
func NeedsDerivationForTest(s MemoryLifecycleState) bool { return needsDerivation(s) }
