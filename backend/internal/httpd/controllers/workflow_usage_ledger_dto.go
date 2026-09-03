package controllers

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// workflow_usage_ledger_dto.go — P3-E's usage ledger on the wire.
//
// EVERY NUMBER CARRIES ITS CLAIM. `source` says whether a token figure was
// reported by a provider, estimated by AO, or is unknown; `cost.basis` says
// whether money was calculated, reported, or unknown, and a calculated cost
// carries the rate card that produced it. A client that renders `total` without
// reading `source` is rendering an estimate as a measurement, which is the one
// thing P3-E forbids — so `source` is required on every block, never omitempty.
//
// AND NOTHING IS A ZERO STANDING IN FOR A GAP. `recorded: false` means the run
// has no usage rows at all (a legacy run, an unmeterable provider): render "no
// usage data recorded", not "0 tokens". A role in `unobservable` ran and spent
// something AO cannot see; it is listed precisely so the total is not mistaken
// for the whole bill.

// UsageTokenTotalsResponse is a summed token vector.
type UsageTokenTotalsResponse struct {
	Input         int64 `json:"input"`
	UncachedInput int64 `json:"uncachedInput"`
	CacheRead     int64 `json:"cacheRead"`
	CacheWrite    int64 `json:"cacheWrite"`
	Output        int64 `json:"output"`
	// Reasoning is null, never 0, when no event in this aggregate reported a
	// reasoning figure — most providers never do.
	Reasoning *int64 `json:"reasoning"`
	Total     int64  `json:"total"`
	// Events is how many ledger rows back these totals.
	Events int64 `json:"events"`
}

// UsageCostResponse is a money figure and the provenance entitling it to exist.
type UsageCostResponse struct {
	// Known is false when no rate covered the models in play. Amount is then
	// meaningless and a client must render "unknown", never "$0.00".
	Known    bool    `json:"known"`
	Basis    string  `json:"basis"`
	Currency string  `json:"currency,omitempty"`
	Amount   float64 `json:"amount"`
	// PricingSource / PricingVersion / EffectiveDate name the rate card. A
	// calculated cost is a LIST-PRICE EQUIVALENT of the tokens spent, not a
	// bill: AO cannot see how a provider actually charges the account behind a
	// harness (a subscription may make the marginal cost nothing at all).
	PricingSource  string `json:"pricingSource,omitempty"`
	PricingVersion string `json:"pricingVersion,omitempty"`
	EffectiveDate  string `json:"effectiveDate,omitempty"`
	// UnpricedModels names the models no rate covered, so a partial cost is
	// visibly partial rather than quietly low.
	UnpricedModels []string `json:"unpricedModels,omitempty"`
}

// ModelUsageLineResponse is one (provider, model) aggregate.
type ModelUsageLineResponse struct {
	Provider string                   `json:"provider,omitempty"`
	Harness  string                   `json:"harness,omitempty"`
	Model    string                   `json:"model"`
	Tokens   UsageTokenTotalsResponse `json:"tokens"`
	Cost     UsageCostResponse        `json:"cost"`
	Source   string                   `json:"source"`
	// ApproximateEvents counts rows whose role attribution fell back to the
	// session's first window because the artifact carried no timestamp.
	ApproximateEvents int64 `json:"approximateEvents"`
}

// RoleUsageLineResponse is one role's spend.
type RoleUsageLineResponse struct {
	Role  string `json:"role"`
	Cycle int64  `json:"cycle"`
	// Repair is true for anything past base execution, so a client does not
	// re-derive that rule from Role and Cycle itself.
	Repair         bool                     `json:"repair"`
	TaskID         string                   `json:"taskId,omitempty"`
	AttemptID      string                   `json:"attemptId,omitempty"`
	AttemptOrdinal int64                    `json:"attemptOrdinal"`
	Provider       string                   `json:"provider,omitempty"`
	Harness        string                   `json:"harness,omitempty"`
	Model          string                   `json:"model,omitempty"`
	Tokens         UsageTokenTotalsResponse `json:"tokens"`
	Cost           UsageCostResponse        `json:"cost"`
	Source         string                   `json:"source"`
	// Observable is false when this role ran on a surface AO cannot meter — a
	// reviewer or decision-resolver pane is a runtime handle, not a session, so
	// no usage binding can exist for it. Tokens is then meaningless.
	Observable         bool   `json:"observable"`
	UnobservableReason string `json:"unobservableReason,omitempty"`
	OpenedAt           string `json:"openedAt,omitempty"`
}

