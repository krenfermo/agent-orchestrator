// Package usagebackfill recovers facts AO could have recorded and did not,
// from the artifacts it still has.
//
// Its only subject today is the cache-creation LIFETIME. Migration 0170 gave
// model_usage_events a column pair for it and every row written before that
// migration carries NULL there -- which is honest (the lifetime was never
// observed) and expensive (those rows price a long-lived cache write at the
// short-lived rate, understating one measured session by 17.9%). The provider
// reported which lifetime it created, on every message, in transcripts that are
// mostly still on disk. This package reads them back.
//
// THE RULES, because a backfill that bends any of them is worse than no
// backfill at all:
//
//   - It reconstructs; it never estimates. No dominant bucket, no apportioned
//     aggregate, no statistical fill, no "probably 1h because everything else
//     was". A row whose lifetime cannot be reconstructed exactly keeps its NULL.
//   - It matches on source_event_key, the exactly-once identity, re-derived by
//     AO's OWN parser from the same artifact. Never on position, order,
//     timestamp or index.
//   - It verifies the whole durable token vector before writing, and the two
//     buckets must sum to the total the ledger already holds. Any mismatch is a
//     skip, never a correction: the stored total is not adjusted to make a
//     reconstruction fit.
//   - It only ever moves a row from unobserved to observed. A row that already
//     carries a lifetime, or half of one, is left exactly as it is.
//   - It writes two integer columns and nothing else. No prompt, no
//     conversation, no path, no other field of the event.
package usagebackfill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	observeusage "github.com/aoagents/agent-orchestrator/backend/internal/observe/usage"
)

// Store is the narrow set of reads and the one write this backfill needs.
// *sqlite.Store satisfies it.
type Store interface {
	ListUsageSourceIDsForCacheTTLBackfill(ctx context.Context) ([]int64, error)
	GetUsageSourceForIngestion(ctx context.Context, id int64) (domain.UsageSourceContext, bool, error)
	GetStoredModelUsageEvent(ctx context.Context, bindingID int64, sourceEventKey string) (domain.StoredModelUsageEvent, bool, error)
	BackfillModelUsageEventCacheTTL(ctx context.Context, bindingID int64, ev domain.ModelUsageEvent) (bool, error)
}

// SkipReason is the closed vocabulary of why a reconstructed event did not
// become an update. Every one of them is a decision to leave a row alone.
type SkipReason string

// The reasons, each counted separately because they say different things about
// what is wrong: a missing transcript is time passing, a vector mismatch is a
// transcript that no longer describes this event, and a partial existing state
// is a row somebody or something half-wrote.
const (
	// SkipTranscriptMissing is an artifact that has been rotated, truncated
	// away or deleted since the events were ingested. Nothing to reconstruct
	// from, and nothing to be done about it.
	SkipTranscriptMissing SkipReason = "transcript_missing"
	// SkipEventKeyMissing is a reconstructed event the ledger has no row for.
	// Expected and harmless: a transcript holds every message, and AO only ever
	// stored the ones its tailer reached.
	SkipEventKeyMissing SkipReason = "event_key_missing"
	// SkipAlreadyObserved is a row that already carries a lifetime. The whole
	// point of the guarded update.
	SkipAlreadyObserved SkipReason = "already_observed"
	// SkipPartialExistingState is a row with one lifetime column set and the
	// other NULL. Unreachable through the write path, and deliberately NOT
	// completed here: the aggregates already treat it as unknown, and quietly
	// normalising it would hide however it came to exist.
	SkipPartialExistingState SkipReason = "partial_existing_state"
	// SkipVectorMismatch is a row whose stored token vector is not the one the
	// transcript describes for this key. The transcript is no longer about this
	// event; writing a lifetime onto it would be writing someone else's fact.
	SkipVectorMismatch SkipReason = "vector_mismatch"
	// SkipTTLInconsistent is a reconstruction whose two buckets do not sum to
	// the total the ledger holds. No distribution is invented to close the gap.
	SkipTTLInconsistent SkipReason = "ttl_inconsistent"
	// SkipNoCacheCreation is an event that created no cache at all. There is no
	// lifetime to record, and writing 0/0 would assert an observation about
	// nothing.
	SkipNoCacheCreation SkipReason = "no_cache_creation"
	// SkipLifetimeUnreported is a transcript record that carries no
	// cache_creation block -- a harness that predates the vocabulary. Its
	// lifetime is unknown at the source, which is exactly what the row already
	// says.
	SkipLifetimeUnreported SkipReason = "lifetime_unreported"
	// SkipRaceLost is a guarded UPDATE that matched no row despite every check
	// passing on the read. Something changed underneath; the run reports it and
	// touches nothing.
	SkipRaceLost SkipReason = "race_lost"
	// SkipDuplicateRecord is a second reconstruction of a ledger row this run
	// has already handled.
	//
	// Two ways that happens, and both are ordinary. One billed message arrives
	// as several records -- thinking in the first, the tool call in a later one
	// -- sharing a message id and therefore a source_event_key. And an artifact
	// that was REPLACED leaves two source rows on the same binding, both
	// describing the same calls. Either way it is ONE ledger row, identified by
	// (binding, key), and the ingest path collapses it by inserting on first
	// sight. This one collapses it the same way: counting the repeats would
	// promise to recover more tokens than the ledger contains.
	SkipDuplicateRecord SkipReason = "duplicate_record"
)

