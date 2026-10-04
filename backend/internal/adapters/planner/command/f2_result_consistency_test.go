package command

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// f2_result_consistency_test.go — the adapter half of F2's test matrix.
//
// Every case here is a shape the adapter accepted before the fix: it parsed,
// so it was a plan, so it was returned and (under Automatic approval) executed.

// f2Plan builds a real-shaped plan of n steps.
func f2Plan(n int) workflowcore.MasterPlan {
	plan := workflowcore.MasterPlan{
		Version:   "v1",
		Objective: "Extend the greetings module with a farewell entry point and shared helper.",
		Summary:   "Document the current API, extract the shared normalization helper, then add farewell().",
	}
	for i := 0; i < n; i++ {
		plan.Steps = append(plan.Steps, workflowcore.PlannedStep{
			ID:                 "step-" + string(rune('1'+i)),
			Title:              "Extract the shared name-normalization helper",
			Description:        "Move the repeated trim/collapse/title-case sequence out of greeting.js and the feature modules into one shared helper under src/helpers/.",
			Dependencies:       []string{},
			AcceptanceCriteria: []string{"A single new helper under src/helpers/ implements the sequence and every call site uses it."},
			Verify:             workflowcore.VerificationPlan{Commands: []workflowcore.VerificationCommandCheck{}, Files: []workflowcore.VerificationFileCheck{}},
		})
	}
	return plan
}

// f2PlaceholderPlan is the exact shape that caused F2.
func f2PlaceholderPlan() workflowcore.MasterPlan {
	return workflowcore.MasterPlan{
		Version: "v1", Objective: "Extend the greetings module", Summary: "test",
		Steps: []workflowcore.PlannedStep{{
			ID: "s1", Title: "t", Description: "d",
			Dependencies: []string{}, AcceptanceCriteria: []string{"a"},
			Verify: workflowcore.VerificationPlan{
				Commands: []workflowcore.VerificationCommandCheck{{Command: "npm", Args: []string{"test"}, TimeoutSeconds: 60, RetrySafe: true}},
				Files:    []workflowcore.VerificationFileCheck{},
			},
		}},
	}
}

