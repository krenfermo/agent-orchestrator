package pricing_test

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage/pricing"
)

// rate_view_test.go -- RateView is a READ of the rate card and never a second
// rate card. Every test below is a way that could stop being true.

// TestRateViewReportsBothCacheWriteLifetimesSeparately. The two cache-creation
// rates are separate fields because the provider charges differently for them,
// and collapsing them is the defect P7.2B1 was opened to remove.
func TestRateViewReportsBothCacheWriteLifetimesSeparately(t *testing.T) {
	view, ok := pricing.Embedded().RateView("claude-opus-5")
	if !ok {
		t.Fatal("the embedded catalog covers claude-opus-5")
	}
	closeTo(t, "input", view.InputPerMTok, 5.00)
	closeTo(t, "output", view.OutputPerMTok, 25.00)
	closeTo(t, "cache read", view.CacheReadPerMTok, 0.50)
	closeTo(t, "cache write 5m", view.CacheWrite5mPerMTok, 6.25)
	closeTo(t, "cache write 1h", view.CacheWrite1hPerMTok, 10.00)
	if view.CacheWrite1hPerMTok <= view.CacheWrite5mPerMTok {
		t.Error("the long cache lifetime must cost more to create than the short one")
	}
	if !view.Complete() {
		t.Error("a fully covered model must report complete rates")
	}
	if view.Currency != "USD" || view.Source == "" || view.Version == "" {
		t.Errorf("provenance must travel with the rates, got %+v", view)
	}
}

// TestRateViewKeepsTheIdItWasAsked. A record has to be able to show that
// "claude-opus-5[1m]" was the spelling looked up; a view carrying the matching
// row's own pattern would erase exactly that distinction.
func TestRateViewKeepsTheIdItWasAsked(t *testing.T) {
	view, ok := pricing.Embedded().RateView("claude-opus-5-mini-2026")
	if !ok {
		t.Fatal("the explicit prefix rule covers this id")
	}
	if view.ModelID != "claude-opus-5-mini-2026" {
		t.Errorf("modelID = %q, want the id as asked", view.ModelID)
	}
}

// TestRateViewRefusesAModelTheCardDoesNotCover. No default, no family fallback,
// no zero standing in for a rate.
func TestRateViewRefusesAModelTheCardDoesNotCover(t *testing.T) {
	for _, id := range []string{
		"gpt-5.6-sol",
		// The bare alias: it is not "claude-sonnet-5" and must not be priced as
		// if it were. The canonical key P7.1 uses to line a residual up against
		// an attributed line is a MATCHING device and must never become a
		// pricing device.
		"sonnet",
		// The long-context variant. Real, reported by the harness, and not on
		// the card -- the bracket is not a hyphen, so the prefix rule cannot
		// reach it, which is correct because a long-context variant is exactly
		// the kind of thing that might not cost the same.
		"claude-opus-5[1m]",
		"",
		"   ",
	} {
		if view, ok := pricing.Embedded().RateView(id); ok {
			t.Errorf("RateView(%q) returned rates %+v; an uncovered model must produce none", id, view)
		}
	}
}

