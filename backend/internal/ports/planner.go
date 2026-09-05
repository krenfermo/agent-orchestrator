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

// The launch-failure sentinels below split what used to be one undifferentiated
// "the planner could not be started". Before them, every non-zero exit of the
// planner subprocess -- a missing binary, an expired credential, a provider
// overload, an unsupported CLI flag -- reached the coordinator as the same
// untyped error, and the only text it carried was the first 500 bytes of the
// CLI's own JSON envelope, which is metrics. The coordinator therefore failed
// the objective permanently on `planner_start_failed` with a stop sentence that
// could name neither the cause nor a repair.
//
// Each sentinel below is a durable claim about WHY the planner did not produce
// a plan, and the coordinator's retry policy is derived from that claim rather
// than from prose.

// ErrPlannerBinaryMissing means the planner executable could not be resolved at
// all: not on the PATH the subprocess would have inherited, or present but not
// executable. Retrying cannot change it, so the coordinator fails immediately
// and names the binary it looked for.
var ErrPlannerBinaryMissing = errors.New("planner: provider binary could not be resolved")

// ErrPlannerAuthRequired means the provider itself reported that its
// credentials are missing, expired or rejected. Never auto-retried: an
// unattended retry would stop at the same login prompt, and AO never touches
// authentication state on a person's behalf.
var ErrPlannerAuthRequired = errors.New("planner: provider credentials are unavailable")

// ErrPlannerRuntimeHomeUnreadable means the profile/home directory the planner
// subprocess would have run against (HOME, CLAUDE_CONFIG_DIR or CODEX_HOME)
// does not exist or cannot be read. This is the shape of the earlier
// TrustedLocal incident, where an isolated runtime-home hid the host's real
// credential store from the provider CLI; naming it is what keeps that class of
// failure from reading as "auth is broken".
var ErrPlannerRuntimeHomeUnreadable = errors.New("planner: provider profile directory is not readable")

// ErrPlannerUnsupportedInvocation means the provider CLI rejected the
// invocation itself -- an unknown flag, an unsupported output format, a version
// that does not implement the planner contract. Permanent: the same command
// against the same CLI will be rejected the same way.
var ErrPlannerUnsupportedInvocation = errors.New("planner: provider rejected the invocation")

// ErrPlannerLaunchFailed means the planner process started and exited without
// producing a plan, for a reason the envelope did not attribute to any of the
// causes above. It is the honest residual: retryable a bounded number of times,
// because a provider that died mid-call often succeeds on the next one, but
// never retried forever.
var ErrPlannerLaunchFailed = errors.New("planner: provider exited before a plan was produced")