// f2Envelope renders a print-mode envelope. A nil result means "structured
// output only"; otherwise result carries its own (possibly different) plan.
func f2Envelope(t *testing.T, structured *workflowcore.MasterPlan, result *workflowcore.MasterPlan, outputTokens int64, isError bool) []byte {
	t.Helper()
	env := map[string]any{"is_error": isError, "subtype": "success"}
	if isError {
		env["subtype"] = "error_during_execution"
	}
	if structured != nil {
		raw, err := json.Marshal(structured)
		if err != nil {
			t.Fatal(err)
		}
		env["structured_output"] = json.RawMessage(raw)
	}
	if result != nil {
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		env["result"] = string(raw)
	}
	if outputTokens > 0 {
		env["usage"] = map[string]any{"input_tokens": 1000, "output_tokens": outputTokens}
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func f2Request() workflowcore.PlannerRequest {
	return workflowcore.PlannerRequest{
		Objective: "Extend the greetings module with a farewell entry point and a shared helper.",
		MaxSteps:  12,
	}
}

// f2Generate runs the adapter against a scripted sequence of envelopes, one per
// attempt, and reports how many attempts were consumed.
func f2Generate(t *testing.T, envelopes ...[]byte) (workflowcore.PlannerResponse, int64, error) {
	t.Helper()
	var calls atomic.Int64
	p := Planner{Binary: "claude", Model: "sonnet", runCommand: func(context.Context, string, []string, string, []string) ([]byte, error) {
		i := calls.Add(1) - 1
		if int(i) >= len(envelopes) {
			i = int64(len(envelopes) - 1)
		}
		return envelopes[i], nil
	}}
	resp, err := p.Generate(context.Background(), f2Request())
	return resp, calls.Load(), err
}

// A + E: a real-shaped plan is accepted, and so is a concise one whose reported
// usage is proportionate to it.
func TestF2_RealPlanAccepted(t *testing.T) {
	plan := f2Plan(3)
	resp, calls, err := f2Generate(t, f2Envelope(t, &plan, &plan, 9000, false))
	if err != nil {
		t.Fatalf("a real three-step plan must be accepted, got %v", err)
	}
	if len(resp.Plan.Steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(resp.Plan.Steps))
	}
	if calls != 1 {
		t.Errorf("attempts = %d, want 1 (no retry for a good result)", calls)
	}
	if resp.Evidence.ResultSource != "structured_output" {
		t.Errorf("resultSource = %q, want structured_output", resp.Evidence.ResultSource)
	}
	if resp.Evidence.ConsistencySignal != "" {
		t.Errorf("a good result must record no consistency signal, got %q", resp.Evidence.ConsistencySignal)
	}
}

// B: a legitimately concise one-step plan with modest reported usage is
// accepted. The detector must never punish a small answer to a small question.
func TestF2_ConciseOneStepPlanAccepted(t *testing.T) {
	plan := f2Plan(1)
	if _, _, err := f2Generate(t, f2Envelope(t, &plan, nil, 1200, false)); err != nil {
		t.Fatalf("a concise one-step plan must be accepted, got %v", err)
	}
	// And with no usage reported at all: unknown spend must never be read as
	// evidence of loss.
	if _, _, err := f2Generate(t, f2Envelope(t, &plan, nil, 0, false)); err != nil {
		t.Fatalf("a plan whose usage the CLI did not report must be accepted, got %v", err)
	}
}

// D: the incident itself — a placeholder returned alongside thousands of
// reported output tokens is refused as inconsistent, not accepted as a plan.
func TestF2_LargeUsageWithTinyResultIsInconsistent(t *testing.T) {
	placeholder := f2PlaceholderPlan()
	env := f2Envelope(t, &placeholder, nil, 17534, false)
	_, calls, err := f2Generate(t, env, env)
	if !errors.Is(err, ports.ErrPlannerResultInconsistent) {
		t.Fatalf("want ErrPlannerResultInconsistent, got %v", err)
	}
	if calls < 2 {
		t.Errorf("attempts = %d, want the adapter to have retried an inconsistent result", calls)
	}
	evidence, ok := workflowcore.PlannerEvidenceFrom(err)
	if !ok {
		t.Fatal("expected attempt evidence on the error")
	}
	if evidence.Classification != workflowcore.PlannerAttemptResultInconsistent {
		t.Errorf("classification = %q, want %q", evidence.Classification, workflowcore.PlannerAttemptResultInconsistent)
	}
	if !strings.Contains(evidence.ConsistencySignal, "17534") {
		t.Errorf("the signal must name the reported spend it was measured against, got %q", evidence.ConsistencySignal)
	}
	if evidence.Usage == nil || evidence.Usage.OutputTokens != 17534 {
		t.Errorf("a refused attempt still spent its tokens and must still meter them, got %+v", evidence.Usage)
	}
}

// F: the two halves of the envelope disagree. AO has no principled way to pick
// a winner, so it picks neither.
func TestF2_EnvelopeHalvesDisagreeIsInconsistent(t *testing.T) {
	realPlan, placeholder := f2Plan(3), f2PlaceholderPlan()
	env := f2Envelope(t, &placeholder, &realPlan, 9000, false)
	_, _, err := f2Generate(t, env, env)
	if !errors.Is(err, ports.ErrPlannerResultInconsistent) {
		t.Fatalf("want ErrPlannerResultInconsistent for disagreeing halves, got %v", err)
	}
	evidence, _ := workflowcore.PlannerEvidenceFrom(err)
	if !strings.Contains(evidence.ConsistencySignal, "disagree") {
		t.Errorf("signal should name the disagreement, got %q", evidence.ConsistencySignal)
	}
}

// The CLI's own failure verdict was never read before F2: an error envelope
// carrying a stub would have been mined for a plan and executed.
func TestF2_ProviderErrorEnvelopeIsRefused(t *testing.T) {
	plan := f2Plan(3)
	env := f2Envelope(t, &plan, nil, 9000, true)
	_, _, err := f2Generate(t, env, env)
	if !errors.Is(err, ports.ErrPlannerResultInconsistent) {
		t.Fatalf("an is_error envelope must never yield a plan, got %v", err)
	}
}

// G: the adapter's own bounded retry recovers a real plan after one bad
// attempt, and returns it once.
func TestF2_RetryRecoversTheRealPlan(t *testing.T) {
	placeholder, realPlan := f2PlaceholderPlan(), f2Plan(3)
	resp, calls, err := f2Generate(t,
		f2Envelope(t, &placeholder, nil, 17534, false),
		f2Envelope(t, &realPlan, &realPlan, 9000, false),
	)
	if err != nil {
		t.Fatalf("the retry should have recovered a real plan, got %v", err)
	}
	if len(resp.Plan.Steps) != 3 || resp.Plan.Summary == "test" {
		t.Fatalf("returned the placeholder instead of the recovered plan: %+v", resp.Plan)
	}
	if calls != 2 {
		t.Errorf("attempts = %d, want exactly 2", calls)
	}
}

// The ratio signal must stay a detector of gross loss, never a size rule. A
// plan that is small but proportionate to a small reported spend is accepted.
func TestF2_RatioSignalOnlyFiresOnGrossLoss(t *testing.T) {
	small := f2Plan(1)
	raw, _ := json.Marshal(small)
	// Reported spend a little above what the plan itself accounts for: healthy.
	proportionate := int64(len(raw)/bytesPerOutputToken) * 8
	if proportionate < minReportedOutputTokensForRatio {
		proportionate = minReportedOutputTokensForRatio + 1
	}
	if reason := resultConsistency(plannerEnvelope{}, len(raw), proportionate); reason != "" {
		t.Fatalf("a proportionate plan must not be flagged, got %q", reason)
	}
	// Below the floor the signal does not speak at all, whatever the size.
	if reason := resultConsistency(plannerEnvelope{}, 40, minReportedOutputTokensForRatio-1); reason != "" {
		t.Fatalf("the ratio signal must stay silent below its reporting floor, got %q", reason)
	}
}
