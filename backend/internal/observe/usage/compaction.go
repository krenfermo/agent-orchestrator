package usage

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage/pricing"
)

// compaction.go -- reading back what the parser observed.
//
// The parser writes conversation replacements and the harness's own spend
// rollup into the source's durable state, because they are facts derived from
// that one artifact and they have to survive a restart. This file is the only
// supported way to read them out again, so the state's shape stays private to
// this package and a caller gets domain types instead of a JSON envelope.
//
// It decodes and folds; it never parses a transcript and never touches a file.

// CompactionObservations is one source's P7.1 observations.
type CompactionObservations struct {
	// Boundaries are the conversation replacements this source reported, in
	// the order they were observed.
	Boundaries []domain.CompactionBoundary
	// Count is how many boundaries the source reported, which can exceed
	// len(Boundaries) when a transcript reported more than the bounded list
	// keeps. Dropped says how many are missing from the detail.
	Count   int
	Dropped int
	// Harness is the self-reported spend rollup. Observed=false means the
	// harness has not written one yet, which is the normal state of a session
	// that is still running.
	Harness domain.HarnessSessionTotals
}

// Empty reports whether the source observed nothing at all -- the state of
// every source that predates P7.1 and of every session that never compacted.
func (o CompactionObservations) Empty() bool {
	return o.Count == 0 && len(o.Boundaries) == 0 && !o.Harness.Observed
}

// ExtractCompactionObservations reads one source's observations out of its
// durable parser state.
//
// A source whose state is absent, empty, malformed, or written by a build that
// had never heard of compaction observations returns an empty result and no
// error. That is not leniency for its own sake: this read is reporting, and a
// reporting path that fails a whole session because one source's state could
// not be decoded would turn a cosmetic gap into an outage. The ingest path
// keeps its own strict decode, and that is the one that matters.
func ExtractCompactionObservations(source domain.UsageSourceRecord, sessionID domain.SessionID, harness domain.AgentHarness) CompactionObservations {
	var out CompactionObservations
	if strings.TrimSpace(source.ParserStateJSON) == "" {
		return out
	}
	state, err := decodeParserState(source)
	if err != nil || state == nil || state.Claude == nil {
		return out
	}
	claude := state.Claude
	out.Count = claude.CompactionCount
	out.Dropped = claude.CompactionsDropped
	for _, observed := range claude.Compactions {
		boundary := domain.CompactionBoundary{
			RecordUUID: observed.UUID,
			SessionID:  string(sessionID),
			Harness:    harness,
			ModelID:    observed.ModelID,
			Trigger:    domain.NormalizeCompactionTrigger(observed.Trigger),
			ObservedAt: parseObservedAt(observed.Timestamp),

			PreTokens:               observed.PreTokens,
			PostTokens:              observed.PostTokens,
			CumulativeDroppedTokens: observed.CumulativeDroppedTokens,
			DurationMs:              observed.DurationMs,
		}
		out.Boundaries = append(out.Boundaries, boundary)
	}
	if rollup := claude.HarnessTotals; rollup != nil {
		out.Harness = domain.HarnessSessionTotals{
			Observed:            true,
			ReportedCostUSD:     rollup.TotalCostUSD,
			AnyUnknownModelCost: rollup.AnyUnknownModelCost,
		}
		for _, model := range rollup.Models {
			out.Harness.Models = append(out.Harness.Models, domain.HarnessModelTotals{
				ModelID: model.ModelID,
				Tokens: domain.UsageTokenTotals{
					// InputTokens is the whole input the harness re-read:
					// uncached, cache reads and cache writes together, which is
					// the same convention model_usage_events.input_tokens
					// follows. Adding the three here rather than trusting a
					// fourth reported number keeps both sides of the
					// reconciliation defined identically.
					InputTokens:         model.InputTokens + model.CacheReadTokens + model.CacheWriteTokens,
					UncachedInputTokens: model.InputTokens,
					CacheReadTokens:     model.CacheReadTokens,
					CacheWriteTokens:    model.CacheWriteTokens,
					OutputTokens:        model.OutputTokens,
				},
				ThinkingTokens:  model.ThinkingTokens,
				ReportedCostUSD: model.ReportedCostUSD,
			})
		}
	}
	return out
}

// MergeCompactionObservations folds several sources of one session together.
//
// A session can have more than one source -- a replaced artifact, a subagent
// transcript. Boundaries concatenate (each carries its own uuid, and the parser
// already refused to record one twice within a source). The rollup does NOT
// sum: it is a per-session cumulative figure written by the main transcript, so
// the richest one observed wins and two of them are never added.
func MergeCompactionObservations(parts ...CompactionObservations) CompactionObservations {
	var out CompactionObservations
	seen := map[string]bool{}
	for _, part := range parts {
		for _, boundary := range part.Boundaries {
			if boundary.RecordUUID != "" {
				if seen[boundary.RecordUUID] {
					continue
				}
				seen[boundary.RecordUUID] = true
			}
			out.Boundaries = append(out.Boundaries, boundary)
		}
		out.Count += part.Count
		out.Dropped += part.Dropped
		if part.Harness.Observed && len(part.Harness.Models) > len(out.Harness.Models) {
			out.Harness = part.Harness
		}
	}
	return out
}

