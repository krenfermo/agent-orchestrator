package controllers

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// workflow_usage_dynamics_dto.go -- the SHAPE block of the usage response.
//
// It sits beside `tokens` (what a provider reported spending) and `context`
// (what AO assembled and sent), and it is a third quantity again: how the
// conversation MOVED. None of the three may be added to either of the others,
// and this one is the only one that answers "why is a one-field fix costing
// $23" -- because that answer is 193 calls against a context that grew from
// 54k to 324k, and no total contains it.

// ContextTrajectoryResponse is a conversation's shape over the calls AO can
// place in time.
type ContextTrajectoryResponse struct {
	// Observable is false when AO placed no call in time. Every other field is
	// then meaningless: render them as unknown, never as zero.
	Observable bool `json:"observable"`
	// ProviderCalls is the N in "billable input is roughly the sum of the
	// context over the calls". It is the first lever on cost.
	ProviderCalls int64 `json:"providerCalls"`
	// FirstContextTokens / lastContextTokens are the conversation's size on the
	// first and last placeable call; growthTokens is their difference, floored
	// at zero (a conversation that was REPLACED did not grow by a negative
	// amount). peakContextTokens is the largest single call.
	FirstContextTokens int64 `json:"firstContextTokens"`
	LastContextTokens  int64 `json:"lastContextTokens"`
	PeakContextTokens  int64 `json:"peakContextTokens"`
	GrowthTokens       int64 `json:"growthTokens"`
	// GrowthPerCall is the mean growth between consecutive calls. Null when
	// there were fewer than two calls, because there was then no "between".
	GrowthPerCall *int64 `json:"growthPerCall"`
	// FirstObservedAt / lastObservedAt bound the series. Empty when not
	// observable.
	FirstObservedAt string `json:"firstObservedAt,omitempty"`
	LastObservedAt  string `json:"lastObservedAt,omitempty"`
	// ElapsedSeconds is the wall time the series covers. Null when unknown.
	ElapsedSeconds *int64 `json:"elapsedSeconds"`
	// UnplaceableEvents is how many of this scope's usage events carried no
	// observed time and were left out. Non-zero makes every figure above a
	// LOWER BOUND, which the UI must say rather than imply.
	UnplaceableEvents int64 `json:"unplaceableEvents"`
	// CumulativeInputTokens is the sum of the context over the calls -- the
	// quantity both levers move. meanContextPerCall is it divided by the call
	// count, null when there were no calls.
	CumulativeInputTokens int64  `json:"cumulativeInputTokens"`
	MeanContextPerCall    *int64 `json:"meanContextPerCall"`
	// Turns is what the calls DID. Absent when nothing was classified.
	Turns *TurnMixResponse `json:"turns,omitempty"`
}

// TurnMixResponse is the call-count breakdown by turn shape.
//
// COUNTS OF CALLS, NEVER TOKENS. Two calls of the same class can differ
// tenfold in cost; this block answers "what was the model doing" and the
// trajectory beside it answers "what did that cost". A UI that multiplies them
// is making an estimate and must label it as one.
type TurnMixResponse struct {
	// Classified and unclassified partition the scope's calls. A non-zero
	// unclassified makes every share below a share OF THE CLASSIFIED CALLS,
	// which the UI must say: those calls predate the turn_class column, came
	// from a Codex rollout, or could not be decoded.
	Classified   int64 `json:"classified"`
	Unclassified int64 `json:"unclassified"`
	// Counts is keyed by the closed turn-class vocabulary. A class with no
	// calls is ABSENT rather than present-and-zero.
	Counts map[string]int64 `json:"counts,omitempty"`
	// Work and coordination split the classified calls. coordinationPercent is
	// null when nothing was classified -- rendering 0% for "AO could not look"
	// is the exact misreport the null prevents.
	WorkCalls           int64 `json:"workCalls"`
	CoordinationCalls   int64 `json:"coordinationCalls"`
	CoordinationPercent *int  `json:"coordinationPercent"`
}

