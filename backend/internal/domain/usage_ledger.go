package domain

import "time"

// usage_ledger.go — P3-E's vocabulary for "what did this cost, and how well do
// we actually know that".
//
// The whole point of these types is that a number never travels without the
// claim it is entitled to make. AO can measure three quite different things and
// they must never be added together or presented as one:
//
//   - what a provider's own transcript reported it spent (TokenSourceProvider);
//   - what AO itself assembled and handed over, sized by a byte-derived
//     heuristic (TokenSourceEstimated);
//   - what AO cannot see at all (TokenSourceUnknown).
//
// A reviewer pane is the third case today and that is a fact about AO, not a
// zero. Anything that renders one of these must say which it is.

// TokenMeasurementSource records how a token figure came to be known.
type TokenMeasurementSource string

// TokenMeasurementSource values.
const (
	// TokenSourceProvider is a count parsed out of the provider's own
	// transcript — the only source entitled to be shown without a "~".
	TokenSourceProvider TokenMeasurementSource = "provider_reported"
	// TokenSourceAOCounted is a figure AO counted exactly itself (bytes it
	// assembled, items it packed). Exact, but not a token count.
	TokenSourceAOCounted TokenMeasurementSource = "ao_counted"
	// TokenSourceEstimated is derived from a byte count by AO's own
	// bytes-per-token heuristic. Never a provider's tokenizer.
	TokenSourceEstimated TokenMeasurementSource = "estimated"
	// TokenSourceUnknown is "AO cannot observe this". Distinct from zero.
	TokenSourceUnknown TokenMeasurementSource = "unknown"
	// TokenSourceMixed labels an aggregate whose parts do not share one
	// source, so a reader knows the total is not uniformly reported.
	TokenSourceMixed TokenMeasurementSource = "mixed"
)

// CostBasis records how a money figure came to be known. AO has no provider
// that reports cost in a transcript today, so CostProviderReported exists as
// the shape a future signal writes into and is never produced by inference.
type CostBasis string

// CostBasis values.
const (
	CostProviderReported CostBasis = "provider_reported"
	CostCalculated       CostBasis = "calculated"
	CostUnknown          CostBasis = "unknown"
)

// AttributionBasis records how confidently a token was assigned to a role.
// "exact" means the event carried its own observed time and fell inside a
// dispatch window; "approximate" means it did not, and was folded into the
// session's earliest window instead.
type AttributionBasis string

// AttributionBasis values.
const (
	AttributionExact       AttributionBasis = "exact"
	AttributionApproximate AttributionBasis = "approximate"
)

// UsageAttributionWindow is one role's claim on one session's timeline: the
// durable record that says "from this instant, tokens spent in this session
// belong to this role of this step of this run".
type UsageAttributionWindow struct {
	ID        int64
	DedupeKey string
	// SubjectKind and SessionID together are the window's SUBJECT: SessionID
	// holds the subject id whatever the kind is (it keeps its name because
	// renaming a column would rewrite a table for a word). They must match the
	// subject the surface binds usage under, or its events resolve to no window.
	SubjectKind         UsageSubjectKind
	SessionID           string
	ProjectID           string
	WorkflowRunID       string
	ParentWorkflowRunID string
	TaskID              string
	WorkflowStepID      string
	AttemptID           string
	AttemptOrdinal      int64
	Cycle               int64
	Role                WorkflowRole
	Harness             string
	Provider            string
	Model               string
	OpenedAt            time.Time
	CreatedAt           time.Time
	// HasUsageBinding reports whether this window's SUBJECT has any usage
	// binding at all.
	//
	// Since P3-E's completion pass every provider-backed role can carry one, so
	// false now means what it says: this surface has reported no provider
	// conversation yet. It is still not zero — a role that spent tokens AO has
	// not yet bound is unknown, not free — but it is no longer a permanent
	// property of being a reviewer or a resolver.
	HasUsageBinding bool
}

// Subject is the window's subject, assembled from the two stored columns. A
// window written before subjects existed reports the session subject it was.
func (w UsageAttributionWindow) Subject() UsageSubject {
	kind := w.SubjectKind
	if kind == "" {
		kind = UsageSubjectSession
	}
	return UsageSubject{Kind: kind, ID: w.SessionID}
}

