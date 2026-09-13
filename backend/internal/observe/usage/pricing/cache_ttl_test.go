package pricing_test

import (
	"math"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage/pricing"
)

func closeTo(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.0001 {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func TestCacheCreationIsPricedByLifetime(t *testing.T) {
	table := pricing.Embedded()
	// claude-opus-5: input 5.00, read 0.50, 5m write 6.25, 1h write 10.00.
	for _, tc := range []struct {
		name  string
		split domain.CacheCreationSplit
		want  float64
	}{
		{"short lived", domain.CacheCreationSplit{Ephemeral5mTokens: 1_000_000}, 6.25},
		{"long lived", domain.CacheCreationSplit{Ephemeral1hTokens: 1_000_000}, 10.00},
		{"both", domain.CacheCreationSplit{Ephemeral5mTokens: 500_000, Ephemeral1hTokens: 500_000}, 8.125},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cost := table.Cost("claude-opus-5", domain.UsageTokenTotals{
				InputTokens:      tc.split.Total(),
				CacheWriteTokens: tc.split.Total(),
				CacheCreation:    tc.split,
			})
			if !cost.Known {
				t.Fatal("a split with every lifetime named must price")
			}
			if cost.TTLAssumedTokens != 0 {
				t.Fatalf("nothing was assumed, got %d", cost.TTLAssumedTokens)
			}
			closeTo(t, tc.name, cost.Amount, tc.want)
		})
	}
}

func TestTheSplitIsUsedAndTheTotalIsNotAddedToIt(t *testing.T) {
	table := pricing.Embedded()
	cost := table.Cost("claude-opus-5", domain.UsageTokenTotals{
		CacheWriteTokens: 1_000_000,
		CacheCreation:    domain.CacheCreationSplit{Ephemeral1hTokens: 1_000_000},
	})
	// 10.00 and not 16.25: the split IS the total, divided.
	closeTo(t, "no double count", cost.Amount, 10.00)
}

func TestAnUnreportedLifetimeIsPricedShortAndDisclosed(t *testing.T) {
	table := pricing.Embedded()
	cost := table.Cost("claude-opus-5", domain.UsageTokenTotals{
		InputTokens: 1_000_000, CacheWriteTokens: 1_000_000,
	})
	// It still prices -- blanking every legacy cost would take the whole
	// ledger down with it -- but the quantity resting on the assumption is
	// reported, and on the measured corpus 97.9% of creation is long-lived, so
	// a figure like this is very likely an understatement.
	if !cost.Known {
		t.Fatal("a lifetime-unknown vector must still produce a figure")
	}
	closeTo(t, "priced short", cost.Amount, 6.25)
	if cost.TTLAssumedTokens != 1_000_000 {
		t.Fatalf("assumed = %d, want the whole quantity disclosed", cost.TTLAssumedTokens)
	}
}

func TestAModelWithoutALongLifetimeRateRefusesLongLivedCreation(t *testing.T) {
	// An operator rate card that knows the model and only its short rate.
	dir := t.TempDir()
	write(t, dir, `{
	  "source":"operator","version":"1","effectiveDate":"2026-09-12","currency":"USD",
	  "models":[{"match":"claude-opus-5","inputPerMTok":5,"outputPerMTok":25,
	             "cacheReadPerMTok":0.5,"cacheWritePerMTok":6.25}]
	}`)
	table, err := pricing.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cost := table.Cost("claude-opus-5", domain.UsageTokenTotals{
		CacheWriteTokens: 1000,
		CacheCreation:    domain.CacheCreationSplit{Ephemeral1hTokens: 1000},
	})
	if cost.Known {
		t.Fatal("a rate card with no long-lifetime rate cannot price long-lived creation")
	}
	if cost.TTLUnknownTokens != 1000 {
		t.Fatalf("unpriceable = %d, want 1000", cost.TTLUnknownTokens)
	}
	if len(cost.UnpricedModels) != 1 {
		t.Fatalf("the model must be named, got %v", cost.UnpricedModels)
	}
}

