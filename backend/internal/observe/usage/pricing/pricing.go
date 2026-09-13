// Package pricing owns what a token costs. It is backend-owned on purpose:
// a rate is provenance-bearing data, and a rate hidden in a React component is
// a number nobody can audit, version, or override.
//
// THE RULE THIS PACKAGE EXISTS TO ENFORCE. A cost figure may only exist when a
// rate covers the exact model that spent the tokens. There is no default rate,
// no "close enough" family fallback beyond an explicit prefix a human wrote
// down, and no zero. A model this catalog does not know produces
// domain.CostUnknown, and the tokens are still reported in full — P3-E §3:
// "Si pricing no está disponible: tokens sí, cost = unknown. No inventar
// precio."
//
// PROVENANCE TRAVELS WITH THE NUMBER. Every Catalog carries a Source, a
// Version and an EffectiveDate, and every UsageCost this package produces
// repeats them, so the UI can say which rate card produced $0.42 and from when
// — rather than presenting a list price as if it were a bill.
//
// WHAT A CALCULATED COST IS NOT. It is a list-price equivalent of the tokens
// spent. AO cannot see how a provider actually bills the account behind a
// harness: Claude Code may be running against a subscription, in which case the
// marginal cash cost of these tokens is not this number at all. That is why
// domain.UsageChannel exists and stays "unknown" until a real billing signal
// appears, and why nothing here is ever labeled provider_reported.
package pricing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// RateCardFileName is the file, under AO's data dir, that lets an operator
// supply or correct rates without a rebuild. It is the supported way to price
// a model this binary's embedded catalog does not know (every OpenAI/Codex
// model today) or to replace a rate that has since changed.
const RateCardFileName = "usage-pricing.json"

// ModelRate is one model's price, per million tokens, in Currency.
//
// The four dimensions match what the V1 usage parser actually normalizes.
// UncachedInput, CacheRead and CacheWrite partition a provider-reported input
// count, so they are billed separately rather than all at the input rate — the
// whole reason cache accounting is worth capturing at all.
type ModelRate struct {
	// Match is compared against a model id after normalization. A model id
	// equal to Match, or beginning with Match followed by a "-", matches; the
	// longest matching prefix wins, so "claude-opus-5" never steals a row
	// written for "claude-opus-5-mini".
	Match    string `json:"match"`
	Provider string `json:"provider,omitempty"`

	InputPerMTok     float64 `json:"inputPerMTok"`
	OutputPerMTok    float64 `json:"outputPerMTok"`
	CacheReadPerMTok float64 `json:"cacheReadPerMTok"`
	// CacheWritePerMTok is the rate for creating a cache entry with the
	// SHORT (5-minute) lifetime. It kept its original name because that is
	// what it always meant -- the embedded catalog's own Source string has
	// said "5-minute-TTL cache multipliers" since the day it was written.
	CacheWritePerMTok float64 `json:"cacheWritePerMTok"`
	// CacheWrite1hPerMTok is the rate for creating an entry with the LONG
	// (1-hour) lifetime, which costs more because it lasts longer.
	//
	// Zero means this rate card does not carry the long rate. It is NOT read
	// as free and NOT read as equal to the short one: a model whose long-TTL
	// writes cannot be priced reports an unknown cost for them, which is the
	// same rule an unpriced model already follows.
	CacheWrite1hPerMTok float64 `json:"cacheWrite1hPerMTok"`
}

// PricesCacheTTL reports whether this rate covers both cache lifetimes.
func (m ModelRate) PricesCacheTTL() bool {
	return m.CacheWritePerMTok >= 0 && m.CacheWrite1hPerMTok > 0
}

// Catalog is a versioned set of rates plus the provenance every cost derived
// from it must carry.
type Catalog struct {
	Source        string      `json:"source"`
	Version       string      `json:"version"`
	EffectiveDate string      `json:"effectiveDate"`
	Currency      string      `json:"currency"`
	Models        []ModelRate `json:"models"`
}

