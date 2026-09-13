package usagebackfill_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	observeusage "github.com/aoagents/agent-orchestrator/backend/internal/observe/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/usagebackfill"
)

// cachettl_test.go -- what the backfill may and may not do to a row.
//
// Every test here is one sentence about the same thing: a lifetime is either
// reconstructed exactly from the artifact that recorded it, or it stays
// unknown. There is no third outcome, and most of these cases exist to prove
// that a plausible-looking third outcome is refused.

const testTranscriptModel = "claude-opus-5"

// transcriptRecord renders one billed assistant message the way Claude Code
// writes it. `split` is the cache_creation block, or "" for a harness that
// predates the vocabulary.
func transcriptRecord(id string, unc, read, write, out int64, split string) string {
	creation := ""
	if split != "" {
		creation = "," + split
	}
	return fmt.Sprintf(
		`{"type":"assistant","isSidechain":false,"uuid":"%s","timestamp":"2026-09-11T20:54:29.736Z",`+
			`"message":{"id":"%s","model":"%s","stop_reason":"tool_use","usage":`+
			`{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":%d%s}}}`,
		id, id, testTranscriptModel, unc, read, write, out, creation)
}

func lifetimes(five, hour int64) string {
	return fmt.Sprintf(`"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}`, five, hour)
}

type fixture struct {
	store   *sqlite.Store
	session domain.SessionRecord
	source  domain.UsageSourceRecord
	binding domain.UsageBindingRecord
	path    string
}