// ProviderUsageLineResponse is one vendor's slice.
type ProviderUsageLineResponse struct {
	Provider string                   `json:"provider"`
	Tokens   UsageTokenTotalsResponse `json:"tokens"`
	Cost     UsageCostResponse        `json:"cost"`
	Source   string                   `json:"source"`
}

// RunUsageLineResponse is one run's contribution to a family or project.
type RunUsageLineResponse struct {
	WorkflowRunID string                   `json:"workflowRunId"`
	Tokens        UsageTokenTotalsResponse `json:"tokens"`
	Cost          UsageCostResponse        `json:"cost"`
	Source        string                   `json:"source"`
}

// UsageBudgetResponse is a ceiling and where the run stands against it.
type UsageBudgetResponse struct {
	// State is "unset" | "ok" | "warning" | "exhausted". "unset" is NOT "0%
	// used": nobody configured a ceiling, and a client must render no meter.
	State string `json:"state"`
	// Scope is "run", "family" (an autonomous parent plus its children) or
	// "project_day".
	Scope             string   `json:"scope"`
	Reason            string   `json:"reason,omitempty"`
	TokenBudget       int64    `json:"tokenBudget,omitempty"`
	CostBudget        float64  `json:"costBudget,omitempty"`
	WarnPercent       int      `json:"warnPercent"`
	TokensUsed        int64    `json:"tokensUsed"`
	TokenPercent      *float64 `json:"tokenPercent"`
	CostPercent       *float64 `json:"costPercent"`
	ParentScoped      bool     `json:"parentScoped"`
	ProjectDailyToken int64    `json:"projectDailyTokenBudget,omitempty"`
	ProjectDailyCost  float64  `json:"projectDailyCostBudget,omitempty"`
}

// WorkflowUsageLedgerResponse is the canonical per-run answer.
type WorkflowUsageLedgerResponse struct {
	WorkflowRunID string                   `json:"workflowRunId"`
	Recorded      bool                     `json:"recorded"`
	Source        string                   `json:"source"`
	Totals        UsageTokenTotalsResponse `json:"totals"`
	Cost          UsageCostResponse        `json:"cost"`

	// BaseTokens / RepairTokens split the same total by cycle. Repair is
	// broken out because a review->fix loop is where a run's cost runs away,
	// and folding it invisibly into the worker's number hides exactly that.
	BaseTokens   UsageTokenTotalsResponse `json:"baseTokens"`
	RepairTokens UsageTokenTotalsResponse `json:"repairTokens"`
	BaseCost     UsageCostResponse        `json:"baseCost"`
	RepairCost   UsageCostResponse        `json:"repairCost"`

	Roles     []RoleUsageLineResponse     `json:"roles"`
	Models    []ModelUsageLineResponse    `json:"models"`
	Providers []ProviderUsageLineResponse `json:"providers"`

	// Complete reports that every provider-backed role this run dispatched has
	// reported its spend, so `totals` is the whole bill rather than a floor.
	// False means some surface has not reported yet; `incompleteReason` names
	// which roles. A role that consumes no provider at all never makes a run
	// incomplete.
	Complete bool `json:"complete"`
	// IncompleteReason names the roles still unmeasured. Empty when complete.
	IncompleteReason string `json:"incompleteReason,omitempty"`

	// Unobservable lists roles whose surface has not reported its spend yet.
	// Their tokens are UNKNOWN, never zero.
	Unobservable []RoleUsageLineResponse `json:"unobservable,omitempty"`

	// Children and familyTotals are populated for an autonomous parent.
	Children     []RunUsageLineResponse   `json:"children,omitempty"`
	FamilyTotals UsageTokenTotalsResponse `json:"familyTotals"`
	FamilyCost   UsageCostResponse        `json:"familyCost"`

	// ApproximateEvents / totalEvents say how much of the ROLE breakdown rests
	// on a fallback attribution rather than on observed event times. The run
	// TOTAL is unaffected by this — only the split between roles is.
	ApproximateEvents int64 `json:"approximateEvents"`
	TotalEvents       int64 `json:"totalEvents"`

	Budget UsageBudgetResponse `json:"budget"`

	// Context is what AO ASSEMBLED for this run. It is a SEPARATE block from
	// the token figures above and must never be added to them: those are what a
	// provider reported it received, this is what AO built and sent, measured
	// in bytes and converted to tokens by AO's own heuristic. Absent when no
	// evidence was recorded.
	Context *WorkflowContextResponse `json:"context,omitempty"`
}

