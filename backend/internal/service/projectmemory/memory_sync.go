package projectmemory

import (
	"context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	pm "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// memory_sync.go -- the one call that brings a repository's durable memory up
// to its checkout (P4-H §3).
//
// WHY THIS EXISTS AT ALL, which is also the P4-H audit's finding. Every piece
// of machinery memory needed already shipped: a bounded indexer, an
// incremental update, generation fencing, drift detection, a promotion path.
// What never shipped was a CALLER. `pm.Service.Sync` had exactly two entry
// points -- an operator typing `ao memory rebuild`, and the P2-B dispatch
// wrapper, which is inert unless AO_MEMORY_MODE is set away from its default
// of `off`. So on a normal installation nothing ever derived anything, and
// four real projects had zero durable facts while every subsystem reported
// itself healthy. That is the whole of the bug.
//
// THE MODE IS NOT THE SWITCH FOR THIS. AO_MEMORY_MODE governs whether memory
// is allowed to enter a DISPATCH -- whether an agent is handed a pack, and
// whether that pack may replace a legacy document. Those are consequential and
// deserve a staged rollout. Whether AO *knows things about a project it has
// imported* is a different question with a different risk profile: deriving
// facts nobody reads costs one bounded pass and can mislead nobody. Gating
// derivation behind the dispatch rollout is what made the Memory and Search
// tabs empty on every installation that had not opted into the rollout.
//
// So derivation runs regardless of mode, and mode keeps governing consumption.
// A cautious operator gets a populated Memory tab and no change to what agents
// are sent, which is the correct default for both questions.

// MemorySyncResult reports what one derivation pass did.
type MemorySyncResult struct {
	RepoID   string
	RepoPath string
	// Kind is "full" or "incremental": which route the pass took. It is
	// reported rather than inferred because the cost difference is two orders
	// of magnitude and an operator watching a slow installation needs to see
	// which one is running.
	Kind string
	// State is the lifecycle state after the pass.
	State MemoryLifecycleState

	Generation       int64
	IndexedCommit    string
	ItemsWritten     int
	ItemsReconfirmed int
	ItemsInvalidated int64
	RelationsWritten int
	// Insights are the P4-H high-level facts: how many were derived, whether
	// the code graph contributed, and why it did not when it did not.
	InsightsDerived     int
	InsightsGraphBacked bool
	InsightsSkipReason  string

	Skipped    bool
	SkipReason string
	Duration   time.Duration
}

// DeriveMemory derives or refreshes one repository's durable project memory.
//
// With full false it takes the cheapest route it can prove is correct: an
// incremental update from the last indexed commit when git can supply a
// trustworthy change set, a full pass otherwise. With full true it forces a
// re-derivation of every admitted file, which is the operator's answer to
// memory that is wrong in a way an incremental pass cannot notice.
//
// It never blocks on the code graph. A repository whose graph is not built
// derives the scan-backed facts now and picks up the graph-backed ones at the
// next pass -- see insight_pass.go, and §10's requirement that a first task
// arriving early is served rather than made to wait.
func (s *Service) DeriveMemory(
	ctx context.Context, projectID domain.ProjectID, repoPath string, full bool,
) (MemorySyncResult, error) {
	if s.memory == nil {
		return MemorySyncResult{}, fmt.Errorf("project memory is not configured for this daemon")
	}
	resolved, err := s.resolveRepo(ctx, projectID, repoPath)
	if err != nil {
		return MemorySyncResult{}, err
	}
	commit, branch := pm.HeadOf(ctx, resolved)
	started := time.Now()

	out := MemorySyncResult{RepoPath: resolved}
	if full {
		full, err := s.memory.Rebuild(ctx, projectID, resolved, commit, branch, false)
		if err != nil {
			return MemorySyncResult{}, err
		}
		out.Kind = "full"
		out.RepoID = full.RepoID
		out.Generation = full.Generation
		out.IndexedCommit = full.IndexedCommit
		out.ItemsWritten = full.ItemsWritten
		out.ItemsReconfirmed = full.ItemsReconfirmed
		out.ItemsInvalidated = full.ItemsInvalidated
		out.RelationsWritten = full.RelationsWritten
		out.InsightsDerived = full.InsightsDerived
		out.InsightsGraphBacked = full.InsightsGraphBacked
		out.InsightsSkipReason = full.InsightsSkipReason
		out.Skipped, out.SkipReason = full.Skipped, full.SkipReason
	} else {
		upd, err := s.memory.Sync(ctx, projectID, resolved, commit, branch)
		if err != nil {
			return MemorySyncResult{}, err
		}
		out.Kind = "incremental"
		if upd.FellBackToFullIndex {
			out.Kind = "full"
		}
		out.RepoID = upd.RepoID
		out.Generation = upd.Generation
		out.IndexedCommit = upd.IndexedCommit
		out.ItemsWritten = upd.ItemsWritten
		out.ItemsReconfirmed = upd.ItemsReconfirmed
		out.ItemsInvalidated = upd.ItemsInvalidated
		out.RelationsWritten = upd.RelationsWritten
		out.InsightsDerived = upd.InsightsDerived
		out.InsightsGraphBacked = upd.InsightsGraphBacked
		out.InsightsSkipReason = upd.InsightsSkipReason
		out.Skipped, out.SkipReason = upd.Skipped, upd.SkipReason
	}
	out.Duration = time.Since(started)
	if _, state, ok := s.memoryLifecycle(ctx, projectID, resolved); ok {
		out.State = state
	}
	return out, nil
}

// MemorySync satisfies controllers.ProjectIntelligenceMemoryService, so the
// Intelligence page's one refresh action covers both halves of what AO knows.
//
// It is a thin projection of the method above rather than a second
// implementation: the reconciler and the button must run the same pass, or
// "it works when I press refresh" becomes a real and undiagnosable difference.
func (s *Service) MemorySync(
	ctx context.Context, projectID domain.ProjectID, repoPath string, full bool,
) (controllers.ProjectIntelligenceMemorySync, error) {
	out, err := s.DeriveMemory(ctx, projectID, repoPath, full)
	if err != nil {
		return controllers.ProjectIntelligenceMemorySync{}, err
	}
	return controllers.ProjectIntelligenceMemorySync{
		Kind: out.Kind, State: string(out.State),
		Generation: out.Generation, IndexedCommit: out.IndexedCommit,
		ItemsWritten: out.ItemsWritten, ItemsReconfirmed: out.ItemsReconfirmed,
		ItemsInvalidated:    out.ItemsInvalidated,
		InsightsDerived:     out.InsightsDerived,
		InsightsGraphBacked: out.InsightsGraphBacked,
		InsightsSkipReason:  out.InsightsSkipReason,
		Skipped:             out.Skipped, SkipReason: out.SkipReason,
		Millis: out.Duration.Milliseconds(),
	}, nil
}
