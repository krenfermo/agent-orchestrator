package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// fakeDynamicsStore serves a canned SERIES, so the fold that turns calls into a
// shape is tested apart from the SQL that orders them.
type fakeDynamicsStore struct {
	events      []store.UsageTrajectoryEvent
	unplaceable int64
	err         error
}

func (f *fakeDynamicsStore) ListRunContextTrajectoryEvents(context.Context, string) ([]store.UsageTrajectoryEvent, error) {
	return f.events, f.err
}

func (f *fakeDynamicsStore) CountRunUnplaceableUsageEvents(context.Context, string) (int64, error) {
	return f.unplaceable, nil
}

var trajectoryBase = time.Date(2026, 9, 9, 17, 10, 0, 0, time.UTC)

// call builds one provider call: context is the whole input the provider
// re-read, which is what makes the series meaningful.
func call(step string, role domain.WorkflowRole, minute int, context, cacheRead, output int64) store.UsageTrajectoryEvent {
	return store.UsageTrajectoryEvent{
		WorkflowStepID: step, Role: role, ModelID: "claude-opus-5",
		ObservedAt: trajectoryBase.Add(time.Duration(minute) * time.Minute),
		Tokens: domain.UsageTokenTotals{
			InputTokens: context, CacheReadTokens: cacheRead,
			UncachedInputTokens: context - cacheRead, OutputTokens: output, EventCount: 1,
		},
	}
}

// The wf-1c2cb9bd shape, in miniature: a context that grows call after call
// while the total says nothing about why. Growth is the difference between the
// ends of the series and cannot be recovered from any sum over it.
func TestTrajectoryReportsTheEndsOfTheSeriesAndNotItsArea(t *testing.T) {
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{events: []store.UsageTrajectoryEvent{
		call("step-work", domain.WorkflowRoleWorker, 0, 54_402, 26_009, 400),
		call("step-work", domain.WorkflowRoleWorker, 5, 120_000, 118_000, 900),
		call("step-work", domain.WorkflowRoleWorker, 20, 170_000, 168_000, 700),
	}}, nil)

	got, err := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{})
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if !got.Recorded || !got.Trajectory.Observable {
		t.Fatalf("a run with three placeable calls must be recorded and observable, got %+v", got.Trajectory)
	}
	if got.Trajectory.ProviderCalls != 3 {
		t.Errorf("providerCalls = %d, want 3", got.Trajectory.ProviderCalls)
	}
	if got.Trajectory.FirstContextTokens != 54_402 || got.Trajectory.LastContextTokens != 170_000 {
		t.Errorf("series ends = %d..%d, want 54402..170000",
			got.Trajectory.FirstContextTokens, got.Trajectory.LastContextTokens)
	}
	if got.Trajectory.GrowthTokens != 115_598 {
		t.Errorf("growth = %d, want 115598", got.Trajectory.GrowthTokens)
	}
	if got.Trajectory.PeakContextTokens != 170_000 {
		t.Errorf("peak = %d, want 170000", got.Trajectory.PeakContextTokens)
	}
	elapsed, ok := got.Trajectory.Elapsed()
	if !ok || elapsed != 20*time.Minute {
		t.Errorf("elapsed = %v (%t), want 20m", elapsed, ok)
	}
}

// PHASE J.3 -- a run whose context goes from 50k to 170k must say so.
func TestContextGrowthPastTheProfileEarnsAWarning(t *testing.T) {
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{events: []store.UsageTrajectoryEvent{
		call("step-work", domain.WorkflowRoleWorker, 0, 50_000, 20_000, 500),
		call("step-work", domain.WorkflowRoleWorker, 25, 170_000, 168_000, 500),
	}}, nil)

	got, err := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{
		Strategy: domain.ExecutionStrategyTask,
	})
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	w, found := findAdvisory(got.Warnings, domain.AdvisoryContextGrowthHigh)
	if !found {
		t.Fatalf("120k of growth on a Task earned no warning; got %+v", got.Warnings)
	}
	if w.Severity != domain.AdvisoryWarn {
		t.Errorf("severity = %q, want warn", w.Severity)
	}
	if w.Observed != 120_000 || w.Threshold != 100_000 {
		t.Errorf("observed/threshold = %d/%d, want 120000/100000", w.Observed, w.Threshold)
	}
	if w.Profile != domain.ExecutionStrategyTask {
		t.Errorf("profile = %q, want task", w.Profile)
	}
}