// ProjectUsageResponse is a project's rollup for one period.
type ProjectUsageResponse struct {
	ProjectID string `json:"projectId"`
	Period    string `json:"period"`
	From      string `json:"from,omitempty"`
	To        string `json:"to"`
	Recorded  bool   `json:"recorded"`
	Source    string `json:"source"`
	// PeriodBasis states what the period actually filtered on, so nobody reads
	// these buckets as provider billing periods: AO buckets by the instant it
	// DISPATCHED the work, which is a fact it recorded itself.
	PeriodBasis string `json:"periodBasis"`

	Totals    UsageTokenTotalsResponse    `json:"totals"`
	Cost      UsageCostResponse           `json:"cost"`
	Workflows int64                       `json:"workflows"`
	Roles     []RoleUsageLineResponse     `json:"roles"`
	Providers []ProviderUsageLineResponse `json:"providers"`
	Models    []ModelUsageLineResponse    `json:"models"`
	Runs      []RunUsageLineResponse      `json:"runs,omitempty"`
	// AverageTokensPerWorkflow is null, not 0, when no workflow spent anything
	// in the period: a division with no denominator has no answer.
	AverageTokensPerWorkflow *int64              `json:"averageTokensPerWorkflow"`
	Budget                   UsageBudgetResponse `json:"budget"`
}

// CompactRunUsageResponse is the Board card's figure.
type CompactRunUsageResponse struct {
	TotalTokens int64             `json:"totalTokens"`
	Cost        UsageCostResponse `json:"cost"`
	Source      string            `json:"source"`
	Recorded    bool              `json:"recorded"`
}

func usageTokenTotalsResponse(t domain.UsageTokenTotals) UsageTokenTotalsResponse {
	out := UsageTokenTotalsResponse{
		Input: t.InputTokens, UncachedInput: t.UncachedInputTokens,
		CacheRead: t.CacheReadTokens, CacheWrite: t.CacheWriteTokens,
		Output: t.OutputTokens, Total: t.Total(), Events: t.EventCount,
	}
	if t.ReasoningKnown {
		reasoning := t.ReasoningTokens
		out.Reasoning = &reasoning
	}
	return out
}

func usageCostResponse(c domain.UsageCost) UsageCostResponse {
	basis := string(c.Basis)
	if basis == "" {
		basis = string(domain.CostUnknown)
	}
	return UsageCostResponse{
		Known: c.Known, Basis: basis, Currency: c.Currency, Amount: c.Amount,
		PricingSource: c.PricingSource, PricingVersion: c.PricingVersion,
		EffectiveDate: c.EffectiveDate, UnpricedModels: c.UnpricedModels,
	}
}

func modelUsageLineResponses(models []domain.ModelUsageLine) []ModelUsageLineResponse {
	out := make([]ModelUsageLineResponse, 0, len(models))
	for _, m := range models {
		out = append(out, ModelUsageLineResponse{
			Provider: m.Provider, Harness: m.Harness, Model: m.ModelID,
			Tokens: usageTokenTotalsResponse(m.Tokens), Cost: usageCostResponse(m.Cost),
			Source: string(m.Source), ApproximateEvents: m.ApproximateEvents,
		})
	}
	return out
}

func roleUsageLineResponses(roles []domain.RoleUsageLine) []RoleUsageLineResponse {
	out := make([]RoleUsageLineResponse, 0, len(roles))
	for _, r := range roles {
		entry := RoleUsageLineResponse{
			Role: string(r.Role), Cycle: r.Cycle,
			Repair: r.Role == domain.WorkflowRoleFixWorker || r.Cycle > 0,
			TaskID: r.TaskID, AttemptID: r.AttemptID, AttemptOrdinal: r.AttemptOrdinal,
			Provider: r.Provider, Harness: r.Harness, Model: r.Model,
			Tokens: usageTokenTotalsResponse(r.Tokens), Cost: usageCostResponse(r.Cost),
			Source: string(r.Source), Observable: r.Observable,
			UnobservableReason: r.UnobservableReason,
		}
		if r.OpenedAt != nil {
			entry.OpenedAt = r.OpenedAt.Format(rfc3339Milli)
		}
		out = append(out, entry)
	}
	return out
}

