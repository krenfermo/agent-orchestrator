package usage

import (
	"context"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
)

// context.go — the AO-assembled context read model.
//
// It answers "how much context did AO build for this run, what was it made of,
// how much of it came from project memory, and how much did AO avoid
// assembling" — and it answers all four WITHOUT ever touching the provider
// token ledger. The two are separate quantities and the separation is the whole
// point: adding an estimated byte-derived token count to a provider-reported
// one produces a number that is neither.
//
// Every total here is a sum of MEASURED bytes only. A dispatch that could not
// measure a size contributes nothing and increments Unmeasured, so a partial
// total is visible as a lower bound rather than reading as a complete one — the
// same discipline observe.SummarizeContextSelection already applies.

// EvidenceSource is the narrow read this file needs.
// *projectmemory.DirSource satisfies it.
type EvidenceSource interface {
	ListForRun(ctx context.Context, runKey string) ([]baseline.EvidenceRecord, int, error)
}

// ContextReader builds the context-composition view for a run.
type ContextReader struct{ source EvidenceSource }

// NewContextReader constructs the reader. A nil source yields an unrecorded
// view rather than an error: a daemon with the baseline harness switched off is
// an ordinary configuration, not a failure.
func NewContextReader(source EvidenceSource) *ContextReader {
	return &ContextReader{source: source}
}

// WorkflowRun summarizes one run's assembled context.
func (r *ContextReader) WorkflowRun(ctx context.Context, runID string) (domain.ContextCompositionView, error) {
	view := domain.ContextCompositionView{EstimateMethod: baseline.EstimateMethod}
	if r == nil || r.source == nil {
		return view, nil
	}
	records, skipped, err := r.source.ListForRun(ctx, runID)
	if err != nil {
		return view, err
	}
	view.SkippedRecords = int64(skipped)
	if len(records) == 0 {
		return view, nil
	}
	view.Recorded = true
	view.Dispatches = int64(len(records))

	byRole := map[domain.WorkflowRole]*domain.ContextRoleLine{}
	var roleOrder []domain.WorkflowRole
	bySource := map[domain.ContextSourceKind]int64{}
	memory := &domain.ContextMemoryView{}
	fallbacks := map[string]bool{}

	for _, record := range records {
		if role := record.Role; role != "" {
			line, ok := byRole[role]
			if !ok {
				line = &domain.ContextRoleLine{Role: role}
				byRole[role] = line
				roleOrder = append(roleOrder, role)
			}
			line.Dispatches++
			if bytes, ok := measured(record.Context.ContextSentBytes); ok {
				line.AssembledBytes += bytes
			}
		}
		if bytes, ok := measured(record.Context.ContextSentBytes); ok {
			view.AssembledBytes += bytes
		} else {
			view.Unmeasured++
		}
		foldMemory(record, memory, bySource, &view, fallbacks)
		foldRouting(record, bySource, &view)
	}

	// Whatever the source breakdown could not account for stays in "other"
	// rather than being spread across the named sources, so no bucket is ever
	// inflated by a figure AO did not attribute.
	var attributed int64
	for _, bytes := range bySource {
		attributed += bytes
	}
	if remainder := view.AssembledBytes - attributed; remainder > 0 {
		bySource[domain.ContextSourceOther] += remainder
	}

	view.EstimatedAssembledTokens = baseline.EstimateTokensFromBytes(view.AssembledBytes)
	view.EstimatedAvoidedTokens = baseline.EstimateTokensFromBytes(view.AvoidedAssembledBytes)
	for _, role := range roleOrder {
		line := byRole[role]
		line.EstimatedAssembledTokens = baseline.EstimateTokensFromBytes(line.AssembledBytes)
		view.ByRole = append(view.ByRole, *line)
	}
	for _, kind := range []domain.ContextSourceKind{
		domain.ContextSourceTaskSpec, domain.ContextSourceProjectMemory,
		domain.ContextSourceSharedKnowledge, domain.ContextSourceRepoContent,
		domain.ContextSourceIndexReuse, domain.ContextSourceOther,
	} {
		if bytes := bySource[kind]; bytes > 0 {
			view.BySource = append(view.BySource, domain.ContextSourceLine{
				Source: kind, Bytes: bytes,
				EstimatedTokens: baseline.EstimateTokensFromBytes(bytes),
			})
		}
	}
	for reason := range fallbacks {
		memory.FallbackReasons = append(memory.FallbackReasons, reason)
	}
	sort.Strings(memory.FallbackReasons)
	view.Memory = *memory
	return view, nil
}