// UsageTokenTotals is a summed token vector. Every field is a plain int64
// because these are sums over rows that all reported the dimension; a
// dimension a provider never reports is carried by ReasoningKnown-style flags
// rather than by a zero.
type UsageTokenTotals struct {
	InputTokens         int64
	UncachedInputTokens int64
	CacheReadTokens     int64
	CacheWriteTokens    int64
	OutputTokens        int64
	ReasoningTokens     int64
	// ReasoningKnown is false when no event in this aggregate carried a
	// reasoning figure, so ReasoningTokens must not be rendered as 0.
	ReasoningKnown bool
	EventCount     int64
}

// Total is input plus output. Cache reads/writes are already inside
// InputTokens (the V1 parser folds them there), so they are not added again.
func (t UsageTokenTotals) Total() int64 { return t.InputTokens + t.OutputTokens }

// Add folds another vector in.
func (t UsageTokenTotals) Add(o UsageTokenTotals) UsageTokenTotals {
	t.InputTokens += o.InputTokens
	t.UncachedInputTokens += o.UncachedInputTokens
	t.CacheReadTokens += o.CacheReadTokens
	t.CacheWriteTokens += o.CacheWriteTokens
	t.OutputTokens += o.OutputTokens
	t.ReasoningTokens += o.ReasoningTokens
	t.ReasoningKnown = t.ReasoningKnown || o.ReasoningKnown
	t.EventCount += o.EventCount
	return t
}

// UsageCost is a money figure and the provenance that entitles it to exist.
// Known=false is the only honest answer when no pricing covers the model, and
// callers must render it as "unknown" rather than as $0.00.
type UsageCost struct {
	Known    bool
	Basis    CostBasis
	Currency string
	Amount   float64
	// PricingSource, PricingVersion and EffectiveDate are the provenance of a
	// calculated cost: which published rate card, which revision of it, and
	// from when. Empty for CostUnknown.
	PricingSource  string
	PricingVersion string
	EffectiveDate  string
	// UnpricedModels lists models in this aggregate that no rate covered, so
	// a partial cost is visibly partial rather than quietly low.
	UnpricedModels []string
}

// Add folds another cost in. An unknown part makes the whole partial: the
// amount still sums, but the unpriced models travel with it.
func (c UsageCost) Add(o UsageCost) UsageCost {
	if !o.Known && len(o.UnpricedModels) == 0 {
		return c
	}
	if o.Known {
		c.Amount += o.Amount
		c.Known = true
		if c.Basis == "" || c.Basis == CostUnknown {
			c.Basis = o.Basis
		}
		if c.Currency == "" {
			c.Currency = o.Currency
		}
		if c.PricingSource == "" {
			c.PricingSource, c.PricingVersion, c.EffectiveDate = o.PricingSource, o.PricingVersion, o.EffectiveDate
		}
	}
	c.UnpricedModels = appendUnique(c.UnpricedModels, o.UnpricedModels...)
	return c
}

func appendUnique(dst []string, values ...string) []string {
	for _, v := range values {
		if v == "" {
			continue
		}
		found := false
		for _, existing := range dst {
			if existing == v {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, v)
		}
	}
	return dst
}

// ModelUsageLine is one (provider, model) aggregate: the finest grain any
// cost can honestly be computed at, since a rate is a property of a model.
type ModelUsageLine struct {
	Provider string
	Harness  string
	ModelID  string
	Tokens   UsageTokenTotals
	Cost     UsageCost
	Source   TokenMeasurementSource
	// ApproximateEvents counts events in this line whose role attribution
	// fell back to the session's earliest window.
	ApproximateEvents int64
}

// RoleUsageLine is one role's spend across every model it used.
type RoleUsageLine struct {
	Role WorkflowRole
	// Cycle is 0 for base execution and 1..n for repair cycles, so
	// "base 40k, repair +18k" is a fold over these rather than a second
	// ledger.
	Cycle  int64
	TaskID string
	// AttemptID / AttemptOrdinal keep a failed provider attempt's tokens
	// distinct from its successor's. A failover that burned tokens before
	// failing still shows them.
	AttemptID      string
	AttemptOrdinal int64
	Tokens         UsageTokenTotals
	Cost           UsageCost
	Source         TokenMeasurementSource
	Models         []ModelUsageLine
	// Observable is false when this role's surface has not reported a provider
	// conversation, so AO holds no measurement for it. Tokens is then
	// meaningless and must not be rendered — as UNKNOWN, never as zero.
	//
	// Since P3-E's completion pass this is a transient state, not a property of
	// a role: every provider-backed role (worker, repair, reviewer, decision
	// resolver, planner) can bind usage. A role that stays unobservable is a
	// surface whose hook never reported, which the read model says out loud
	// rather than quietly folding into a total.
	Observable bool
	// UnobservableReason names why, for the tooltip. Empty when observable.
	UnobservableReason string
	Provider           string
	Harness            string
	Model              string
	OpenedAt           *time.Time
}

