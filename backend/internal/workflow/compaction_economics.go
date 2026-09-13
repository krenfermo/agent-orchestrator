package workflow

import (
	stdctx "context"
	"encoding/json"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// compaction_economics.go -- P7.2B2. The shadow economic gate's one call site.
//
// WHAT THIS FILE DOES NOT DO, AND THE ORDER MATTERS BECAUSE EVERYTHING ELSE
// HERE IS A CONSEQUENCE OF IT.
//
// It does not decide anything. It does not read SessionLifecyclePolicy's mind,
// change its verdict, gate maybeCompactBeforeFix, withhold a /compact directive,
// touch the fix prompt, the fact pack, the retry loop, the fix budget, the
// waiting_input path, a cancellation or a workflow result. The function below
// returns NOTHING -- not a bool anyone branches on, not an error anyone checks.
// Its entire observable effect is one row in workflow_checkpoints.
//
// That is not a temporary state on the way to enforcement. It is the property
// P7.2B2 exists to establish: a cohort of verdicts computed against real work,
// large enough to calibrate the model, collected at ZERO risk, because a defect
// in the arithmetic cannot reach execution. The proof is mechanical -- the call
// site assigns the result to nothing, and
// TestTheShadowVerdictChangesNothingAboutDelivery asserts byte-identical
// delivery with every verdict including COMPACT.
//
// WHERE IT RUNS. Immediately BEFORE maybeCompactBeforeFix, on the lifecycle
// decision that has just been computed, inside applyFixLifecycleDecision. That
// is the real pre-decision boundary: the run, the step, the cycle, the session,
// the lifecycle reasons, the frozen policy and the prompt AO is about to send
// are all in hand, and nothing the compaction causes has happened yet. Running
// it AFTER the compaction request would make every observation it reads a
// candidate for contamination by the very act being judged; running it here
// makes the look-ahead guard structural instead of a promise.
//
// COST. One indexed read, already in the codebase, bounded by the run --
// ListRunWorkerCallObservations, at most one row per worker call. No transcript
// is opened, no corpus is scanned, no 900MB table is walked. An estimator that
// would need a heavy scan returns UNKNOWN instead, which is the honest answer
// and also the cheap one.
//
// FAILURE. Every failure path returns silently. A read error, a nil port, a
// store that refuses the checkpoint: the fix cycle is delivered unchanged and
// nothing is marked needing attention. Shadow telemetry that could break a
// workflow would be worse than no shadow telemetry.

// compactionEconomicsDurablePhase is the checkpoint phase carrying a shadow
// verdict.
//
// durable_phase is free TEXT with no CHECK constraint and 111 distinct phases
// are already in use, so this needs NO MIGRATION -- the same way P7 added
// session_compaction_requested. retry_state is TEXT with a json_valid check and
// the payload is JSON, and payload_version already exists for the version
// string.
//
// parser_state_json was explicitly NOT chosen. It is the right home for facts
// derived from one transcript artifact, which is what P7.1's observations are. A
// shadow verdict is the opposite kind of thing: a decision about a run, a step
// and a cycle, and putting run-scoped data in an artifact-scoped column would
// make it unreadable by the only code that has the run in hand.
const compactionEconomicsDurablePhase = "compaction_economic_decision"

// compactionEconomicsStore is the narrow read the gate needs, obtained by type
// assertion on the coordinator's Store exactly as usageBudgetStore is.
type compactionEconomicsStore interface {
	ListRunWorkerCallObservations(ctx stdctx.Context, runID string) ([]domain.SessionCallObservation, error)
}

// UsageRateCard reads a model's RATES, beside UsagePricer which prices a vector.
// *pricing.Table satisfies it. Optional: without it every verdict is UNKNOWN
// with reason pricing_unknown, which is the correct answer for a gate that
// cannot see a rate card.
//
// It is a separate port from UsagePricer on purpose. Cost answers "what did this
// cost"; the economic model needs the ratios between rates, and recovering those
// by pricing unit vectors and dividing is a trick that reads like one.
type UsageRateCard interface {
	RateView(modelID string) (domain.ModelRateView, bool)
}

// SessionCompactionObservations supplies what a session has already observed
// about its own compactions. Satisfied by
// *observe/usage.CompactionReader. Optional.
//
// This is the ONLY source for the context-after estimate, and it is a per-session
// read of records the parser already wrote. A session that has never compacted
// returns nothing and the verdict is UNKNOWN -- not a default, not a constant.
type SessionCompactionObservations interface {
	SessionCompactionAccounting(ctx stdctx.Context, sessionID domain.SessionID) (domain.CompactionAccounting, error)
}

// CompactionSummaryPriors supplies the cross-session prior for how many tokens a
// harness GENERATES producing a summary.
//
// WIRED IN P7.2B2.1. It was left unwired in P7.2B2 on the belief that reading it
// meant walking the corpus on every fix boundary. It does not: a prior about
// compaction is only ever about sessions that COMPACTED, and filtering to those
// in SQL collapses the candidate list to a handful whatever the corpus size. The
// per-session arithmetic then reuses the fold that already existed.
//
// S is structurally cross-session and always will be: it is only measurable from
// a harness's end-of-session rollup, so a running session has no rollup and can
// never estimate its own summary cost. Satisfied by
// *observe/usage.CompactionReader. Optional: without it the verdict is UNKNOWN
// with reason summary_cost_unknown, which is also what a cohort of fewer than
// three independent sessions produces.
type CompactionSummaryPriors interface {
	CompactionSummaryObservations(ctx stdctx.Context, harness, modelID string) ([]domain.CompactionSummarySessionObservation, error)
}

// evaluateShadowCompactionEconomics computes what the economic model would have
// recommended, records it, and returns nothing.
//
// The signature is the contract: no return value means no caller can branch on
// it, so no future edit can turn this into enforcement without changing this
// line and being seen to do it.
func (c *Coordinator) evaluateShadowCompactionEconomics(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	fixStep domain.WorkflowStep,
	sessionID domain.SessionID,
	decision domain.SessionLifecycleDecision,
	cycleNumber int,
	prompt string,
) {
	// Only a COMPACT decision is evaluated. A REUSE or NEW decision has no
	// compaction to be economic about, and recording a verdict for one would
	// pad the cohort with rows that never corresponded to a choice.
	if decision.Action != domain.LifecycleCompact || sessionID == "" {
		return
	}
	if c.compactionEconomics == nil {
		return
	}
	if c.shadowCompactionVerdictAlreadyRecorded(ctx, run.ID, fixStep.ID, cycleNumber) {
		return
	}
	record := c.computeShadowCompactionVerdict(ctx, run, fixStep, sessionID, decision, cycleNumber, prompt)
	if err := c.recordShadowCompactionVerdict(ctx, run, sessionID, record); err != nil && c.log != nil {
		// Debug, not Info: a shadow record AO could not write is an absent data
		// point and nothing else. It must not read like a workflow problem.
		c.log.Debug("workflow: the shadow economic verdict could not be recorded",
			"run", run.ID, "step", fixStep.ID, "cycle", cycleNumber, "error", err)
	}
}

// computeShadowCompactionVerdict assembles the frozen input and evaluates it.
//
// Every read failure and every missing port lands on an UNKNOWN verdict with a
// reason naming what was missing, because a verdict AO could not compute is a
// fact about the cohort worth recording -- "the gate could price nothing for six
// weeks" is the finding that says enforcement is not ready.
func (c *Coordinator) computeShadowCompactionVerdict(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	fixStep domain.WorkflowStep,
	sessionID domain.SessionID,
	decision domain.SessionLifecycleDecision,
	cycleNumber int,
	prompt string,
) domain.CompactionEconomicsRecord {
	policy := policyForRun(run)
	profile := domain.UsageBudgetProfileFor(policy.Strategy.Effective).
		WithOverrides(policy.EffectiveUsageBudgetPolicy())

	in := domain.CompactionEconomicsInput{
		WorkflowRunID:          run.ID,
		StepID:                 fixStep.ID,
		Cycle:                  cycleNumber,
		SessionID:              string(sessionID),
		MaxFixCycles:           policy.MaxFixCycles,
		Strategy:               string(policy.Strategy.Effective),
		ContextThresholdTokens: profile.ContextPerCallTokens,
		// COPIED, not aliased. decision.Reasons belongs to the caller's own
		// SessionLifecycleDecision, which is persisted moments later; handing the
		// gate the same backing array would be the one way a shadow evaluation
		// could reach out and alter the decision it is shadowing. Nothing in the
		// gate appends to it today, and this copy is what makes that a property
		// rather than a promise.
		LifecycleReasons:     append([]domain.SessionLifecycleReason(nil), decision.Reasons...),
		SafetyFactor:         domain.DefaultCompactionSafetyFactor,
		MinReductionFraction: domain.DefaultCompactionMinReductionFraction,
		// Delta is the prompt AO is about to send, at the 4-bytes-per-token
		// estimator this repo already uses elsewhere. It is measured BEFORE the
		// fact pack is prepended, so it is an UNDERSTATEMENT of what will
		// actually be sent -- and a smaller delta means a smaller reduction
		// means a verdict further from COMPACT, which is the safe direction.
		PromptTokens:      int64(len(prompt) / 4),
		PromptTokensKnown: true,
	}

	calls, err := c.compactionEconomics.ListRunWorkerCallObservations(ctx, run.ID)
	if err != nil {
		return domain.EvaluateCompactionEconomics(in)
	}
	// THE LOOK-AHEAD FILTER. Only calls the decision could already have seen:
	// strictly earlier cycles, and strictly before now. The current cycle has
	// made no calls at the moment this runs, so the cycle filter is belt to the
	// timestamp's braces -- and both are here because a guard that exists in one
	// place is a guard one refactor can remove.
	decisionAt := c.clock()
	series := make([]domain.SessionCallObservation, 0, len(calls))
	for _, call := range calls {
		if call.Cycle >= int64(cycleNumber) {
			continue
		}
		if !call.ObservedAt.Before(decisionAt) {
			continue
		}
		series = append(series, call)
	}
	if len(series) == 0 {
		return domain.EvaluateCompactionEconomics(in)
	}

	// B, P and the model, from the worker series itself. The series is ordered
	// oldest-first by the query, so the ends are the ends.
	first, last := series[0], series[len(series)-1]
	// B is the conversation the /compact turn will READ: the last call's context
	// plus what that call generated. The generated half is included because the
	// summarizer reads the whole conversation including the reply it is about to
	// summarize.
	in.ContextBeforeTokens = last.Tokens.InputTokens + last.Tokens.OutputTokens
	// P is the stable cached prefix -- system prompt, tool schemas, project
	// instructions -- which survives compaction and is therefore still a cache
	// READ on the first call after it rather than a rewrite. The session's first
	// placeable call's cache read is the closest thing AO observes to it.
	in.StablePrefixTokens = first.Tokens.CacheReadTokens
	in.ModelID = last.ModelID
	in.UsageObservable = true

	var creation domain.CacheCreationSplit
	var vector domain.UsageTokenTotals
	models := map[string]struct{}{}
	for _, call := range series {
		creation = creation.Add(call.Tokens.CacheCreation)
		vector = vector.Add(call.Tokens)
		if call.ModelID != "" {
			models[call.ModelID] = struct{}{}
		}
	}
	in.ObservedCacheCreation = creation
	// A series that ran on more than one model is not something to average. The
	// rates differ, the cache lifetimes may differ, and B and P would then be
	// measured on one model and priced on another. Reported as inconsistent
	// rather than resolved by picking one.
	if len(models) > 1 {
		in.ObservedCacheCreation = domain.CacheCreationSplit{UnknownTTLTokens: creation.Total()}
		in.ObservedCostTTLUnknownTokens = creation.Total()
	}

	// The disclosures on AO's OWN cost for this session. If AO priced these
	// tokens on an assumed cache lifetime, every comparison below would inherit
	// the assumption, so the gate refuses rather than discloses.
	if c.usagePricer != nil && in.ModelID != "" {
		cost := c.usagePricer.Cost(in.ModelID, vector)
		in.ObservedCostTTLAssumedTokens = cost.TTLAssumedTokens
		if cost.TTLUnknownTokens > in.ObservedCostTTLUnknownTokens {
			in.ObservedCostTTLUnknownTokens = cost.TTLUnknownTokens
		}
	}

	// Harness and Provider are left EMPTY rather than fetched. They are record
	// metadata and not inputs to the arithmetic -- nothing below reads them --
	// and the only place they live durably is a second table. A read taken on
	// every fix boundary to label a row is exactly the cost the performance rule
	// says to refuse; the model id, which the arithmetic does depend on, comes
	// from the series AO already read.
	if c.usageRateCard != nil && in.ModelID != "" {
		in.Rates, in.RatesKnown = c.usageRateCard.RateView(in.ModelID)
	}

	in.RemainingCalls = domain.EstimateRemainingCalls(domain.CompactionRemainingCallsEstimatorInput{
		CurrentCycle: cycleNumber,
		CycleCalls:   cycleCallCounts(series),
	})
	in.ContextAfter = c.estimateShadowContextAfter(ctx, sessionID, in.StablePrefixTokens, in.PromptTokens, decisionAt)
	in.SummaryTokens = c.estimateShadowSummaryTokens(ctx, sessionID, in.Harness, in.ModelID, decisionAt)

	return domain.EvaluateCompactionEconomics(in)
}

// cycleCallCounts folds a call series into one count per cycle.
func cycleCallCounts(series []domain.SessionCallObservation) []domain.CompactionCycleCalls {
	counts := map[int64]int64{}
	for _, call := range series {
		counts[call.Cycle]++
	}
	out := make([]domain.CompactionCycleCalls, 0, len(counts))
	for cycle, calls := range counts {
		out = append(out, domain.CompactionCycleCalls{Cycle: int(cycle), Calls: calls})
	}
	return out
}

// estimateShadowContextAfter reads this session's own earlier compactions and
// asks the pure estimator.
//
// A missing port, a read error, or a session that has never compacted all
// produce an unknown estimate rather than a guess. There is no fallback constant
// here and there must not be one: the alternative to evidence is UNKNOWN, and a
// cohort of verdicts computed from an invented A would be worse than an empty
// cohort because it would look like data.
//
// decisionAt is passed through to the estimator, which filters the boundaries
// again on it. That duplication is deliberate -- see the estimator's own comment.
func (c *Coordinator) estimateShadowContextAfter(
	ctx stdctx.Context, sessionID domain.SessionID, prefix, promptTokens int64, decisionAt time.Time,
) domain.EstimatedTokens {
	if c.compactionObservations == nil {
		return domain.EstimatedTokens{Basis: domain.CompactionBasisNone}
	}
	accounting, err := c.compactionObservations.SessionCompactionAccounting(ctx, sessionID)
	if err != nil {
		return domain.EstimatedTokens{Basis: domain.CompactionBasisNone}
	}
	return domain.EstimateContextAfter(domain.CompactionContextAfterEstimatorInput{
		StablePrefixTokens: prefix,
		PromptTokens:       promptTokens,
		Boundaries:         accounting.Boundaries,
		DecisionAt:         decisionAt,
	})
}

// estimateShadowSummaryTokens asks the cross-session prior. See
// CompactionSummaryPriors for why it is unwired in P7.2B2 and what that costs.
func (c *Coordinator) estimateShadowSummaryTokens(
	ctx stdctx.Context, sessionID domain.SessionID, harness, modelID string, decisionAt time.Time,
) domain.EstimatedTokens {
	if c.compactionSummaryPriors == nil {
		return domain.EstimatedTokens{Basis: domain.CompactionBasisNone}
	}
	observations, err := c.compactionSummaryPriors.CompactionSummaryObservations(ctx, harness, modelID)
	if err != nil {
		return domain.EstimatedTokens{Basis: domain.CompactionBasisNone}
	}
	return domain.EstimateSummaryTokens(domain.CompactionSummaryEstimatorInput{
		ExcludeSessionID: string(sessionID),
		Observations:     observations,
		// The decision instant, so the estimator can drop an observation that is
		// younger than the verdict it would inform. Passed even though the read
		// happens now and therefore cannot see the future: the filter is what
		// makes that a property of the code rather than of the call order.
		DecisionAt: decisionAt,
	})
}

// shadowCompactionVerdictAlreadyRecorded reports whether this exact cycle
// already has a verdict at this gate version.
//
// A read failure returns TRUE -- recording nothing rather than risking a second
// row for one decision. The cohort's value depends on one record per decision:
// a duplicated verdict would double-count in every statistic computed over it,
// and a missing one is visibly missing.
func (c *Coordinator) shadowCompactionVerdictAlreadyRecorded(ctx stdctx.Context, runID, stepID string, cycleNumber int) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return true
	}
	want := shadowCompactionVerdictMarker(stepID, cycleNumber)
	for _, cp := range cps {
		if cp.DurablePhase == compactionEconomicsDurablePhase && strings.Contains(cp.RetryState, want) {
			return true
		}
	}
	return false
}

