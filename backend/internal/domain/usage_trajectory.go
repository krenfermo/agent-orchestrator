package domain

import "time"

// usage_trajectory.go -- P5/P6: the shape of a run's context, not its area.
//
// Every other read model in usage_ledger.go answers a question a SUM can
// answer: what did this cost, which role spent it, which model. A run that
// spent 36.2M tokens on a one-field defect is not explained by any of them,
// because the explanation is not in the total. It is in the series:
//
//	billable input  ~=  sum over i of context(i)
//
// with 193 calls and context growing from 54,402 to 323,736 tokens. No sum over
// that series recovers either end of it, so the cost is unexplainable from the
// existing ledger alone -- which is exactly what an operator watching
// wf-1c2cb9bd hit. This file adds the two facts the sum destroys: how many
// calls there were, and how big the conversation was at each end.
//
// NOTHING HERE IS A SECOND TELEMETRY. Every field below is folded from
// model_usage_events rows AO already stores, read through the same attribution
// view the cost ledger reads. No prompt, no message body and no secret is
// stored or re-read to compute any of it.

// ContextTrajectory is how one conversation's context moved over the calls AO
// can place in time.
//
// "Context" here is the provider's own input_tokens for a call: the V1 parser
// folds cache reads and writes into that figure, so it is the whole
// conversation the provider re-read, not a component of it.
type ContextTrajectory struct {
	// Observable is false when AO placed no event in time for this scope --
	// no provider reported, or every event arrived without an observed_at.
	// Every other field is then meaningless and must be rendered as unknown,
	// never as zero.
	Observable bool
	// ProviderCalls is how many placeable calls this scope made. It is the N
	// in "cost ~= sum of context(i)", and the first lever on that cost.
	ProviderCalls int64
	// FirstContextTokens and LastContextTokens are the conversation's size on
	// the first and last placeable call. GrowthTokens is their difference,
	// floored at zero: a conversation that shrank (a session switch, a fresh
	// pack) did not grow by a negative amount, it started over.
	FirstContextTokens int64
	LastContextTokens  int64
	PeakContextTokens  int64
	GrowthTokens       int64
	// FirstObservedAt / LastObservedAt bound the series. Nil when Observable
	// is false.
	FirstObservedAt *time.Time
	LastObservedAt  *time.Time
	// UnplaceableEvents is how many of this scope's events carried no
	// observed_at and were therefore left out. Non-zero makes every figure
	// above a lower bound, which the UI must say rather than imply.
	UnplaceableEvents int64
	// CumulativeInputTokens is the sum of the context over the calls: the
	// quantity the identity at the top of this file is about, and the one
	// number that moves when EITHER lever moves. The ledger's own input total
	// is the same arithmetic over the same rows -- it is repeated here so a
	// trajectory can be read on its own without a second fetch, and so a
	// SEGMENT of a run (one step, one repair cycle) has the figure at all,
	// which no run-level total can give it.
	CumulativeInputTokens int64
	// Turns is what those calls DID. Counts of calls, never tokens -- see
	// TurnMix. A scope whose events all predate migration 0169 has an empty
	// mix with every call in Unclassified, which is a different statement from
	// "no coordination happened".
	Turns TurnMix
}

// MeanContextPerCall is the average size of the conversation over the calls,
// and whether it is knowable. This is the second lever stated as one number:
// halving it halves the bill at an unchanged call count.
func (t ContextTrajectory) MeanContextPerCall() (int64, bool) {
	if !t.Observable || t.ProviderCalls <= 0 {
		return 0, false
	}
	return t.CumulativeInputTokens / t.ProviderCalls, true
}

// GrowthPerCall is the mean growth between consecutive calls, and whether it
// is knowable. Fewer than two calls means there was no "between".
func (t ContextTrajectory) GrowthPerCall() (int64, bool) {
	if !t.Observable || t.ProviderCalls < 2 {
		return 0, false
	}
	return t.GrowthTokens / (t.ProviderCalls - 1), true
}

// Elapsed is the wall time the series covers, and whether it is knowable.
func (t ContextTrajectory) Elapsed() (time.Duration, bool) {
	if !t.Observable || t.FirstObservedAt == nil || t.LastObservedAt == nil {
		return 0, false
	}
	d := t.LastObservedAt.Sub(*t.FirstObservedAt)
	if d < 0 {
		return 0, false
	}
	return d, true
}

// StepUsageLine is one workflow step's spend and context trajectory.
//
// It exists because "which step cost the $23" had no answer: the ledger's
// finest grain was the ROLE, and a role that is dispatched three times (a
// worker, then two fix cycles into the same session) reported one line. The
// attribution window has carried workflow_step_id since P3-E; this is the read
// that finally groups by it.
type StepUsageLine struct {
	WorkflowStepID string
	// Role and Cycle are the window's, carried so a step line can say which
	// repair cycle it belongs to without a second lookup.
	Role       WorkflowRole
	Cycle      int64
	Tokens     UsageTokenTotals
	Cost       UsageCost
	Source     TokenMeasurementSource
	Trajectory ContextTrajectory
}

// RunContextDynamics is the whole-run answer: the trajectory, the per-step
// breakdown, and the advisories that combination earns.
type RunContextDynamics struct {
	WorkflowRunID string
	Trajectory    ContextTrajectory
	Steps         []StepUsageLine
	// Warnings is what AO advises about this run's shape. Advisory only: see
	// UsageAdvisory.
	Warnings []UsageAdvisory
	// Recorded is false when the run has no placeable usage at all. The UI
	// must then say "no context data recorded", never "0 tokens".
	Recorded bool
}

// SessionContextReading is how big ONE session's conversation is, at the
// moment a lifecycle decision has to be made about it.
//
// It exists because SessionLifecycleRequest.ContextPressure was declared with
// a note saying no production call site could set it: "AO has no per-session
// live token/context signal yet". That was true when it was written and stopped
// being true when the usage pipeline started placing calls in time. This is
// that signal, in the smallest shape a decision needs.
//
// Observable=false is the honest "AO has not seen this session spend anything
// yet" -- a session that has just been spawned, a harness whose transcript has
// not been discovered, a provider that reported nothing. It must never be read
// as "the conversation is small": a decision taken from an unobserved reading
// is a decision taken on no information, and the lifecycle policy has a
// separate reason code (unknown_usage) for saying exactly that.
type SessionContextReading struct {
	Observable bool
	// ProviderCalls is how many of this session's calls AO could place in
	// time. Zero is what makes Observable false.
	ProviderCalls int64
	// LastContextTokens is the conversation's size on the most recent placeable
	// call: the figure the NEXT call will pay, which is what a decision about
	// the next call needs. PeakContextTokens is the largest it has been, kept
	// beside it because a conversation that was already compacted once reads
	// small at the end and large at the peak, and the difference is the fact.
	LastContextTokens int64
	PeakContextTokens int64
}

// UnderPressure reports whether the conversation is at or above a threshold,
// and whether the question is answerable at all.
//
// Two returns rather than one for the reason this whole file keeps repeating:
// an unobserved session is not a session under no pressure. A caller that
// collapses these into a single bool re-creates the exact "unknown read as
// zero" bug the trajectory type is built to prevent.
func (r SessionContextReading) UnderPressure(thresholdTokens int64) (bool, bool) {
	if !r.Observable || thresholdTokens <= 0 {
		return false, false
	}
	return r.LastContextTokens >= thresholdTokens, true
}
