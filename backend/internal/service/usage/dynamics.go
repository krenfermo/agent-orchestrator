package usage

import (
	"context"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// dynamics.go -- what a run's cost LOOKS like, next to what it totals.
//
// The ledger next door answers "what did this cost". It answered that
// correctly for wf-1c2cb9bd ($23.21, 36.2M tokens) and the answer explained
// nothing, because the number a person needs in order to act is not the total:
// it is 193 calls against a conversation that grew from 54k to 324k. This file
// derives that, and the advisories it earns, from the SAME rows the ledger
// reads. There is no second ingestion, no second table and no new hook.
//
// EVERYTHING HERE IS ADVISORY. Nothing in this file can stop a dispatch, park
// a run, or cancel an attempt -- see usage_advisory.go for why that line is
// where it is.

// dynamicsStore is the narrow read this file needs.
type dynamicsStore interface {
	ListRunContextTrajectoryEvents(ctx context.Context, runID string) ([]store.UsageTrajectoryEvent, error)
	CountRunUnplaceableUsageEvents(ctx context.Context, runID string) (int64, error)
}

// ProgressEvidence is what the CALLER knows about whether a run is getting
// anywhere. It is passed in rather than read here because durable progress
// lives in the workflow ledger (checkpoints, step transitions, git
// observations) and liveness lives on the session, and a usage reader has no
// business reaching into either.
//
// Known=false means nobody looked. That is treated as "AO cannot say", never
// as "there was no progress": the growth_without_progress advisory is a
// three-way conjunction, and an unknown term makes the conjunction unknown.
type ProgressEvidence struct {
	Known bool
	// DispatchedAt is when the currently executing step was handed to an
	// agent. Progress is measured from here, not from run creation: a run that
	// waited twenty minutes for capacity has not failed to progress.
	DispatchedAt time.Time
	// LastDurableProgressAt is the newest fact a person would recognise as the
	// run having got somewhere -- a checkpoint, a step transition, observed
	// git evidence. Zero when there is none since dispatch.
	LastDurableProgressAt time.Time
	// WorkerLastSignalAt is the running worker's Activity.Liveness(): the last
	// moment AO heard from it at all, which since migration 0168 is a
	// different clock from the last moment it CHANGED STATE. Zero when there
	// is no running worker or no reading.
	WorkerLastSignalAt time.Time
}

// workerAliveWindow is how recently a worker must have been heard from for the
// growth_without_progress conjunction to treat it as alive.
//
// Generous on purpose. A worker mid-tool-call can be silent for minutes
// legitimately (a test suite, a build, a long fetch), and the cost of being too
// tight here is to warn about a healthy run -- which is precisely the misreport
// this whole branch exists to end. A worker that is silent for longer than this
// is the recovery path's problem, and the recovery path is untouched.
const workerAliveWindow = 10 * time.Minute

// alive reports whether the worker has been heard from recently enough.
func (p ProgressEvidence) alive(now time.Time) bool {
	if !p.Known || p.WorkerLastSignalAt.IsZero() {
		return false
	}
	return now.Sub(p.WorkerLastSignalAt) <= workerAliveWindow
}

// madeProgress reports whether a durable progress fact exists at or after
// dispatch.
func (p ProgressEvidence) madeProgress() bool {
	if !p.Known || p.LastDurableProgressAt.IsZero() {
		return false
	}
	if p.DispatchedAt.IsZero() {
		return true
	}
	return !p.LastDurableProgressAt.Before(p.DispatchedAt)
}

// DynamicsOptions carries what the trajectory cannot know by itself.
type DynamicsOptions struct {
	// Strategy selects the advisory profile. An unset strategy gets the
	// autonomous (middle) profile -- see UsageBudgetProfileFor.
	Strategy domain.ExecutionStrategy
	// Budget is the run's FROZEN policy budget, for its advisory overrides.
	// The hard ceilings in it are not read here at all.
	Budget domain.UsageBudgetPolicy
	// RunStartedAt / RunEndedAt bound the wall clock. A nil RunEndedAt means
	// the run is still going and Now is the other end.
	RunStartedAt time.Time
	RunEndedAt   *time.Time
	Now          time.Time
	Progress     ProgressEvidence
}

// DynamicsReader derives a run's context dynamics from the usage ledger.
type DynamicsReader struct {
	store  dynamicsStore
	prices costPricer
}

// costPricer is the pricing capability this file needs. *pricing.Table
// satisfies it; taking the interface keeps the nil-pricing degradation the
// ledger already promises (tokens reported, cost unknown).
type costPricer interface {
	Cost(modelID string, tokens domain.UsageTokenTotals) domain.UsageCost
}

// NewDynamicsReader constructs the reader. A nil pricer yields unknown costs
// and every token figure still reported.
func NewDynamicsReader(s dynamicsStore, prices costPricer) *DynamicsReader {
	return &DynamicsReader{store: s, prices: prices}
}

// WorkflowRun builds one run's context dynamics.
func (r *DynamicsReader) WorkflowRun(ctx context.Context, runID string, opts DynamicsOptions) (domain.RunContextDynamics, error) {
	out := domain.RunContextDynamics{WorkflowRunID: runID}
	if r == nil || r.store == nil {
		return out, nil
	}
	events, err := r.store.ListRunContextTrajectoryEvents(ctx, runID)
	if err != nil {
		return out, err
	}
	unplaceable, err := r.store.CountRunUnplaceableUsageEvents(ctx, runID)
	if err != nil {
		// A count AO could not take must not delete a trajectory AO did take.
		// It costs the "this is a lower bound" flag and nothing else.
		unplaceable = 0
	}
	if len(events) == 0 {
		out.Trajectory.UnplaceableEvents = unplaceable
		return out, nil
	}
	out.Recorded = true
	out.Trajectory = trajectoryOf(events, unplaceable)
	out.Steps = r.stepLines(events)
	out.Warnings = r.advise(out, events, opts)
	return out, nil
}

// trajectoryOf folds a series of calls into its shape. The events must already
// be in provider order, which the query guarantees.
func trajectoryOf(events []store.UsageTrajectoryEvent, unplaceable int64) domain.ContextTrajectory {
	t := domain.ContextTrajectory{
		Observable:        true,
		ProviderCalls:     int64(len(events)),
		UnplaceableEvents: unplaceable,
	}
	first, last := events[0], events[len(events)-1]
	t.FirstContextTokens = first.Tokens.InputTokens
	t.LastContextTokens = last.Tokens.InputTokens
	firstAt, lastAt := first.ObservedAt, last.ObservedAt
	t.FirstObservedAt, t.LastObservedAt = &firstAt, &lastAt
	for _, e := range events {
		if e.Tokens.InputTokens > t.PeakContextTokens {
			t.PeakContextTokens = e.Tokens.InputTokens
		}
	}
	// Floored at zero deliberately. A conversation that ends smaller than it
	// started did not grow by a negative amount -- it was replaced (a session
	// switch, a fresh context pack), and reporting "-40k of growth" would
	// invite somebody to read a reset as a saving.
	if growth := t.LastContextTokens - t.FirstContextTokens; growth > 0 {
		t.GrowthTokens = growth
	}
	return t
}

// stepLines groups the same events by workflow step, so "which step cost the
// $23" finally has an answer. The grain is (step, role, cycle): a repair
// delivered into the worker's own session is a different cycle of a different
// step, and folding the two would reproduce the exact double-count P3-E's
// ledger was built to end.
func (r *DynamicsReader) stepLines(events []store.UsageTrajectoryEvent) []domain.StepUsageLine {
	type key struct {
		step  string
		role  domain.WorkflowRole
		cycle int64
	}
	order := []key{}
	byKey := map[key][]store.UsageTrajectoryEvent{}
	for _, e := range events {
		k := key{step: e.WorkflowStepID, role: e.Role, cycle: e.Cycle}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], e)
	}
	out := make([]domain.StepUsageLine, 0, len(order))
	for _, k := range order {
		group := byKey[k]
		line := domain.StepUsageLine{
			WorkflowStepID: k.step, Role: k.role, Cycle: k.cycle,
			Source:     domain.TokenSourceProvider,
			Trajectory: trajectoryOf(group, 0),
		}
		byModel := map[string]domain.UsageTokenTotals{}
		modelOrder := []string{}
		for _, e := range group {
			line.Tokens = line.Tokens.Add(e.Tokens)
			if _, seen := byModel[e.ModelID]; !seen {
				modelOrder = append(modelOrder, e.ModelID)
			}
			byModel[e.ModelID] = byModel[e.ModelID].Add(e.Tokens)
		}
		// Priced per model, never on the aggregate: two models in one step
		// have two rates, and one blended figure would be neither.
		for _, model := range modelOrder {
			line.Cost = line.Cost.Add(r.cost(model, byModel[model]))
		}
		out = append(out, line)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Trajectory.FirstObservedAt == nil || out[j].Trajectory.FirstObservedAt == nil {
			return false
		}
		return out[i].Trajectory.FirstObservedAt.Before(*out[j].Trajectory.FirstObservedAt)
	})
	return out
}

