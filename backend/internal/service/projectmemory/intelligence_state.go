package projectmemory

import "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"

// intelligence_state.go -- the one vocabulary P4-G asks for, derived rather
// than stored.
//
// The durable row already carries everything needed to answer "can AO answer
// questions about this project right now": a phase (idle/building/failed), a
// served generation, an indexed commit, and a repository identity. What it
// does NOT carry is the two states that are questions about the WORLD rather
// than about the row -- "nothing has been built yet" and "the checkout has
// moved on since it was". Those are derived here, on every read, because
// persisting them would mean a background writer had to keep them true and
// they would be wrong the moment somebody committed.
//
// This is why there is no new column and no new table in P4-G for status.

// IntelligenceState is what a person is shown, and what the reconciler acts on.
type IntelligenceState string

const (
	// IntelligencePending means nothing has been indexed yet. A project that
	// was just imported sits here until the reconciler reaches it.
	IntelligencePending IntelligenceState = "pending"
	// IntelligenceIndexing means a build is in flight. The previous complete
	// generation, if there is one, is still what readers are served.
	IntelligenceIndexing IntelligenceState = "indexing"
	// IntelligenceReady means the served graph matches the checkout AO can
	// see, at a commit and a repository identity it can still vouch for.
	IntelligenceReady IntelligenceState = "ready"
	// IntelligenceStale means there IS a graph, and AO can prove it no longer
	// describes the checkout -- the commit moved, or the repository is not the
	// one those facts came from. Deliberately distinct from ready: serving
	// stale structure as current is the one failure this subsystem refuses to
	// make, so it is named on the wire rather than smoothed over.
	IntelligenceStale IntelligenceState = "stale"
	// IntelligenceFailed means the last build ended on an error. Any
	// previously served generation is still readable; what is broken is the
	// attempt to move past it.
	IntelligenceFailed IntelligenceState = "failed"
)

// intelligenceState derives the state of one repository's graph.
//
// Order matters and is the order an operator would ask in: is something
// happening right now, did the last attempt break, has anything been built at
// all, and can AO still vouch for what it has. Building is checked first
// because a build in flight explains everything else about the row.
func intelligenceState(state store.CodeGraphState, drift string) IntelligenceState {
	switch {
	case state.Building():
		return IntelligenceIndexing
	case state.Phase == store.CodeGraphFailed:
		return IntelligenceFailed
	case !state.Indexed():
		return IntelligencePending
	case drift != "":
		return IntelligenceStale
	default:
		return IntelligenceReady
	}
}

// needsIndex reports whether the reconciler should act on this state.
//
// Failed is deliberately NOT included. A build that failed will fail again for
// the same reason, and a reconciler that retries it every tick turns one
// broken repository into a permanent busy loop and a stream of identical
// notifications. Recovering from failed is a deliberate act -- a manual sync
// or rebuild -- which is also the moment somebody is present to read the
// error.
func needsIndex(s IntelligenceState) bool {
	return s == IntelligencePending || s == IntelligenceStale
}