func providerUsageLineResponses(providers []domain.ProviderUsageLine) []ProviderUsageLineResponse {
	out := make([]ProviderUsageLineResponse, 0, len(providers))
	for _, p := range providers {
		out = append(out, ProviderUsageLineResponse{
			Provider: p.Provider, Tokens: usageTokenTotalsResponse(p.Tokens),
			Cost: usageCostResponse(p.Cost), Source: string(p.Source),
		})
	}
	return out
}

func runUsageLineResponses(runs []domain.RunUsageLine) []RunUsageLineResponse {
	out := make([]RunUsageLineResponse, 0, len(runs))
	for _, r := range runs {
		out = append(out, RunUsageLineResponse{
			WorkflowRunID: r.WorkflowRunID, Tokens: usageTokenTotalsResponse(r.Tokens),
			Cost: usageCostResponse(r.Cost), Source: string(r.Source),
		})
	}
	return out
}

func usageBudgetResponse(b domain.UsageBudgetStatus) UsageBudgetResponse {
	state := string(b.State)
	if state == "" {
		state = string(domain.BudgetUnset)
	}
	return UsageBudgetResponse{
		State: state, Scope: b.Scope, Reason: b.Reason,
		TokenBudget: b.Policy.WorkflowTokenBudget, CostBudget: b.Policy.WorkflowCostBudgetUSD,
		WarnPercent: b.Policy.EffectiveWarnPercent(), TokensUsed: b.TokensUsed,
		TokenPercent: b.TokenPercent, CostPercent: b.CostPercent,
		ParentScoped:      b.Policy.ParentScoped(),
		ProjectDailyToken: b.Policy.ProjectDailyTokenBudget,
		ProjectDailyCost:  b.Policy.ProjectDailyCostBudgetUSD,
	}
}

func workflowUsageLedgerResponse(l domain.WorkflowUsageLedger) WorkflowUsageLedgerResponse {
	return WorkflowUsageLedgerResponse{
		WorkflowRunID: l.WorkflowRunID, Recorded: l.Recorded, Source: string(l.Source),
		Totals: usageTokenTotalsResponse(l.Totals), Cost: usageCostResponse(l.Cost),
		BaseTokens:   usageTokenTotalsResponse(l.BaseTokens),
		RepairTokens: usageTokenTotalsResponse(l.RepairTokens),
		BaseCost:     usageCostResponse(l.BaseCost), RepairCost: usageCostResponse(l.RepairCost),
		Roles: roleUsageLineResponses(l.Roles), Models: modelUsageLineResponses(l.Models),
		Providers:         providerUsageLineResponses(l.Providers),
		Complete:          l.Complete,
		IncompleteReason:  l.IncompleteReason,
		Unobservable:      roleUsageLineResponses(l.Unobservable),
		Children:          runUsageLineResponses(l.Children),
		FamilyTotals:      usageTokenTotalsResponse(l.FamilyTotals),
		FamilyCost:        usageCostResponse(l.FamilyCost),
		ApproximateEvents: l.ApproximateEvents, TotalEvents: l.TotalEvents,
		Budget: usageBudgetResponse(l.Budget),
	}
}

func projectUsageResponse(s domain.ProjectUsageSummary) ProjectUsageResponse {
	out := ProjectUsageResponse{
		ProjectID: s.ProjectID, Period: string(s.Period), Recorded: s.Recorded,
		Source: string(s.Source), PeriodBasis: "dispatch_time",
		Totals: usageTokenTotalsResponse(s.Totals), Cost: usageCostResponse(s.Cost),
		Workflows: s.Workflows, Roles: roleUsageLineResponses(s.Roles),
		Providers:                providerUsageLineResponses(s.Providers),
		Models:                   modelUsageLineResponses(s.Models),
		Runs:                     runUsageLineResponses(s.Runs),
		AverageTokensPerWorkflow: s.AverageTokensPerWorkflow,
		Budget:                   usageBudgetResponse(s.Budget),
	}
	if !s.From.IsZero() {
		out.From = s.From.Format(rfc3339Milli)
	}
	out.To = s.To.Format(rfc3339Milli)
	return out
}