func (r *DynamicsReader) cost(modelID string, tokens domain.UsageTokenTotals) domain.UsageCost {
	if r == nil || r.prices == nil {
		return domain.UsageCost{Basis: domain.CostUnknown, UnpricedModels: []string{modelID}}
	}
	return r.prices.Cost(modelID, tokens)
}

// advise is the whole of P5's anomaly detection: every advisory AO can earn
// from facts it durably holds, and not one it cannot.
//
// WHAT IS DELIBERATELY ABSENT, and why. Three of the anomaly shapes asked for
// -- the same command repeated, excessive polling, the same test suite run
// again and again -- are NOT derivable from anything AO stores. The activity
// signal carries a tool NAME (ports.ActivitySignal.ToolName) and no arguments,
// and it is not persisted at all: lifecycle keeps in-flight tool ids in memory
// to correlate a dialog and drops them at the turn boundary. Detecting a
// repeated command would therefore require a new durable tool-call ledger
// carrying command text -- which is both a new telemetry (explicitly out of
// scope) and a store of user content AO has no reason to hold. They are named
// here rather than approximated, in the same spirit as the pricing package
// refusing to invent a cost.
func (r *DynamicsReader) advise(
	d domain.RunContextDynamics,
	events []store.UsageTrajectoryEvent,
	opts DynamicsOptions,
) []domain.UsageAdvisory {
	profile := domain.UsageBudgetProfileFor(opts.Strategy).WithOverrides(opts.Budget)
	var out []domain.UsageAdvisory
	add := func(code domain.UsageAdvisoryCode, sev domain.UsageAdvisorySeverity, observed, threshold int64) {
		out = append(out, domain.UsageAdvisory{
			Code: code, Severity: sev, Observed: observed,
			Threshold: threshold, Profile: profile.Strategy,
		})
	}

	if calls := d.Trajectory.ProviderCalls; calls > profile.ProviderCalls {
		add(domain.AdvisoryProviderCallsHigh, domain.AdvisoryWarn, calls, profile.ProviderCalls)
	}
	if growth := d.Trajectory.GrowthTokens; growth > profile.ContextGrowthTokens {
		add(domain.AdvisoryContextGrowthHigh, domain.AdvisoryWarn, growth, profile.ContextGrowthTokens)
	}
	if elapsed, ok := runElapsed(opts); ok && elapsed > profile.WallClock {
		add(domain.AdvisoryDurationHigh, domain.AdvisoryWarn,
			int64(elapsed/time.Second), int64(profile.WallClock/time.Second))
	}

	var tokens domain.UsageTokenTotals
	var cost domain.UsageCost
	for _, line := range d.Steps {
		tokens = tokens.Add(line.Tokens)
		cost = cost.Add(line.Cost)
	}
	// A cost AO does not know cannot cross a line. A PARTIAL cost may, because
	// a lower bound already past the threshold is certainly past it -- the same
	// asymmetry EvaluateWorkflowBudget applies, and it is safe here for a
	// stronger reason: this advisory stops nothing.
	if profile.CostUSD > 0 && cost.Known && cost.Amount > profile.CostUSD {
		add(domain.AdvisoryCostHigh, domain.AdvisoryWarn,
			int64(cost.Amount*100), int64(profile.CostUSD*100))
	}
	if share, dominant := tokens.CacheReadDominant(); dominant {
		add(domain.AdvisoryCacheReadDominant, domain.AdvisoryInfo, int64(share), cacheReadDominantSharePercent)
	}

	// The conjunction. All three terms, or nothing: growth alone is what an
	// agentic loop does, no-progress alone is normal early in a step, and a
	// worker that is not alive belongs to the recovery path and not to this
	// advisory. On the worked example this would NOT have fired -- git evidence
	// appeared five minutes in -- which is the property that makes it worth
	// having.
	if d.Trajectory.GrowthTokens > 0 && opts.Progress.Known &&
		!opts.Progress.madeProgress() && opts.Progress.alive(opts.Now) {
		add(domain.AdvisoryGrowthWithoutProgress, domain.AdvisoryWarn,
			d.Trajectory.GrowthTokens, 0)
	}
	return out
}

// cacheReadDominantSharePercent mirrors the domain threshold for the advisory's
// Threshold field, so a UI can render "99% of a 90% line" without importing a
// private constant.
const cacheReadDominantSharePercent = 90

// runElapsed is the run's wall clock, and whether it is knowable.
func runElapsed(opts DynamicsOptions) (time.Duration, bool) {
	if opts.RunStartedAt.IsZero() {
		return 0, false
	}
	end := opts.Now
	if opts.RunEndedAt != nil {
		end = *opts.RunEndedAt
	}
	if end.IsZero() || end.Before(opts.RunStartedAt) {
		return 0, false
	}
	return end.Sub(opts.RunStartedAt), true
}