// anthropicListPrices is the embedded catalog.
//
// Input and output rates are Anthropic's published first-party API list
// prices. Cache rates are derived from them by Anthropic's published cache
// multipliers — 0.1x input for a read, 1.25x input to create a 5-minute entry,
// 2x input to create a 1-hour one — which is why Source names both halves: a
// reader must be able to tell a quoted rate from a derived one.
//
// THE LONG RATE IS NOT AN EMBELLISHMENT. On every Claude transcript AO holds --
// 4,523 assistant messages across 75 files -- 97.9% of cache creation is at the
// ONE-HOUR lifetime, and pricing all of it at the 5-minute rate understated one
// measured session by 15.2%. Reproducing the harness's own cost figure for run
// wf-1c2cb9bd requires exactly this rate and no other change:
//
//	1,488*5.00 + 36,634,616*0.50 + 294,688*10.00 + 139,003*25.00 = $24.805628
//	  and the harness's own cost-state reports                      $24.805628
//
// There is deliberately NO entry for any OpenAI/Codex model. AO meters Codex
// tokens exactly as it meters Claude's, but this binary has no rate it can
// vouch for, so those tokens report cost=unknown until an operator supplies a
// rate card. That is the honest outcome, not a gap to paper over with a guess.
var anthropicListPrices = Catalog{
	Source:        "anthropic-list-price + published cache multipliers (0.1x read, 1.25x 5m write, 2x 1h write)",
	Version:       "2026-09-12",
	EffectiveDate: "2026-06-24",
	Currency:      "USD",
	Models: []ModelRate{
		anthropicRate("claude-fable-5-1", 10.00, 50.00, 0.25, 12.50, 20.00),
		anthropicRate("claude-mythos-5-1", 10.00, 50.00, 0.25, 12.50, 20.00),
		anthropicRate("claude-fable-5", 10.00, 50.00, 1.00, 12.50, 20.00),
		anthropicRate("claude-opus-5", 5.00, 25.00, 0.50, 6.25, 10.00),
		anthropicRate("claude-opus-4-8", 5.00, 25.00, 0.50, 6.25, 10.00),
		anthropicRate("claude-opus-4-7", 5.00, 25.00, 0.50, 6.25, 10.00),
		anthropicRate("claude-opus-4-6", 5.00, 25.00, 0.50, 6.25, 10.00),
		anthropicRate("claude-sonnet-5", 2.00, 10.00, 0.20, 2.50, 4.00),
		anthropicRate("claude-sonnet-4-6", 3.00, 15.00, 0.30, 3.75, 6.00),
		anthropicRate("claude-haiku-4-5", 1.00, 5.00, 0.10, 1.25, 2.00),
	},
}

// anthropicRate builds one embedded catalog row. The vendor is hard-coded
// because the embedded catalog holds Anthropic rates only: AO meters Codex
// tokens exactly the same way, but this binary has no OpenAI rate it can vouch
// for, so those models report cost=unknown until an operator supplies a rate
// card. An operator's card names its own provider per row.
func anthropicRate(match string, in, out, cacheRead, cacheWrite5m, cacheWrite1h float64) ModelRate {
	return ModelRate{
		Match: match, Provider: "anthropic",
		InputPerMTok: in, OutputPerMTok: out,
		CacheReadPerMTok:    cacheRead,
		CacheWritePerMTok:   cacheWrite5m,
		CacheWrite1hPerMTok: cacheWrite1h,
	}
}

// Table is a resolved catalog ready to price with: the embedded rates, with an
// operator rate card layered on top when one exists.
type Table struct {
	source        string
	version       string
	effectiveDate string
	currency      string
	// rates is sorted longest-Match-first so prefix resolution is a linear
	// scan that cannot be fooled by a shorter row.
	rates []ModelRate
}

// ErrRateCardInvalid is the sentinel every rejected rate card wraps. A bad
// rate card must never silently fall back to embedded prices for the models it
// meant to override, so loading it is an error rather than a warning.
var ErrRateCardInvalid = errors.New("invalid usage rate card")

var (
	defaultOnce  sync.Once
	defaultTable *Table
)

// Embedded returns the compiled-in catalog with no operator overrides. Tests
// and callers that must not touch the filesystem use this.
func Embedded() *Table { return newTable(anthropicListPrices, nil) }

// Load resolves the pricing table for dataDir, layering
// <dataDir>/usage-pricing.json over the embedded catalog when it exists. A
// missing file is not an error — most installations have none.
func Load(dataDir string) (*Table, error) {
	path := filepath.Join(dataDir, RateCardFileName)
	raw, err := os.ReadFile(path) //nolint:gosec // operator-owned file under AO's own data dir
	if errors.Is(err, fs.ErrNotExist) {
		return Embedded(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read rate card %s: %w", path, err)
	}
	var override Catalog
	if err := json.Unmarshal(raw, &override); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrRateCardInvalid, path, err)
	}
	if err := validate(override); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrRateCardInvalid, path, err)
	}
	return newTable(anthropicListPrices, &override), nil
}

