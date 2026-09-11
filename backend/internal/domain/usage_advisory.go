package domain

import "time"

// usage_advisory.go -- P5/P6: the ceilings that only ever SPEAK.
//
// P3-E gave a run two ceilings, tokens and cost, and one enforcement rule: a
// hard limit is consulted at a safe boundary, before AO starts new work, and
// never mid-response. That rule is not relaxed here and these ceilings do not
// participate in it at all.
//
// WHY A SECOND KIND OF CEILING. The measured failure (wf-1c2cb9bd: 193 calls,
// 36.2M tokens, $23.21, ~50 minutes, for a one-field defect) crossed no token
// or cost ceiling, because none was set -- and would not have been caught by
// one that was, because a token ceiling says "this is big" a long time after
// the interesting moment. The interesting facts were the SHAPE: the call count,
// the growth, and the wall clock. Those three are what this file adds, and it
// adds them the only way they can be added safely today:
//
//	THEY WARN. THEY NEVER STOP ANYTHING.
//
// That is not timidity, it is the same refusal P3-E already documents. Killing
// a run over a clock reintroduces exactly the ambiguous lifecycle -- an attempt
// interrupted between "the model is generating" and "AO recorded it" -- that
// the workflow package spends thousands of lines preventing. A duration budget
// has no durable recovery path today, so it gets no authority today. When one
// is built and PROVEN, promoting a threshold from advisory to enforcing is a
// one-line change at the evaluation site; inventing the authority first and the
// recovery later is how a budget eats a run nobody can restart.

// UsageAdvisoryCode is the closed vocabulary of things AO will say about a
// run's shape. Closed for the same reason every other reason vocabulary in AO
// is: an advisory AO cannot name is one no UI can translate and no test can
// pin.
type UsageAdvisoryCode string

// UsageAdvisoryCode values.
const (
	// AdvisoryDurationHigh -- the run has been going longer than its profile's
	// expected wall clock.
	AdvisoryDurationHigh UsageAdvisoryCode = "duration_above_profile"
	// AdvisoryProviderCallsHigh -- more model invocations than the profile
	// expects. This is the FIRST lever on cost: billable input is roughly the
	// sum of the context over the calls, so N is half the product.
	AdvisoryProviderCallsHigh UsageAdvisoryCode = "provider_calls_above_profile"
	// AdvisoryContextGrowthHigh -- the conversation has grown by more than the
	// profile expects. The second lever, and the one that compounds: every
	// later call re-reads everything the growth added.
	AdvisoryContextGrowthHigh UsageAdvisoryCode = "context_growth_above_profile"
	// AdvisoryCostHigh -- calculated spend past the profile's advisory figure.
	// Never emitted from a partial cost that is merely a lower bound past the
	// line... it IS emitted then, because a lower bound past the line is
	// certainly past it; what it is never emitted from is an unknown cost.
	AdvisoryCostHigh UsageAdvisoryCode = "cost_above_profile"
	// AdvisoryGrowthWithoutProgress is the conjunction that separates "working
	// hard" from "wedged": the context is growing, no durable progress fact
	// has appeared since dispatch, and the worker is alive. All three matter.
	// Growth alone is what an agentic loop DOES. No progress alone is normal
	// early in a step. A dead worker is the recovery path's business, not
	// this one's.
	AdvisoryGrowthWithoutProgress UsageAdvisoryCode = "growth_without_progress"
	// AdvisoryContextPerCallHigh -- the mean size of the conversation over the
	// run's calls is above what the profile expects. This is the SECOND lever
	// stated on its own: growth says the conversation got big, this says every
	// call paid for it. A run can cross this without crossing growth (a
	// conversation that started huge and barely moved) and vice versa.
	AdvisoryContextPerCallHigh UsageAdvisoryCode = "context_per_call_above_profile"
	// AdvisoryCoordinationTurnsDominant -- more of the classified calls were
	// coordination than work. Derived from turn classes (usage_turn_class.go),
	// so a run whose calls AO could not classify never earns it.
	AdvisoryCoordinationTurnsDominant UsageAdvisoryCode = "coordination_turns_dominant"
	// AdvisoryRepeatedWaitShape -- the run spent whole context re-reads asking
	// whether something it had already started had finished. The one turn
	// class that is pure overhead by construction; see TurnWait.
	AdvisoryRepeatedWaitShape UsageAdvisoryCode = "repeated_wait_check_shape"
	// AdvisoryCacheReadDominant is informational, not a fault: it names the
	// arithmetic when nearly all the bill is the model re-reading a
	// conversation that never shrinks. It exists so a reader is not left to
	// interpret a large number alone.
	AdvisoryCacheReadDominant UsageAdvisoryCode = "cache_read_dominant"
)

