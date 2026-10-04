package projectmemory

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// insight_pass.go — where the high-level facts are derived from, and when.
//
// This is the join between the two lifecycles P4-G left separate. The Code
// Graph reaches READY on its own schedule; the memory pass runs on its own.
// Rather than adding a third lifecycle for insights, a memory pass simply
// READS whatever the graph has when it runs:
//
//   - graph ready   -> graph-backed and scan-backed facts, both derived
//   - graph absent,
//     unbuilt or
//     failing       -> scan-backed facts only, and the outcome says why
//
// That is what makes the ordering requirement in §10 a preference rather than
// a precondition. A first task that arrives before the graph is built still
// gets memory; it gets less of it, and the missing half arrives at the next
// pass. Nothing waits, and nothing is wrong in the meantime — a fact that was
// never derived is absent, never a placeholder.
//
// The generation fence is the reason this lives INSIDE a pass rather than in a
// job of its own. Insight items are written at the pass's generation, so they
// are retired by the same sweep, invalidated by the same path rules and
// refused by the same compare-and-set as every other fact. A separate writer
// would need its own copy of all three, and the copy would be the thing that
// drifts.

// insightOutcome reports what the high-level derivation did, so a pass can
// state it rather than leave a caller to infer it from item counts.
type insightOutcome struct {
	// Derived is how many high-level facts the derivation produced. It is the
	// number offered to the store, not the number the store wrote: an
	// unchanged fact is reconfirmed, and that is counted in the pass's own
	// ItemsReconfirmed like every other reconfirmation.
	Derived int
	// GraphBacked reports whether the code graph contributed. False means the
	// facts are the scan-backed subset.
	GraphBacked bool
	// SkipReason explains a degraded or absent derivation in words. Empty
	// exactly when the derivation ran with the graph.
	SkipReason string
}

// graphEvidence reads the structural half of the evidence for one repository.
//
// Every failure degrades to "no graph evidence" with a stated reason, never to
// an error. A memory pass must not fail because the graph is unbuilt, mid-
// build, or broken — that would make memory strictly less available than it
// was before the graph existed, which is the opposite of the point.
func graphEvidence(
	ctx context.Context, cg CodeGraph, projectID domain.ProjectID, repoID string,
) (arch codegraph.Architecture, generation int64, commit, reason string) {
	if cg == nil {
		return codegraph.Architecture{}, 0, "", "no code graph is configured"
	}
	state, found, err := cg.Status(ctx, projectID, repoID)
	switch {
	case err != nil:
		return codegraph.Architecture{}, 0, "", fmt.Sprintf("code graph status unavailable: %v", err)
	case !found:
		return codegraph.Architecture{}, 0, "", "the code graph has not been built for this repository yet"
	case !state.Indexed():
		return codegraph.Architecture{}, 0, "", "the code graph has no completed build yet"
	}
	_, arch, ok, err := cg.Architecture(ctx, projectID, repoID)
	switch {
	case err != nil:
		return codegraph.Architecture{}, 0, "", fmt.Sprintf("code graph architecture unavailable: %v", err)
	case !ok:
		return codegraph.Architecture{}, 0, "", "the code graph holds no architecture summary for this repository"
	}
	return arch, state.ServedGeneration, state.IndexedCommit, ""
}

// deriveInsightItems assembles the evidence and produces the facts. It is the
// one function both pass types call, so a full pass and an incremental one can
// never derive a repository differently.
func deriveInsightItems(
	ctx context.Context, cg CodeGraph, base itemBase,
	projectID domain.ProjectID, repoID, repoPath, treeDigest string,
	signals *pathSignals, filesAdmitted int, scope insightScope,
) ([]domain.ProjectMemoryItem, insightOutcome) {
	arch, generation, commit, reason := graphEvidence(ctx, cg, projectID, repoID)
	ev := insightEvidence{
		RepoPath:        repoPath,
		Signals:         signals,
		Arch:            arch,
		GraphReady:      reason == "",
		GraphGeneration: generation,
		GraphCommit:     commit,
		TreeDigest:      treeDigest,
		FilesAdmitted:   filesAdmitted,
	}
	items := deriveInsights(base, ev, scope)
	return items, insightOutcome{
		Derived:     len(items),
		GraphBacked: ev.GraphReady,
		SkipReason:  reason,
	}
}