// The same growth under the Master profile is ordinary and must stay quiet.
// A threshold that fires for every kind of run is a threshold nobody reads.
func TestTheSameGrowthIsNotAWarningUnderAWiderProfile(t *testing.T) {
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{events: []store.UsageTrajectoryEvent{
		call("step-work", domain.WorkflowRoleWorker, 0, 50_000, 20_000, 500),
		call("step-work", domain.WorkflowRoleWorker, 25, 170_000, 168_000, 500),
	}}, nil)

	got, _ := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{
		Strategy: domain.ExecutionStrategyMaster,
	})
	if _, found := findAdvisory(got.Warnings, domain.AdvisoryContextGrowthHigh); found {
		t.Fatalf("120k of growth warned under the master profile (400k), got %+v", got.Warnings)
	}
}

// PHASE J.4 -- cache-read dominance is reported as its own INFORMATIONAL fact,
// never folded into the total and never dressed up as a fault. 98.8% was
// measured on a run with no loop, no retry and no defect.
func TestCacheReadDominanceIsInformationNotAFault(t *testing.T) {
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{events: []store.UsageTrajectoryEvent{
		call("step-work", domain.WorkflowRoleWorker, 0, 100_000, 99_000, 500),
		call("step-work", domain.WorkflowRoleWorker, 1, 100_000, 99_000, 500),
	}}, nil)

	got, _ := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{
		Strategy: domain.ExecutionStrategyTask,
	})
	w, found := findAdvisory(got.Warnings, domain.AdvisoryCacheReadDominant)
	if !found {
		t.Fatalf("99%% cache read was not reported at all; got %+v", got.Warnings)
	}
	if w.Severity != domain.AdvisoryInfo {
		t.Errorf("severity = %q, want info: re-reading a cached conversation is the arithmetic of an agentic loop, not a defect", w.Severity)
	}
	if w.Observed != 99 {
		t.Errorf("observed share = %d, want 99", w.Observed)
	}
}

// Provider calls are the FIRST lever on cost, so they get their own line
// rather than being inferred from a token total.
func TestTooManyProviderCallsForTheProfileEarnsAWarning(t *testing.T) {
	var events []store.UsageTrajectoryEvent
	for i := 0; i < 130; i++ {
		events = append(events, call("step-work", domain.WorkflowRoleWorker, i, 60_000, 59_000, 200))
	}
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{events: events}, nil)
	got, _ := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{
		Strategy: domain.ExecutionStrategyTask,
	})
	w, found := findAdvisory(got.Warnings, domain.AdvisoryProviderCallsHigh)
	if !found {
		t.Fatalf("130 calls on a Task earned no warning; got %+v", got.Warnings)
	}
	if w.Observed != 130 || w.Threshold != 120 {
		t.Errorf("observed/threshold = %d/%d, want 130/120", w.Observed, w.Threshold)
	}
}

// A policy's advisory override wins over the profile default, and it moves the
// line in BOTH directions -- otherwise an operator could only ever be warned
// more, never less.
func TestAPolicyOverrideMovesTheAdvisoryLine(t *testing.T) {
	events := []store.UsageTrajectoryEvent{
		call("step-work", domain.WorkflowRoleWorker, 0, 50_000, 20_000, 500),
		call("step-work", domain.WorkflowRoleWorker, 5, 90_000, 85_000, 500),
	}
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{events: events}, nil)

	quiet, _ := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{
		Strategy: domain.ExecutionStrategyTask,
	})
	if _, found := findAdvisory(quiet.Warnings, domain.AdvisoryContextGrowthHigh); found {
		t.Fatalf("40k of growth warned against the 100k default")
	}

	loud, _ := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{
		Strategy: domain.ExecutionStrategyTask,
		Budget:   domain.UsageBudgetPolicy{WorkflowContextGrowthWarnTokens: 10_000},
	})
	w, found := findAdvisory(loud.Warnings, domain.AdvisoryContextGrowthHigh)
	if !found {
		t.Fatalf("a 10k override did not fire on 40k of growth; got %+v", loud.Warnings)
	}
	if w.Threshold != 10_000 {
		t.Errorf("threshold = %d, want the override 10000", w.Threshold)
	}
}