func compactRunUsageResponse(u domain.CompactRunUsage) CompactRunUsageResponse {
	return CompactRunUsageResponse{
		TotalTokens: u.TotalTokens, Cost: usageCostResponse(u.Cost),
		Source: string(u.Source), Recorded: u.Recorded,
	}
}

// --- AO-assembled context (P3-E §7/§8/§9/§28) ------------------------------
//
// SEPARATE FROM `tokens` ON PURPOSE. Everything below sizes what AO ASSEMBLED
// and handed a provider. It is measured in bytes and converted to tokens by
// AO's own heuristic, never by a provider tokenizer, and AO cannot see what a
// coding harness reads inside the worktree afterwards. So these figures are not
// the provider's input tokens, are never added to them, and the two blocks are
// never merged into one "context" number.

// ContextSourceResponse is one source's contribution to assembled context.
type ContextSourceResponse struct {
	Source string `json:"source" enum:"task_spec,project_memory,shared_knowledge,repo_content,index_reuse,other"`
	// Bytes is measured; estimatedTokens is derived from it.
	Bytes           int64 `json:"bytes"`
	EstimatedTokens int64 `json:"estimatedTokens"`
}

// ContextRoleResponse is one role's share of the assembled context.
type ContextRoleResponse struct {
	Role                     string `json:"role"`
	Dispatches               int64  `json:"dispatches"`
	AssembledBytes           int64  `json:"assembledBytes"`
	EstimatedAssembledTokens int64  `json:"estimatedAssembledTokens"`
}

// ContextMemoryResponse is what project memory did for the run.
type ContextMemoryResponse struct {
	// Mode is the rollout stage in force. Empty means no dispatch recorded one.
	Mode string `json:"mode,omitempty" enum:"off,assisted,preferred"`
	// Generation and indexedCommit are the provenance of the memory served. An
	// empty commit means AO had memory it could not vouch for and did not use.
	Generation    int64  `json:"generation"`
	IndexedCommit string `json:"indexedCommit,omitempty"`
	// Provider names the graph backend actually in use. AO ships LocalGraph;
	// it is reported by its real name and never as "Graphify", which is a
	// separate external adapter that does not exist yet.
	Provider string `json:"provider"`

	PackItems           int64 `json:"packItems"`
	PackBytes           int64 `json:"packBytes"`
	EstimatedPackTokens int64 `json:"estimatedPackTokens"`
	Candidates          int64 `json:"candidates"`
	RejectedByBudget    int64 `json:"rejectedByBudget"`
	StaleExcluded       int64 `json:"staleExcluded"`

	CacheHits   int64 `json:"cacheHits"`
	CacheMisses int64 `json:"cacheMisses"`

	Syncs            int64 `json:"syncs"`
	FullSyncs        int64 `json:"fullSyncs"`
	IncrementalSyncs int64 `json:"incrementalSyncs"`
	NoOpSyncs        int64 `json:"noOpSyncs"`
	SyncFilesRead    int64 `json:"syncFilesRead"`

	// SharedCandidates against sharedSelected is the pair that carries the
	// argument: a task next to an earlier one should show candidates it took,
	// an unrelated task candidates it excluded.
	SharedCandidates   int64 `json:"sharedCandidates"`
	SharedSelected     int64 `json:"sharedSelected"`
	SharedExcluded     int64 `json:"sharedExcluded"`
	TaskLocalItems     int64 `json:"taskLocalItems"`
	WorkflowLocalItems int64 `json:"workflowLocalItems"`
	CanonicalItems     int64 `json:"canonicalItems"`

	FallbackReasons []string `json:"fallbackReasons,omitempty"`
}

