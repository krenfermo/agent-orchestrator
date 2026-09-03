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

	InputPerMTok      float64 `json:"inputPerMTok"`
	OutputPerMTok     float64 `json:"outputPerMTok"`
	CacheReadPerMTok  float64 `json:"cacheReadPerMTok"`
	CacheWritePerMTok float64 `json:"cacheWritePerMTok"`
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
// prices. Cache rates are derived from them by Anthropic's published
// 5-minute-TTL cache multipliers — 0.1x input for a cache read, 1.25x input
// for a cache write — which is why Source names both halves: a reader must be
// able to tell a quoted rate from a derived one.
//
// There is deliberately NO entry for any OpenAI/Codex model. AO meters Codex
// tokens exactly as it meters Claude's, but this binary has no rate it can
// vouch for, so those tokens report cost=unknown until an operator supplies a
// rate card. That is the honest outcome, not a gap to paper over with a guess.
var anthropicListPrices = Catalog{
	Source:        "anthropic-list-price + published 5m cache multipliers (0.1x read, 1.25x write)",
	Version:       "2026-06-24",
	EffectiveDate: "2026-06-24",
	Currency:      "USD",
	Models: []ModelRate{
		anthropicRate("claude-fable-5-1", 10.00, 50.00, 0.25, 12.50),
		anthropicRate("claude-mythos-5-1", 10.00, 50.00, 0.25, 12.50),
		anthropicRate("claude-fable-5", 10.00, 50.00, 1.00, 12.50),
		anthropicRate("claude-opus-5", 5.00, 25.00, 0.50, 6.25),
		anthropicRate("claude-opus-4-8", 5.00, 25.00, 0.50, 6.25),
		anthropicRate("claude-opus-4-7", 5.00, 25.00, 0.50, 6.25),
		anthropicRate("claude-opus-4-6", 5.00, 25.00, 0.50, 6.25),
		anthropicRate("claude-sonnet-5", 2.00, 10.00, 0.20, 2.50),
		anthropicRate("claude-sonnet-4-6", 3.00, 15.00, 0.30, 3.75),
		anthropicRate("claude-haiku-4-5", 1.00, 5.00, 0.10, 1.25),
	},
}

// anthropicRate builds one embedded catalog row. The vendor is hard-coded
// because the embedded catalog holds Anthropic rates only: AO meters Codex
// tokens exactly the same way, but this binary has no OpenAI rate it can vouch
// for, so those models report cost=unknown until an operator supplies a rate
// card. An operator's card names its own provider per row.
func anthropicRate(match string, in, out, cacheRead, cacheWrite float64) ModelRate {
	return ModelRate{
		Match: match, Provider: "anthropic",
		InputPerMTok: in, OutputPerMTok: out,
		CacheReadPerMTok: cacheRead, CacheWritePerMTok: cacheWrite,
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
	amount := float64(tokens.UncachedInputTokens)*rate.InputPerMTok/perMillion +
		float64(tokens.CacheReadTokens)*rate.CacheReadPerMTok/perMillion +
		float64(tokens.CacheWriteTokens)*rate.CacheWritePerMTok/perMillion +
		float64(tokens.OutputTokens)*rate.OutputPerMTok/perMillion
	return domain.UsageCost{
		Known: true, Basis: domain.CostCalculated, Currency: t.currency,
		Amount:        amount,
		PricingSource: t.source, PricingVersion: t.version, EffectiveDate: t.effectiveDate,
	}
}

// normalize lowercases and trims a model id. It deliberately does NOT strip a
// date suffix or any other component: a longer id simply falls through to the
// prefix rule in Rate, where a human-written Match decides what counts as the
// same model.
func normalize(id string) string { return strings.ToLower(strings.TrimSpace(id)) }
