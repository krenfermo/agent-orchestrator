package usage

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

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
	// CacheCreation is this source's cache creation summed per lifetime, and
	// CacheCreationTotal is the same messages' reported totals. The pair is
	// what lets a consumer check the aggregate against the ledger's own sum
	// before trusting it to apportion anything.
	CacheCreation      domain.CacheCreationSplit
	CacheCreationTotal int64
}

// Empty reports whether the source observed nothing at all -- the state of
// every source that predates P7.1 and of every session that never compacted.
func (o CompactionObservations) Empty() bool {
	return o.Count == 0 && len(o.Boundaries) == 0 && !o.Harness.Observed &&
		o.CacheCreation.Total() == 0
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
	out.CacheCreation = domain.CacheCreationSplit{
		Ephemeral5mTokens: claude.CacheCreation5m,
		Ephemeral1hTokens: claude.CacheCreation1h,
		UnknownTTLTokens:  claude.CacheCreationUnknown,
	}
	out.CacheCreationTotal = claude.CacheCreationTotal
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
		out.CacheCreation = out.CacheCreation.Add(part.CacheCreation)
		out.CacheCreationTotal += part.CacheCreationTotal
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
	// ListCompactedSessionsForSummaryPrior is the candidate list for the
	// cross-session summary prior: the sessions that actually compacted.
	ListCompactedSessionsForSummaryPrior(ctx context.Context, limit int64) ([]domain.SessionID, error)
}

// maxSummaryPriorCandidateSessions bounds the candidate list the summary prior
// reads.
//
// A prior about compaction is only ever about sessions that compacted, which is a
// handful; this ceiling exists so a corpus nobody expected cannot turn a decision
// path into an unbounded read. Reaching it is the signal to fold the prior into a
// durable aggregate instead of deriving it per decision.
const maxSummaryPriorCandidateSessions = 64

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
	// Since migration 0170 the ledger row carries the cache lifetime itself, so
	// the aggregate is authoritative and nothing has to be apportioned.
	//
	// The parser's own session aggregate stays as a FALLBACK, for the rows
	// written before that column existed: they come back as lifetime-unknown,
	// and the parser -- which re-read the same transcript -- may know better.
	// It is used only when the ledger knows nothing at all and the two agree on
	// the total, because a disagreement means they are not counting the same
	// messages and apportioning across that would invent a distribution.
	ledgerWrites, ledgerUnknown := int64(0), int64(0)
	for _, aggregate := range aggregates {
		ledgerWrites += aggregate.Tokens.CacheWriteTokens
		ledgerUnknown += aggregate.Tokens.CacheCreation.UnknownTTLTokens
	}
	splitUsable := ledgerUnknown == ledgerWrites && ledgerWrites > 0 &&
		merged.CacheCreationTotal == ledgerWrites && merged.CacheCreation.Known()
	attributed := make([]domain.ModelUsageLine, 0, len(aggregates))
	for _, aggregate := range aggregates {
		tokens := domain.UsageTokenTotals{
			InputTokens:         aggregate.Tokens.InputTokens,
			UncachedInputTokens: aggregate.Tokens.UncachedInputTokens,
			CacheReadTokens:     aggregate.Tokens.CacheReadTokens,
			CacheWriteTokens:    aggregate.Tokens.CacheWriteTokens,
			OutputTokens:        aggregate.Tokens.OutputTokens,
		}
		tokens.CacheCreation = aggregate.Tokens.CacheCreation
		if splitUsable && len(aggregates) == 1 {
			// One model, one aggregate, one transcript, and a ledger that knows
			// no lifetime for any of it: the parser's split belongs to this
			// line whole. With more than one model the parser aggregate cannot
			// say which of them each write belonged to, so the lifetime stays
			// unknown rather than being divided by a ratio nobody measured.
			tokens.CacheCreation = merged.CacheCreation
		}
		attributed = append(attributed, domain.ModelUsageLine{
			Harness: string(aggregate.Harness),
			ModelID: aggregate.ModelID,
			Tokens:  tokens,
			Source:  domain.TokenSourceProvider,
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

// --- reconstruction ---------------------------------------------------------

// ReconstructEvents re-derives every usage event a transcript describes, using
// the SAME parser the ingest path uses.
//
// It exists for one caller: the historical cache-lifetime backfill, which has
// to answer "what would AO have recorded for this artifact, had it known about
// cache lifetimes" — and the only defensible answer is the one AO's own parser
// gives. Re-deriving with a second, simpler reader would be reconstructing the
// past from a different program than the one that recorded it.
//
// Parsed from a FRESH parser state, deliberately. The stored state is a resume
// cursor for a tailer; a reconstruction reads the whole artifact from the start
// and must not inherit a half-open message from a previous batch boundary.
// Every event it returns therefore carries the same SourceEventKey the ingest
// path would have produced for it, which is the identity the caller matches on.
//
// Reads the artifact. Writes nothing, and returns no content: the events are
// numbers, a model id and a key, exactly as the ingest path produces them.
func ReconstructEvents(source domain.UsageSourceContext, r io.Reader, now time.Time) ([]domain.ModelUsageEvent, error) {
	switch source.Source.Kind {
	case domain.UsageSourceClaudeMain, domain.UsageSourceClaudeSubagent:
	default:
		return nil, fmt.Errorf("reconstruct events: unsupported source kind %q", source.Source.Kind)
	}
	state, err := newParserState(source.Source.Kind)
	if err != nil {
		return nil, fmt.Errorf("reconstruct events: %w", err)
	}
	records, offset, err := readAllJSONLRecords(r)
	if err != nil {
		return nil, fmt.Errorf("reconstruct events: %w", err)
	}
	result := parseRecordsWithState(source, records, offset, now, state)
	if result.err != nil {
		return nil, fmt.Errorf("reconstruct events: %w", result.err)
	}
	return result.Events, nil
}

// readAllJSONLRecords splits an artifact into the same per-line records the
// tailer feeds the parser. Blank lines are skipped; a line too large for the
// scanner is an error rather than a silently dropped event, because a
// reconstruction that quietly saw less than the artifact holds would under-
// report and call it a clean run.
func readAllJSONLRecords(r io.Reader) ([]jsonlRecord, int64, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), defaultRecordBytes)
	var (
		records []jsonlRecord
		offset  int64
	)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) > 0 {
			records = append(records, jsonlRecord{
				Data:   append([]byte(nil), line...),
				Offset: offset,
			})
		}
		offset += int64(len(line)) + 1
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}
	return records, offset, nil
}