// UsageAdvisorySeverity separates "here is a fact about this run" from "this
// looks wrong".
type UsageAdvisorySeverity string

// UsageAdvisorySeverity values.
const (
	// AdvisoryInfo is a fact worth surfacing that is not a problem.
	AdvisoryInfo UsageAdvisorySeverity = "info"
	// AdvisoryWarn is AO saying this run's shape is worth a look. It changes
	// what AO SAYS and nothing else -- no state, no dispatch, no cancel.
	AdvisoryWarn UsageAdvisorySeverity = "warn"
)

// UsageAdvisory is one thing AO has to say about a run's cost or shape.
type UsageAdvisory struct {
	Code     UsageAdvisoryCode
	Severity UsageAdvisorySeverity
	// Observed and Threshold are the two numbers that produced this advisory,
	// in the advisory's own unit (tokens, calls, seconds, or cents). They
	// travel so a UI can render "193 of an expected 120" rather than a bare
	// sentence, and so a test can pin the boundary.
	Observed  int64
	Threshold int64
	// Profile names the budget profile the threshold came from, so a person
	// reading a warning can tell whether the ceiling is theirs or AO's
	// default.
	Profile ExecutionStrategy
}

// --- profiles --------------------------------------------------------------

// UsageBudgetProfile is the advisory shape of one execution strategy.
//
// These are EXPECTATIONS, not limits. A Task that takes 40 minutes is not
// broken; it is unusual, and AO says so. The numbers are anchored on the one
// run that was actually measured rather than chosen to look round: wf-1c2cb9bd
// was a Task, and it crossed all four.
type UsageBudgetProfile struct {
	Strategy ExecutionStrategy
	// WallClock is how long a run of this kind is expected to take.
	WallClock time.Duration
	// ProviderCalls is how many model invocations it is expected to need.
	ProviderCalls int64
	// ContextGrowthTokens is how much the conversation is expected to grow
	// between the first call and the last.
	ContextGrowthTokens int64
	// CostUSD is the advisory spend figure. Distinct from
	// UsageBudgetPolicy.WorkflowCostBudgetUSD, which is a HARD ceiling: this
	// one never stops anything, so it can be set at a figure a person
	// actually wants to hear about.
	CostUSD float64
	// ContextPerCallTokens is how big the conversation is expected to be on
	// the average call. Anchored the same way ProviderCalls is: the measured
	// run's own mean (36,082,816 / 193 = 186,957) rounded DOWN, so the figure
	// that should have been said out loud sits above the line rather than
	// under it.
	ContextPerCallTokens int64
}

// Profile defaults, per strategy.
//
// Task's numbers are the measured run rounded DOWN, not up: 193 calls and 50
// minutes for a single-field fix is the thing that should have been said out
// loud, so the threshold sits below it rather than above.
var (
	taskUsageProfile = UsageBudgetProfile{
		Strategy: ExecutionStrategyTask, WallClock: 30 * time.Minute,
		ProviderCalls: 120, ContextGrowthTokens: 100_000, CostUSD: 5,
		ContextPerCallTokens: 150_000,
	}
	autonomousUsageProfile = UsageBudgetProfile{
		Strategy: ExecutionStrategyAutonomous, WallClock: 2 * time.Hour,
		ProviderCalls: 600, ContextGrowthTokens: 250_000, CostUSD: 25,
		ContextPerCallTokens: 200_000,
	}
	masterUsageProfile = UsageBudgetProfile{
		Strategy: ExecutionStrategyMaster, WallClock: 6 * time.Hour,
		ProviderCalls: 2000, ContextGrowthTokens: 400_000, CostUSD: 75,
		ContextPerCallTokens: 250_000,
	}
)

