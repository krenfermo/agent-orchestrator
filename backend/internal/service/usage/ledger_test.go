package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage/pricing"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// fakeLedgerStore serves canned ledger lines so the FOLD can be tested apart
// from the SQL. The SQL itself is covered against a real database in
// internal/storage/sqlite/store/usage_attribution_store_test.go.
type fakeLedgerStore struct {
	windows []domain.UsageAttributionWindow
	run     []store.UsageLedgerLine
	project []store.UsageLedgerLine
	compact []store.UsageLedgerLine
	family  []store.UsageLedgerLine
	count   int64
}

func (f *fakeLedgerStore) ListUsageAttributionWindowsForRun(context.Context, string) ([]domain.UsageAttributionWindow, error) {
	return f.windows, nil
}
func (f *fakeLedgerStore) AggregateWorkflowRunUsage(context.Context, string) ([]store.UsageLedgerLine, error) {
	return f.run, nil
}
func (f *fakeLedgerStore) AggregateProjectUsage(context.Context, string, time.Time, time.Time) ([]store.UsageLedgerLine, error) {
	return f.project, nil
}
func (f *fakeLedgerStore) AggregateCompactRunUsageForProject(context.Context, string) ([]store.UsageLedgerLine, error) {
	return f.compact, nil
}
func (f *fakeLedgerStore) AggregateRunFamilyUsage(context.Context, string) ([]store.UsageLedgerLine, error) {
	return f.family, nil
}
func (f *fakeLedgerStore) CountProjectUsageWorkflows(context.Context, string, time.Time, time.Time) (int64, error) {
	return f.count, nil
}

func line(role domain.WorkflowRole, cycle int64, model string, input, output int64) store.UsageLedgerLine {
	return store.UsageLedgerLine{
		WorkflowRunID: "wf-1", Role: role, Cycle: cycle, Provider: "anthropic",
		Harness: "claude-code", ModelID: model,
		Tokens: domain.UsageTokenTotals{
			InputTokens: input, UncachedInputTokens: input, OutputTokens: output, EventCount: 1,
		},
	}
}

func TestWorkflowRun_SplitsBaseExecutionFromRepair(t *testing.T) {
	// "Base 40k, repair +18k" is the number that tells an operator whether
	// re-work is what made a run expensive. It is a fold over the cycle on each
	// window, not a second ledger.
	reader := usagesvc.NewLedgerReader(&fakeLedgerStore{
		run: []store.UsageLedgerLine{
			line(domain.WorkflowRoleWorker, 0, "claude-opus-5", 36_000, 4_000),
			line(domain.WorkflowRoleFixWorker, 1, "claude-opus-5", 16_000, 2_000),
		},
	}, pricing.Embedded())

	ledger, err := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.RunUsageOptions{})
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if ledger.Totals.Total() != 58_000 {
		t.Fatalf("total = %d, want 58000", ledger.Totals.Total())
	}
	if ledger.BaseTokens.Total() != 40_000 {
		t.Fatalf("base = %d, want 40000", ledger.BaseTokens.Total())
	}
	if ledger.RepairTokens.Total() != 18_000 {
		t.Fatalf("repair = %d, want 18000", ledger.RepairTokens.Total())
	}
	if ledger.Source != domain.TokenSourceProvider {
		t.Fatalf("source = %q, want provider_reported", ledger.Source)
	}
	if !ledger.Recorded {
		t.Fatal("a run with events is recorded")
	}
}

func TestWorkflowRun_EmptyLedgerIsNotRecordedAndNotZeroCost(t *testing.T) {
	reader := usagesvc.NewLedgerReader(&fakeLedgerStore{}, pricing.Embedded())
	ledger, err := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.RunUsageOptions{})
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if ledger.Recorded {
		t.Fatal("a run with no events must report recorded=false so the UI says 'no usage data recorded'")
	}
	if ledger.Source != domain.TokenSourceUnknown {
		t.Fatalf("source = %q, want unknown", ledger.Source)
	}
	if ledger.Cost.Known {
		t.Fatal("no events means no cost, and 'no cost' is unknown rather than $0.00")
	}
}

