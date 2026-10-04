package usage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage/pricing"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// ledger.go — the canonical, backend-owned answer to "what has this cost".
//
// WHY THIS EXISTS RATHER THAN THE FRONTEND ADDING THINGS UP. Before P3-E the
// run detail asked the session-usage reader once per step and summed the
// answers. Because AO deliberately sends repair prompts into the worker's own
// session, a run with one worker and two fix steps asked about the SAME session
// three times and added the same tokens three times. The number was not
// unattributed, it was wrong — and it was wrong in a direction that made the
// product look cheaper or dearer at random. A total has to be computed once,
// from the ledger, by the side that knows what a session is; everything above
// this file renders what it returns.
//
// WHAT EVERY NUMBER HERE IS ENTITLED TO CLAIM. Tokens in this ledger were
// parsed out of a provider's own transcript, so they are provider_reported.
// They are NOT the same quantity as "context AO assembled", which is estimated
// from bytes and lives in the context view; the two are never added and never
// presented as one figure. Cost is calculated from a rate card that names
// itself, or it is unknown. Nothing here produces a zero to stand in for a
// fact AO does not have.

type ledgerStore interface {
	ListUsageAttributionWindowsForRun(ctx context.Context, runID string) ([]domain.UsageAttributionWindow, error)
	AggregateWorkflowRunUsage(ctx context.Context, runID string) ([]store.UsageLedgerLine, error)
	AggregateProjectUsage(ctx context.Context, projectID string, from, to time.Time) ([]store.UsageLedgerLine, error)
	AggregateCompactRunUsageForProject(ctx context.Context, projectID string) ([]store.UsageLedgerLine, error)
	AggregateRunFamilyUsage(ctx context.Context, runID string) ([]store.UsageLedgerLine, error)
	CountProjectUsageWorkflows(ctx context.Context, projectID string, from, to time.Time) (int64, error)
}

// LedgerReader derives every P3-E usage read model from the durable ledger.
type LedgerReader struct {
	store  ledgerStore
	prices *pricing.Table
}

// NewLedgerReader constructs the reader. prices may be nil, in which case every
// cost is unknown and every token count is still reported — the degradation
// P3-E §3 requires.
func NewLedgerReader(s ledgerStore, prices *pricing.Table) *LedgerReader {
	return &LedgerReader{store: s, prices: prices}
}

// Pricing exposes the resolved rate card so callers can report its provenance
// even when nothing was priced.
func (r *LedgerReader) Pricing() *pricing.Table {
	if r == nil {
		return nil
	}
	return r.prices
}

// RunUsageOptions carries what the ledger cannot know by itself.
type RunUsageOptions struct {
	// Budget is the run's FROZEN policy budget, read from its policy
	// snapshot by the caller. Passing it here rather than reading Settings
	// keeps the P3-E promise that a later Settings edit cannot change what an
	// in-flight run was allowed to spend.
	Budget domain.UsageBudgetPolicy
	// IncludeFamily makes the ledger also fold every child run, which is what
	// an autonomous parent's budget must be measured against.
	IncludeFamily bool
}

// WorkflowRun builds one run's full usage ledger.
func (r *LedgerReader) WorkflowRun(ctx context.Context, runID string, opts RunUsageOptions) (domain.WorkflowUsageLedger, error) {
	if r == nil || r.store == nil {
		return domain.WorkflowUsageLedger{}, fmt.Errorf("usage ledger store is unavailable")
	}
	windows, err := r.store.ListUsageAttributionWindowsForRun(ctx, runID)
	if err != nil {
		return domain.WorkflowUsageLedger{}, err
	}
	lines, err := r.store.AggregateWorkflowRunUsage(ctx, runID)
	if err != nil {
		return domain.WorkflowUsageLedger{}, err
	}

	ledger := domain.WorkflowUsageLedger{WorkflowRunID: runID}
	for _, w := range windows {
		if w.ProjectID != "" {
			ledger.ProjectID = w.ProjectID
			break
		}
	}

	roles := r.rolesFromLines(lines)
	ledger.Roles = roles
	for _, role := range roles {
		ledger.Totals = ledger.Totals.Add(role.Tokens)
		ledger.Cost = ledger.Cost.Add(role.Cost)
		if isRepair(role) {
			ledger.RepairTokens = ledger.RepairTokens.Add(role.Tokens)
			ledger.RepairCost = ledger.RepairCost.Add(role.Cost)
		} else {
			ledger.BaseTokens = ledger.BaseTokens.Add(role.Tokens)
			ledger.BaseCost = ledger.BaseCost.Add(role.Cost)
		}
		for _, m := range role.Models {
			ledger.ApproximateEvents += m.ApproximateEvents
		}
		ledger.TotalEvents += role.Tokens.EventCount
	}
	ledger.Models = r.modelsFromLines(lines)
	ledger.Providers = providersFromModels(ledger.Models)
	ledger.Source = sourceForTotals(ledger.Totals)
	ledger.Recorded = ledger.Totals.EventCount > 0
	ledger.Unobservable = unobservableRoles(windows)
	ledger.Complete, ledger.IncompleteReason = completeness(ledger.Unobservable)

	if opts.IncludeFamily {
		familyLines, ferr := r.store.AggregateRunFamilyUsage(ctx, runID)
		if ferr != nil {
			return domain.WorkflowUsageLedger{}, ferr
		}
		ledger.Children, ledger.FamilyTotals, ledger.FamilyCost = r.runsFromLines(familyLines, runID)
	} else {
		ledger.FamilyTotals, ledger.FamilyCost = ledger.Totals, ledger.Cost
	}

	scopeTokens, scopeCost, scope := ledger.Totals, ledger.Cost, "run"
	if opts.IncludeFamily {
		scopeTokens, scopeCost, scope = ledger.FamilyTotals, ledger.FamilyCost, "family"
	}
	ledger.Budget = EvaluateWorkflowBudget(opts.Budget, scopeTokens, scopeCost, scope)
	return ledger, nil
}