// UsageBudgetProfileFor returns the advisory profile for a strategy.
//
// An unrecognised or unset strategy gets the AUTONOMOUS profile, not the Task
// one. A run whose kind AO cannot read must not be measured against the
// tightest expectations and warned about constantly; the middle profile is the
// honest default for "AO does not know what shape this is".
func UsageBudgetProfileFor(s ExecutionStrategy) UsageBudgetProfile {
	switch s {
	case ExecutionStrategyTask:
		return taskUsageProfile
	case ExecutionStrategyMaster:
		return masterUsageProfile
	default:
		return autonomousUsageProfile
	}
}

// WithOverrides applies a policy's explicit advisory thresholds over the
// profile's defaults. Zero means "keep the default" for the same reason zero
// means "no limit" on the hard ceilings: a policy snapshot written by a binary
// that had never heard of these fields must not be read as a run whose every
// expectation is zero and therefore permanently in warning.
func (p UsageBudgetProfile) WithOverrides(policy UsageBudgetPolicy) UsageBudgetProfile {
	if policy.WorkflowWallClockWarnSeconds > 0 {
		p.WallClock = time.Duration(policy.WorkflowWallClockWarnSeconds) * time.Second
	}
	if policy.WorkflowProviderCallWarn > 0 {
		p.ProviderCalls = policy.WorkflowProviderCallWarn
	}
	if policy.WorkflowContextGrowthWarnTokens > 0 {
		p.ContextGrowthTokens = policy.WorkflowContextGrowthWarnTokens
	}
	if policy.WorkflowCostWarnUSD > 0 {
		p.CostUSD = policy.WorkflowCostWarnUSD
	}
	return p
}

// CoordinationDominantPercent and RepeatedWaitShapeCalls expose the two
// unanchored thresholds so an evaluation site and a UI read the same number
// without either re-declaring it.
func CoordinationDominantPercent() int { return coordinationDominantPercent }
func RepeatedWaitShapeCalls() int64    { return repeatedWaitShapeCalls }

// coordinationDominantPercent is the share of CLASSIFIED calls that must be
// coordination before AO says so.
//
// Unlike every other threshold in this file, this one is NOT anchored on a
// measurement, and saying so matters: the measured run (wf-1c2cb9bd) was 99.0%
// work -- 183 commands and 8 edits against 2 calls that only talked -- so
// there was nothing to anchor it on. It is set where the sentence stops being
// arguable: past half, more of the run's calls were talking about the work
// than doing it. A future run that actually exhibits the shape is what will
// move this number, not a guess refined in advance.
const coordinationDominantPercent = 50

// repeatedWaitShapeCalls is how many wait-class calls a run may make before AO
// mentions it.
//
// Also unanchored, for the same reason and more sharply: the measured run made
// ZERO. It waited the way a runtime should be waited on -- a bounded loop
// inside one command, and a background task that woke the session when it
// finished -- so the polling shape everyone expects to find was not there to
// measure. Ten is the point past which a run has paid ten whole context
// re-reads for the word "yet".
const repeatedWaitShapeCalls = 10

// cacheReadDominantPercent is the share of billable input that must be cache
// reads before AO says so. It is not a fault threshold -- 98.8% was measured on
// a run with no loop, no retry and no defect -- it is the point past which the
// number stops being self-explanatory.
const cacheReadDominantPercent = 90

// CacheReadDominant reports whether cache reads account for
// cacheReadDominantPercent or more of billable input, and the share itself.
// Zero input reports false rather than dividing.
func (t UsageTokenTotals) CacheReadDominant() (int, bool) {
	if t.InputTokens <= 0 {
		return 0, false
	}
	share := int(t.CacheReadTokens * 100 / t.InputTokens)
	return share, share >= cacheReadDominantPercent
}