func TestAnUnpricedModelStaysUnpricedWhateverTheLifetime(t *testing.T) {
	cost := pricing.Embedded().Cost("gpt-5.6-sol", domain.UsageTokenTotals{
		CacheWriteTokens: 1000,
		CacheCreation:    domain.CacheCreationSplit{Ephemeral1hTokens: 1000},
	})
	if cost.Known || len(cost.UnpricedModels) != 1 {
		t.Fatalf("cost = %+v, want unknown and the model named", cost)
	}
}

// TestTheMeasuredSessionsReconcileWithTheHarness is the arithmetic that made
// this checkpoint exist. Both vectors and both cost figures are measured: the
// tokens from AO's own ledger, the split from the transcript's cache_creation
// block, the target from the harness's own end-of-session rollup.
func TestTheMeasuredSessionsReconcileWithTheHarness(t *testing.T) {
	table := pricing.Embedded()
	for _, tc := range []struct {
		name                                string
		unc, read, write, out               int64
		fiveMinute, oneHour                 int64
		harnessReported                     float64
		wantBefore, wantAfter               float64
		wantExplainedBefore, wantAfterShare float64
	}{
		{
			name: "wf-1c2cb9bd, 193 calls, never compacted",
			unc:  386, read: 35_787_742, write: 294_688, out: 139_003,
			fiveMinute: 0, oneHour: 294_688,
			harnessReported: 24.805628,
			wantBefore:      23.2127, wantAfter: 24.3178,
			wantExplainedBefore: 93.6, wantAfterShare: 98.0,
		},
		{
			name: "wf-66f0ee54, 28 calls, compacted twice",
			unc:  56, read: 1_719_470, write: 110_284, out: 30_500,
			fiveMinute: 0, oneHour: 110_284,
			harnessReported: 3.4028525,
			wantBefore:      2.3118, wantAfter: 2.7254,
			wantExplainedBefore: 67.9, wantAfterShare: 80.1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := domain.UsageTokenTotals{
				UncachedInputTokens: tc.unc, CacheReadTokens: tc.read,
				CacheWriteTokens: tc.write, OutputTokens: tc.out,
			}
			before := table.Cost("claude-opus-5", base)
			if before.TTLAssumedTokens != tc.write {
				t.Fatalf("the pre-checkpoint vector assumes a lifetime for all %d writes, got %d",
					tc.write, before.TTLAssumedTokens)
			}
			withTTL := base
			withTTL.CacheCreation = domain.CacheCreationSplit{
				Ephemeral5mTokens: tc.fiveMinute, Ephemeral1hTokens: tc.oneHour,
			}
			after := table.Cost("claude-opus-5", withTTL)
			if after.TTLAssumedTokens != 0 {
				t.Fatal("nothing is assumed once the lifetime is known")
			}
			if math.Abs(before.Amount-tc.wantBefore) > 0.001 {
				t.Fatalf("before = %.4f, want %.4f", before.Amount, tc.wantBefore)
			}
			if math.Abs(after.Amount-tc.wantAfter) > 0.001 {
				t.Fatalf("after = %.4f, want %.4f", after.Amount, tc.wantAfter)
			}
			explainedBefore := 100 * before.Amount / tc.harnessReported
			explainedAfter := 100 * after.Amount / tc.harnessReported
			if math.Abs(explainedBefore-tc.wantExplainedBefore) > 0.15 {
				t.Fatalf("explained before = %.1f%%, want %.1f%%", explainedBefore, tc.wantExplainedBefore)
			}
			if math.Abs(explainedAfter-tc.wantAfterShare) > 0.15 {
				t.Fatalf("explained after = %.1f%%, want %.1f%%", explainedAfter, tc.wantAfterShare)
			}
			// The gap must close and must NOT close completely: what is left
			// is spend outside the calls, which is P7.1's subject and not this
			// checkpoint's.
			if after.Amount >= tc.harnessReported {
				t.Fatalf("after (%.4f) must stay under the harness figure (%.4f)", after.Amount, tc.harnessReported)
			}
		})
	}
}
