package pricing_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage/pricing"
)

func TestCost_UnknownModelIsUnknownNotZero(t *testing.T) {
	// The single most important behaviour in this package. A model no rate
	// covers must produce "unknown", because $0.00 is a claim — it says the
	// tokens were free — and AO has no basis for it.
	table := pricing.Embedded()
	cost := table.Cost("gpt-5-codex", domain.UsageTokenTotals{
		UncachedInputTokens: 1_000_000, OutputTokens: 1_000_000,
	})
	if cost.Known {
		t.Fatalf("cost = %+v, want unknown for an uncovered model", cost)
	}
	if cost.Basis != domain.CostUnknown {
		t.Fatalf("basis = %q, want unknown", cost.Basis)
	}
	if cost.Amount != 0 || len(cost.UnpricedModels) != 1 || cost.UnpricedModels[0] != "gpt-5-codex" {
		t.Fatalf("unpriced model must be named, got %+v", cost)
	}
}

func TestCost_PricesEachDimensionSeparately(t *testing.T) {
	// Cache reads and writes are not billed at the input rate, which is the
	// whole reason those dimensions are captured. A rate card that folded them
	// into `input` would overcharge a cache-heavy run by an order of magnitude.
	table := pricing.Embedded()
	cost := table.Cost("claude-opus-5", domain.UsageTokenTotals{
		UncachedInputTokens: 1_000_000,
		CacheReadTokens:     1_000_000,
		CacheWriteTokens:    1_000_000,
		OutputTokens:        1_000_000,
	})
	if !cost.Known {
		t.Fatal("claude-opus-5 is in the embedded catalog and must price")
	}
	// 5.00 input + 0.50 cache read + 6.25 cache write + 25.00 output
	if diff := cost.Amount - 36.75; diff > 0.0001 || diff < -0.0001 {
		t.Fatalf("amount = %f, want 36.75", cost.Amount)
	}
	if cost.Basis != domain.CostCalculated {
		t.Fatalf("basis = %q, want calculated — AO computes this, no provider reports it", cost.Basis)
	}
	if cost.PricingSource == "" || cost.PricingVersion == "" || cost.EffectiveDate == "" {
		t.Fatalf("a calculated cost must carry its rate card's provenance, got %+v", cost)
	}
}

func TestRate_LongestPrefixWins(t *testing.T) {
	// A dated snapshot id must resolve to its family; a longer, more specific
	// row must never be stolen by a shorter one.
	table := pricing.Embedded()
	if _, ok := table.Rate("claude-haiku-4-5-20251001"); !ok {
		t.Fatal("a dated model id must resolve to its family's rate")
	}
	if _, ok := table.Rate("claude"); ok {
		t.Fatal("a bare vendor prefix must not resolve to any model's rate")
	}
	if _, ok := table.Rate(""); ok {
		t.Fatal("an empty model id has no rate")
	}
}

func TestLoad_RateCardOverridesAndCarriesItsOwnProvenance(t *testing.T) {
	// The supported way to price a model this binary does not know. The
	// resulting cost must NOT keep claiming to be Anthropic's list price.
	dir := t.TempDir()
	write(t, dir, `{
	  "source": "acme-negotiated-rates",
	  "version": "2026-09",
	  "effectiveDate": "2026-09-01",
	  "currency": "USD",
	  "models": [
	    {"match": "gpt-5-codex", "provider": "openai",
	     "inputPerMTok": 2.0, "outputPerMTok": 8.0,
	     "cacheReadPerMTok": 0.2, "cacheWritePerMTok": 2.5}
	  ]
	}`)
	table, err := pricing.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cost := table.Cost("gpt-5-codex", domain.UsageTokenTotals{
		UncachedInputTokens: 1_000_000, OutputTokens: 1_000_000,
	})
	if !cost.Known || cost.Amount != 10.0 {
		t.Fatalf("cost = %+v, want a known 10.00 from the operator's rates", cost)
	}
	if cost.PricingVersion != "2026-09" {
		t.Fatalf("pricingVersion = %q, want the override's own version", cost.PricingVersion)
	}
	// The embedded rates survive underneath for models the card did not name.
	if _, ok := table.Rate("claude-opus-5"); !ok {
		t.Fatal("an override must layer over the embedded catalog, not replace it wholesale")
	}
}

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	table, err := pricing.Load(t.TempDir())
	if err != nil {
		t.Fatalf("a missing rate card is the ordinary case, got %v", err)
	}
	if _, ok := table.Rate("claude-opus-5"); !ok {
		t.Fatal("embedded rates must still be in force")
	}
}

func TestLoad_InvalidRateCardIsRefusedRatherThanPartiallyApplied(t *testing.T) {
	// A rate card with a negative rate is a configuration error. Applying the
	// rows that happened to parse would produce a cost nobody could account
	// for, so the whole card is refused.
	dir := t.TempDir()
	write(t, dir, `{"source":"x","version":"1","currency":"USD",
	  "models":[{"match":"m","inputPerMTok":-1,"outputPerMTok":1,"cacheReadPerMTok":0,"cacheWritePerMTok":0}]}`)
	if _, err := pricing.Load(dir); !errors.Is(err, pricing.ErrRateCardInvalid) {
		t.Fatalf("err = %v, want ErrRateCardInvalid", err)
	}
}

func TestCost_NilTablePricesNothing(t *testing.T) {
	var table *pricing.Table
	if cost := table.Cost("claude-opus-5", domain.UsageTokenTotals{OutputTokens: 100}); cost.Known {
		t.Fatalf("a nil table must price nothing, got %+v", cost)
	}
}

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, pricing.RateCardFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("write rate card: %v", err)
	}
}