// WorkflowUsageLedger is the canonical, backend-owned answer to "what has this
// run spent". The frontend renders it; it never re-adds attempts to reach a
// total of its own.
type WorkflowUsageLedger struct {
	WorkflowRunID string
	ProjectID     string
	Totals        UsageTokenTotals
	Cost          UsageCost
	Source        TokenMeasurementSource
	// BaseTokens and RepairTokens split the same totals by cycle, so a run
	// can say how much of its spend was re-work.
	BaseTokens   UsageTokenTotals
	RepairTokens UsageTokenTotals
	BaseCost     UsageCost
	RepairCost   UsageCost

	Roles     []RoleUsageLine
	Models    []ModelUsageLine
	Providers []ProviderUsageLine

	// Children is the per-child breakdown of an autonomous parent, and
	// FamilyTotals the parent's own spend plus every child's. Both are empty
	// for a run with no children.
	Children     []RunUsageLine
	FamilyTotals UsageTokenTotals
	FamilyCost   UsageCost

	// Unobservable lists the roles this run dispatched whose spend AO cannot
	// see, so the UI shows them as "not observable" rather than omitting them
	// and implying the total is complete.
	Unobservable []RoleUsageLine

	// ApproximateEvents and TotalEvents together say how much of the role
	// breakdown rests on a fallback rather than on observed event times.
	ApproximateEvents int64
	TotalEvents       int64

	// Budget is the run's frozen token/cost budget and where it now stands.
	Budget UsageBudgetStatus

	// Recorded is false when the run has no usage rows at all — a legacy run,
	// or one whose provider AO cannot meter. The UI must then say "no usage
	// data recorded", never "0 tokens".
	Recorded bool

	// Complete reports that every provider-backed role this run dispatched has
	// reported its spend, so Totals is the whole bill rather than a floor.
	//
	// It is the answer to the question the previous pass had to leave open. A
	// run is complete when nothing it dispatched is still unmeasured; it is
	// incomplete when some surface has not reported, and IncompleteReason says
	// which roles. A role that consumes no provider at all must never make a run
	// incomplete — "we cannot see it" and "there is nothing to see" are
	// different findings, and only the first is a gap.
	Complete bool
	// IncompleteReason names the roles still unmeasured. Empty when Complete.
	IncompleteReason string
}

// ProviderUsageLine is one vendor's slice of a total.
type ProviderUsageLine struct {
	Provider string
	Tokens   UsageTokenTotals
	Cost     UsageCost
	Source   TokenMeasurementSource
}

// RunUsageLine is one run's contribution inside a family or project rollup.
type RunUsageLine struct {
	WorkflowRunID string
	Tokens        UsageTokenTotals
	Cost          UsageCost
	Source        TokenMeasurementSource
}

// CompactRunUsage is the Board's per-card figure: a total, a cost when one is
// knowable, and the source that says how to render them. Deliberately nothing
// else — a card is not a financial dashboard.
type CompactRunUsage struct {
	WorkflowRunID string
	TotalTokens   int64
	Cost          UsageCost
	Source        TokenMeasurementSource
	Recorded      bool
}

// UsagePeriod names a project rollup's window.
type UsagePeriod string

// UsagePeriod values.
const (
	UsagePeriodToday   UsagePeriod = "today"
	UsagePeriodWeek    UsagePeriod = "7d"
	UsagePeriodMonth   UsagePeriod = "30d"
	UsagePeriodAllTime UsagePeriod = "all"
)

// Valid reports whether p is a period the read model can serve.
func (p UsagePeriod) Valid() bool {
	switch p {
	case UsagePeriodToday, UsagePeriodWeek, UsagePeriodMonth, UsagePeriodAllTime:
		return true
	default:
		return false
	}
}

// ProjectUsageSummary is a project's spend over one period.
type ProjectUsageSummary struct {
	ProjectID string
	Period    UsagePeriod
	From      time.Time
	To        time.Time
	Totals    UsageTokenTotals
	Cost      UsageCost
	Source    TokenMeasurementSource
	Workflows int64
	// AverageTokensPerWorkflow is nil when no workflow fell in the period —
	// a division by zero is not zero.
	AverageTokensPerWorkflow *int64
	Roles                    []RoleUsageLine
	Providers                []ProviderUsageLine
	Models                   []ModelUsageLine
	Runs                     []RunUsageLine
	Budget                   UsageBudgetStatus
	Recorded                 bool
}

