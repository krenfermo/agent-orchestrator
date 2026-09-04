package projectmemory

import (
	stdctx "context"
	"fmt"
	"strings"
)

// memory.go — what project memory decided for one dispatch (P2-B §8, §17).
//
// This is an ADDITIVE extension of the evidence schema, in the same shape and
// for the same reason as RoutingMetrics: the field is a pointer with omitempty,
// so a dispatch with no memory story produces byte-for-byte the record this
// schema always produced, EvidenceSchemaVersion is unchanged, and a consumer
// that predates P2-B keeps reading every record.
//
// The question it exists to answer is the one an operator actually asks:
// **why did this role receive this context?** Answering that needs the mode
// that was in force, the memory's provenance, whether a sync happened and of
// what kind, what the pack cost, what was deduplicated against it, and — when
// memory contributed nothing — why not.
//
// One honesty rule governs every field here, and it is the same rule that
// governs the P2-A baseline: these numbers describe **AO-assembled context
// only**. AO does not observe what a coding harness reads inside the worktree
// (see docs/p2-project-memory-audit.md §1), so no field here is, or may be
// reported as, a saving in agent-side file reads.

// MemoryMetrics is the project-memory story of one dispatch.
type MemoryMetrics struct {
	// Mode is the rollout stage in force ("off", "assisted", "preferred").
	Mode string `json:"mode"`
	// Role is the pack role this context was assembled for.
	Role string `json:"role,omitempty"`

	// Generation and IndexedCommit are the provenance of the memory used. An
	// empty commit means AO had no memory it could vouch for.
	Generation    int64  `json:"generation"`
	IndexedCommit string `json:"indexedCommit,omitempty"`

	// SyncPerformed reports whether this dispatch caused any sync work at all,
	// and SyncKind says what kind ("none", "incremental", "full", "coalesced",
	// "skipped"). The pair is what proves a warm project's normal path is a
	// no-op rather than a scan.
	SyncPerformed bool   `json:"syncPerformed"`
	SyncKind      string `json:"syncKind,omitempty"`
	// SyncFilesRead counts paths the sync opened. Zero on the warm path.
	SyncFilesRead int `json:"syncFilesRead"`
	// SyncMillis is how long the freshness check took, waiting included.
	SyncMillis int64 `json:"syncMillis"`

	// PackItems and PackBytes describe the memory actually attached.
	PackItems int `json:"packItems"`
	PackBytes int `json:"packBytes"`
	// PackCandidates and PackRejectedByBudget describe what selection had to
	// choose from and what the budget excluded — the pair that shows a budget
	// is doing work rather than never binding.
	PackCandidates       int `json:"packCandidates"`
	PackRejectedByBudget int `json:"packRejectedByBudget"`
	PackReducedToSummary int `json:"packReducedToSummary"`
	PackStaleExcluded    int `json:"packStaleExcluded"`
	EstimatedPackTokens  int `json:"estimatedPackTokens"`
	EstimatedInputTokens int `json:"estimatedInputTokens"`

	// LegacyBytes is what the dispatch's pre-existing context sources weighed
	// before memory touched them; TaskBytes is the task/objective text, which
	// memory never replaces.
	LegacyBytes int `json:"legacyBytes"`
	TaskBytes   int `json:"taskBytes"`
	// DedupeSavedBytes is legacy context memory demonstrably covered and
	// therefore replaced. It is non-zero only in "preferred" mode, and only
	// for sources whose equivalence was proved per document — never on the
	// strength of the mode.
	DedupeSavedBytes int `json:"dedupeSavedBytes"`
	// ContextBytes is the total AO-assembled context after memory: legacy that
	// survived, plus the pack, plus the task text.
	ContextBytes int `json:"contextBytes"`
	// FallbackBytes is the legacy context AO sent because memory could not be
	// used. It is the honest counterpart to DedupeSavedBytes.
	FallbackBytes int `json:"fallbackBytes"`

	// --- code graph (the structural source category) --------------------
	//
	// Additive and omitempty, for the same reason the shared-knowledge block
	// below is: a dispatch with no code graph produces byte-for-byte the record
	// it produced before this phase, and a consumer that predates it keeps
	// reading every record.
	//
	// The pair that carries the argument is GraphSymbolsConsidered against
	// GraphSymbolsSelected. "The graph contributed 1,800 bytes" says nothing on
	// its own; "it considered 340 symbols and sent 24" is a bounded retrieval
	// doing its job, and "it considered 340 and sent 340" is a budget that
	// never binds.

	// GraphBackend names the implementation that produced the graph. It is
	// reported by its real name and never as a vendor's -- see
	// docs/usage-accounting.md on why LocalGraph is not called Graphify.
	GraphBackend string `json:"graphBackend,omitempty"`
	// GraphGeneration and GraphIndexedCommit are the graph's provenance, which
	// may legitimately differ from the memory generation above: the two halves
	// are synced together but versioned separately.
	GraphGeneration    int64  `json:"graphGeneration,omitempty"`
	GraphIndexedCommit string `json:"graphIndexedCommit,omitempty"`
	// GraphSyncKind is what the graph sync had to do ("full", "incremental",
	// "noop"), and GraphFilesParsed against GraphFilesReused is the evidence
	// that an incremental sync costs a file's work rather than a repository's.
	GraphSyncKind    string `json:"graphSyncKind,omitempty"`
	GraphFilesParsed int    `json:"graphFilesParsed,omitempty"`
	GraphFilesReused int    `json:"graphFilesReused,omitempty"`
	GraphSyncMillis  int64  `json:"graphSyncMillis,omitempty"`
	// GraphSymbols and GraphEdges size the served graph.
	GraphSymbols int `json:"graphSymbols,omitempty"`
	GraphEdges   int `json:"graphEdges,omitempty"`
	// What the graph contributed to THIS dispatch's context.
	GraphSymbolsConsidered int `json:"graphSymbolsConsidered,omitempty"`
	GraphSymbolsSelected   int `json:"graphSymbolsSelected,omitempty"`
	GraphEdgesConsidered   int `json:"graphEdgesConsidered,omitempty"`
	GraphEdgesSelected     int `json:"graphEdgesSelected,omitempty"`
	// GraphBytes is what the rendered graph section weighs, and
	// EstimatedGraphTokens is that at the shared four-bytes-per-token
	// estimate.
	//
	// It is IN ADDITION to PackBytes, not a subset of it: PackBytes counts the
	// durable facts selection chose, and the graph is a separate source
	// category with its own budget. ContextBytes below includes both, so the
	// total stays the honest one.
	GraphBytes           int `json:"graphBytes,omitempty"`
	EstimatedGraphTokens int `json:"estimatedGraphTokens,omitempty"`
	// GraphFallbackReason says why the graph contributed nothing, when it
	// contributed nothing.
	GraphFallbackReason string `json:"graphFallbackReason,omitempty"`

	// CacheHit reports whether the pack came from the pack cache.
	CacheHit bool `json:"cacheHit"`
	// CacheKey is the authority the pack was cached under, for diagnosing a
	// suspected stale reuse. It is a digest, not the content.
	CacheKey string `json:"cacheKey,omitempty"`

	// --- shared task knowledge (P2-C §18) --------------------------------
	//
	// Additive, and omitempty for the same reason the whole struct is: a
	// dispatch with no shared knowledge produces byte-for-byte the record it
	// produced before P2-C, and a consumer that predates P2-C keeps reading
	// every record.
	//
	// The pair that carries the argument is SharedCandidates against
	// SharedSelected. A task working in the same area as an earlier one should
	// show candidates it considered and took; an unrelated task should show
	// candidates it considered and excluded. Neither claim can be made from a
	// single number, which is why both are here.

	// SharedCandidates counts task-produced facts selection was allowed to
	// choose from, and SharedSelected counts what the dispatch received.
	SharedCandidates int `json:"sharedCandidates,omitempty"`
	SharedSelected   int `json:"sharedSelected,omitempty"`
	// SharedIrrelevantExcluded counts task-produced facts withheld for having
	// no bearing on this work. It is the number that proves an unrelated task
	// received nothing rather than everything.
	SharedIrrelevantExcluded int `json:"sharedIrrelevantExcluded,omitempty"`
	// SharedUnauthorizedExcluded counts task-produced facts this reader was
	// not entitled to — another task's unintegrated view, or a sibling's
	// workflow-local knowledge.
	SharedUnauthorizedExcluded int `json:"sharedUnauthorizedExcluded,omitempty"`
	// SupersededExcluded and ConflictingExcluded count facts withheld because
	// they are no longer current, or because AO could not order them against
	// an incompatible peer.
	SupersededExcluded  int `json:"supersededExcluded,omitempty"`
	ConflictingExcluded int `json:"conflictingExcluded,omitempty"`
	// DecisionsSelected and RisksSelected break the shared knowledge down by
	// what it is.
	DecisionsSelected int `json:"decisionsSelected,omitempty"`
	RisksSelected     int `json:"risksSelected,omitempty"`
	// TaskLocalItems, WorkflowLocalItems and CanonicalItems report which scope
	// the pack's facts came from. Together they are what makes "did this task
	// read a sibling's unintegrated work" answerable after the fact — the
	// question sibling safety exists to guarantee an answer to.
	TaskLocalItems     int `json:"taskLocalItems,omitempty"`
	WorkflowLocalItems int `json:"workflowLocalItems,omitempty"`
	CanonicalItems     int `json:"canonicalItems,omitempty"`
	// KnowledgeBytes is what the task-produced facts weigh inside the pack. It
	// is a subset of PackBytes, never an addition to it: shared knowledge
	// competes inside the same budget rather than beside it (P2-C §19).
	KnowledgeBytes int `json:"knowledgeBytes,omitempty"`

	// FallbackReason is set whenever memory contributed less than it might
	// have, and says why in words an operator can act on.
	FallbackReason string `json:"fallbackReason,omitempty"`
	// PackDigest identifies the exact memory this dispatch was given. Two
	// dispatches with the same digest were given the same memory.
	PackDigest string `json:"packDigest,omitempty"`
}

