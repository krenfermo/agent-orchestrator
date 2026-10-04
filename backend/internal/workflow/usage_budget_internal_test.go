package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// usage_budget_internal_test.go — P3-E's gate, at the boundary it actually
// guards.
//
// The behaviours under test are all refusals to invent something: an unset
// budget does not become a budget of zero, a spend AO cannot measure does not
// become a stop, and a cost with no rate card does not become an enforceable
// ceiling.

type fakeUsageLedger struct {
	lines []domain.ModelUsageLine
	err   error
	calls int
}

func (f *fakeUsageLedger) SumRunFamilyUsage(stdctx.Context, string) ([]domain.ModelUsageLine, error) {
	f.calls++
	return f.lines, f.err
}

type fakePricer struct{ perModel map[string]float64 }

func (p fakePricer) Cost(modelID string, tokens domain.UsageTokenTotals) domain.UsageCost {
	rate, ok := p.perModel[modelID]
	if !ok {
		return domain.UsageCost{Basis: domain.CostUnknown, UnpricedModels: []string{modelID}}
	}
	return domain.UsageCost{
		Known: true, Basis: domain.CostCalculated, Currency: "USD",
		Amount: rate * float64(tokens.Total()) / 1_000_000,
	}
}

func usageBudgetRun(t *testing.T, policy domain.UsageBudgetPolicy) domain.WorkflowRun {
	t.Helper()
	snapshot, err := json.Marshal(domain.WorkflowPolicy{MaxFixCycles: 3, Usage: policy})
	if err != nil {
		t.Fatal(err)
	}
	return domain.WorkflowRun{
		ID: "wf-budget", ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunRunning, PolicySnapshot: string(snapshot),
	}
}

func usageTokens(n int64) []domain.ModelUsageLine {
	return []domain.ModelUsageLine{{
		Provider: "anthropic", ModelID: "claude-opus-5",
		Tokens: domain.UsageTokenTotals{InputTokens: n, UncachedInputTokens: n, EventCount: 1},
	}}
}

func TestUsageBudget_UnsetNeverBlocks(t *testing.T) {
	ledger := &fakeUsageLedger{lines: usageTokens(10_000_000)}
	c := New(Deps{Store: sqlitetest.MustOpen(t), UsagePricer: fakePricer{}})
	c.usageBudgets = ledger

	status := c.usageBudgetStatus(stdctx.Background(), usageBudgetRun(t, domain.UsageBudgetPolicy{}))
	if status.State != domain.BudgetUnset {
		t.Fatalf("state = %q, want unset", status.State)
	}
	if status.Blocking() {
		t.Fatal("a run whose policy names no ceiling must never be stopped by one")
	}
	if ledger.calls != 0 {
		t.Fatal("an unset budget must not even read the ledger — there is nothing to measure against")
	}
}

func TestUsageBudget_ExhaustedAtTheHardLimit(t *testing.T) {
	c := New(Deps{Store: sqlitetest.MustOpen(t), UsagePricer: fakePricer{}})
	c.usageBudgets = &fakeUsageLedger{lines: usageTokens(200_000)}

	status := c.usageBudgetStatus(stdctx.Background(),
		usageBudgetRun(t, domain.UsageBudgetPolicy{WorkflowTokenBudget: 200_000}))
	if status.State != domain.BudgetExhausted || !status.Blocking() {
		t.Fatalf("status = %+v, want exhausted and blocking", status)
	}
	if status.Scope != "family" {
		t.Fatalf("scope = %q, want family — a parent's ceiling covers its children by default", status.Scope)
	}
	if status.Reason == "" {
		t.Fatal("a stop must name what stopped it")
	}
}

func TestUsageBudget_WarningDoesNotBlock(t *testing.T) {
	c := New(Deps{Store: sqlitetest.MustOpen(t), UsagePricer: fakePricer{}})
	c.usageBudgets = &fakeUsageLedger{lines: usageTokens(85_000)}

	status := c.usageBudgetStatus(stdctx.Background(),
		usageBudgetRun(t, domain.UsageBudgetPolicy{WorkflowTokenBudget: 100_000}))
	if status.State != domain.BudgetWarning {
		t.Fatalf("state = %q, want warning at 85%%", status.State)
	}
	if status.Blocking() {
		t.Fatal("a warning is advice; it must never stop a dispatch by itself (P3-E §30)")
	}
}