// Default is the process-wide table, resolved once against AO's data dir. A
// rate card that fails to load leaves the embedded catalog in place and the
// error is returned to the caller that asked for it, never swallowed into a
// silently wrong price.
func Default(dataDir string) *Table {
	defaultOnce.Do(func() {
		table, err := Load(dataDir)
		if err != nil || table == nil {
			table = Embedded()
		}
		defaultTable = table
	})
	return defaultTable
}

func validate(c Catalog) error {
	if strings.TrimSpace(c.Source) == "" {
		return errors.New("source is required")
	}
	if strings.TrimSpace(c.Version) == "" {
		return errors.New("version is required")
	}
	if strings.TrimSpace(c.Currency) == "" {
		return errors.New("currency is required")
	}
	for i, m := range c.Models {
		if strings.TrimSpace(m.Match) == "" {
			return fmt.Errorf("models[%d]: match is required", i)
		}
		for name, v := range map[string]float64{
			"inputPerMTok": m.InputPerMTok, "outputPerMTok": m.OutputPerMTok,
			"cacheReadPerMTok": m.CacheReadPerMTok, "cacheWritePerMTok": m.CacheWritePerMTok,
			"cacheWrite1hPerMTok": m.CacheWrite1hPerMTok,
		} {
			if v < 0 {
				return fmt.Errorf("models[%d] (%s): %s must not be negative", i, m.Match, name)
			}
		}
	}
	return nil
}

func newTable(base Catalog, override *Catalog) *Table {
	t := &Table{
		source: base.Source, version: base.Version,
		effectiveDate: base.EffectiveDate, currency: base.Currency,
	}
	byMatch := map[string]ModelRate{}
	for _, m := range base.Models {
		byMatch[normalize(m.Match)] = m
	}
	if override != nil {
		// An override replaces the provenance wholesale. A cost computed
		// partly from an operator's rates is not "anthropic list price", and
		// saying so would be the exact mislabeling this package forbids.
		t.source = override.Source + " (over " + base.Source + ")"
		t.version = override.Version
		t.effectiveDate = override.EffectiveDate
		t.currency = override.Currency
		for _, m := range override.Models {
			byMatch[normalize(m.Match)] = m
		}
	}
	for key, m := range byMatch {
		m.Match = key
		t.rates = append(t.rates, m)
	}
	sort.Slice(t.rates, func(i, j int) bool {
		if len(t.rates[i].Match) != len(t.rates[j].Match) {
			return len(t.rates[i].Match) > len(t.rates[j].Match)
		}
		return t.rates[i].Match < t.rates[j].Match
	})
	return t
}

// Source names the rate card behind every cost this table produces. It is the
// first half of a calculated cost's provenance: a reader must be able to tell
// which prices produced a figure, and whether any of them were derived rather
// than quoted.
func (t *Table) Source() string { return t.source }

// Version is the rate card's revision, so two costs computed months apart are
// comparable only when this matches.
func (t *Table) Version() string { return t.version }

// EffectiveDate is when the rates in this table took effect.
func (t *Table) EffectiveDate() string { return t.effectiveDate }

// Currency is the currency every amount from this table is denominated in.
func (t *Table) Currency() string { return t.currency }

// Rate returns the rate covering modelID, and whether one exists at all.
func (t *Table) Rate(modelID string) (ModelRate, bool) {
	if t == nil {
		return ModelRate{}, false
	}
	id := normalize(modelID)
	if id == "" {
		return ModelRate{}, false
	}
	for _, m := range t.rates {
		if id == m.Match || strings.HasPrefix(id, m.Match+"-") {
			return m, true
		}
	}
	return ModelRate{}, false
}