// Attached reports whether memory actually contributed to this dispatch.
func (m MemoryMetrics) Attached() bool { return m.PackItems > 0 && m.PackBytes > 0 }

// ReductionPercent reports what fraction of the AO-assembled context memory
// removed, and whether the figure is meaningful at all.
//
// It is deliberately computed only against DedupeSavedBytes — context memory
// demonstrably REPLACED — and never against the pack's own size. A pack that
// adds 6 KB and replaces nothing has reduced nothing, and a metric that
// reported otherwise would be the exact dishonesty this schema exists to
// prevent.
func (m MemoryMetrics) ReductionPercent() (float64, bool) {
	before := m.ContextBytes + m.DedupeSavedBytes
	if before <= 0 || m.DedupeSavedBytes <= 0 {
		return 0, false
	}
	return float64(m.DedupeSavedBytes) / float64(before) * 100, true
}

// Validate rejects a record that cannot be true, so a miscounted metric is
// caught where it is produced rather than believed downstream.
func (m MemoryMetrics) Validate() error {
	for _, f := range []struct {
		name string
		v    int
	}{
		{"packItems", m.PackItems}, {"packBytes", m.PackBytes},
		{"packCandidates", m.PackCandidates}, {"packRejectedByBudget", m.PackRejectedByBudget},
		{"legacyBytes", m.LegacyBytes}, {"taskBytes", m.TaskBytes},
		{"dedupeSavedBytes", m.DedupeSavedBytes}, {"contextBytes", m.ContextBytes},
		{"fallbackBytes", m.FallbackBytes}, {"estimatedPackTokens", m.EstimatedPackTokens},
	} {
		if f.v < 0 {
			return fmt.Errorf("memory metrics: %s is negative (%d)", f.name, f.v)
		}
	}
	if m.PackItems > 0 && m.PackCandidates > 0 && m.PackItems > m.PackCandidates {
		return fmt.Errorf("memory metrics: %d items selected from %d candidates", m.PackItems, m.PackCandidates)
	}
	if m.SharedSelected > m.PackItems {
		return fmt.Errorf("memory metrics: %d shared items inside a %d-item pack", m.SharedSelected, m.PackItems)
	}
	if m.KnowledgeBytes > m.PackBytes {
		// Shared knowledge competes INSIDE the pack budget. A knowledge figure
		// larger than the pack would mean it had been counted as an addition
		// to the context rather than a part of it, which is the exact
		// misreading P2-C §19 forbids.
		return fmt.Errorf("memory metrics: %d knowledge bytes inside a %d-byte pack", m.KnowledgeBytes, m.PackBytes)
	}
	if strings.TrimSpace(m.Mode) == "" {
		return fmt.Errorf("memory metrics: mode is required")
	}
	return nil
}