func TestWorkflowRun_UnpricedModelKeepsTokensAndDropsOnlyTheMoney(t *testing.T) {
	// The degradation P3-E §3 demands: an unknown rate must never suppress a
	// token count, and must never be smoothed into a cheaper-looking total
	// without saying so.
	reader := usagesvc.NewLedgerReader(&fakeLedgerStore{
		run: []store.UsageLedgerLine{
			line(domain.WorkflowRoleWorker, 0, "claude-opus-5", 1_000_000, 0),
			line(domain.WorkflowRoleWorker, 0, "some-local-model", 5_000_000, 0),
		},
	}, pricing.Embedded())

	ledger, err := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.RunUsageOptions{})
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if ledger.Totals.Total() != 6_000_000 {
		t.Fatalf("tokens = %d, want every token reported regardless of pricing", ledger.Totals.Total())
	}
	if !ledger.Cost.Known {
		t.Fatal("the priced part is still known; the cost is partial, not absent")
	}
	if len(ledger.Cost.UnpricedModels) != 1 || ledger.Cost.UnpricedModels[0] != "some-local-model" {
		t.Fatalf("a partial cost must name what it is missing, got %+v", ledger.Cost.UnpricedModels)
	}
}

func TestWorkflowRun_UnreportedRolesAreListedAndMakeTheRunIncomplete(t *testing.T) {
	// A surface that has not reported its spend leaves the run's total a FLOOR,
	// and the ledger says so out loud rather than letting the number read as the
	// whole bill.
	reader := usagesvc.NewLedgerReader(&fakeLedgerStore{
		windows: []domain.UsageAttributionWindow{
			{SessionID: "s1", SubjectKind: domain.UsageSubjectSession, Role: domain.WorkflowRoleWorker, HasUsageBinding: true, WorkflowRunID: "wf-1"},
			{SessionID: "rr-1", SubjectKind: domain.UsageSubjectRuntimePane, Role: domain.WorkflowRoleReviewer, HasUsageBinding: false, WorkflowRunID: "wf-1", Harness: "codex"},
			{SessionID: "wqr-1", SubjectKind: domain.UsageSubjectRuntimePane, Role: domain.WorkflowRoleDecisionResolver, HasUsageBinding: false, WorkflowRunID: "wf-1"},
		},
		run: []store.UsageLedgerLine{line(domain.WorkflowRoleWorker, 0, "claude-opus-5", 100, 10)},
	}, pricing.Embedded())

	ledger, err := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.RunUsageOptions{})
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if len(ledger.Unobservable) != 2 {
		t.Fatalf("unobservable = %d, want the reviewer and the resolver", len(ledger.Unobservable))
	}
	for _, r := range ledger.Unobservable {
		if r.Observable || r.Source != domain.TokenSourceUnknown {
			t.Fatalf("role %+v must be labeled unknown, never zero", r)
		}
		if r.UnobservableReason != "awaiting_provider_report" {
			t.Fatalf("reason = %q — a surface that has not reported is pending, not unmeasurable by design", r.UnobservableReason)
		}
	}
	if ledger.Complete {
		t.Fatal("a run with an unreported surface must not claim its total is the whole bill")
	}
	if ledger.IncompleteReason == "" {
		t.Fatal("an incomplete total must name what it is missing")
	}
}

func TestWorkflowRun_EveryRoleReportedMakesTheRunComplete(t *testing.T) {
	// The state P3-E's completion bar asks for: every provider-backed role has
	// reported, so the total is the bill rather than a floor.
	reader := usagesvc.NewLedgerReader(&fakeLedgerStore{
		windows: []domain.UsageAttributionWindow{
			{SessionID: "s1", SubjectKind: domain.UsageSubjectSession, Role: domain.WorkflowRoleWorker, HasUsageBinding: true, WorkflowRunID: "wf-1"},
			{SessionID: "rr-1", SubjectKind: domain.UsageSubjectRuntimePane, Role: domain.WorkflowRoleReviewer, HasUsageBinding: true, WorkflowRunID: "wf-1"},
			{SessionID: "wqr-1", SubjectKind: domain.UsageSubjectRuntimePane, Role: domain.WorkflowRoleDecisionResolver, HasUsageBinding: true, WorkflowRunID: "wf-1"},
			{SessionID: "wf-1#1", SubjectKind: domain.UsageSubjectPlannerInvocation, Role: domain.WorkflowRolePlanner, HasUsageBinding: true, WorkflowRunID: "wf-1"},
		},
		run: []store.UsageLedgerLine{
			line(domain.WorkflowRoleWorker, 0, "claude-opus-5", 100, 10),
			line(domain.WorkflowRoleReviewer, 0, "gpt-5-codex", 50, 5),
			line(domain.WorkflowRoleDecisionResolver, 0, "gpt-5-codex", 20, 2),
			line(domain.WorkflowRolePlanner, 0, "claude-sonnet-5", 30, 3),
		},
	}, pricing.Embedded())

	ledger, err := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.RunUsageOptions{})
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if !ledger.Complete || ledger.IncompleteReason != "" {
		t.Fatalf("complete = %v (%q), want a complete total", ledger.Complete, ledger.IncompleteReason)
	}
	if len(ledger.Unobservable) != 0 {
		t.Fatalf("unobservable = %+v, want none", ledger.Unobservable)
	}
	// Cross-provider: tokens complete, cost PARTIAL, because no rate covers the
	// Codex models. The total must not silently read as the full spend in money.
	if ledger.Totals.Total() != 220 {
		t.Fatalf("tokens = %d, want 220 across all four roles", ledger.Totals.Total())
	}
	if !ledger.Cost.Known {
		t.Fatal("the Anthropic part is priced, so a partial cost is known")
	}
	if len(ledger.Cost.UnpricedModels) == 0 {
		t.Fatal("a cost that could not price the Codex roles must say which models it is missing")
	}
}

