package ports

import "errors"

// ErrPlannerTimeout is returned by a workflow.Planner adapter when the
// planner call was cancelled by its own deadline (not the caller's ctx).
// Checkpoint 8P-E.10: master_coordinator classified this by substring-
// matching err.Error() for "timeout" before this sentinel existed, which
// could misfire if an objective's own text happened to contain the word.
// errors.Is against this sentinel is exact regardless of message text.
var ErrPlannerTimeout = errors.New("planner: timed out waiting for a response")

// ErrPlannerOutputMalformed is returned by a workflow.Planner adapter when
// the planner produced output that could not be turned into a plan envelope
// or a plan, after any adapter-internal tolerant extraction/retry has been
// exhausted. Distinguished from ErrPlannerTimeout (which classifies as
// planner_timeout) so master_coordinator can classify it as
// planner_parse_failed without relying on error text.
var ErrPlannerOutputMalformed = errors.New("planner: output could not be parsed into a plan")

// ErrPlannerResultInconsistent is returned by a workflow.Planner adapter when
// the subprocess succeeded and its output PARSED cleanly into a plan, but that
// plan cannot be reconciled with the invocation that produced it — the two
// halves of the provider envelope disagree, or the provider was billed for far
// more work than the plan AO received could account for.
//
// This is the F2 failure and it is materially different from
// ErrPlannerOutputMalformed. Malformed means AO got nothing it could read.
// Inconsistent means AO got something perfectly readable that is provably not
// the answer the provider produced: a schema-shaped placeholder accepted while
// 17.5k output tokens of real plan were billed and lost. Parsing cannot detect
// it, because the placeholder is valid; only comparing the result against the
// invocation can.
//
// It classifies as retryable for exactly the reason a timeout does: it is a
// fact about one attempt, not a verdict about the objective, and re-running the
// planner is the thing most likely to recover the real plan.
var ErrPlannerResultInconsistent = errors.New("planner: result cannot be reconciled with the invocation that produced it")
