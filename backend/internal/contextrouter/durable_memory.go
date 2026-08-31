package contextrouter

import (
	"context"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	memory "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// durable_memory.go — how P2-A's durable project memory reaches the roles.
//
// The audit (docs/p2-project-memory-audit.md §8) found that the router already
// consumes a MemorySource, and that the Planner and the Spawner surfaces are
// already routed — and that the Spawner path is also how BOTH Repair Agents
// are dispatched. So the whole of role integration for those roles is one
// adapter: present the durable store as the MemorySource the router has always
// read, and the facts flow to Planner, Worker and both repairers with no new
// dispatch surface and no change to any prompt builder.
//
// Three properties of this adapter are load-bearing:
//
//   - **Fail closed.** Only canonical, currently-valid facts are exposed. A
//     stale or invalidated fact is never handed to the router at all, and a
//     task-local fact — one task's view of unintegrated work — is filtered out
//     entirely here, because the router has no concept of which task it is
//     assembling for and could not make that distinction itself.
//   - **Fail soft.** A storage error yields no items and no error. The router
//     already degrades gracefully on a missing evidence source, and a memory
//     outage must cost context quality, never a dispatch.
//   - **Bounded.** The router ranks and packs, but it should not be handed an
//     unbounded candidate set on the dispatch path; maxDurableMemoryItems caps
//     what crosses the boundary.
//
// This adapter deliberately does NOT replace the JSON-backed store in
// Default(). Both satisfy MemorySource; which one a router gets is decided by
// its composition root, so the Phase-0 baseline harness keeps measuring what it
// always measured.

// maxDurableMemoryItems bounds what one project contributes to a routed
// payload. The router's own budget is the real constraint; this is the guard
// that keeps a pathologically large memory from being read and ranked in full
// on every dispatch.
const maxDurableMemoryItems = 500

// DurableMemorySource adapts P2-A's durable project memory to MemorySource.
type DurableMemorySource struct {
	repo memory.Repository
}

// NewDurableMemorySource wraps a durable project-memory repository as a
// router evidence source.
func NewDurableMemorySource(repo memory.Repository) *DurableMemorySource {
	return &DurableMemorySource{repo: repo}
}

// List returns the project's canonical, currently-valid facts, in the legacy
// MemoryItem shape the router ranks.
//
// It spans every repository of the project on purpose. The router keys memory
// by project id and ranks by which paths a task touched, so a multi-repo
// project's facts compete on relevance rather than on which checkout the
// dispatch happens to name — which is what a Planner working across
// repositories needs, and what a Worker's path-based ranking correctly
// suppresses.
func (s *DurableMemorySource) List(project string) ([]memory.MemoryItem, error) {
	if s == nil || s.repo == nil {
		return nil, nil
	}
	projectID := domain.ProjectID(strings.TrimSpace(project))
	if projectID == "" {
		return nil, nil
	}
	// The MemorySource contract has no context parameter — it predates this
	// adapter. Using Background here is correct rather than lazy: the read is
	// a single indexed query against local SQLite, and the router's own
	// deadline governs the dispatch it belongs to.
	items, err := s.repo.ListProjectMemoryItemsForProject(context.Background(), projectID)
	if err != nil {
		// Fail soft, deliberately. Returning the error would make the router
		// report a failed evidence source, and a memory outage must cost
		// context quality rather than a dispatch. An empty answer degrades to
		// exactly the pre-P2-A behaviour.
		return nil, nil //nolint:nilerr // an unreadable memory is an empty memory, never a failed dispatch
	}

	out := make([]memory.MemoryItem, 0, min(len(items), maxDurableMemoryItems))
	for _, item := range items {
		if len(out) >= maxDurableMemoryItems {
			break
		}
		if !item.State.Authoritative() {
			continue
		}
		if item.Origin != domain.OriginCanonical {
			// One task's unintegrated view must never become another task's
			// premise, and the router cannot tell which task it is assembling
			// for. Filtering here is the only place that distinction can be
			// made correctly.
			continue
		}
		out = append(out, legacyMemoryItem(item))
	}
	return out, nil
}

// legacyMemoryItem projects a durable fact into the MemoryItem shape the
// router's ranking already understands.
//
// The mapping is deliberately lossy in one direction only: everything the
// router ranks on (scope, type, content, path, confidence, freshness) is
// carried, and everything it does not read (generation, state reason,
// invalidation time, metadata) is dropped rather than encoded into a field
// that means something else. Stale is always false because a non-authoritative
// item never reaches this function.
func legacyMemoryItem(item domain.ProjectMemoryItem) memory.MemoryItem {
	body := item.Summary
	if item.Content != "" {
		body = item.Summary + "\n" + item.Content
	}
	scope := item.Key.Key
	if scope == "" {
		scope = string(item.Key.Scope)
	}
	source := memory.Source{
		Kind:   memory.SourceKind("project-memory"),
		Ref:    item.ID,
		Detail: "AO project memory, generation " + strconv.FormatInt(item.Generation, 10),
	}
	if len(item.SourcePaths) > 0 {
		// The router matches a single anchor path against the change set. The
		// first source path is the deterministic choice: SourcePaths is sorted
		// and de-duplicated by the domain, so "first" is stable rather than
		// arbitrary.
		source.Path = item.SourcePaths[0]
	}
	return memory.MemoryItem{
		ID:           item.ID,
		Project:      string(item.Key.ProjectID),
		Scope:        scope,
		Type:         memory.ItemType(item.Key.Type),
		Content:      body,
		Source:       source,
		CreatedAt:    item.CreatedAt,
		UpdatedAt:    item.UpdatedAt,
		SourceCommit: item.SourceCommit,
		Confidence:   item.Confidence,
		ContentHash:  item.ContentHash,
	}
}