// Report is what one run of the backfill did, and it is the whole audit record.
// Counts and token totals only: no path, no key, no content.
type Report struct {
	DryRun bool

	SourcesConsidered int
	SourcesRead       int
	// SourcesUnsupported counts sources whose format carries no cache lifetime
	// at all -- a Codex rollout, today. Not an error: there is nothing in that
	// artifact to recover, and reporting it as a parser failure would make a
	// clean run look broken.
	SourcesUnsupported int
	ParserErrors       int

	EventsReconstructed int
	RowsMatched         int
	RowsUpdated         int

	// Skipped counts every reason. A run where the numbers do not add up is a
	// bug in this package, and TestReportAccountsForEveryEvent says so.
	Skipped map[SkipReason]int

	// Recovered5m / Recovered1h are the cache-creation tokens this run gave a
	// lifetime to. In a dry run they are what it WOULD have.
	Recovered5m int64
	Recovered1h int64
	// RecoverableTotal is the sum of the two: the cache creation that stops
	// depending on an assumed lifetime.
	RecoverableTotal int64
}

func newReport(dryRun bool) *Report {
	return &Report{DryRun: dryRun, Skipped: map[SkipReason]int{}}
}

func (r *Report) skip(reason SkipReason) { r.Skipped[reason]++ }

// SkippedTotal is every event that did not become an update.
func (r *Report) SkippedTotal() int {
	total := 0
	for _, n := range r.Skipped {
		total += n
	}
	return total
}

// SkipReasons lists the reasons this run recorded, in a stable order.
func (r *Report) SkipReasons() []SkipReason {
	out := make([]SkipReason, 0, len(r.Skipped))
	for reason := range r.Skipped {
		out = append(out, reason)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Options configures one run.
type Options struct {
	// Apply is the ONLY thing that makes this write. Everything else about a
	// run is identical either way -- same reads, same reconstruction, same
	// verification, same report -- so a dry run is a true rehearsal and not a
	// different code path that happens to agree.
	Apply bool
	// Now is the clock handed to the parser. Injected so a reconstruction is
	// reproducible.
	Now time.Time
	// OpenArtifact reads one transcript. Injected for tests; nil means the
	// filesystem.
	OpenArtifact func(path string) (fs.File, error)
}

// Run reconstructs cache lifetimes for every event that lacks one.
func Run(ctx context.Context, store Store, opts Options) (*Report, error) {
	if store == nil {
		return nil, errors.New("usage backfill: no store")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	open := opts.OpenArtifact
	if open == nil {
		open = func(path string) (fs.File, error) { return os.Open(path) } //nolint:gosec // operator-owned transcript under the user's own home
	}

	report := newReport(!opts.Apply)
	// One ledger row can be reconstructed more than once in a single run: a
	// message spans several transcript records, and an artifact that was
	// REPLACED leaves two sources pointing at the same binding, both of which
	// describe the same calls. The row is identified by (binding, key), so the
	// guard against counting it twice has to be too -- a per-source guard
	// collapses the first case and misses the second, which is how a dry run
	// ends up promising to recover more tokens than the ledger contains.
	seen := map[string]bool{}
	ids, err := store.ListUsageSourceIDsForCacheTTLBackfill(ctx)
	if err != nil {
		return nil, err
	}
	report.SourcesConsidered = len(ids)

	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		source, found, err := store.GetUsageSourceForIngestion(ctx, id)
		if err != nil {
			return report, err
		}
		if !found {
			report.skip(SkipTranscriptMissing)
			continue
		}
		if !observeusage.ReconstructableSourceKind(source.Source.Kind) {
			report.SourcesUnsupported++
			continue
		}
		events, err := reconstruct(source, open, opts.Now)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				report.skip(SkipTranscriptMissing)
				continue
			}
			report.ParserErrors++
			continue
		}
		report.SourcesRead++
		report.EventsReconstructed += len(events)
		// Collapse repeats the way the ingest path does -- first sight wins --
		// so a dry run counts the rows it could actually change and not the
		// records it happened to read.
		for _, ev := range events {
			rowKey := fmt.Sprintf("%d\x1f%s", source.Source.BindingID, ev.SourceEventKey)
			if seen[rowKey] {
				report.skip(SkipDuplicateRecord)
				continue
			}
			seen[rowKey] = true
			if err := applyEvent(ctx, store, source, ev, opts.Apply, report); err != nil {
				return report, err
			}
		}
	}
	report.RecoverableTotal = report.Recovered5m + report.Recovered1h
	return report, nil
}