// PHASE J.7 -- a long test suite is not a dead worker, and it is not a stalled
// run either. The growth-without-progress conjunction needs all three terms,
// and a durable progress fact since dispatch is what a legitimately slow step
// has.
func TestALongRunningStepWithDurableProgressIsNotWarnedAbout(t *testing.T) {
	now := trajectoryBase.Add(40 * time.Minute)
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{events: []store.UsageTrajectoryEvent{
		call("step-work", domain.WorkflowRoleWorker, 0, 50_000, 20_000, 500),
		call("step-work", domain.WorkflowRoleWorker, 30, 90_000, 85_000, 500),
	}}, nil)

	got, _ := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{
		Strategy: domain.ExecutionStrategyTask,
		Now:      now,
		Progress: usagesvc.ProgressEvidence{
			Known:        true,
			DispatchedAt: trajectoryBase,
			// Git evidence five minutes in -- the wf-1c2cb9bd shape exactly.
			LastDurableProgressAt: trajectoryBase.Add(5 * time.Minute),
			WorkerLastSignalAt:    now.Add(-20 * time.Second),
		},
	})
	if _, found := findAdvisory(got.Warnings, domain.AdvisoryGrowthWithoutProgress); found {
		t.Fatalf("a run with durable progress five minutes in was called stalled; got %+v", got.Warnings)
	}
}

// ...and the conjunction DOES fire when all three terms hold, or it would be
// decoration.
func TestGrowthWithNoProgressAndALiveWorkerFires(t *testing.T) {
	now := trajectoryBase.Add(40 * time.Minute)
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{events: []store.UsageTrajectoryEvent{
		call("step-work", domain.WorkflowRoleWorker, 0, 50_000, 20_000, 500),
		call("step-work", domain.WorkflowRoleWorker, 30, 90_000, 85_000, 500),
	}}, nil)

	got, _ := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{
		Strategy: domain.ExecutionStrategyTask,
		Now:      now,
		Progress: usagesvc.ProgressEvidence{
			Known:              true,
			DispatchedAt:       trajectoryBase,
			WorkerLastSignalAt: now.Add(-20 * time.Second),
		},
	})
	if _, found := findAdvisory(got.Warnings, domain.AdvisoryGrowthWithoutProgress); !found {
		t.Fatalf("growth + no progress + a live worker did not fire; got %+v", got.Warnings)
	}
}

// A worker AO has not heard from belongs to the recovery path, not to an
// advisory. Firing here would put a second, competing opinion about a silent
// worker in front of a person.
func TestASilentWorkerIsNotThisAdvisorysBusiness(t *testing.T) {
	now := trajectoryBase.Add(40 * time.Minute)
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{events: []store.UsageTrajectoryEvent{
		call("step-work", domain.WorkflowRoleWorker, 0, 50_000, 20_000, 500),
		call("step-work", domain.WorkflowRoleWorker, 30, 90_000, 85_000, 500),
	}}, nil)

	got, _ := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{
		Strategy: domain.ExecutionStrategyTask,
		Now:      now,
		Progress: usagesvc.ProgressEvidence{
			Known: true, DispatchedAt: trajectoryBase,
			WorkerLastSignalAt: now.Add(-30 * time.Minute),
		},
	})
	if _, found := findAdvisory(got.Warnings, domain.AdvisoryGrowthWithoutProgress); found {
		t.Fatalf("a worker silent for 30 minutes produced a cost advisory; got %+v", got.Warnings)
	}
}