// --- the read model ---------------------------------------------------------
//
// It lives beside the extractor rather than in service/usage for a structural
// reason, not a stylistic one: service/usage is already imported BY this
// package (the ingestor uses it), so a reader there that needed the extractor
// would close an import cycle. The observations are this package's own derived
// state, so the fold over them belongs here.
//
// Read-only and side-effect free. It answers a question and decides nothing:
// no caller may change a lifecycle action, a threshold or a dispatch with it,
// and nothing in the compaction path consults it.
//
// It needs NO new query. A session's bindings, each binding's sources and the
// session's per-model aggregate are three store reads that already exist, and
// the observations ride in the source's own durable parser state -- so the
// whole read model is a fold over rows AO already had, with no migration and
// no schema change.

// CompactionStore is the narrow read the accounting needs. *sqlite.Store
// satisfies it with methods that predate this checkpoint.
type CompactionStore interface {
	ListUsageBindingsForSession(ctx context.Context, sessionID domain.SessionID) ([]domain.UsageBindingRecord, error)
	ListUsageSourcesForBinding(ctx context.Context, bindingID int64) ([]domain.UsageSourceRecord, error)
	ListUsageModelAggregates(ctx context.Context, sessionID domain.SessionID) ([]domain.UsageModelAggregate, error)
}

// CompactionReader builds one session's compaction accounting.
type CompactionReader struct {
	store  CompactionStore
	prices *pricing.Table
}

// NewCompactionReader constructs the reader. prices may be nil, in which case
// every token figure is still reported and every cost is unknown -- the same
// degradation P3-E requires of every other read model.
func NewCompactionReader(s CompactionStore, prices *pricing.Table) *CompactionReader {
	return &CompactionReader{store: s, prices: prices}
}

// SessionCompactionAccounting reports one session's compactions and the gap
// between what the harness charged itself and what AO attributed.
func (r *CompactionReader) SessionCompactionAccounting(ctx context.Context, sessionID domain.SessionID) (domain.CompactionAccounting, error) {
	if r == nil || r.store == nil {
		return domain.CompactionAccounting{}, fmt.Errorf("compaction reader is not configured")
	}
	bindings, err := r.store.ListUsageBindingsForSession(ctx, sessionID)
	if err != nil {
		return domain.CompactionAccounting{}, fmt.Errorf("list usage bindings: %w", err)
	}
	var parts []CompactionObservations
	for _, binding := range bindings {
		sources, err := r.store.ListUsageSourcesForBinding(ctx, binding.ID)
		if err != nil {
			return domain.CompactionAccounting{}, fmt.Errorf("list usage sources: %w", err)
		}
		for _, source := range sources {
			observed := ExtractCompactionObservations(source, sessionID, binding.Harness)
			if observed.Empty() {
				continue
			}
			parts = append(parts, observed)
		}
	}
	merged := MergeCompactionObservations(parts...)

	aggregates, err := r.store.ListUsageModelAggregates(ctx, sessionID)
	if err != nil {
		return domain.CompactionAccounting{}, fmt.Errorf("aggregate session usage: %w", err)
	}
	attributed := make([]domain.ModelUsageLine, 0, len(aggregates))
	for _, aggregate := range aggregates {
		attributed = append(attributed, domain.ModelUsageLine{
			Harness: string(aggregate.Harness),
			ModelID: aggregate.ModelID,
			Tokens: domain.UsageTokenTotals{
				InputTokens:         aggregate.Tokens.InputTokens,
				UncachedInputTokens: aggregate.Tokens.UncachedInputTokens,
				CacheReadTokens:     aggregate.Tokens.CacheReadTokens,
				CacheWriteTokens:    aggregate.Tokens.CacheWriteTokens,
				OutputTokens:        aggregate.Tokens.OutputTokens,
			},
			Source: domain.TokenSourceProvider,
		})
	}
	var pricer domain.ModelTokenPricer
	if r.prices != nil {
		pricer = r.prices
	}
	accounting := domain.BuildCompactionAccounting(domain.CompactionAccountingInput{
		SessionID:  string(sessionID),
		Attributed: attributed,
		Harness:    merged.Harness,
		Boundaries: merged.Boundaries,
	}, pricer)
	// The bounded detail list can hold fewer boundaries than the source
	// counted. Compactions is the COUNT, so a session that compacted more
	// times than the list keeps still reports the right number of them with an
	// incomplete set of details.
	if merged.Count > accounting.Compactions {
		accounting.Compactions = merged.Count
	}
	return accounting, nil
}