// TestAnOperatorRateCardChangesTheViewAndNothingElse. The documented, auditable
// way to price a variant: name it explicitly.
func TestAnOperatorRateCardChangesTheViewAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{
	  "source": "acme-negotiated-rates",
	  "version": "2026-09",
	  "effectiveDate": "2026-09-01",
	  "currency": "USD",
	  "models": [
	    {"match": "claude-opus-5[1m]", "provider": "anthropic",
	     "inputPerMTok": 5.0, "outputPerMTok": 25.0,
	     "cacheReadPerMTok": 0.5, "cacheWritePerMTok": 6.25, "cacheWrite1hPerMTok": 10.0}
	  ]
	}`)
	table, err := pricing.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	view, ok := table.RateView("claude-opus-5[1m]")
	if !ok {
		t.Fatal("an explicit row must make the variant priceable")
	}
	closeTo(t, "cache write 1h", view.CacheWrite1hPerMTok, 10.00)
	// The table composes provenance -- "the override, over the embedded catalog"
	// -- so a reader can see both. The override must be named and the version
	// must be the override's own.
	if !strings.Contains(view.Source, "acme-negotiated-rates") || view.Version != "2026-09" {
		t.Errorf("the view must carry the OVERRIDE's provenance, got %q %q", view.Source, view.Version)
	}
	// And the embedded rates survive underneath, unchanged.
	base, ok := table.RateView("claude-opus-5")
	if !ok {
		t.Fatal("an override layers over the embedded catalog rather than replacing it")
	}
	closeTo(t, "embedded cache write 1h", base.CacheWrite1hPerMTok, 10.00)
}

// TestACardWithoutTheLongRateRefusesLongLivedCreation. The rule that makes an
// unstated rate a refusal instead of a discount.
func TestACardWithoutTheLongRateRefusesLongLivedCreation(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{
	  "source": "short-only", "version": "1", "effectiveDate": "2026-01-01", "currency": "USD",
	  "models": [
	    {"match": "acme-1", "inputPerMTok": 1.0, "outputPerMTok": 2.0,
	     "cacheReadPerMTok": 0.1, "cacheWritePerMTok": 1.25}
	  ]
	}`)
	table, err := pricing.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	view, ok := table.RateView("acme-1")
	if !ok {
		t.Fatal("the card covers acme-1")
	}
	// Complete() does not demand the long rate: a vector with no long-lived
	// creation in it never touches that rate, and refusing such a model would
	// decline to price a session the card covers perfectly well.
	if !view.Complete() {
		t.Error("a card stating the short rate is complete for short-lived creation")
	}
	if _, ok := view.CacheWriteRateFor(domain.CacheWriteLifetime5m); !ok {
		t.Error("the short rate is stated and must be usable")
	}
	if rate, ok := view.CacheWriteRateFor(domain.CacheWriteLifetime1h); ok {
		t.Errorf("the long rate is NOT stated; got %v as if it were", rate)
	}
	// And the unknown lifetime has no rate at all, ever.
	if _, ok := view.CacheWriteRateFor(domain.CacheWriteLifetimeUnknown); ok {
		t.Error("an unknown cache lifetime must never resolve to a rate")
	}
}

// TestDominantCacheWriteLifetimeFailsClosed. Which rate the NEXT write is priced
// at is read from what the previous writes were, and any doubt is Unknown.
func TestDominantCacheWriteLifetimeFailsClosed(t *testing.T) {
	cases := []struct {
		name  string
		split domain.CacheCreationSplit
		want  domain.CacheWriteLifetime
	}{
		{"all long lived", domain.CacheCreationSplit{Ephemeral1hTokens: 100}, domain.CacheWriteLifetime1h},
		{"all short lived", domain.CacheCreationSplit{Ephemeral5mTokens: 100}, domain.CacheWriteLifetime5m},
		// The DEARER lifetime wins a mix. A gate picking the majority would
		// price a 51/49 session entirely at the cheap rate and understate the
		// rewrite it is about to pay for.
		{"mostly short lived", domain.CacheCreationSplit{Ephemeral5mTokens: 99, Ephemeral1hTokens: 1}, domain.CacheWriteLifetime1h},
		{"one unknown token", domain.CacheCreationSplit{Ephemeral1hTokens: 100, UnknownTTLTokens: 1}, domain.CacheWriteLifetimeUnknown},
		// A session that has created no cache has told AO nothing about what its
		// next creation will cost. Unknown, not 5m.
		{"nothing created yet", domain.CacheCreationSplit{}, domain.CacheWriteLifetimeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.DominantCacheWriteLifetime(tc.split); got != tc.want {
				t.Errorf("lifetime = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheRateRatiosComeFromTheCardAndAreNotUniform is the test that fails if
// anyone writes 62.5 or 70 into the gate.
//
// Break-even in CALLS is model-independent across most of the embedded card,
// which is a coincidence of uniform cache multipliers rather than a law --
// fable-5-1 breaks it by 4x with a cache-read rate a quarter of its siblings'.
func TestTheRateRatiosComeFromTheCardAndAreNotUniform(t *testing.T) {
	table := pricing.Embedded()
	ratio := func(id string) float64 {
		view, ok := table.RateView(id)
		if !ok {
			t.Fatalf("the embedded catalog must cover %s", id)
		}
		return (view.OutputPerMTok + view.CacheWrite1hPerMTok) / view.CacheReadPerMTok
	}
	opus := ratio("claude-opus-5")
	sonnet := ratio("claude-sonnet-5")
	fable := ratio("claude-fable-5-1")

	// At the ONE-HOUR write rate the uniform part of the card is 70, not the
	// 62.5 that the five-minute rate produces.
	closeTo(t, "opus-5 (Co+Cw1h)/Cr", opus, 70.0)
	closeTo(t, "sonnet-5 (Co+Cw1h)/Cr", sonnet, 70.0)
	if fable <= opus*3 {
		t.Errorf("fable-5-1 ratio = %.1f against opus-5's %.1f; the card is NOT uniform and a hardcoded constant would be wrong on a model already in it",
			fable, opus)
	}
}