// FamilySpend is the cheap read a budget gate needs: a run plus its children,
// with no role breakdown built.
func (r *LedgerReader) FamilySpend(ctx context.Context, runID string) (domain.UsageTokenTotals, domain.UsageCost, error) {
	if r == nil || r.store == nil {
		return domain.UsageTokenTotals{}, domain.UsageCost{}, fmt.Errorf("usage ledger store is unavailable")
	}
	lines, err := r.store.AggregateRunFamilyUsage(ctx, runID)
	if err != nil {
		return domain.UsageTokenTotals{}, domain.UsageCost{}, err
	}
	_, totals, cost := r.runsFromLines(lines, runID)
	return totals, cost, nil
}

// Project builds a project's rollup for one period.
func (r *LedgerReader) Project(ctx context.Context, projectID string, period domain.UsagePeriod, now time.Time, budget domain.UsageBudgetPolicy) (domain.ProjectUsageSummary, error) {
	if r == nil || r.store == nil {
		return domain.ProjectUsageSummary{}, fmt.Errorf("usage ledger store is unavailable")
	}
	from, to := PeriodBounds(period, now)
	lines, err := r.store.AggregateProjectUsage(ctx, projectID, from, to)
	if err != nil {
		return domain.ProjectUsageSummary{}, err
	}
	workflows, err := r.store.CountProjectUsageWorkflows(ctx, projectID, from, to)
	if err != nil {
		return domain.ProjectUsageSummary{}, err
	}

	summary := domain.ProjectUsageSummary{
		ProjectID: projectID, Period: period, From: from, To: to, Workflows: workflows,
	}
	summary.Roles = r.rolesFromLines(lines)
	for _, role := range summary.Roles {
		summary.Totals = summary.Totals.Add(role.Tokens)
		summary.Cost = summary.Cost.Add(role.Cost)
	}
	summary.Models = r.modelsFromLines(lines)
	summary.Providers = providersFromModels(summary.Models)
	summary.Runs, _, _ = r.runsFromLines(lines, "")
	summary.Source = sourceForTotals(summary.Totals)
	summary.Recorded = summary.Totals.EventCount > 0
	if workflows > 0 {
		avg := summary.Totals.Total() / workflows
		summary.AverageTokensPerWorkflow = &avg
	}
	summary.Budget = EvaluateProjectDailyBudget(budget, summary.Totals, summary.Cost, period)
	return summary, nil
}

// CompactForProject returns the Board's per-run figure for a whole project in
// one query. Runs absent from the map have no usage rows; a caller must render
// that as "no usage data recorded", never as zero.
func (r *LedgerReader) CompactForProject(ctx context.Context, projectID string) (map[string]domain.CompactRunUsage, error) {
	if r == nil || r.store == nil {
		return nil, fmt.Errorf("usage ledger store is unavailable")
	}
	lines, err := r.store.AggregateCompactRunUsageForProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.CompactRunUsage, len(lines))
	for _, line := range lines {
		entry := out[line.WorkflowRunID]
		entry.WorkflowRunID = line.WorkflowRunID
		entry.TotalTokens += line.Tokens.Total()
		entry.Cost = entry.Cost.Add(r.cost(line.ModelID, line.Tokens))
		entry.Recorded = entry.Recorded || line.Tokens.EventCount > 0
		entry.Source = domain.TokenSourceProvider
		out[line.WorkflowRunID] = entry
	}
	return out, nil
}