func TestUsageBudget_UnmeasurableNeverBlocks(t *testing.T) {
	// Two ways AO can fail to measure: no ledger wired at all, and a read that
	// errors. Both must fail OPEN. Refusing to dispatch on the strength of a
	// number AO does not have would stop real work for a fiction.
	policy := domain.UsageBudgetPolicy{WorkflowTokenBudget: 1}

	noLedger := New(Deps{Store: sqlitetest.MustOpen(t), UsagePricer: fakePricer{}})
	noLedger.usageBudgets = nil // a store that predates the attribution table
	if status := noLedger.usageBudgetStatus(stdctx.Background(), usageBudgetRun(t, policy)); status.Blocking() {
		t.Fatal("a coordinator with no ledger must not enforce a ceiling it cannot measure")
	} else if status.Reason == "" {
		t.Fatal("an unmeasurable budget must say why")
	}

	broken := New(Deps{Store: sqlitetest.MustOpen(t), UsagePricer: fakePricer{}})
	broken.usageBudgets = &fakeUsageLedger{err: errors.New("db gone")}
	if status := broken.usageBudgetStatus(stdctx.Background(), usageBudgetRun(t, policy)); status.Blocking() {
		t.Fatal("a failed ledger read must not become a budget stop")
	}
}

func TestUsageBudget_CostCeilingIsUnenforceableWithoutARate(t *testing.T) {
	// A local model with no rate card entry. The token ceiling (which needs no
	// prices) still applies; the COST ceiling cannot be judged and says so.
	c := New(Deps{Store: sqlitetest.MustOpen(t), UsagePricer: fakePricer{}})
	c.usageBudgets = &fakeUsageLedger{lines: []domain.ModelUsageLine{{
		ModelID: "some-local-model",
		Tokens:  domain.UsageTokenTotals{InputTokens: 5_000_000, EventCount: 1},
	}}}

	status := c.usageBudgetStatus(stdctx.Background(),
		usageBudgetRun(t, domain.UsageBudgetPolicy{WorkflowCostBudgetUSD: 0.01}))
	if status.Blocking() {
		t.Fatal("a cost ceiling with no price for the model in play must not stop the run")
	}
	if status.CostPercent != nil {
		t.Fatal("no known cost means no percentage; 0% would claim the run spent nothing")
	}
}

func TestUsageBudget_ParkingRecordsWhyAndDoesNotCancel(t *testing.T) {
	// The stop is a park for a human, not a cancellation, and it is recorded on
	// the append-only ledger so a restart reads the same reason.
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Unix(1700000000, 0).UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(domain.WorkflowPolicy{
		MaxFixCycles: 3, Usage: domain.UsageBudgetPolicy{WorkflowTokenBudget: 10},
	})
	run, _, err := store.CreateWorkflowRun(ctx, domain.WorkflowRun{
		ID: "wf-budget", ProjectID: "p", Objective: "o",
		State: domain.WorkflowRunRunning, PolicySnapshot: string(snapshot),
		CreatedAt: now, UpdatedAt: now,
	}, nil)
	if err != nil {
		t.Fatalf("create workflow run: %v", err)
	}

	c := New(Deps{Store: store, Projects: store, Clock: func() time.Time { return now }, UsagePricer: fakePricer{}})
	c.usageBudgets = &fakeUsageLedger{lines: usageTokens(1_000)}

	if !c.usageBudgetBlocks(ctx, run, "a worker dispatch") {
		t.Fatal("a run 100x over its ceiling must be blocked")
	}
	after, ok, err := store.GetWorkflowRun(ctx, run.ID)
	if err != nil || !ok {
		t.Fatalf("re-read run: %v", err)
	}
	if after.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("state = %q, want needs_attention — a budget stop is a park for a person, not a cancel", after.State)
	}
	checkpoints, err := store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, cp := range checkpoints {
		if cp.DurablePhase == usageBudgetExhaustedPhase {
			found = true
			if cp.NextAction == "" {
				t.Fatal("a budget stop must record what a human should do about it")
			}
		}
	}
	if !found {
		t.Fatalf("no %s checkpoint recorded; a restart would not know why the run stopped", usageBudgetExhaustedPhase)
	}
}