func reconstruct(source domain.UsageSourceContext, open func(string) (fs.File, error), now time.Time) ([]domain.ModelUsageEvent, error) {
	path := source.Source.ArtifactPath
	if path == "" {
		return nil, fs.ErrNotExist
	}
	file, err := open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return observeusage.ReconstructEvents(source, file, now)
}

// applyEvent is the whole decision for one reconstructed event.
func applyEvent(
	ctx context.Context,
	store Store,
	source domain.UsageSourceContext,
	ev domain.ModelUsageEvent,
	apply bool,
	report *Report,
) error {
	split := ev.Tokens.CacheCreation
	switch {
	case ev.Tokens.CacheWriteTokens == 0:
		report.skip(SkipNoCacheCreation)
		return nil
	case split.Total() == 0:
		// The record carried no cache_creation block at all. Its lifetime is
		// unknown at the source, which is what the row already says.
		report.skip(SkipLifetimeUnreported)
		return nil
	case !split.Known() || split.Total() != ev.Tokens.CacheWriteTokens:
		report.skip(SkipTTLInconsistent)
		return nil
	}

	stored, found, err := store.GetStoredModelUsageEvent(ctx, source.Source.BindingID, ev.SourceEventKey)
	if err != nil {
		return err
	}
	if !found {
		report.skip(SkipEventKeyMissing)
		return nil
	}
	switch {
	case stored.CacheCreationObserved:
		report.skip(SkipAlreadyObserved)
		return nil
	case stored.CacheCreationPartial:
		report.skip(SkipPartialExistingState)
		return nil
	case !sameVector(stored, ev):
		report.skip(SkipVectorMismatch)
		return nil
	}
	report.RowsMatched++

	if apply {
		updated, err := store.BackfillModelUsageEventCacheTTL(ctx, source.Source.BindingID, ev)
		if err != nil {
			return err
		}
		if !updated {
			report.skip(SkipRaceLost)
			return nil
		}
	}
	report.RowsUpdated++
	report.Recovered5m += split.Ephemeral5mTokens
	report.Recovered1h += split.Ephemeral1hTokens
	return nil
}

// sameVector reports whether the stored row and the reconstruction describe the
// same call in every durable dimension.
//
// Reasoning tokens are compared through their known-ness as well as their
// value, because a stored NULL and a reconstructed zero are different facts and
// a backfill must not treat them as the same event.
func sameVector(stored domain.StoredModelUsageEvent, ev domain.ModelUsageEvent) bool {
	if stored.ModelID != ev.ModelID {
		return false
	}
	if stored.Tokens.InputTokens != ev.Tokens.InputTokens ||
		stored.Tokens.UncachedInputTokens != ev.Tokens.UncachedInputTokens ||
		stored.Tokens.CacheReadTokens != ev.Tokens.CacheReadTokens ||
		stored.Tokens.CacheWriteTokens != ev.Tokens.CacheWriteTokens ||
		stored.Tokens.OutputTokens != ev.Tokens.OutputTokens {
		return false
	}
	storedReasoning, reconstructedReasoning := stored.Tokens.ReasoningTokens, ev.Tokens.ReasoningTokens
	if (storedReasoning == nil) != (reconstructedReasoning == nil) {
		return false
	}
	if storedReasoning != nil && *storedReasoning != *reconstructedReasoning {
		return false
	}
	return true
}

// String renders the report as a plain-text audit block: counts, token totals
// and the closed vocabulary of skip reasons. Nothing else can reach it.
func (r *Report) String() string {
	mode := "DRY RUN (nothing was written)"
	if !r.DryRun {
		mode = "APPLIED"
	}
	out := fmt.Sprintf("cache TTL backfill -- %s\n\n", mode)
	out += fmt.Sprintf("  sources considered      %d\n", r.SourcesConsidered)
	out += fmt.Sprintf("  sources read            %d\n", r.SourcesRead)
	out += fmt.Sprintf("  sources unsupported     %d\n", r.SourcesUnsupported)
	out += fmt.Sprintf("  parser errors           %d\n", r.ParserErrors)
	out += fmt.Sprintf("  events reconstructed    %d\n", r.EventsReconstructed)
	out += fmt.Sprintf("  rows matched exactly    %d\n", r.RowsMatched)
	if r.DryRun {
		out += fmt.Sprintf("  rows that WOULD update  %d\n", r.RowsUpdated)
	} else {
		out += fmt.Sprintf("  rows updated            %d\n", r.RowsUpdated)
	}
	out += fmt.Sprintf("  events skipped          %d\n", r.SkippedTotal())
	for _, reason := range r.SkipReasons() {
		out += fmt.Sprintf("    %-24s %d\n", reason, r.Skipped[reason])
	}
	out += fmt.Sprintf("\n  cache creation recovered, 5m   %d\n", r.Recovered5m)
	out += fmt.Sprintf("  cache creation recovered, 1h   %d\n", r.Recovered1h)
	out += fmt.Sprintf("  cache creation recovered total %d\n", r.RecoverableTotal)
	return out
}
