package usage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// compaction_summary_prior_test.go -- P7.2B2.1: the read that makes the
// cross-session summary prior possible, and every way it must refuse.
//
// The reader's job is narrow ON PURPOSE: gather one observation per session that
// compacted and whose harness wrote a rollup, and gather nothing else. It prices
// nothing, groups nothing and decides nothing about sample sufficiency -- those
// belong to the rate card and the estimator, and a reader that silently dropped
// observations on its own judgement would make the sample count it reports a
// fiction.

// summaryPriorStore builds a fake store whose candidate list is `sessions` and
// whose per-session fold comes from one parsed source.
func summaryPriorStore(t *testing.T, sessions []domain.SessionID, records ...string) fakeCompactionStore {
	t.Helper()
	source := usageSource(domain.UsageSourceClaudeMain)
	// An ordinary turn first, so the parser knows the model the session is
	// running when a boundary arrives. That model, in the ledger's own spelling,
	// is the cohort identity the boundary carries.
	records = append([]string{claudeTurnWithLifetimes("turn-0", "claude-opus-5", 30_000, 0, 4_000, 150)}, records...)
	rows := make([]jsonlRecord, 0, len(records))
	for i, r := range records {
		rows = append(rows, jsonlRecord{Offset: int64(i * 900), Data: []byte(r)})
	}
	result := parseRecords(source, rows, int64(len(records)*900), time.Unix(1700000000, 0).UTC())
	if result.err != nil {
		t.Fatalf("parse: %v", result.err)
	}
	return fakeCompactionStore{
		compacted: sessions,
		bindings:  []domain.UsageBindingRecord{{ID: 42, Harness: domain.HarnessClaudeCode}},
		sources: map[int64][]domain.UsageSourceRecord{42: {{
			ID: 7, BindingID: 42, Kind: domain.UsageSourceClaudeMain,
			ByteOffset:      result.Cursor.ByteOffset,
			ParserStateJSON: result.Cursor.ParserStateJSON,
			State:           domain.UsageSourceComplete,
		}}},
		// A ledger that attributed far less output than the harness charged
		// itself, so the residual -- the summarization -- is positive.
		aggregates: []domain.UsageModelAggregate{{
			Harness: domain.HarnessClaudeCode, ModelID: "claude-opus-5",
			Tokens: domain.UsageTokenMetrics{OutputTokens: 30_500},
		}},
	}
}

// TestTheSummaryPriorReadsOneObservationPerCompactingSession is the happy path,
// and it checks the two fields the estimator actually consumes plus the timestamp
// that makes the observation orderable against a decision.
func TestTheSummaryPriorReadsOneObservationPerCompactingSession(t *testing.T) {
	store := summaryPriorStore(t, []domain.SessionID{"s-1", "s-2", "s-3"},
		compactBoundaryRecord, costStateRecord)
	reader := NewCompactionReader(store, nil)

	got, err := reader.CompactionSummaryObservations(context.Background(), "claude-code", "claude-opus-5")
	if err != nil {
		t.Fatalf("CompactionSummaryObservations: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("observations = %d, want 3 (one per candidate session)", len(got))
	}
	for _, o := range got {
		if o.SessionID == "" {
			t.Error("an observation with no session id cannot be counted as independent")
		}
		if o.Compactions != 1 {
			t.Errorf("compactions = %d, want 1", o.Compactions)
		}
		// The harness charged itself 47,486 output across its models; the ledger
		// holds 30,500. The residual is the summarization, and it INCLUDES the
		// reasoning the summary's text does not contain.
		if o.UnattributedOutputTokens <= 0 {
			t.Errorf("unattributed output = %d, want positive", o.UnattributedOutputTokens)
		}
		if o.ObservedAt == nil {
			t.Error("an observation with no timestamp cannot be ordered against a decision and the estimator will drop it")
		}
		if o.Harness != "claude-code" || o.ModelID != "claude-opus-5" {
			t.Errorf("cohort = %q / %q, want claude-code / claude-opus-5 exactly as AO recorded them", o.Harness, o.ModelID)
		}
	}
	// And the whole point: three independent sessions is exactly the minimum, so
	// the estimator now answers.
	prior := domain.EstimateSummaryTokens(domain.CompactionSummaryEstimatorInput{
		ExcludeSessionID: "judged",
		Harness:          "claude-code",
		ModelID:          "claude-opus-5",
		DecisionAt:       time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		Observations:     got,
	})
	if !prior.Known || prior.Samples != 3 {
		t.Errorf("prior = %+v, want known with 3 samples", prior)
	}
}

// A session that never compacted, or whose harness wrote no rollup, contributes
// nothing. The candidate query already excludes both; the reader refuses again,
// because a guard in one place is a guard one refactor can remove.
func TestTheSummaryPriorRefusesSessionsWithNoEvidence(t *testing.T) {
	t.Run("no rollup", func(t *testing.T) {
		store := summaryPriorStore(t, []domain.SessionID{"s-1"}, compactBoundaryRecord)
		got, err := NewCompactionReader(store, nil).CompactionSummaryObservations(context.Background(), "", "")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("observations = %d, want 0: without a rollup the summary cost is unknowable", len(got))
		}
	})

	t.Run("no compaction", func(t *testing.T) {
		store := summaryPriorStore(t, []domain.SessionID{"s-1"}, costStateRecord)
		got, err := NewCompactionReader(store, nil).CompactionSummaryObservations(context.Background(), "", "")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("observations = %d, want 0: a session that never compacted says nothing about summary cost", len(got))
		}
	})

	t.Run("the harness reported no output AO cannot see", func(t *testing.T) {
		store := summaryPriorStore(t, []domain.SessionID{"s-1"}, compactBoundaryRecord, costStateRecord)
		// The ledger accounts for at least as much output as the harness admits
		// to, on EVERY model in the rollup, so the residual floors at zero. It has
		// to be every model: crediting one generously leaves the others' output
		// unattributed, which is a real residual and not a rounding artefact.
		store.aggregates = []domain.UsageModelAggregate{
			{
				Harness: domain.HarnessClaudeCode, ModelID: "claude-opus-5",
				Tokens: domain.UsageTokenMetrics{OutputTokens: 10_000_000},
			},
			{
				Harness: domain.HarnessClaudeCode, ModelID: "claude-haiku-4-5-20251001",
				Tokens: domain.UsageTokenMetrics{OutputTokens: 10_000},
			},
		}
		got, err := NewCompactionReader(store, nil).CompactionSummaryObservations(context.Background(), "", "")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("observations = %d, want 0 rather than a zero-cost summary", len(got))
		}
	})
}