// shadowCompactionVerdictMarker identifies one cycle's verdict inside the
// checkpoint payload, at one gate version.
//
// Derived from the step, the cycle and the gate version -- never from a clock --
// so recovery reconstructs the same marker rather than writing a second row, and
// a later gate version is deliberately a DIFFERENT marker: re-evaluating the
// same decision under new arithmetic is a new record, not a duplicate.
//
// The TRAILING QUOTE is load-bearing for the same reason the compaction marker's
// trailing comma is: without a terminator, cycle 1's marker is a prefix of
// cycle 10's payload, and a run allowed ten repair cycles would find cycle 10's
// record while looking for cycle 1's.
func shadowCompactionVerdictMarker(stepID string, cycleNumber int) string {
	return `"step":"` + stepID + `","cycle":` + itoaInt(cycleNumber) +
		`,"gate":"` + domain.CompactionEconomicsGateVersion + `"`
}

// recordShadowCompactionVerdict writes the verdict at RUN level with the step in
// the payload.
//
// Not associated with the fix step, and that convention is copied exactly from
// persistSessionLifecycleDecision and recordSessionCompactionRequest rather than
// re-derived: several read paths mean "the latest checkpoint for this exact step"
// with a specific durable phase, and a second checkpoint for the same step at the
// same simulated clock tick would tie on CreatedAt and could shadow the one they
// need.
func (c *Coordinator) recordShadowCompactionVerdict(
	ctx stdctx.Context, run domain.WorkflowRun, sessionID domain.SessionID, record domain.CompactionEconomicsRecord,
) error {
	payload, err := json.Marshal(shadowCompactionVerdictPayload{
		Step:    record.StepID,
		Cycle:   record.Cycle,
		Gate:    record.GateVersion,
		Verdict: record,
	})
	if err != nil {
		return err
	}
	sid := string(sessionID)
	var sessionIDPtr *string
	if sid != "" {
		sessionIDPtr = &sid
	}
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-" + c.newID(),
		WorkflowRunID: run.ID,
		ProjectID:     run.ProjectID,
		SessionID:     sessionIDPtr,
		RetryState:    string(payload),
		DurablePhase:  compactionEconomicsDurablePhase,
		// The PAYLOAD's version, not the gate's: a field can be added to the
		// record without the arithmetic changing, and a reader has to be able to
		// tell those apart.
		PayloadVersion: domain.CompactionEconomicsPayloadVersion,
		CreatedAt:      c.clock(),
	})
	return err
}

