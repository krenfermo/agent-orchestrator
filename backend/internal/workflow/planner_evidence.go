package workflow

import (
	"encoding/json"
	"errors"
)

// Planner attempt classifications recorded in PlannerAttemptEvidence. They
// name what ENDED the attempt, which is the one thing a timeout postmortem
// cannot recover afterwards: "the planner timed out" reads identically
// whether the adapter's own bounded deadline expired, an unrelated caller's
// short-lived context died under it, or the subprocess itself failed.
const (
	PlannerAttemptOK              = "ok"
	PlannerAttemptTimeout         = "planner_timeout"
	PlannerAttemptParentCancelled = "parent_cancelled"
	PlannerAttemptCommandFailed   = "command_failed"
	PlannerAttemptMalformed       = "malformed_output"
	// PlannerAttemptResultInconsistent is F2's class: the subprocess
	// succeeded and its output parsed into a plan, but that plan could not be
	// reconciled with the invocation that produced it. Distinct from
	// malformed_output, which means nothing readable came back at all --
	// here something perfectly readable came back and is provably not the
	// answer the provider was billed for.
	PlannerAttemptResultInconsistent = "result_inconsistent"
)

// PlannerAttemptEvidence is what one planner invocation records about itself.
//
// The wf-80dc9f12 production timeout could not be diagnosed from AO's own
// durable state: the plan row said "context deadline exceeded" and nothing
// else, so whether the adapter had computed a 3-minute or a 12-minute budget,
// how large the payload it sent actually was, and whether the deadline that
// fired was even its own were all unanswerable without re-running the daemon.
// Every field here exists to answer one of those questions. It carries sizes
// and durations only -- never the prompt, the objective text, the documents,
// or any environment value.
type PlannerAttemptEvidence struct {
	// CalculatedTimeoutMS is the budget the adapter computed for this
	// attempt from its own base/max and the request's shape.
	CalculatedTimeoutMS int64 `json:"calculatedTimeoutMs"`
	// ParentDeadlineMS is how much time the CALLER's context still had when
	// the attempt started, and is set only when that context had a deadline
	// at all (HasParentDeadline). A value below CalculatedTimeoutMS means the
	// planner's own budget was never the binding constraint.
	ParentDeadlineMS  int64 `json:"parentDeadlineMs,omitempty"`
	HasParentDeadline bool  `json:"hasParentDeadline"`
	// EffectiveTimeoutMS is min(CalculatedTimeoutMS, ParentDeadlineMS) -- the
	// deadline that actually governed the attempt.
	EffectiveTimeoutMS int64 `json:"effectiveTimeoutMs"`

	ObjectiveBytes int `json:"objectiveBytes"`
	ContextBytes   int `json:"contextBytes"`
	PayloadBytes   int `json:"payloadBytes"`
	DocumentCount  int `json:"documentCount"`
	MaxSteps       int `json:"maxSteps"`

	DurationMS     int64  `json:"durationMs"`
	Classification string `json:"classification"`

	// Usage is what the provider reported this planner call actually spent, and
	// UsageModel the model it names. Both are nil/empty when the CLI reported
	// nothing -- which is UNKNOWN, not zero: the planner is a real `claude
	// --print` subprocess and it always spends something.
	//
	// It rides on the attempt evidence rather than on PlannerResponse because a
	// FAILED attempt also spends tokens, and evidence is the one thing the
	// adapter returns on both paths.
	Usage      *PlannerTokenUsage `json:"usage,omitempty"`
	UsageModel string             `json:"usageModel,omitempty"`

	// F2 provenance: enough to explain, from durable state alone, WHICH half
	// of the provider envelope AO turned into a plan and how big each half
	// was -- the questions the placeholder-plan incident could not answer
	// afterwards. Sizes and a reason string only: never the plan body, never
	// the provider's prose, never a raw terminal dump.
	//
	// ResultSource is "structured_output" or "result".
	ResultSource string `json:"resultSource,omitempty"`
	// StructuredOutputBytes / ResultBytes are the two halves as received, and
	// PlanBytes is the one AO actually parsed.
	StructuredOutputBytes int `json:"structuredOutputBytes,omitempty"`
	ResultBytes           int `json:"resultBytes,omitempty"`
	PlanBytes             int `json:"planBytes,omitempty"`
	// ConsistencySignal names why the result was refused, empty when it was
	// accepted. It is the operator-facing half of ErrPlannerResultInconsistent.
	ConsistencySignal string `json:"consistencySignal,omitempty"`
}