// Per-step attribution: the answer to "which step cost the money", which the
// role-grained ledger cannot give when one role is dispatched several times
// into the same session.
func TestSpendIsAttributedToTheStepThatIncurredIt(t *testing.T) {
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{events: []store.UsageTrajectoryEvent{
		call("step-work", domain.WorkflowRoleWorker, 0, 40_000, 10_000, 1_000),
		call("step-work", domain.WorkflowRoleWorker, 2, 60_000, 55_000, 1_000),
		call("step-fix", domain.WorkflowRoleFixWorker, 10, 80_000, 78_000, 500),
	}}, nil)

	got, _ := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{})
	if len(got.Steps) != 2 {
		t.Fatalf("steps = %d, want 2 (work and fix), got %+v", len(got.Steps), got.Steps)
	}
	work, fix := got.Steps[0], got.Steps[1]
	if work.WorkflowStepID != "step-work" || fix.WorkflowStepID != "step-fix" {
		t.Fatalf("steps out of provider order: %q then %q", work.WorkflowStepID, fix.WorkflowStepID)
	}
	if work.Tokens.InputTokens != 100_000 {
		t.Errorf("work input = %d, want 100000", work.Tokens.InputTokens)
	}
	if work.Trajectory.ProviderCalls != 2 || fix.Trajectory.ProviderCalls != 1 {
		t.Errorf("calls = %d/%d, want 2/1", work.Trajectory.ProviderCalls, fix.Trajectory.ProviderCalls)
	}
	if fix.Tokens.InputTokens != 80_000 {
		t.Errorf("fix input = %d, want 80000: a repair's spend must not leak into the base step", fix.Tokens.InputTokens)
	}
}

// A conversation that was REPLACED (a session switch, a fresh context pack)
// did not grow by a negative amount. Reporting one would invite somebody to
// read a reset as a saving.
func TestAShrunkenContextReportsZeroGrowthNotNegative(t *testing.T) {
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{events: []store.UsageTrajectoryEvent{
		call("step-work", domain.WorkflowRoleWorker, 0, 200_000, 190_000, 500),
		call("step-work", domain.WorkflowRoleWorker, 5, 20_000, 5_000, 500),
	}}, nil)

	got, _ := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{})
	if got.Trajectory.GrowthTokens != 0 {
		t.Errorf("growth = %d, want 0", got.Trajectory.GrowthTokens)
	}
	if got.Trajectory.PeakContextTokens != 200_000 {
		t.Errorf("peak = %d, want 200000: the peak is still a fact about the run", got.Trajectory.PeakContextTokens)
	}
}

// A run AO cannot place in time reports UNKNOWN, never zero -- the same
// discipline the token ledger applies to an unobservable role.
func TestARunWithNoPlaceableEventsIsUnrecordedNotFree(t *testing.T) {
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{unplaceable: 12}, nil)
	got, err := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{})
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if got.Recorded || got.Trajectory.Observable {
		t.Fatalf("a run with no placeable event reported itself observable: %+v", got)
	}
	if got.Trajectory.UnplaceableEvents != 12 {
		t.Errorf("unplaceable = %d, want 12: the count is what makes the silence legible", got.Trajectory.UnplaceableEvents)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("an unmeasured run produced advisories: %+v", got.Warnings)
	}
}

// Nothing in this file may stop anything. The advisory path holds no reference
// to a dispatcher and returns no decision -- this test pins the contract at the
// only place it could regress: the type itself.
func TestAdvisoriesCarryNoAuthority(t *testing.T) {
	reader := usagesvc.NewDynamicsReader(&fakeDynamicsStore{events: []store.UsageTrajectoryEvent{
		call("step-work", domain.WorkflowRoleWorker, 0, 500_000, 499_000, 5_000),
		call("step-work", domain.WorkflowRoleWorker, 300, 900_000, 899_000, 5_000),
	}}, nil)
	got, _ := reader.WorkflowRun(context.Background(), "wf-1", usagesvc.DynamicsOptions{
		Strategy: domain.ExecutionStrategyTask, Now: trajectoryBase.Add(5 * time.Hour),
		RunStartedAt: trajectoryBase,
	})
	if len(got.Warnings) == 0 {
		t.Fatal("a five-hour Task with 400k of growth produced no advisory at all")
	}
	for _, w := range got.Warnings {
		if w.Severity != domain.AdvisoryWarn && w.Severity != domain.AdvisoryInfo {
			t.Errorf("advisory %q carries severity %q, which is not one of the two advisory levels",
				w.Code, w.Severity)
		}
	}
}

func findAdvisory(list []domain.UsageAdvisory, code domain.UsageAdvisoryCode) (domain.UsageAdvisory, bool) {
	for _, w := range list {
		if w.Code == code {
			return w, true
		}
	}
	return domain.UsageAdvisory{}, false
}