// A candidate list AO cannot read is an error the caller sees; ONE unreadable
// session inside a readable list costs one sample and never the cohort.
func TestTheSummaryPriorDegradesOneSessionAtATime(t *testing.T) {
	t.Run("the candidate list fails", func(t *testing.T) {
		store := summaryPriorStore(t, nil, compactBoundaryRecord, costStateRecord)
		store.compactedErr = fmt.Errorf("ledger unavailable")
		_, err := NewCompactionReader(store, nil).CompactionSummaryObservations(context.Background(), "", "")
		if err == nil {
			t.Error("a failed candidate read must be reported, not silently answered as an empty cohort")
		}
	})

	t.Run("a nil reader answers nothing rather than panicking", func(t *testing.T) {
		var reader *CompactionReader
		got, err := reader.CompactionSummaryObservations(context.Background(), "", "")
		if err != nil || got != nil {
			t.Errorf("got %v / %v, want nil / nil", got, err)
		}
	})
}

// The candidate ceiling is a ceiling. A cohort large enough to reach it has earned
// a folded aggregate, and truncating is better than an unbounded read on a
// decision path.
func TestTheSummaryPriorCandidateListIsBounded(t *testing.T) {
	many := make([]domain.SessionID, maxSummaryPriorCandidateSessions+25)
	for i := range many {
		many[i] = domain.SessionID(fmt.Sprintf("s-%d", i))
	}
	store := summaryPriorStore(t, many, compactBoundaryRecord, costStateRecord)
	got, err := NewCompactionReader(store, nil).CompactionSummaryObservations(context.Background(), "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != maxSummaryPriorCandidateSessions {
		t.Errorf("observations = %d, want the ceiling of %d", len(got), maxSummaryPriorCandidateSessions)
	}
}

// The reader must not price anything. Pricing is the rate card's job, and an
// observation that the reader had already judged unpriceable would be a sample the
// estimator never got to count -- which is how a sample count becomes a fiction.
func TestTheSummaryPriorCarriesTokensAndNeverAPrice(t *testing.T) {
	store := summaryPriorStore(t, []domain.SessionID{"s-1"}, compactBoundaryRecord, costStateRecord)
	// A reader with NO rate card at all must still produce the observation.
	got, err := NewCompactionReader(store, nil).CompactionSummaryObservations(context.Background(), "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("observations = %d, want 1 even with no rate card", len(got))
	}
	if got[0].UnattributedOutputTokens <= 0 {
		t.Error("the observation must carry its token figure regardless of pricing")
	}
}

// TestTheSummaryPriorFiltersOnTheExactCohort: the port's (harness, modelID) is a
// filter, compared as recorded. No alias is resolved and no spelling normalized,
// so the rollup's "claude-opus-5[1m]" is NOT the boundary's "claude-opus-5".
func TestTheSummaryPriorFiltersOnTheExactCohort(t *testing.T) {
	store := summaryPriorStore(t, []domain.SessionID{"s-1", "s-2", "s-3"}, compactBoundaryRecord, costStateRecord)
	reader := NewCompactionReader(store, nil)
	for name, tc := range map[string]struct {
		harness, model string
		want           int
	}{
		"exact cohort":           {"claude-code", "claude-opus-5", 3},
		"inventory, no filter":   {"", "", 3},
		"another model":          {"claude-code", "claude-sonnet-5", 0},
		"the rollup's alias":     {"claude-code", "claude-opus-5[1m]", 0},
		"case is not normalized": {"claude-code", "Claude-Opus-5", 0},
		"another harness":        {"codex", "claude-opus-5", 0},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := reader.CompactionSummaryObservations(context.Background(), tc.harness, tc.model)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("observations = %d, want %d", len(got), tc.want)
			}
		})
	}
}

// A session whose boundaries were taken on two different models belongs to
// neither cohort: its residual is one number and cannot be divided between them.
// It stays visible to an inventory with NO identity, and a filtered read -- the
// gate's -- never returns it.
func TestASessionThatCompactedOnTwoModelsBelongsToNoCohort(t *testing.T) {
	second := strings.Replace(compactBoundaryRecord, `"uuid":"boundary-1"`, `"uuid":"boundary-2"`, 1)
	store := summaryPriorStore(t, []domain.SessionID{"s-1"},
		compactBoundaryRecord,
		claudeTurnWithLifetimes("turn-sonnet", "claude-sonnet-5", 10_000, 0, 1_000, 100),
		second, costStateRecord)
	reader := NewCompactionReader(store, nil)

	all, err := reader.CompactionSummaryObservations(context.Background(), "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(all) != 1 || all[0].Harness != "" || all[0].ModelID != "" {
		t.Fatalf("inventory = %+v, want one observation with no cohort identity", all)
	}
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5"} {
		got, err := reader.CompactionSummaryObservations(context.Background(), "claude-code", model)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("a mixed-model session reached the %s cohort: %+v", model, got)
		}
	}
}