// ReconstructableSourceKind reports whether ReconstructEvents can read this
// artifact format at all.
//
// A Codex rollout is not an error and not a gap: its envelope has no cache
// lifetime vocabulary, so there is nothing in it to reconstruct. A caller that
// treated it as a parser failure would report a clean run as broken.
func ReconstructableSourceKind(kind domain.UsageSourceKind) bool {
	switch kind {
	case domain.UsageSourceClaudeMain, domain.UsageSourceClaudeSubagent:
		return true
	default:
		return false
	}
}

// CompactionSummaryObservations builds the cross-session evidence the shadow
// economic gate's summary-cost prior is derived from.
//
// WHAT IT IS. One observation per session that has compacted and whose harness
// wrote its end-of-session rollup: how many times it compacted, and how many
// output tokens the harness charged itself that AO's ledger holds no event for.
// That residual is the summarization -- a summary is generated, and the
// generation reaches no assistant record -- and it INCLUDES reasoning, which is
// most of what a summarizer produces and none of what its text contains.
//
// WHY IT IS A RESIDUAL RATHER THAN A MEASUREMENT. The harness reports its spend
// once per session, not once per compaction, so there is no per-compaction output
// figure to read. The residual is an UPPER BOUND on the summarization, which is
// exactly what a fail-closed gate wants: overstating the cost of compacting can
// only move a verdict away from COMPACT.
//
// COHORT. Every observation carries the session's (harness, model) exactly as AO
// recorded them, and a non-empty harness or modelID argument keeps only the
// matching sessions. Exact string equality: no normalization, no alias
// resolution. Empty arguments return every cohort, which is what an inventory
// wants and what the estimator refuses to price -- it requires both.
//
// WHAT IT DELIBERATELY DOES NOT DO. It does not price anything and does not decide
// whether there are enough samples. Pricing is the rate card's job and the sample
// rule is the estimator's.
//
// COST. One bounded read for the candidate list plus the per-session fold that
// already exists, over a population that is by construction tiny. No transcript is
// opened and no corpus is walked.
func (r *CompactionReader) CompactionSummaryObservations(ctx context.Context, harness, modelID string) ([]domain.CompactionSummarySessionObservation, error) {
	if r == nil || r.store == nil {
		return nil, nil
	}
	// One past the ceiling, so reaching it is DETECTED rather than silently
	// truncated. The prior is a maximum: computed over an arbitrary subset it can
	// only understate, which is the optimistic direction, so a cohort AO cannot
	// read whole is an error and the gate answers UNKNOWN.
	sessions, err := r.store.ListCompactedSessionsForSummaryPrior(ctx, maxSummaryPriorCandidateSessions+1)
	if err != nil {
		return nil, fmt.Errorf("list compacted sessions: %w", err)
	}
	if len(sessions) > maxSummaryPriorCandidateSessions {
		return nil, fmt.Errorf("summary prior candidates exceed the ceiling of %d: fold them into an aggregate rather than read a subset",
			maxSummaryPriorCandidateSessions)
	}
	out := make([]domain.CompactionSummarySessionObservation, 0, len(sessions))
	for _, sessionID := range sessions {
		accounting, aerr := r.SessionCompactionAccounting(ctx, sessionID)
		if aerr != nil {
			// One unreadable session costs one sample, never the cohort. A prior
			// derived from fewer sessions simply reports a smaller sample count,
			// and the estimator refuses below its minimum.
			continue
		}
		if !accounting.HarnessObserved || accounting.Compactions <= 0 {
			continue
		}
		// THE UPPER BOUND HOLDS ONLY WHILE THE ROLLUP COVERS THE LEDGER. Credit
		// nobody could absorb means AO attributed spend the rollup does not
		// admit to: a rollup written before later turns (a resumed session), or
		// a ledger counting sources the rollup does not. Either way the residual
		// is no longer an upper bound on anything, so the session is not a
		// sample.
		if accounting.UnattributedNegative {
			continue
		}
		// Every counted compaction must be a distinct, detailed boundary. A count
		// above the detail is a boundary list that overflowed (its identity is
		// unknown) or the same boundaries counted by two generations of one
		// artifact -- and dividing the residual by an inflated count UNDERSTATES
		// the per-compaction figure.
		if accounting.Compactions != len(accounting.Boundaries) {
			continue
		}
		if accounting.Unattributed.OutputTokens <= 0 {
			// The harness charged itself no output AO cannot see. On a session
			// that compacted that is a measurement problem rather than a free
			// summary, so it contributes nothing rather than a zero.
			continue
		}
		// The cohort is read from the BOUNDARIES, never from the rollup: the
		// boundary carries the model in the ledger's own spelling (the same
		// spelling the judged session's calls carry), while the rollup spells
		// it its own way ("claude-opus-5[1m]"). Matching on the boundary is
		// therefore exact without resolving any alias.
		obsHarness, obsModel := summaryCohortIdentity(accounting.Boundaries)
		if harness != "" && obsHarness != harness {
			continue
		}
		if modelID != "" && obsModel != modelID {
			continue
		}
		out = append(out, domain.CompactionSummarySessionObservation{
			SessionID:                string(sessionID),
			Harness:                  obsHarness,
			ModelID:                  obsModel,
			Compactions:              accounting.Compactions,
			UnattributedOutputTokens: accounting.Unattributed.OutputTokens,
			// The LATEST boundary AO can place in time. It is what lets a
			// decision drop an observation that is younger than itself; a
			// session whose boundaries carry no timestamp is inadmissible to a
			// time-filtered prior rather than assumed old.
			ObservedAt: latestBoundaryTime(accounting.Boundaries),
		})
	}
	return out, nil
}

// summaryCohortIdentity is the one (harness, model) every boundary of a session
// agrees on, or two empty strings when they do not agree or name nothing.
//
// A session that compacted once on one model and once on another is not a sample
// of either cohort: its residual is one number and cannot be divided between
// them without inventing a ratio.
func summaryCohortIdentity(boundaries []domain.CompactionBoundary) (string, string) {
	var harness, model string
	for i, b := range boundaries {
		h, m := string(b.Harness), b.ModelID
		if h == "" || m == "" {
			return "", ""
		}
		if i == 0 {
			harness, model = h, m
			continue
		}
		if h != harness || m != model {
			return "", ""
		}
	}
	return harness, model
}

// latestBoundaryTime is the newest placeable boundary timestamp, or nil when none
// of them can be placed in time.
func latestBoundaryTime(boundaries []domain.CompactionBoundary) *time.Time {
	var latest *time.Time
	for i := range boundaries {
		at := boundaries[i].ObservedAt
		if at == nil {
			continue
		}
		if latest == nil || at.After(*latest) {
			latest = at
		}
	}
	return latest
}