// StepUsageResponse is one workflow step's spend and shape -- the answer to
// "which step cost the money", which the role-grained ledger cannot give
// because one role can be dispatched into the same session several times.
type StepUsageResponse struct {
	WorkflowStepID string                    `json:"workflowStepId"`
	Role           string                    `json:"role"`
	Cycle          int64                     `json:"cycle"`
	Tokens         UsageTokenTotalsResponse  `json:"tokens"`
	Cost           UsageCostResponse         `json:"cost"`
	Source         string                    `json:"source"`
	Trajectory     ContextTrajectoryResponse `json:"trajectory"`
}

// UsageAdvisoryResponse is one thing AO has to say about a run's shape.
//
// ADVISORY ONLY. Nothing in this list stops a dispatch, parks a run or cancels
// an attempt; it changes what AO SAYS. A `warn` means "worth a look", not
// "something is broken", and `info` is a fact offered so a large number is not
// left to be interpreted alone.
type UsageAdvisoryResponse struct {
	Code     string `json:"code" enum:"duration_above_profile,provider_calls_above_profile,context_growth_above_profile,cost_above_profile,growth_without_progress,cache_read_dominant,context_per_call_above_profile,coordination_turns_dominant,repeated_wait_check_shape"`
	Severity string `json:"severity" enum:"info,warn"`
	// Observed and threshold are the two numbers that produced this advisory,
	// in the advisory's own unit: tokens for growth, calls for calls, SECONDS
	// for duration, CENTS for cost, PERCENT for cache dominance.
	Observed  int64 `json:"observed"`
	Threshold int64 `json:"threshold"`
	// Profile names the execution strategy whose expectations produced the
	// threshold, so a reader can tell AO's default from their own override.
	Profile string `json:"profile,omitempty"`
}

// WorkflowUsageDynamicsResponse is the whole shape block.
type WorkflowUsageDynamicsResponse struct {
	// Recorded is false when the run has no placeable usage at all. The UI must
	// then say "no context data recorded", never "0 tokens".
	Recorded   bool                      `json:"recorded"`
	Trajectory ContextTrajectoryResponse `json:"trajectory"`
	Steps      []StepUsageResponse       `json:"steps,omitempty"`
	Warnings   []UsageAdvisoryResponse   `json:"warnings,omitempty"`
}

// WorkerLivenessResponse is the running agent's two clocks.
//
// THE POINT OF THIS BLOCK IS THAT THERE ARE TWO. `lastSignalAt` is the last
// moment AO was heard from at all; `lastTransitionAt` is when the agent entered
// the state it is in now. An agent that has been `active` for twenty minutes
// has an ancient transition and a signal from seconds ago, and reporting the
// first as "last activity" is the misreport this block exists to end. A UI must
// never render "stuck" or "silent" from `lastTransitionAt`.
type WorkerLivenessResponse struct {
	// Observed is false for a terminal run, a run with no running step, or a
	// session AO could not read. Both clocks are then absent -- never "0s ago".
	Observed  bool   `json:"observed"`
	SessionID string `json:"sessionId,omitempty"`
	StepID    string `json:"stepId,omitempty"`
	StepKind  string `json:"stepKind,omitempty"`
	State     string `json:"state,omitempty"`
	// LastSignalAt is the liveness clock. This is the one to render as "last
	// activity".
	LastSignalAt string `json:"lastSignalAt,omitempty"`
	// LastTransitionAt is the state-change clock, kept beside it because
	// "working on the same thing since 17:10" is a real fact -- it is only a
	// misreport when it is labelled liveness.
	LastTransitionAt string `json:"lastTransitionAt,omitempty"`
	// SilentForSeconds is now minus lastSignalAt, computed server-side so
	// every surface agrees. Null when not observed.
	SilentForSeconds *int64 `json:"silentForSeconds"`
}