// shadowCompactionVerdictPayload is the checkpoint's JSON shape.
//
// Step, Cycle and Gate are hoisted to the front in exactly that order because
// shadowCompactionVerdictMarker matches on them as a substring, and Go's
// encoding/json emits struct fields in declaration order. A reordering of these
// three breaks idempotence, which is why they are not simply read off the nested
// record.
type shadowCompactionVerdictPayload struct {
	Step    string                           `json:"step"`
	Cycle   int                              `json:"cycle"`
	Gate    string                           `json:"gate"`
	Verdict domain.CompactionEconomicsRecord `json:"verdict"`
}

// CompactionEconomicsDurablePhase is the checkpoint phase a reader filters on to
// find shadow verdicts. Exported because reading the cohort back is the entire
// point of writing it, and a reader should not have to know the string.
const CompactionEconomicsDurablePhase = compactionEconomicsDurablePhase

// DecodeCompactionEconomicsRecord unmarshals one shadow verdict out of a
// checkpoint's RetryState.
//
// ok=false on an empty or unparseable payload, mirroring every other
// decode helper in this package: a record a reader cannot decode is one absent
// data point, never a reason to fail the read of the rest of the cohort.
func DecodeCompactionEconomicsRecord(retryState string) (domain.CompactionEconomicsRecord, bool) {
	var p shadowCompactionVerdictPayload
	if strings.TrimSpace(retryState) == "" {
		return domain.CompactionEconomicsRecord{}, false
	}
	if err := json.Unmarshal([]byte(retryState), &p); err != nil {
		return domain.CompactionEconomicsRecord{}, false
	}
	return p.Verdict, true
}

// DecodeShadowCompactionVerdictForTest is the ...ForTest alias the external
// workflow_test package uses, matching this package's existing convention.
func DecodeShadowCompactionVerdictForTest(retryState string) (domain.CompactionEconomicsRecord, bool) {
	return DecodeCompactionEconomicsRecord(retryState)
}

// ShadowCompactionVerdictPhaseForTest exposes the durable phase name.
func ShadowCompactionVerdictPhaseForTest() string { return CompactionEconomicsDurablePhase }
