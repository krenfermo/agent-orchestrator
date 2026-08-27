package domain

import "strings"

// workflow_write_intent.go — the plan's own answer to "is this task supposed to
// change the workspace at all?".
//
// AO's completion rule for a work step is, and remains, evidence-only: a worker
// that goes idle having produced no commit, no dirty file, no staged file and
// no untracked file has produced nothing AO can point at, and for an
// IMPLEMENTATION task that is genuinely ambiguous — the worker may have done
// the work in some place AO cannot see, or may have done nothing at all, and
// AO cannot tell those apart. `ambiguous_worker_state` is the honest name for
// that, and it stays.
//
// But it was being applied to tasks whose ACCEPTED OUTCOME is "the workspace is
// unchanged": verification steps, inspections, audits, read-only repository
// checks. For those, "no verifiable change" is not AO failing to prove
// anything. It is the declared success condition, and stopping the run on it
// parks a task that did exactly what it was asked to do.
//
// The two cases cannot be told apart from the worker's behaviour, because the
// worker behaves identically in both. They can only be told apart from the
// PLAN — from what the task was required to produce. So the plan says it, out
// loud, in a durable field, and the classifier reads it.
//
// The zero value is deliberately the conservative one. A plan that predates
// this field, a plan whose planner declined to answer, and a standalone
// objective with no plan at all all resolve to WriteIntentUnspecified, which is
// treated exactly as WriteIntentMutating: fail closed. Nothing about the
// mutation-required path changes unless a plan has explicitly and durably
// declared otherwise.
type WorkflowWriteIntent string

const (
	// WorkflowWriteIntentUnspecified is "nobody said". It is the zero value,
	// it is what every legacy plan deserializes to, and it is treated as
	// mutating everywhere. It exists as a distinct value from `mutating` only
	// so a reader can tell a plan that declared its intent from one that never
	// had the chance.
	WorkflowWriteIntentUnspecified WorkflowWriteIntent = ""
	// WorkflowWriteIntentMutating is a task expected to change the workspace:
	// code, tests, documentation, configuration. A mutating task that goes
	// idle having changed nothing is still ambiguous, and still stops the run.
	WorkflowWriteIntentMutating WorkflowWriteIntent = "mutating"
	// WorkflowWriteIntentReadOnly is a task whose accepted outcome REQUIRES the
	// workspace to be unchanged: run the existing checks, inspect state, report
	// what is there. For one of these, an unchanged workspace is the result,
	// not the absence of one — and a workspace that DID change is the failure.
	WorkflowWriteIntentReadOnly WorkflowWriteIntent = "read_only"
)

// NormalizeWorkflowWriteIntent trims and lower-cases a declared intent, and
// maps anything it does not recognise to Unspecified rather than guessing. An
// intent AO cannot read is an intent AO does not have.
func NormalizeWorkflowWriteIntent(raw string) WorkflowWriteIntent {
	switch WorkflowWriteIntent(strings.ToLower(strings.TrimSpace(raw))) {
	case WorkflowWriteIntentMutating:
		return WorkflowWriteIntentMutating
	case WorkflowWriteIntentReadOnly:
		return WorkflowWriteIntentReadOnly
	default:
		return WorkflowWriteIntentUnspecified
	}
}

// Valid reports whether this is a value the plan schema accepts. The empty
// value is valid: it is how every plan written before this field existed
// deserializes, and rejecting it would invalidate them all.
func (w WorkflowWriteIntent) Valid() bool {
	switch w {
	case WorkflowWriteIntentUnspecified, WorkflowWriteIntentMutating, WorkflowWriteIntentReadOnly:
		return true
	default:
		return false
	}
}

// ReadOnly reports whether the plan DECLARED that this task must not change the
// workspace. It is the only question the completion classifier asks, and it is
// false for both `mutating` and — importantly — for an intent nobody declared.
func (w WorkflowWriteIntent) ReadOnly() bool { return w == WorkflowWriteIntentReadOnly }