func foldMemory(
	record baseline.EvidenceRecord,
	memory *domain.ContextMemoryView,
	bySource map[domain.ContextSourceKind]int64,
	view *domain.ContextCompositionView,
	fallbacks map[string]bool,
) {
	m := record.Memory
	if m == nil {
		return
	}
	if m.Mode != "" {
		memory.Mode = m.Mode
	}
	if m.Generation > memory.Generation {
		memory.Generation = m.Generation
		memory.IndexedCommit = m.IndexedCommit
	}
	memory.PackItems += int64(m.PackItems)
	memory.PackBytes += int64(m.PackBytes)
	memory.EstimatedPackTokens += int64(m.EstimatedPackTokens)
	memory.Candidates += int64(m.PackCandidates)
	memory.RejectedByBudget += int64(m.PackRejectedByBudget)
	memory.StaleExcluded += int64(m.PackStaleExcluded)
	if m.CacheHit {
		memory.CacheHits++
	} else {
		memory.CacheMisses++
	}
	if m.SyncPerformed {
		memory.Syncs++
		memory.SyncFilesRead += int64(m.SyncFilesRead)
		switch m.SyncKind {
		case "full":
			memory.FullSyncs++
		case "incremental":
			memory.IncrementalSyncs++
		case "none", "skipped", "coalesced", "":
			memory.NoOpSyncs++
		}
	} else {
		memory.NoOpSyncs++
	}
	memory.SharedCandidates += int64(m.SharedCandidates)
	memory.SharedSelected += int64(m.SharedSelected)
	memory.SharedExcluded += int64(m.SharedIrrelevantExcluded + m.SharedUnauthorizedExcluded +
		m.SupersededExcluded + m.ConflictingExcluded)
	memory.TaskLocalItems += int64(m.TaskLocalItems)
	memory.WorkflowLocalItems += int64(m.WorkflowLocalItems)
	memory.CanonicalItems += int64(m.CanonicalItems)
	if reason := strings.TrimSpace(m.FallbackReason); reason != "" {
		fallbacks[reason] = true
	}

	bySource[domain.ContextSourceTaskSpec] += int64(m.TaskBytes)
	bySource[domain.ContextSourceProjectMemory] += int64(m.PackBytes)
	bySource[domain.ContextSourceSharedKnowledge] += int64(m.KnowledgeBytes)
	bySource[domain.ContextSourceRepoContent] += int64(m.LegacyBytes)

	// DedupeSavedBytes is context memory demonstrably REPLACED — proved per
	// document, and non-zero only in "preferred" mode. It is the one memory
	// figure entitled to be called avoided, which is why the pack's own size
	// never contributes here: a pack that adds 6 KB and replaces nothing has
	// avoided nothing.
	if m.DedupeSavedBytes > 0 {
		view.AvoidedAssembledBytes += int64(m.DedupeSavedBytes)
		view.AvoidedComparable = true
	}
}

func foldRouting(
	record baseline.EvidenceRecord,
	bySource map[domain.ContextSourceKind]int64,
	view *domain.ContextCompositionView,
) {
	routing := record.Routing
	if routing == nil || !routing.Enabled {
		return
	}
	if reused, ok := measured(routing.ReusedBytes); ok {
		bySource[domain.ContextSourceIndexReuse] += reused
	}
	// The router's own comparable baseline: everything it assembled as a
	// candidate, against what it chose to send. The difference is content AO
	// built and did not hand over — an avoided AO-ASSEMBLED size, not a claim
	// about what a provider billed.
	potential, okP := measured(routing.PotentialBytes)
	selected, okS := measured(routing.SelectedBytes)
	if okP && okS && potential > selected {
		view.AvoidedAssembledBytes += potential - selected
		view.AvoidedComparable = true
	}
}

// measured returns a metric's value only when the dispatch actually MEASURED
// it. An estimated or absent metric is not summed into a byte total: these
// totals are byte counts, and every byte count in the evidence is measured or
// missing.
func measured(metric baseline.Metric) (int64, bool) {
	if metric.Value == nil || metric.Basis != baseline.BasisMeasured {
		return 0, false
	}
	return *metric.Value, true
}
