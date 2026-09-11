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