// PlannerTokenUsage is one planner invocation's provider-reported token vector.
//
// InputTokens is the TOTAL input with the cached halves counted inside it, which
// is the same convention the transcript parser uses -- so a planner call and a
// worker turn can be summed without double counting the cache.
type PlannerTokenUsage struct {
	InputTokens         int64 `json:"inputTokens"`
	UncachedInputTokens int64 `json:"uncachedInputTokens"`
	CacheReadTokens     int64 `json:"cacheReadTokens"`
	CacheWriteTokens    int64 `json:"cacheWriteTokens"`
	OutputTokens        int64 `json:"outputTokens"`
}

// LogArgs renders the evidence as slog key/value pairs.
func (e PlannerAttemptEvidence) LogArgs() []any {
	args := []any{
		"calculatedTimeoutMs", e.CalculatedTimeoutMS,
		"effectiveTimeoutMs", e.EffectiveTimeoutMS,
		"hasParentDeadline", e.HasParentDeadline,
		"objectiveBytes", e.ObjectiveBytes,
		"contextBytes", e.ContextBytes,
		"payloadBytes", e.PayloadBytes,
		"documentCount", e.DocumentCount,
		"maxSteps", e.MaxSteps,
		"durationMs", e.DurationMS,
		"classification", e.Classification,
	}
	if e.HasParentDeadline {
		args = append(args, "parentDeadlineMs", e.ParentDeadlineMS)
	}
	if e.ResultSource != "" {
		args = append(args, "resultSource", e.ResultSource,
			"structuredOutputBytes", e.StructuredOutputBytes,
			"resultBytes", e.ResultBytes, "planBytes", e.PlanBytes)
	}
	if e.Usage != nil {
		args = append(args, "reportedOutputTokens", e.Usage.OutputTokens)
	}
	if e.ConsistencySignal != "" {
		args = append(args, "consistencySignal", e.ConsistencySignal)
	}
	return args
}

// JSON renders the evidence for durable storage. It never fails in practice
// (every field is a scalar); an unexpected failure degrades to "{}" rather
// than losing the stop record the evidence rides along with.
func (e PlannerAttemptEvidence) JSON() string {
	b, err := json.Marshal(struct {
		Planner PlannerAttemptEvidence `json:"plannerAttempt"`
	}{Planner: e})
	if err != nil {
		return "{}"
	}
	return string(b)
}

// PlannerAttemptError carries a failed attempt's evidence alongside the
// failure itself, so the coordinator can persist it without the planner port
// growing a second return value that every decorator would have to thread
// through. Unwrap keeps every existing errors.Is classification
// (ports.ErrPlannerTimeout, ports.ErrPlannerOutputMalformed, and the provider
// text classifyProviderFailure reads) working unchanged.
type PlannerAttemptError struct {
	Evidence PlannerAttemptEvidence
	Err      error
}

func (e *PlannerAttemptError) Error() string { return e.Err.Error() }
func (e *PlannerAttemptError) Unwrap() error { return e.Err }

// PlannerEvidenceFrom extracts attempt evidence from an error chain.
func PlannerEvidenceFrom(err error) (PlannerAttemptEvidence, bool) {
	var attempt *PlannerAttemptError
	if errors.As(err, &attempt) {
		return attempt.Evidence, true
	}
	return PlannerAttemptEvidence{}, false
}