// --- budget ---------------------------------------------------------------

// UsageBudgetPolicy is the frozen token/cost ceiling a run executes under. It
// lives inside WorkflowPolicy for the same reason every other policy does: one
// snapshot per run holds the whole decision-making configuration, so a later
// Settings edit cannot widen what an in-flight run may spend, and a restart
// cannot change the answer.
//
// Zero means "no limit". That is deliberate: an unset budget must not become a
// budget of zero the first time a run is created by an older binary.
type UsageBudgetPolicy struct {
	Version string `json:"version,omitempty"`
	// WorkflowTokenBudget caps total tokens for the run AND, for an
	// autonomous parent, for its whole family — see ParentScope.
	WorkflowTokenBudget int64 `json:"workflowTokenBudget,omitempty"`
	// WorkflowCostBudgetUSD caps calculated cost. Only enforceable while
	// pricing covers the models in play; an unpriced model can exhaust a
	// token budget but never a cost one.
	WorkflowCostBudgetUSD float64 `json:"workflowCostBudgetUsd,omitempty"`
	// ProjectDailyTokenBudget / ProjectDailyCostBudgetUSD are the project's
	// per-day ceilings as they stood when the run was created.
	ProjectDailyTokenBudget   int64   `json:"projectDailyTokenBudget,omitempty"`
	ProjectDailyCostBudgetUSD float64 `json:"projectDailyCostBudgetUsd,omitempty"`
	// WarnPercent is the soft threshold, in percent of the hard limit, at
	// which advice starts saying so. 0 falls back to DefaultUsageWarnPercent.
	WarnPercent int `json:"warnPercent,omitempty"`
	// ParentScope, when true, makes an autonomous parent's budget cover every
	// child it launches rather than each child holding the full budget again.
	// Default true via EffectiveUsageBudgetPolicy: ten children each entitled
	// to the parent's whole budget is the exact failure P3-E §16 names.
	ParentScope *bool `json:"parentScope,omitempty"`
}

// DefaultUsageWarnPercent is the soft threshold when a policy names none.
const DefaultUsageWarnPercent = 80

// ParentScoped reports whether children share the parent's budget.
func (p UsageBudgetPolicy) ParentScoped() bool {
	return p.ParentScope == nil || *p.ParentScope
}

// EffectiveWarnPercent is WarnPercent, or the default when unset/nonsensical.
func (p UsageBudgetPolicy) EffectiveWarnPercent() int {
	if p.WarnPercent <= 0 || p.WarnPercent >= 100 {
		return DefaultUsageWarnPercent
	}
	return p.WarnPercent
}

// Configured reports whether any ceiling is actually set.
func (p UsageBudgetPolicy) Configured() bool {
	return p.WorkflowTokenBudget > 0 || p.WorkflowCostBudgetUSD > 0 ||
		p.ProjectDailyTokenBudget > 0 || p.ProjectDailyCostBudgetUSD > 0
}

// UsageBudgetState is where a run stands against its ceiling.
type UsageBudgetState string

// UsageBudgetState values.
const (
	// BudgetUnset means no ceiling was configured — never rendered as "0% used".
	BudgetUnset UsageBudgetState = "unset"
	BudgetOK    UsageBudgetState = "ok"
	// BudgetWarning is past the soft threshold. Advisory only: it never
	// changes a run's state by itself (P3-E §30).
	BudgetWarning UsageBudgetState = "warning"
	// BudgetExhausted is at or past the hard limit. Acted on only at a safe
	// boundary — before a NEW dispatch, never mid-response.
	BudgetExhausted UsageBudgetState = "exhausted"
)

// UsageBudgetStatus is the budget read model: the ceiling, the spend, and what
// that combination means.
type UsageBudgetStatus struct {
	Policy UsageBudgetPolicy
	State  UsageBudgetState
	// TokenPercent / CostPercent are nil when the corresponding ceiling is
	// unset, so a UI cannot render an unset budget as an empty meter.
	TokenPercent *float64
	CostPercent  *float64
	TokensUsed   int64
	// CostUsed carries its own Known flag; an unpriced model leaves a cost
	// budget unenforceable, which Enforceable reports.
	CostUsed UsageCost
	// Scope names what the numbers cover: "run" or "family" (an autonomous
	// parent plus its children).
	Scope string
	// Reason is a short machine-readable code for why the state is what it
	// is, e.g. "workflow_token_budget_exhausted".
	Reason string
}

// Blocking reports whether this status must stop a NEW dispatch.
func (s UsageBudgetStatus) Blocking() bool { return s.State == BudgetExhausted }
