package domain

// model_rate_view.go -- reading four rates instead of one total.
//
// UsageCost answers "what did this vector cost". A gate that has to compare two
// futures needs something else: the RATES, because the arithmetic it does is
// ratios between them. `(Co/Cr)` and `(Cw/Cr)` are what decide whether
// compacting a conversation pays, and no amount of pricing whole vectors
// recovers them without dividing one result by another and hoping the divisor
// was not zero.
//
// So this is a READ of the rate card and never a second rate card. It carries
// no default, no fallback and no derived value: a model the card does not cover
// produces no view at all, exactly as pricing.Table.Cost produces no amount.
//
// THE TWO CACHE-WRITE RATES ARE SEPARATE FIELDS AND THAT IS THE WHOLE POINT.
// A provider that sells a longer cache lifetime charges more to create one, and
// P7.2B1 measured that every cache write in every session AO has metered was
// created at the ONE-HOUR lifetime. A view that carried a single "cache write"
// rate would hand the gate the cheap number for the dear tokens and flatter
// every compaction by the difference -- 6.25 against 10.00 on Opus 5, which is
// the largest single term in a compaction's cost.

// ModelRateView is one model's per-million-token rates, as the rate card
// carries them.
//
// Zero is not a price. A field left at zero means the card does not state that
// rate, and a caller that needs it must refuse rather than treat it as free --
// which is what Complete and CacheWriteRateFor are for.
type ModelRateView struct {
	// ModelID is the id the rate was resolved FOR, as the caller spelled it.
	// Carried so a record can show which spelling was priced: the difference
	// between "claude-opus-5" and "claude-opus-5[1m]" is exactly the kind of
	// thing a cohort has to be able to re-identify later.
	ModelID string

	InputPerMTok     float64
	OutputPerMTok    float64
	CacheReadPerMTok float64
	// CacheWrite5mPerMTok and CacheWrite1hPerMTok are the two cache-creation
	// rates. A card that states only the short one leaves the long one at zero,
	// which is a refusal and not a discount.
	CacheWrite5mPerMTok float64
	CacheWrite1hPerMTok float64

	Currency      string
	Source        string
	Version       string
	EffectiveDate string
}

// Complete reports whether every rate the economic model divides by or
// multiplies is actually stated.
//
// CacheWrite1hPerMTok is deliberately NOT required here: a vector with no
// long-lived creation in it never touches that rate, and demanding it would
// refuse to price a session the card covers perfectly well. The caller asks for
// the lifetime it actually needs through CacheWriteRateFor.
func (r ModelRateView) Complete() bool {
	return r.CacheReadPerMTok > 0 && r.OutputPerMTok > 0 && r.CacheWrite5mPerMTok > 0
}

// CacheWriteRateFor is the rate for creating a cache entry with the given
// lifetime, and whether the card states it.
//
// The unknown lifetime has NO rate. That is the P7.2B1 rule carried into the
// gate: cache creation whose lifetime nothing reported cannot be priced at the
// short rate "for now", because the short rate is wrong for 96.7% of the
// creation AO has actually measured. An unknown lifetime is a refusal.
func (r ModelRateView) CacheWriteRateFor(ttl CacheWriteLifetime) (float64, bool) {
	switch ttl {
	case CacheWriteLifetime5m:
		return r.CacheWrite5mPerMTok, r.CacheWrite5mPerMTok > 0
	case CacheWriteLifetime1h:
		return r.CacheWrite1hPerMTok, r.CacheWrite1hPerMTok > 0
	default:
		return 0, false
	}
}

// CacheWriteLifetime is which of the provider's two cache lifetimes a write
// was, or will be, created with. Closed enum; Unknown is a real state and the
// one that refuses.
type CacheWriteLifetime string

// The lifetimes AO recognises. The strings are the provider's own vocabulary,
// matching CacheCreationSplit's field names.
const (
	CacheWriteLifetime5m      CacheWriteLifetime = "5m"
	CacheWriteLifetime1h      CacheWriteLifetime = "1h"
	CacheWriteLifetimeUnknown CacheWriteLifetime = "unknown"
)

// Valid reports whether a lifetime is part of the closed enum.
func (l CacheWriteLifetime) Valid() bool {
	switch l {
	case CacheWriteLifetime5m, CacheWriteLifetime1h, CacheWriteLifetimeUnknown:
		return true
	default:
		return false
	}
}

// DominantCacheWriteLifetime is the lifetime a session's NEXT cache write should
// be priced at, inferred from the lifetimes its previous writes were created
// with.
//
// The rule is fail-closed in the only direction that matters: any unknown
// contribution at all makes the answer Unknown, and where both known lifetimes
// are present the LONGER one wins because it is the dearer one. A gate that
// picked the majority lifetime would price a 51%/49% session entirely at the
// cheap rate and understate the rewrite it is about to pay for.
//
// A split with nothing in it is Unknown, not 5m: a session that has created no
// cache yet has told AO nothing about what its next creation will cost.
func DominantCacheWriteLifetime(s CacheCreationSplit) CacheWriteLifetime {
	if s.UnknownTTLTokens > 0 || s.Total() == 0 {
		return CacheWriteLifetimeUnknown
	}
	if s.Ephemeral1hTokens > 0 {
		return CacheWriteLifetime1h
	}
	return CacheWriteLifetime5m
}