// WorkflowContextResponse is the run's whole assembled-context story.
type WorkflowContextResponse struct {
	// Recorded is false when the run has no evidence at all. Render "no context
	// data recorded", never zeroes.
	Recorded   bool  `json:"recorded"`
	Dispatches int64 `json:"dispatches"`
	// Unmeasured counts dispatches whose payload size AO could not measure. A
	// nonzero value makes every total below a LOWER BOUND, which `complete`
	// states outright.
	Unmeasured     int64 `json:"unmeasured"`
	SkippedRecords int64 `json:"skippedRecords"`
	Complete       bool  `json:"complete"`

	AssembledBytes           int64 `json:"assembledBytes"`
	EstimatedAssembledTokens int64 `json:"estimatedAssembledTokens"`

	ByRole   []ContextRoleResponse   `json:"byRole,omitempty"`
	BySource []ContextSourceResponse `json:"bySource,omitempty"`

	// AvoidedAssembledBytes / estimatedAvoidedTokens are context AO
	// demonstrably did not assemble, measured against a real baseline: memory
	// that replaced an equivalent document, and router candidates assembled but
	// not sent.
	//
	// `avoidedComparable: false` means NO baseline supports a saving figure and
	// the client must show none — not zero. And even when true, this is an
	// AO-ASSEMBLED size: it is NOT a claim that the provider billed that many
	// fewer tokens. The label to use is "estimated AO context avoided".
	AvoidedAssembledBytes  int64 `json:"avoidedAssembledBytes"`
	EstimatedAvoidedTokens int64 `json:"estimatedAvoidedTokens"`
	AvoidedComparable      bool  `json:"avoidedComparable"`

	Memory ContextMemoryResponse `json:"memory"`

	// EstimateMethod names the bytes-per-token heuristic behind every token
	// figure here, so nobody mistakes it for a provider tokenizer.
	EstimateMethod string `json:"estimateMethod"`
	// Basis is always "ao_assembled": a constant on the wire, so a client
	// cannot render this block beside provider tokens without seeing that they
	// measure different things.
	Basis string `json:"basis" enum:"ao_assembled"`
}

// memoryGraphProvider names the graph backend AO actually ships. It is stated
// rather than branded: calling LocalGraph "Graphify" would claim an external
// adapter that does not exist (P3-E §29).
const memoryGraphProvider = "LocalGraph"

func workflowContextResponse(v domain.ContextCompositionView) WorkflowContextResponse {
	out := WorkflowContextResponse{
		Recorded: v.Recorded, Dispatches: v.Dispatches, Unmeasured: v.Unmeasured,
		SkippedRecords: v.SkippedRecords, Complete: v.Unmeasured == 0 && v.SkippedRecords == 0,
		AssembledBytes:           v.AssembledBytes,
		EstimatedAssembledTokens: v.EstimatedAssembledTokens,
		AvoidedAssembledBytes:    v.AvoidedAssembledBytes,
		EstimatedAvoidedTokens:   v.EstimatedAvoidedTokens,
		AvoidedComparable:        v.AvoidedComparable,
		EstimateMethod:           v.EstimateMethod,
		Basis:                    "ao_assembled",
		Memory: ContextMemoryResponse{
			Mode: v.Memory.Mode, Generation: v.Memory.Generation,
			IndexedCommit: v.Memory.IndexedCommit, Provider: memoryGraphProvider,
			PackItems: v.Memory.PackItems, PackBytes: v.Memory.PackBytes,
			EstimatedPackTokens: v.Memory.EstimatedPackTokens,
			Candidates:          v.Memory.Candidates, RejectedByBudget: v.Memory.RejectedByBudget,
			StaleExcluded: v.Memory.StaleExcluded,
			CacheHits:     v.Memory.CacheHits, CacheMisses: v.Memory.CacheMisses,
			Syncs: v.Memory.Syncs, FullSyncs: v.Memory.FullSyncs,
			IncrementalSyncs: v.Memory.IncrementalSyncs, NoOpSyncs: v.Memory.NoOpSyncs,
			SyncFilesRead:    v.Memory.SyncFilesRead,
			SharedCandidates: v.Memory.SharedCandidates, SharedSelected: v.Memory.SharedSelected,
			SharedExcluded: v.Memory.SharedExcluded, TaskLocalItems: v.Memory.TaskLocalItems,
			WorkflowLocalItems: v.Memory.WorkflowLocalItems, CanonicalItems: v.Memory.CanonicalItems,
			FallbackReasons: v.Memory.FallbackReasons,
		},
	}
	for _, r := range v.ByRole {
		out.ByRole = append(out.ByRole, ContextRoleResponse{
			Role: string(r.Role), Dispatches: r.Dispatches,
			AssembledBytes: r.AssembledBytes, EstimatedAssembledTokens: r.EstimatedAssembledTokens,
		})
	}
	for _, sline := range v.BySource {
		out.BySource = append(out.BySource, ContextSourceResponse{
			Source: string(sline.Source), Bytes: sline.Bytes, EstimatedTokens: sline.EstimatedTokens,
		})
	}
	return out
}