// --- folds ----------------------------------------------------------------

func (r *LedgerReader) cost(modelID string, tokens domain.UsageTokenTotals) domain.UsageCost {
	if r == nil || r.prices == nil {
		return domain.UsageCost{Basis: domain.CostUnknown, UnpricedModels: []string{modelID}}
	}
	return r.prices.Cost(modelID, tokens)
}

type roleKey struct {
	role      domain.WorkflowRole
	cycle     int64
	taskID    string
	attemptID string
}

func (r *LedgerReader) rolesFromLines(lines []store.UsageLedgerLine) []domain.RoleUsageLine {
	order := make([]roleKey, 0, len(lines))
	byKey := map[roleKey]*domain.RoleUsageLine{}
	for _, line := range lines {
		key := roleKey{role: line.Role, cycle: line.Cycle, taskID: line.TaskID, attemptID: line.AttemptID}
		entry, ok := byKey[key]
		if !ok {
			entry = &domain.RoleUsageLine{
				Role: line.Role, Cycle: line.Cycle, TaskID: line.TaskID,
				AttemptID: line.AttemptID, AttemptOrdinal: line.AttemptOrdinal,
				Provider: line.Provider, Harness: line.Harness, Observable: true,
			}
			byKey[key] = entry
			order = append(order, key)
		}
		model := domain.ModelUsageLine{
			Provider: line.Provider, Harness: line.Harness, ModelID: line.ModelID,
			Tokens: line.Tokens, Cost: r.cost(line.ModelID, line.Tokens),
			Source: domain.TokenSourceProvider, ApproximateEvents: line.ApproximateEvents,
		}
		entry.Models = append(entry.Models, model)
		entry.Tokens = entry.Tokens.Add(line.Tokens)
		entry.Cost = entry.Cost.Add(model.Cost)
		if entry.Model == "" {
			entry.Model = line.ModelID
		}
	}
	out := make([]domain.RoleUsageLine, 0, len(order))
	for _, key := range order {
		entry := byKey[key]
		entry.Source = sourceForTotals(entry.Tokens)
		out = append(out, *entry)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return roleRank(out[i].Role) < roleRank(out[j].Role)
		}
		if out[i].Cycle != out[j].Cycle {
			return out[i].Cycle < out[j].Cycle
		}
		return out[i].AttemptOrdinal < out[j].AttemptOrdinal
	})
	return out
}

func (r *LedgerReader) modelsFromLines(lines []store.UsageLedgerLine) []domain.ModelUsageLine {
	type key struct{ provider, harness, model string }
	order := make([]key, 0, len(lines))
	byKey := map[key]*domain.ModelUsageLine{}
	for _, line := range lines {
		k := key{line.Provider, line.Harness, line.ModelID}
		entry, ok := byKey[k]
		if !ok {
			entry = &domain.ModelUsageLine{
				Provider: line.Provider, Harness: line.Harness, ModelID: line.ModelID,
				Source: domain.TokenSourceProvider,
			}
			byKey[k] = entry
			order = append(order, k)
		}
		entry.Tokens = entry.Tokens.Add(line.Tokens)
		entry.ApproximateEvents += line.ApproximateEvents
	}
	out := make([]domain.ModelUsageLine, 0, len(order))
	for _, k := range order {
		entry := byKey[k]
		entry.Cost = r.cost(entry.ModelID, entry.Tokens)
		entry.Source = sourceForTotals(entry.Tokens)
		out = append(out, *entry)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Tokens.Total() > out[j].Tokens.Total()
	})
	return out
}

func (r *LedgerReader) runsFromLines(lines []store.UsageLedgerLine, self string) ([]domain.RunUsageLine, domain.UsageTokenTotals, domain.UsageCost) {
	order := make([]string, 0, len(lines))
	byRun := map[string]*domain.RunUsageLine{}
	var totals domain.UsageTokenTotals
	var cost domain.UsageCost
	for _, line := range lines {
		entry, ok := byRun[line.WorkflowRunID]
		if !ok {
			entry = &domain.RunUsageLine{WorkflowRunID: line.WorkflowRunID, Source: domain.TokenSourceProvider}
			byRun[line.WorkflowRunID] = entry
			order = append(order, line.WorkflowRunID)
		}
		lineCost := r.cost(line.ModelID, line.Tokens)
		entry.Tokens = entry.Tokens.Add(line.Tokens)
		entry.Cost = entry.Cost.Add(lineCost)
		totals = totals.Add(line.Tokens)
		cost = cost.Add(lineCost)
	}
	out := make([]domain.RunUsageLine, 0, len(order))
	for _, id := range order {
		if self != "" && id == self {
			// The parent's own spend is already the ledger's Totals; listing
			// it again as a "child" would read as double the family's cost.
			continue
		}
		entry := byRun[id]
		entry.Source = sourceForTotals(entry.Tokens)
		out = append(out, *entry)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Tokens.Total() > out[j].Tokens.Total() })
	return out, totals, cost
}

