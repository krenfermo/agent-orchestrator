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