func TestWorkflowRun_FamilyExcludesTheParentFromItsOwnChildren(t *testing.T) {
	reader := usagesvc.NewLedgerReader(&fakeLedgerStore{
		run: []store.UsageLedgerLine{line(domain.WorkflowRoleWorker, 0, "claude-opus-5", 100, 0)},
		family: []store.UsageLedgerLine{
			{WorkflowRunID: "wf-1", ModelID: "claude-opus-5", Tokens: domain.UsageTokenTotals{InputTokens: 100, EventCount: 1}},
			{WorkflowRunID: "wf-child-a", ModelID: "claude-opus-5", Tokens: domain.UsageTokenTotals{InputTokens: 400, EventCount: 1}},
			{WorkflowRunID: "wf-child-b", ModelID: "claude-opus-5", Tokens: domain.UsageTokenTotals{InputTokens: 500, EventCount: 1}},
		},
	}, pricing.Embedded())

	ledger, err := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.RunUsageOptions{IncludeFamily: true})
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if ledger.FamilyTotals.Total() != 1000 {
		t.Fatalf("family total = %d, want 1000", ledger.FamilyTotals.Total())
	}
	if len(ledger.Children) != 2 {
		t.Fatalf("children = %d, want 2 — the parent must not be listed as its own child", len(ledger.Children))
	}
	for _, c := range ledger.Children {
		if c.WorkflowRunID == "wf-1" {
			t.Fatal("the parent appeared in its own children list, which would double the family total on screen")
		}
	}
}

func TestProject_AverageIsNullWithNoWorkflows(t *testing.T) {
	reader := usagesvc.NewLedgerReader(&fakeLedgerStore{}, pricing.Embedded())
	summary, err := reader.Project(context.Background(), "p", domain.UsagePeriodWeek, time.Now().UTC(), domain.UsageBudgetPolicy{})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if summary.AverageTokensPerWorkflow != nil {
		t.Fatal("an average over zero workflows has no answer and must be nil, not 0")
	}
	if summary.Recorded {
		t.Fatal("nothing recorded in the period")
	}
}

func TestPeriodBounds(t *testing.T) {
	now := time.Date(2026, 9, 3, 14, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		period   domain.UsagePeriod
		wantDays int
	}{
		{domain.UsagePeriodToday, 1},
		{domain.UsagePeriodWeek, 7},
		{domain.UsagePeriodMonth, 30},
	} {
		from, to := usagesvc.PeriodBounds(tc.period, now)
		if got := int(to.Sub(from).Hours() / 24); got != tc.wantDays {
			t.Fatalf("%s spans %d days, want %d", tc.period, got, tc.wantDays)
		}
		if !to.After(now) {
			t.Fatalf("%s upper bound %s must include the present instant", tc.period, to)
		}
	}
	from, _ := usagesvc.PeriodBounds(domain.UsagePeriodAllTime, now)
	if !from.IsZero() {
		t.Fatalf("all-time lower bound = %s, want the zero time", from)
	}
}

func TestEvaluateWorkflowBudget_Thresholds(t *testing.T) {
	policy := domain.UsageBudgetPolicy{WorkflowTokenBudget: 1000, WarnPercent: 80}
	for _, tc := range []struct {
		used int64
		want domain.UsageBudgetState
	}{
		{500, domain.BudgetOK},
		{799, domain.BudgetOK},
		{800, domain.BudgetWarning},
		{999, domain.BudgetWarning},
		{1000, domain.BudgetExhausted},
		{5000, domain.BudgetExhausted},
	} {
		status := usagesvc.EvaluateWorkflowBudget(policy,
			domain.UsageTokenTotals{InputTokens: tc.used}, domain.UsageCost{}, "run")
		if status.State != tc.want {
			t.Fatalf("%d tokens against 1000 => %q, want %q", tc.used, status.State, tc.want)
		}
	}
}