// Summary renders the record as one operator-readable line.
func (m MemoryMetrics) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "mode=%s role=%s sync=%s", m.Mode, orDash(m.Role), orDash(m.SyncKind))
	if m.SyncFilesRead > 0 {
		fmt.Fprintf(&b, "(%d files)", m.SyncFilesRead)
	}
	fmt.Fprintf(&b, " pack=%d/%dB(~%dt)", m.PackItems, m.PackBytes, m.EstimatedPackTokens)
	if m.PackRejectedByBudget > 0 {
		fmt.Fprintf(&b, " budgetDropped=%d", m.PackRejectedByBudget)
	}
	if m.DedupeSavedBytes > 0 {
		fmt.Fprintf(&b, " deduped=%dB", m.DedupeSavedBytes)
	}
	if m.SharedCandidates > 0 {
		fmt.Fprintf(&b, " shared=%d/%d", m.SharedSelected, m.SharedCandidates)
		if m.SharedIrrelevantExcluded > 0 {
			fmt.Fprintf(&b, "(irrelevant=%d)", m.SharedIrrelevantExcluded)
		}
	}
	if m.CacheHit {
		b.WriteString(" cache=hit")
	}
	if m.FallbackReason != "" {
		fmt.Fprintf(&b, " fallback=%q", m.FallbackReason)
	}
	return b.String()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// memoryContextKey carries a dispatch's memory metrics from the wrapper that
// assembled the context to the recorder that writes the evidence, without
// either learning about the other. It is the same mechanism the routing
// metrics already use.
type memoryContextKey struct{}

// WithMemory returns a context carrying this dispatch's memory metrics.
func WithMemory(ctx stdctx.Context, metrics MemoryMetrics) stdctx.Context {
	return stdctx.WithValue(ctx, memoryContextKey{}, metrics)
}

// MemoryFromContext reads the memory metrics a wrapper attached, if any.
func MemoryFromContext(ctx stdctx.Context) (MemoryMetrics, bool) {
	metrics, ok := ctx.Value(memoryContextKey{}).(MemoryMetrics)
	return metrics, ok
}