// Cost prices one model's token vector. An uncovered model returns a cost that
// is not Known and names the model in UnpricedModels, so a partial total shows
// exactly which part it is missing rather than reading as complete-and-cheap.
func (t *Table) Cost(modelID string, tokens domain.UsageTokenTotals) domain.UsageCost {
	rate, ok := t.Rate(modelID)
	if !ok {
		return domain.UsageCost{
			Known: false, Basis: domain.CostUnknown,
			UnpricedModels: []string{strings.TrimSpace(modelID)},
		}
	}
	const perMillion = 1_000_000.0
	writeCost, assumed, unpriceable := cacheCreationCost(rate, tokens)
	if unpriceable > 0 {
		// This model's rate card knows the short cache lifetime and not the
		// long one, and the vector contains long-lived creation. There is no
		// honest amount: the short rate is the wrong price for these tokens by
		// construction.
		return domain.UsageCost{
			Known: false, Basis: domain.CostUnknown,
			UnpricedModels:   []string{strings.TrimSpace(modelID)},
			TTLUnknownTokens: unpriceable,
		}
	}
	amount := float64(tokens.UncachedInputTokens)*rate.InputPerMTok/perMillion +
		float64(tokens.CacheReadTokens)*rate.CacheReadPerMTok/perMillion +
		writeCost +
		float64(tokens.OutputTokens)*rate.OutputPerMTok/perMillion
	return domain.UsageCost{
		Known: true, Basis: domain.CostCalculated, Currency: t.currency,
		Amount:           amount,
		TTLAssumedTokens: assumed,
		PricingSource:    t.source, PricingVersion: t.version, EffectiveDate: t.effectiveDate,
	}
}

// cacheCreationCost prices the cache-creation half of a vector by lifetime,
// returning the amount and how many tokens it could not price.
//
// THE SPLIT IS AUTHORITATIVE AND THE TOTAL IS NEVER ADDED TO IT. A caller that
// populates domain.UsageTokenTotals.CacheCreation is priced from that; a caller
// that reports only CacheWriteTokens -- every caller written before cache
// lifetimes existed in this codebase, and every read that goes through
// model_usage_events, which has no column for the split -- has its figure
// priced at the SHORT rate and the quantity returned as `assumed`.
//
// That second path is a compromise and it is worth saying why, because the
// alternative was considered and rejected for a reason that is not laziness.
// Refusing to price a lifetime-unknown write would be the purer rule, and it
// would also blank every cost figure in the product the day it shipped: the
// per-event store has no TTL column, so every ledger read would become
// "unknown" while AO holds the fact in the transcript it already parsed. So the
// assumption is kept, its exact size travels on the cost as TTLAssumedTokens,
// and a caller that must not accept it checks that field and refuses for
// itself. When the per-event columns exist, `assumed` goes to zero on its own
// and this paragraph can be deleted.
//
// Returns: the amount, tokens priced on an assumed lifetime, and tokens that
// could not be priced at all.
func cacheCreationCost(rate ModelRate, tokens domain.UsageTokenTotals) (amount float64, assumed, unpriceable int64) {
	const perMillion = 1_000_000.0
	split := tokens.CacheCreation
	if split.Total() == 0 {
		if tokens.CacheWriteTokens == 0 {
			return 0, 0, 0
		}
		split = domain.CacheCreationSplit{UnknownTTLTokens: tokens.CacheWriteTokens}
	}
	amount = float64(split.Ephemeral5mTokens) * rate.CacheWritePerMTok / perMillion
	if split.Ephemeral1hTokens > 0 {
		if rate.CacheWrite1hPerMTok <= 0 {
			// The rate card knows this model but not what a long-lived cache
			// entry costs on it. Unpriceable, and named as such.
			return 0, 0, split.Ephemeral1hTokens
		}
		amount += float64(split.Ephemeral1hTokens) * rate.CacheWrite1hPerMTok / perMillion
	}
	if split.UnknownTTLTokens > 0 {
		amount += float64(split.UnknownTTLTokens) * rate.CacheWritePerMTok / perMillion
		// Only an ASSUMPTION when the two lifetimes actually differ in price.
		// Where they do not, the missing fact cannot change the answer and
		// there is nothing to disclose.
		if rate.CacheWrite1hPerMTok != rate.CacheWritePerMTok {
			assumed = split.UnknownTTLTokens
		}
	}
	return amount, assumed, 0
}

// normalize lowercases and trims a model id. It deliberately does NOT strip a
// date suffix or any other component: a longer id simply falls through to the
// prefix rule in Rate, where a human-written Match decides what counts as the
// same model.
func normalize(id string) string { return strings.ToLower(strings.TrimSpace(id)) }