func TestEvaluateWorkflowBudget_UnsetIsNotZeroPercent(t *testing.T) {
	status := usagesvc.EvaluateWorkflowBudget(domain.UsageBudgetPolicy{},
		domain.UsageTokenTotals{InputTokens: 999_999}, domain.UsageCost{}, "run")
	if status.State != domain.BudgetUnset {
		t.Fatalf("state = %q, want unset — nobody configured a ceiling", status.State)
	}
	if status.TokenPercent != nil {
		t.Fatal("an unset budget has no percentage; rendering one would invent a limit")
	}
	if status.Blocking() {
		t.Fatal("an unset budget must never block a dispatch")
	}
}

func TestEvaluateWorkflowBudget_PartialCostCannotExhaust(t *testing.T) {
	// A cost computed with an unpriced model in play is a LOWER bound. It may
	// warn — a lower bound already past the line is certainly past it — but a
	// hard stop on a number AO knows is incomplete is exactly the invented
	// enforcement P3-E forbids.
	policy := domain.UsageBudgetPolicy{WorkflowCostBudgetUSD: 10, WarnPercent: 80}
	partial := domain.UsageCost{
		Known: true, Basis: domain.CostCalculated, Amount: 50,
		UnpricedModels: []string{"some-local-model"},
	}
	status := usagesvc.EvaluateWorkflowBudget(policy, domain.UsageTokenTotals{}, partial, "run")
	if status.State != domain.BudgetWarning {
		t.Fatalf("state = %q, want warning — a partial cost warns but never exhausts", status.State)
	}
	if status.Blocking() {
		t.Fatal("a partial cost must not block a dispatch")
	}

	complete := domain.UsageCost{Known: true, Basis: domain.CostCalculated, Amount: 50}
	if got := usagesvc.EvaluateWorkflowBudget(policy, domain.UsageTokenTotals{}, complete, "run"); got.State != domain.BudgetExhausted {
		t.Fatalf("a COMPLETE cost past the ceiling => %q, want exhausted", got.State)
	}
}

func TestEvaluateWorkflowBudget_UnknownCostLeavesTheCeilingUnenforced(t *testing.T) {
	policy := domain.UsageBudgetPolicy{WorkflowCostBudgetUSD: 1}
	status := usagesvc.EvaluateWorkflowBudget(policy, domain.UsageTokenTotals{}, domain.UsageCost{}, "run")
	if status.Blocking() {
		t.Fatal("a cost ceiling with no cost at all cannot be judged, let alone enforced")
	}
	if status.CostPercent != nil {
		t.Fatal("no cost means no percentage; 0% would claim the run has spent nothing")
	}
	if status.Reason == "" {
		t.Fatal("an unenforceable ceiling must say why")
	}
}

func TestEvaluateProjectDailyBudget_OnlyAppliesToToday(t *testing.T) {
	policy := domain.UsageBudgetPolicy{ProjectDailyTokenBudget: 100}
	spent := domain.UsageTokenTotals{InputTokens: 5000}
	if got := usagesvc.EvaluateProjectDailyBudget(policy, spent, domain.UsageCost{}, domain.UsagePeriodMonth); got.State != domain.BudgetUnset {
		t.Fatalf("30 days against a per-DAY ceiling => %q; comparing them is arithmetic nonsense", got.State)
	}
	if got := usagesvc.EvaluateProjectDailyBudget(policy, spent, domain.UsageCost{}, domain.UsagePeriodToday); got.State != domain.BudgetExhausted {
		t.Fatalf("today against a per-day ceiling => %q, want exhausted", got.State)
	}
}

func TestEffectiveUsageBudgetPolicy_ChildrenShareTheParentCeilingByDefault(t *testing.T) {
	// P3-E §16's example: ten tasks at 100k each under a 200k parent. The
	// default has to be "shared", or the parent's budget means nothing.
	var legacy domain.WorkflowPolicy
	effective := legacy.EffectiveUsageBudgetPolicy()
	if !effective.ParentScoped() {
		t.Fatal("children must share the parent's ceiling unless a policy says otherwise")
	}
	if effective.Configured() {
		t.Fatal("a policy snapshot written before P3-E has NO ceiling — zero must not become a budget of zero")
	}
	if effective.EffectiveWarnPercent() != domain.DefaultUsageWarnPercent {
		t.Fatalf("warn percent = %d, want the default", effective.EffectiveWarnPercent())
	}
}
