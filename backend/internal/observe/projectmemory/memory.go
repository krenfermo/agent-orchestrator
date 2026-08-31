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

	// CacheHit reports whether the pack came from the pack cache.
	CacheHit bool `json:"cacheHit"`
	// CacheKey is the authority the pack was cached under, for diagnosing a
	// suspected stale reuse. It is a digest, not the content.
	CacheKey string `json:"cacheKey,omitempty"`

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