func contextTrajectoryResponse(t domain.ContextTrajectory) ContextTrajectoryResponse {
	out := ContextTrajectoryResponse{
		Observable:         t.Observable,
		ProviderCalls:      t.ProviderCalls,
		FirstContextTokens: t.FirstContextTokens,
		LastContextTokens:  t.LastContextTokens,
		PeakContextTokens:  t.PeakContextTokens,
		GrowthTokens:       t.GrowthTokens,
		UnplaceableEvents:  t.UnplaceableEvents,
	}
	if per, ok := t.GrowthPerCall(); ok {
		out.GrowthPerCall = &per
	}
	if elapsed, ok := t.Elapsed(); ok {
		secs := int64(elapsed.Seconds())
		out.ElapsedSeconds = &secs
	}
	if t.FirstObservedAt != nil {
		out.FirstObservedAt = t.FirstObservedAt.Format(rfc3339Milli)
	}
	if t.LastObservedAt != nil {
		out.LastObservedAt = t.LastObservedAt.Format(rfc3339Milli)
	}
	out.CumulativeInputTokens = t.CumulativeInputTokens
	if mean, ok := t.MeanContextPerCall(); ok {
		out.MeanContextPerCall = &mean
	}
	out.Turns = turnMixResponse(t.Turns)
	return out
}

// turnMixResponse projects the mix, or nil when AO classified nothing at all.
//
// Nil rather than an empty object on purpose: a surface that finds the key can
// trust that at least one call in the scope was looked at, and a surface that
// does not find it must say "not recorded" rather than draw an empty chart.
func turnMixResponse(m domain.TurnMix) *TurnMixResponse {
	if m.Classified <= 0 && m.Unclassified <= 0 {
		return nil
	}
	out := &TurnMixResponse{
		Classified:        m.Classified,
		Unclassified:      m.Unclassified,
		WorkCalls:         m.WorkCalls(),
		CoordinationCalls: m.CoordinationCalls(),
	}
	if len(m.Counts) > 0 {
		out.Counts = make(map[string]int64, len(m.Counts))
		for class, n := range m.Counts {
			out.Counts[string(class)] = n
		}
	}
	if share, ok := m.CoordinationShare(); ok {
		out.CoordinationPercent = &share
	}
	return out
}

func stepUsageResponses(lines []domain.StepUsageLine) []StepUsageResponse {
	if len(lines) == 0 {
		return nil
	}
	out := make([]StepUsageResponse, 0, len(lines))
	for _, l := range lines {
		out = append(out, StepUsageResponse{
			WorkflowStepID: l.WorkflowStepID, Role: string(l.Role), Cycle: l.Cycle,
			Tokens: usageTokenTotalsResponse(l.Tokens), Cost: usageCostResponse(l.Cost),
			Source: string(l.Source), Trajectory: contextTrajectoryResponse(l.Trajectory),
		})
	}
	return out
}

func usageAdvisoryResponses(warnings []domain.UsageAdvisory) []UsageAdvisoryResponse {
	if len(warnings) == 0 {
		return nil
	}
	out := make([]UsageAdvisoryResponse, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, UsageAdvisoryResponse{
			Code: string(w.Code), Severity: string(w.Severity),
			Observed: w.Observed, Threshold: w.Threshold, Profile: string(w.Profile),
		})
	}
	return out
}

func workflowUsageDynamicsResponse(d domain.RunContextDynamics) WorkflowUsageDynamicsResponse {
	return WorkflowUsageDynamicsResponse{
		Recorded:   d.Recorded,
		Trajectory: contextTrajectoryResponse(d.Trajectory),
		Steps:      stepUsageResponses(d.Steps),
		Warnings:   usageAdvisoryResponses(d.Warnings),
	}
}

// workerLivenessResponse projects the running agent's clocks onto the wire.
// Returns nil when nothing was observed, so the field is ABSENT rather than
// present-and-empty: a surface that finds the key must be able to trust both
// clocks in it.
func workerLivenessResponse(w workflowcore.WorkerLiveness, now time.Time) *WorkerLivenessResponse {
	if !w.Observed {
		return nil
	}
	out := &WorkerLivenessResponse{
		Observed: true, SessionID: w.SessionID, StepID: w.StepID,
		StepKind: string(w.StepKind), State: string(w.State),
	}
	if !w.LastSignalAt.IsZero() {
		out.LastSignalAt = w.LastSignalAt.Format(rfc3339Milli)
	}
	if !w.LastTransitionAt.IsZero() {
		out.LastTransitionAt = w.LastTransitionAt.Format(rfc3339Milli)
	}
	if silent, ok := w.SilentFor(now); ok {
		secs := int64(silent.Seconds())
		out.SilentForSeconds = &secs
	}
	return out
}