func providersFromModels(models []domain.ModelUsageLine) []domain.ProviderUsageLine {
	order := make([]string, 0, len(models))
	byProvider := map[string]*domain.ProviderUsageLine{}
	for _, m := range models {
		entry, ok := byProvider[m.Provider]
		if !ok {
			entry = &domain.ProviderUsageLine{Provider: m.Provider}
			byProvider[m.Provider] = entry
			order = append(order, m.Provider)
		}
		entry.Tokens = entry.Tokens.Add(m.Tokens)
		entry.Cost = entry.Cost.Add(m.Cost)
	}
	out := make([]domain.ProviderUsageLine, 0, len(order))
	for _, p := range order {
		entry := byProvider[p]
		entry.Source = sourceForTotals(entry.Tokens)
		out = append(out, *entry)
	}
	return out
}

// unobservableRoles lists the roles this run dispatched whose spend AO holds no
// measurement for.
//
// Since P3-E's completion pass this is no longer a statement about what AO can
// never see. Every provider-backed role can bind usage now — a reviewer and a
// resolver through their own pane's hook, the planner through its response
// envelope — so a role listed here is one whose surface has not reported yet.
// It is reported rather than omitted for the same reason it always was: leaving
// it out would let a partial total read as the whole bill.
func unobservableRoles(windows []domain.UsageAttributionWindow) []domain.RoleUsageLine {
	var out []domain.RoleUsageLine
	added := map[string]bool{}
	for _, w := range windows {
		if w.HasUsageBinding {
			continue
		}
		key := string(w.Role) + "|" + w.Subject().String()
		if added[key] {
			continue
		}
		added[key] = true
		opened := w.OpenedAt
		out = append(out, domain.RoleUsageLine{
			Role: w.Role, Cycle: w.Cycle, TaskID: w.TaskID,
			AttemptID: w.AttemptID, AttemptOrdinal: w.AttemptOrdinal,
			Provider: w.Provider, Harness: w.Harness, Model: w.Model,
			Source: domain.TokenSourceUnknown, Observable: false,
			// The surface exists and is expected to report; it has not yet, or
			// its hook never fired. Distinct from a role that consumes no
			// provider at all, which never reaches this list.
			UnobservableReason: "awaiting_provider_report",
			OpenedAt:           &opened,
		})
	}
	return out
}

// completeness turns the unmeasured roles into the run's own claim about its
// total. A run with nothing unmeasured reports the whole bill; one with an
// unreported surface says so, and names it.
func completeness(unobservable []domain.RoleUsageLine) (bool, string) {
	if len(unobservable) == 0 {
		return true, ""
	}
	seen := map[domain.WorkflowRole]bool{}
	names := make([]string, 0, len(unobservable))
	for _, role := range unobservable {
		if seen[role.Role] {
			continue
		}
		seen[role.Role] = true
		names = append(names, string(role.Role))
	}
	sort.Strings(names)
	return false, "awaiting provider report from: " + strings.Join(names, ", ")
}

func isRepair(role domain.RoleUsageLine) bool {
	return role.Role == domain.WorkflowRoleFixWorker || role.Cycle > 0
}

func sourceForTotals(t domain.UsageTokenTotals) domain.TokenMeasurementSource {
	if t.EventCount == 0 {
		return domain.TokenSourceUnknown
	}
	return domain.TokenSourceProvider
}

func roleRank(role domain.WorkflowRole) int {
	switch role {
	case domain.WorkflowRolePlanner:
		return 0
	case domain.WorkflowRoleWorker:
		return 1
	case domain.WorkflowRoleReviewer:
		return 2
	case domain.WorkflowRoleFixWorker:
		return 3
	case domain.WorkflowRoleDecisionResolver:
		return 4
	case domain.WorkflowRoleVerify:
		return 5
	default:
		return 6
	}
}

// PeriodBounds turns a named period into a half-open [from, to) range in the
// caller's clock. "all" starts at the zero time, which the SQL treats as an
// open lower bound.
func PeriodBounds(period domain.UsagePeriod, now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	switch period {
	case domain.UsagePeriodToday:
		return endOfDay.AddDate(0, 0, -1), endOfDay
	case domain.UsagePeriodWeek:
		return endOfDay.AddDate(0, 0, -7), endOfDay
	case domain.UsagePeriodMonth:
		return endOfDay.AddDate(0, 0, -30), endOfDay
	default:
		return time.Time{}, now.Add(24 * time.Hour)
	}
}