// newFixture seeds a session, a binding and a transcript source pointing at a
// real file, reconstructs the events that transcript describes, and ingests
// them as PRE-0170 rows: the real token vector, the real source_event_key, and
// no lifetime at all.
//
// The keys come from the parser rather than from the test, because the identity
// the backfill matches on is the parser's and a hand-written key would prove
// nothing. `prepare` is the seam each case uses to make the stored rows differ
// from what the artifact says -- drop one, change a vector, pre-observe a
// lifetime.
func newFixture(t *testing.T, transcript string, prepare func([]domain.ModelUsageEvent) []domain.ModelUsageEvent) *fixture {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlitetest.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p1", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatalf("project: %v", err)
	}
	session, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p1", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
		Activity:  domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	binding, err := store.UpsertUsageBinding(ctx, domain.UsageBindingRecord{
		Subject:   domain.UsageSubject{Kind: domain.UsageSubjectSession, ID: string(session.ID)},
		SessionID: session.ID, Harness: domain.HarnessClaudeCode,
		NativeRootID: "root-1", State: domain.UsageBindingActive, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	source, err := store.InsertUsageSource(ctx, domain.UsageSourceRecord{
		BindingID: binding.ID, Kind: domain.UsageSourceClaudeMain,
		NativeSessionID: "native-1", ArtifactPath: path,
		State: domain.UsageSourceActive, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	f := &fixture{store: store, session: session, source: source, binding: binding, path: path}

	events := f.reconstruct(t)
	// Strip the lifetime: these are the rows as they existed before 0170.
	seeded := make([]domain.ModelUsageEvent, 0, len(events))
	for _, ev := range events {
		ev.Tokens.CacheCreation = domain.CacheCreationSplit{}
		seeded = append(seeded, ev)
	}
	if prepare != nil {
		seeded = prepare(seeded)
	}
	if len(seeded) > 0 {
		err = store.ApplyUsageChunk(ctx, source.ID, 0, source.UpdatedAt, domain.SourceCursorState{
			ByteOffset:      int64(len(transcript)),
			State:           domain.UsageSourceActive,
			ParserStateJSON: `{"version":1,"source_kind":"claude_main","claude":{"model_id":"claude-opus-5"}}`,
			UpdatedAt:       now,
		}, seeded)
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	return f
}

// reconstruct returns the transcript's events, deduplicated by key the way one
// ledger row corresponds to one billed message.
func (f *fixture) reconstruct(t *testing.T) []domain.ModelUsageEvent {
	t.Helper()
	source, ok, err := f.store.GetUsageSourceForIngestion(context.Background(), f.source.ID)
	if err != nil || !ok {
		t.Fatalf("source context: %v (found=%v)", err, ok)
	}
	file, err := os.Open(f.path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer func() { _ = file.Close() }()
	events, err := observeusage.ReconstructEvents(source, file, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	seen := map[string]bool{}
	out := make([]domain.ModelUsageEvent, 0, len(events))
	for _, ev := range events {
		if seen[ev.SourceEventKey] {
			continue
		}
		seen[ev.SourceEventKey] = true
		out = append(out, ev)
	}
	return out
}

func (f *fixture) run(t *testing.T, apply bool) *usagebackfill.Report {
	t.Helper()
	report, err := usagebackfill.Run(context.Background(), f.store, usagebackfill.Options{
		Apply: apply, Now: time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return report
}

// execRaw runs one statement straight against the database, for the states the
// write path cannot produce.
func (f *fixture) execRaw(t *testing.T, stmt string) {
	t.Helper()
	if err := f.store.ExecForCacheTTLMaintenanceTest(context.Background(), stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

// split is the session's cache-creation lifetime as every reader sees it.
func (f *fixture) split(t *testing.T) domain.CacheCreationSplit {
	t.Helper()
	agg, err := f.store.ListUsageModelAggregates(context.Background(), f.session.ID)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(agg) == 0 {
		return domain.CacheCreationSplit{}
	}
	return agg[0].Tokens.CacheCreation
}

func TestBackfillRecoversEveryLifetimeShape(t *testing.T) {
	transcript := strings.Join([]string{
		transcriptRecord("m-1h", 2, 1000, 2193, 50, lifetimes(0, 2193)),
		transcriptRecord("m-5m", 2, 3000, 900, 40, lifetimes(900, 0)),
		transcriptRecord("m-mix", 2, 5000, 1000, 30, lifetimes(400, 600)),
	}, "\n") + "\n"
	f := newFixture(t, transcript, nil)

	before := f.split(t)
	if before.Known() || before.UnknownTTLTokens != 4093 {
		t.Fatalf("pre-backfill split = %+v, want all 4,093 unknown", before)
	}

	report := f.run(t, true)
	if report.RowsUpdated != 3 {
		t.Fatalf("updated = %d, want 3 (%s)", report.RowsUpdated, report)
	}
	after := f.split(t)
	if after.Ephemeral5mTokens != 1300 || after.Ephemeral1hTokens != 2793 || after.UnknownTTLTokens != 0 {
		t.Fatalf("post-backfill split = %+v, want 1300 / 2793 / 0", after)
	}
	// The partition holds: what was unknown is now exactly what was recovered.
	if after.Total() != before.Total() {
		t.Fatalf("the total moved: %d -> %d", before.Total(), after.Total())
	}
	if report.Recovered5m != 1300 || report.Recovered1h != 2793 {
		t.Fatalf("report = %d/%d, want 1300/2793", report.Recovered5m, report.Recovered1h)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	transcript := transcriptRecord("m-1h", 2, 1000, 2193, 50, lifetimes(0, 2193)) + "\n"
	f := newFixture(t, transcript, nil)

	report := f.run(t, false)
	if !report.DryRun {
		t.Fatal("a run without Apply must report itself as a dry run")
	}
	if report.RowsUpdated != 1 {
		t.Fatalf("a dry run must still say what it WOULD do, got %d", report.RowsUpdated)
	}
	if got := f.split(t); got.Known() || got.UnknownTTLTokens != 2193 {
		t.Fatalf("a dry run wrote something: %+v", got)
	}
}

func TestSecondApplyUpdatesNothing(t *testing.T) {
	transcript := transcriptRecord("m-1h", 2, 1000, 2193, 50, lifetimes(0, 2193)) + "\n"
	f := newFixture(t, transcript, nil)

	first := f.run(t, true)
	if first.RowsUpdated != 1 {
		t.Fatalf("first apply updated %d, want 1", first.RowsUpdated)
	}
	before := f.split(t)

	second := f.run(t, true)
	if second.RowsUpdated != 0 {
		t.Fatalf("second apply updated %d, want 0 -- the backfill is not idempotent", second.RowsUpdated)
	}
	// Stronger than "skipped as already observed": with every row of this
	// source now carrying a lifetime, the source is not even selected, so the
	// second run does not re-read the artifact at all. A run over a ledger with
	// other unresolved rows still reaches this source's siblings and skips
	// those as already_observed -- both outcomes are the same guarantee.
	if second.SourcesConsidered != 0 {
		t.Fatalf("sources considered = %d, want 0 once nothing is left unknown", second.SourcesConsidered)
	}
	if second.EventsReconstructed != 0 {
		t.Fatalf("the second run re-read %d events it could not use", second.EventsReconstructed)
	}
	if after := f.split(t); after != before {
		t.Fatalf("the second run changed the data: %+v -> %+v", before, after)
	}
}

func TestAMissingTranscriptRecoversNothing(t *testing.T) {
	transcript := transcriptRecord("m-1h", 2, 1000, 2193, 50, lifetimes(0, 2193)) + "\n"
	f := newFixture(t, transcript, nil)
	if err := os.Remove(f.path); err != nil {
		t.Fatalf("remove transcript: %v", err)
	}

	report := f.run(t, true)
	if report.RowsUpdated != 0 || report.Skipped[usagebackfill.SkipTranscriptMissing] != 1 {
		t.Fatalf("report = %s", report)
	}
	if got := f.split(t); got.Known() {
		t.Fatalf("nothing may be recovered without the artifact: %+v", got)
	}
}

func TestAVectorMismatchIsRefused(t *testing.T) {
	// The ledger holds a DIFFERENT output count for this key: the transcript is
	// no longer describing the call that was recorded.
	transcript := transcriptRecord("m-1h", 2, 1000, 2193, 50, lifetimes(0, 2193)) + "\n"
	f := newFixture(t, transcript, func(evs []domain.ModelUsageEvent) []domain.ModelUsageEvent {
		evs[0].Tokens.OutputTokens = 999
		return evs
	})

	report := f.run(t, true)
	if report.RowsUpdated != 0 || report.Skipped[usagebackfill.SkipVectorMismatch] != 1 {
		t.Fatalf("a transcript that no longer matches must be refused: %s", report)
	}
	if got := f.split(t); got.Known() {
		t.Fatalf("the row was written anyway: %+v", got)
	}
}

func TestAnInconsistentLifetimeIsRefused(t *testing.T) {
	// The buckets do not sum to the total the record itself declares.
	transcript := transcriptRecord("m-bad", 2, 1000, 2193, 50, lifetimes(100, 100)) + "\n"
	f := newFixture(t, transcript, nil)

	report := f.run(t, true)
	if report.RowsUpdated != 0 {
		t.Fatalf("an inconsistent split must never be written: %s", report)
	}
	if report.Skipped[usagebackfill.SkipTTLInconsistent] != 1 {
		t.Fatalf("skip reasons = %v", report.Skipped)
	}
	if got := f.split(t); got.Known() || got.UnknownTTLTokens != 2193 {
		t.Fatalf("the row changed: %+v", got)
	}
}

func TestALifetimeTheHarnessNeverReportedIsLeftAlone(t *testing.T) {
	// A transcript from a harness that predates the vocabulary.
	transcript := transcriptRecord("m-legacy", 2, 1000, 2193, 50, "") + "\n"
	f := newFixture(t, transcript, nil)

	report := f.run(t, true)
	if report.RowsUpdated != 0 || report.Skipped[usagebackfill.SkipLifetimeUnreported] != 1 {
		t.Fatalf("report = %s", report)
	}
	if got := f.split(t); got.Known() {
		t.Fatalf("a lifetime nobody reported must not be invented: %+v", got)
	}
}

func TestAnEventWithNoCacheCreationIsSkipped(t *testing.T) {
	transcript := transcriptRecord("m-none", 2, 1000, 0, 50, lifetimes(0, 0)) + "\n"
	f := newFixture(t, transcript, nil)

	report := f.run(t, true)
	if report.RowsUpdated != 0 || report.Skipped[usagebackfill.SkipNoCacheCreation] != 1 {
		t.Fatalf("report = %s", report)
	}
	// Writing 0/0 would assert an observation about a call that created no
	// cache at all.
	if got := f.split(t); got.Known() && got.Total() > 0 {
		t.Fatalf("split = %+v", got)
	}
}

func TestAnEventTheLedgerNeverStoredIsSkipped(t *testing.T) {
	transcript := strings.Join([]string{
		transcriptRecord("m-stored", 2, 1000, 2193, 50, lifetimes(0, 2193)),
		transcriptRecord("m-unstored", 2, 4000, 500, 20, lifetimes(0, 500)),
	}, "\n") + "\n"
	f := newFixture(t, transcript, func(evs []domain.ModelUsageEvent) []domain.ModelUsageEvent {
		return evs[:1] // the ledger never stored the second message
	})

	report := f.run(t, true)
	if report.RowsUpdated != 1 {
		t.Fatalf("updated = %d, want only the stored one", report.RowsUpdated)
	}
	if report.Skipped[usagebackfill.SkipEventKeyMissing] != 1 {
		t.Fatalf("skip reasons = %v", report.Skipped)
	}
}

func TestRepeatedRecordsOfOneMessageCountOnce(t *testing.T) {
	// One billed message, three records -- the ordinary shape. It is ONE row.
	one := transcriptRecord("m-1h", 2, 1000, 2193, 50, lifetimes(0, 2193))
	transcript := strings.Join([]string{one, one, one}, "\n") + "\n"
	f := newFixture(t, transcript, nil)

	report := f.run(t, false)
	if report.RowsUpdated != 1 {
		t.Fatalf("would-update = %d, want 1: a dry run that counts records instead of rows over-promises", report.RowsUpdated)
	}
	if report.Skipped[usagebackfill.SkipDuplicateRecord] != 2 {
		t.Fatalf("skip reasons = %v", report.Skipped)
	}
	if report.Recovered1h != 2193 {
		t.Fatalf("recovered = %d, want the row's 2,193 counted once", report.Recovered1h)
	}
}

func TestTheReportAccountsForEveryReconstructedEvent(t *testing.T) {
	transcript := strings.Join([]string{
		transcriptRecord("m-ok", 2, 1000, 2193, 50, lifetimes(0, 2193)),
		transcriptRecord("m-none", 2, 2000, 0, 10, lifetimes(0, 0)),
		transcriptRecord("m-unstored", 2, 4000, 500, 20, lifetimes(0, 500)),
		transcriptRecord("m-legacy", 2, 6000, 700, 15, ""),
	}, "\n") + "\n"
	f := newFixture(t, transcript, nil)

	report := f.run(t, true)
	// Every event the parser produced is either an update or a counted skip.
	if got := report.RowsUpdated + report.SkippedTotal(); got != report.EventsReconstructed {
		t.Fatalf("%d updated + %d skipped != %d reconstructed\n%s",
			report.RowsUpdated, report.SkippedTotal(), report.EventsReconstructed, report)
	}
}

func TestTheReportCarriesNoContent(t *testing.T) {
	transcript := strings.Join([]string{
		transcriptRecord("m-ok", 2, 1000, 2193, 50, lifetimes(0, 2193)),
		`{"type":"system","subtype":"compact_boundary","uuid":"b","content":"Conversation compacted",` +
			`"cwd":"/Users/someone/secret-project","gitBranch":"ao/wf-1/wf-1"}`,
	}, "\n") + "\n"
	f := newFixture(t, transcript, nil)

	rendered := f.run(t, true).String()
	for _, forbidden := range []string{
		"secret-project", "Conversation compacted", "ao/wf-1", f.path, "claude-opus-5", "m-ok",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("the report leaked %q:\n%s", forbidden, rendered)
		}
	}
}

// --- adversarial: identity ---------------------------------------------------
//
// The development of this command already produced one identity bug (a guard
// keyed on the source when the row is keyed on the binding), so the cases below
// attack the identity directly rather than trusting that it holds.

// secondSource attaches another transcript source to the SAME binding -- what a
// replaced artifact leaves behind, and what a subagent transcript looks like.
func (f *fixture) secondSource(t *testing.T, transcript string) domain.UsageSourceRecord {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sibling.jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatalf("write sibling transcript: %v", err)
	}
	source, err := f.store.InsertUsageSource(context.Background(), domain.UsageSourceRecord{
		BindingID: f.binding.ID, Kind: domain.UsageSourceClaudeMain,
		NativeSessionID: "native-1", ArtifactPath: path,
		State: domain.UsageSourceActive, UpdatedAt: time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("sibling source: %v", err)
	}
	return source
}

func TestASiblingSourceDescribingTheSameRowUpdatesItOnce(t *testing.T) {
	// A replaced artifact: two sources on one binding, both describing the same
	// calls. The row is identified by (binding, key), so it must be counted and
	// updated exactly once -- counting per source is what promised to recover
	// twice the tokens the ledger holds.
	transcript := transcriptRecord("m-1h", 2, 1000, 2193, 50, lifetimes(0, 2193)) + "\n"
	f := newFixture(t, transcript, nil)
	f.secondSource(t, transcript)

	report := f.run(t, true)
	if report.RowsUpdated != 1 {
		t.Fatalf("updated = %d, want 1 (%s)", report.RowsUpdated, report)
	}
	if report.Skipped[usagebackfill.SkipDuplicateRecord] != 1 {
		t.Fatalf("the sibling's reconstruction must be recognised as the same row: %v", report.Skipped)
	}
	if report.Recovered1h != 2193 {
		t.Fatalf("recovered = %d, want the row's tokens counted once", report.Recovered1h)
	}
	if got := f.split(t); got.Ephemeral1hTokens != 2193 || got.UnknownTTLTokens != 0 {
		t.Fatalf("split = %+v", got)
	}
}

func TestTwoCallsWithIdenticalVectorsKeepTheirOwnIdentities(t *testing.T) {
	// Same numbers, different messages: two rows, two keys, two updates. A
	// match on metrics rather than identity would collapse them.
	transcript := strings.Join([]string{
		transcriptRecord("m-a", 2, 1000, 2193, 50, lifetimes(0, 2193)),
		transcriptRecord("m-b", 2, 1000, 2193, 50, lifetimes(2193, 0)),
	}, "\n") + "\n"
	f := newFixture(t, transcript, nil)

	report := f.run(t, true)
	if report.RowsUpdated != 2 {
		t.Fatalf("updated = %d, want 2 (%s)", report.RowsUpdated, report)
	}
	// One went to each lifetime: the rows were told apart.
	if got := f.split(t); got.Ephemeral5mTokens != 2193 || got.Ephemeral1hTokens != 2193 {
		t.Fatalf("split = %+v, want 2193 in each bucket", got)
	}
}

func TestAKeyFromAnotherBindingNeverMatches(t *testing.T) {
	// The same source_event_key under a different binding is a different row.
	transcript := transcriptRecord("m-1h", 2, 1000, 2193, 50, lifetimes(0, 2193)) + "\n"
	f := newFixture(t, transcript, nil)
	key := f.reconstruct(t)[0].SourceEventKey

	other, err := f.store.UpsertUsageBinding(context.Background(), domain.UsageBindingRecord{
		Subject:   domain.UsageSubject{Kind: domain.UsageSubjectSession, ID: string(f.session.ID)},
		SessionID: f.session.ID, Harness: domain.HarnessClaudeCode,
		NativeRootID: "root-2", State: domain.UsageBindingActive,
		UpdatedAt: time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("second binding: %v", err)
	}
	updated, err := f.store.BackfillModelUsageEventCacheTTL(context.Background(), other.ID, domain.ModelUsageEvent{
		ModelID:        testTranscriptModel,
		SourceEventKey: key,
		Tokens: domain.UsageTokenMetrics{
			InputTokens: 3195, UncachedInputTokens: 2, CacheReadTokens: 1000,
			CacheWriteTokens: 2193, OutputTokens: 50,
			CacheCreation: domain.CacheCreationSplit{Ephemeral1hTokens: 2193},
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated {
		t.Fatal("a key under a different binding must not match any row")
	}
}

func TestAStaleVectorLosesTheWrite(t *testing.T) {
	// The row changed between the read and the write -- an ingest that landed
	// in between, or a transcript that no longer describes this call. The
	// guarded UPDATE must match nothing rather than write a lifetime onto a
	// call it was not reconstructed from.
	transcript := transcriptRecord("m-1h", 2, 1000, 2193, 50, lifetimes(0, 2193)) + "\n"
	f := newFixture(t, transcript, nil)
	ev := f.reconstruct(t)[0]
	ev.Tokens.OutputTokens = 999 // the vector the writer believes, now wrong

	updated, err := f.store.BackfillModelUsageEventCacheTTL(context.Background(), f.binding.ID, ev)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated {
		t.Fatal("a stale vector must not win the write")
	}
	if got := f.split(t); got.Known() {
		t.Fatalf("the row was written anyway: %+v", got)
	}
}

func TestAPartialLifetimeIsReportedAndLeftAlone(t *testing.T) {
	// One column written and the other NULL. Unreachable through the write
	// path; exactly what a careless backfill could produce. It must be named
	// and left, never completed from the half that is there.
	for _, tc := range []struct{ name, sql string }{
		{"five set, hour null", `UPDATE model_usage_events SET cache_write_5m_tokens = 2193`},
		{"hour set, five null", `UPDATE model_usage_events SET cache_write_1h_tokens = 2193`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transcript := transcriptRecord("m-1h", 2, 1000, 2193, 50, lifetimes(0, 2193)) + "\n"
			f := newFixture(t, transcript, nil)
			f.execRaw(t, tc.sql)

			before := f.split(t)
			report := f.run(t, true)
			if report.RowsUpdated != 0 {
				t.Fatalf("a partial state must not be completed: %s", report)
			}
			if report.Skipped[usagebackfill.SkipPartialExistingState] != 1 {
				t.Fatalf("skip reasons = %v", report.Skipped)
			}
			// Still reported as unknown by every aggregate, and unchanged.
			if after := f.split(t); after != before {
				t.Fatalf("the row changed: %+v -> %+v", before, after)
			}
			if before.Known() {
				t.Fatalf("a half-written pair must not read as known: %+v", before)
			}
		})
	}
}

func TestAThirdApplyStillUpdatesNothing(t *testing.T) {
	transcript := strings.Join([]string{
		transcriptRecord("m-1h", 2, 1000, 2193, 50, lifetimes(0, 2193)),
		transcriptRecord("m-none", 2, 4000, 0, 20, lifetimes(0, 0)),
	}, "\n") + "\n"
	f := newFixture(t, transcript, nil)

	if got := f.run(t, true).RowsUpdated; got != 1 {
		t.Fatalf("first apply = %d, want 1", got)
	}
	after := f.split(t)
	for i, run := range []int{2, 3} {
		report := f.run(t, true)
		if report.RowsUpdated != 0 {
			t.Fatalf("apply #%d updated %d rows", run, report.RowsUpdated)
		}
		if got := f.split(t); got != after {
			t.Fatalf("apply #%d (iteration %d) changed the data: %+v", run, i, got)
		}
	}
}
