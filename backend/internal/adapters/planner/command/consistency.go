package command

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// consistency.go — F2's data-loss detector.
//
// THE INCIDENT. A planner invocation was billed 17,534 output tokens over 155
// seconds and AO persisted a plan whose entire content was `summary:"test"` and
// one step titled "t" / described "d" / accepted on "a". The plan was schema
// valid, so validation passed it; approval was Automatic, so it was approved;
// and a real worker was dispatched against a task that meant nothing. Nine
// direct replays of the byte-identical invocation all returned a full, correct
// three-step plan, so the trigger lives on the provider side of the boundary
// and AO cannot prevent it.
//
// What AO CAN do is refuse to execute a result it cannot reconcile with the
// invocation that produced it. That is what this file decides, from the
// envelope the adapter already parses — no new provider call, no second source
// of truth.
//
// THE SIGNALS, and why each one is here rather than a single rule:
//
//  1. The CLI's own failure verdict (is_error). Cheap, exact, and previously
//     unread.
//  2. Envelope disagreement. print-mode reports the same answer twice, in
//     `structured_output` and in `result`. When both are present they must
//     agree; when they do not, ONE of them is the lost answer and AO has no
//     principled way to pick — so it picks neither.
//  3. Gross output-token implausibility. A provider billed for thousands of
//     output tokens that hands back a plan accounting for a rounding error of
//     them has lost something.
//
// (3) is deliberately the LAST and weakest of the three, and deliberately not
// a universal "outputTokens > X implies plan > Y bytes" rule. Output tokens
// include reasoning the plan never contains, providers differ in how much of
// that they report, and a genuinely concise plan for a genuinely small
// objective must stay acceptable. So it fires only on a gross mismatch, only
// when the provider actually reported usage, and it names itself in the
// verdict so an operator reading the stop knows which signal spoke. The real
// defence against a placeholder is the semantic floor on the plan itself
// (workflow.PlanResultPlausibility); this is the net under it.
const (
	// minReportedOutputTokensForRatio is the floor below which the ratio
	// signal does not speak at all. A short answer to a small objective is
	// normal and must never be second-guessed on size alone.
	minReportedOutputTokensForRatio = 2000
	// grossLossRatioPercent is the share of reported output tokens the
	// returned plan must plausibly account for. Measured against the nine
	// clean replays of the incident invocation, the plan accounted for
	// 18-25% of reported output tokens; the incident's placeholder accounted
	// for 0.35%. Two percent sits an order of magnitude below every healthy
	// observation and an order of magnitude above the failure, which is the
	// margin that keeps this a detector of loss rather than a quality bar.
	grossLossRatioPercent = 2
	// bytesPerOutputToken converts the plan AO actually received into the
	// same unit the provider reports. Four bytes per token is the usual
	// English-text approximation and errs toward OVERSTATING the plan, which
	// is the safe direction for a check that refuses work.
	bytesPerOutputToken = 4
)

// resultConsistency reports why a parsed plan cannot be reconciled with the
// envelope that carried it, or "" when nothing is wrong.
//
// planBytes is the size of the JSON AO actually turned into a plan — not the
// size of the whole envelope, which is dominated by usage and session metadata
// the model never wrote.
func resultConsistency(env plannerEnvelope, planBytes int, reportedOutputTokens int64) string {
	if env.IsError {
		subtype := env.Subtype
		if subtype == "" {
			subtype = "unknown"
		}
		return fmt.Sprintf("provider reported the invocation itself failed (is_error=true, subtype=%q)", subtype)
	}
	if reason := envelopeDisagreement(env); reason != "" {
		return reason
	}
	if reportedOutputTokens >= minReportedOutputTokensForRatio {
		accountedTokens := int64(planBytes / bytesPerOutputToken)
		// accounted/reported < grossLossRatioPercent/100, without floats.
		if accountedTokens*100 < reportedOutputTokens*grossLossRatioPercent {
			return fmt.Sprintf(
				"provider was billed %d output tokens but the plan AO received accounts for about %d of them (%d bytes); the real result was lost",
				reportedOutputTokens, accountedTokens, planBytes)
		}
	}
	return ""
}

// envelopeDisagreement compares print-mode's two reports of the same answer.
//
// They are normally byte-identical once parsed (verified across every clean
// observation of this invocation). When both are present and they differ, one
// of them is the answer the provider actually produced and the other is not —
// and nothing in the envelope says which. Preferring `structured_output`
// unconditionally, which is what the adapter did before F2, is exactly how a
// real plan gets replaced by a placeholder with no trace.
//
// A missing half is not a disagreement: plenty of healthy envelopes carry only
// one, and refusing those would break every invocation this check is meant to
// protect.
func envelopeDisagreement(env plannerEnvelope) string {
	if len(env.StructuredOutput) == 0 || env.Result == "" {
		return ""
	}
	var structured, result any
	if err := json.Unmarshal(env.StructuredOutput, &structured); err != nil {
		return ""
	}
	if err := json.Unmarshal([]byte(env.Result), &result); err != nil {
		// `result` is not JSON at all — it is prose alongside a structured
		// answer, which is normal for some CLI versions. Nothing to compare.
		return ""
	}
	structuredCanonical, err := json.Marshal(structured)
	if err != nil {
		return ""
	}
	resultCanonical, err := json.Marshal(result)
	if err != nil {
		return ""
	}
	if bytes.Equal(structuredCanonical, resultCanonical) {
		return ""
	}
	return fmt.Sprintf(
		"the provider envelope's two reports of the same answer disagree (structured_output is %d bytes, result is %d bytes); AO cannot tell which one is the real plan",
		len(structuredCanonical), len(resultCanonical))
}
