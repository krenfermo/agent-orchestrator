package domain

import (
	"strings"
	"time"
)

// execution_strategy.go — AO's canonical, durable execution-strategy model.
//
// Before this file a run's shape was an implicit collection of flags: a
// `masterPlan` boolean on the create request decided whether a planner ever
// ran, an `autonomous` boolean inside the frozen execution-policy snapshot
// decided whether anything moved without a person, and a `planApprovalMode`
// column decided whether a generated plan waited for approval. Nothing on
// disk said what KIND of run this was, so every reader re-derived it from a
// different fact and the answer could differ between them.
//
// Execution strategy is now a first-class durable value with its own
// provenance, frozen into the run's policy snapshot at creation and never
// recomputed. It answers three questions any lifecycle component may ask:
// which strategy is this run using, was it chosen or selected by policy, and
// what policy version made that decision.
//
// It is deliberately SEPARATE from approval policy. "Manual" was never an
// execution strategy: it meant "a person approves the plan and drives the
// run". That concern lives in WorkflowPlanApprovalMode plus the frozen
// ExecutionPolicySnapshot.AutonomousMode flag, and this file does not touch
// either.

// ExecutionStrategy is the canonical durable vocabulary for how a workflow
// run is orchestrated. Exactly three members; there is no fourth, and
// "manual" is not one of them.
type ExecutionStrategy string

const (
	// ExecutionStrategyTask is small or bounded work: one unit of work,
	// normalized rather than planned, executed by a worker and then reviewed
	// and verified under the ordinary policy. It does not mean "skip review"
	// and it does not mean "unsafe" — it means no decomposition, no objective
	// hierarchy, and no master planner.
	ExecutionStrategyTask ExecutionStrategy = "task"
	// ExecutionStrategyAutonomous is the standard multi-step mode and the
	// default for normal project work: an objective the planner turns into a
	// durable plan, whose tasks execute in dependency order with review, fix
	// and verification, converging back on the parent.
	ExecutionStrategyAutonomous ExecutionStrategy = "autonomous"
	// ExecutionStrategyMaster is a large initiative deliberately decomposed
	// into coordinated child workstreams. It uses the same durable planner
	// and hierarchy machinery autonomous does; what differs is that it is
	// selected for breadth, and its children carry their own recorded
	// strategy under the child rules in ChildExecutionStrategy.
	ExecutionStrategyMaster ExecutionStrategy = "master"
)

// Valid reports whether s is one of the three canonical strategies.
func (s ExecutionStrategy) Valid() bool {
	switch s {
	case ExecutionStrategyTask, ExecutionStrategyAutonomous, ExecutionStrategyMaster:
		return true
	default:
		return false
	}
}

// Planned reports whether this strategy runs the objective planner and owns a
// durable plan with child workstreams. Task is the only strategy that does
// not — which is exactly what makes it cheaper.
func (s ExecutionStrategy) Planned() bool {
	return s == ExecutionStrategyAutonomous || s == ExecutionStrategyMaster
}

// RequestedExecutionStrategy is what a caller may ASK for: any canonical
// strategy, or "auto" to have AO decide under the deterministic policy in
// SelectExecutionStrategy. It is a separate type from ExecutionStrategy
// because "auto" is never an answer, only a question.
type RequestedExecutionStrategy string

const (
	// RequestedExecutionStrategyUnspecified is an omitted field. Callers that
	// predate this model send it, and the caller (not this package) decides
	// what compatibility answer it maps to — see the API layer.
	RequestedExecutionStrategyUnspecified RequestedExecutionStrategy = ""
	// RequestedExecutionStrategyAuto asks AO to select deterministically.
	RequestedExecutionStrategyAuto RequestedExecutionStrategy = "auto"
	// RequestedExecutionStrategyTask explicitly asks for the task strategy.
	RequestedExecutionStrategyTask RequestedExecutionStrategy = "task"
	// RequestedExecutionStrategyAutonomous explicitly asks for autonomous.
	RequestedExecutionStrategyAutonomous RequestedExecutionStrategy = "autonomous"
	// RequestedExecutionStrategyMaster explicitly asks for master.
	RequestedExecutionStrategyMaster RequestedExecutionStrategy = "master"
)

// Valid reports whether r is an accepted request value. The unspecified zero
// value is NOT valid here: a caller that omitted the field is handled by the
// transport layer's compatibility mapping, not by pretending it asked for
// something.
func (r RequestedExecutionStrategy) Valid() bool {
	switch r {
	case RequestedExecutionStrategyAuto, RequestedExecutionStrategyTask,
		RequestedExecutionStrategyAutonomous, RequestedExecutionStrategyMaster:
		return true
	default:
		return false
	}
}

// Explicit returns the canonical strategy r names, and whether it names one.
// "auto" names none.
func (r RequestedExecutionStrategy) Explicit() (ExecutionStrategy, bool) {
	s := ExecutionStrategy(r)
	return s, s.Valid()
}

// NormalizeRequestedExecutionStrategy trims and lower-cases a caller-supplied
// value. It maps nothing it does not recognise: an unrecognised string comes
// back unchanged so the caller can reject it rather than silently running
// something the user did not ask for.
func NormalizeRequestedExecutionStrategy(raw string) RequestedExecutionStrategy {
	return RequestedExecutionStrategy(strings.ToLower(strings.TrimSpace(raw)))
}

// ExecutionStrategySource records HOW a run's effective strategy was decided.
type ExecutionStrategySource string

const (
	// ExecutionStrategyExplicit means a user or an API client named the
	// strategy. Nothing may override it.
	ExecutionStrategyExplicit ExecutionStrategySource = "explicit"
	// ExecutionStrategyPolicy means AO selected it from the deterministic
	// signals in ExecutionStrategySignals, under PolicyVersion.
	ExecutionStrategyPolicy ExecutionStrategySource = "policy"
	// ExecutionStrategyInherited means a child run took its strategy from its
	// parent under the child rules, and names that parent.
	ExecutionStrategyInherited ExecutionStrategySource = "inherited"
	// ExecutionStrategyRecovered means the run predates this model and its
	// strategy was mapped from durable facts that DO exist (whether it owns a
	// plan row). It is a separate source on purpose: it is a reading of
	// history, never a claim that anybody chose it.
	ExecutionStrategyRecovered ExecutionStrategySource = "recovered"
)

// Recorded reports whether src names a real decision.
func (src ExecutionStrategySource) Recorded() bool {
	switch src {
	case ExecutionStrategyExplicit, ExecutionStrategyPolicy,
		ExecutionStrategyInherited, ExecutionStrategyRecovered:
		return true
	default:
		return false
	}
}

// ExecutionStrategyReason is a stable machine-checkable code explaining a
// policy or recovery selection, so a decision made today stays explainable
// after the policy changes. It mirrors the contract TaskStrategyReason
// already carries for the parallel-placement classifier.
type ExecutionStrategyReason string

const (
	// ExecutionStrategyReasonExplicitRequest — the caller named the strategy.
	ExecutionStrategyReasonExplicitRequest ExecutionStrategyReason = "explicit_request"
	// ExecutionStrategyReasonMultiWorkstream — the request asked for several
	// coordinated workstreams, which is what master exists for.
	ExecutionStrategyReasonMultiWorkstream ExecutionStrategyReason = "multi_workstream_initiative"
	// ExecutionStrategyReasonDecompositionRequired — the request declared that
	// the objective must be broken down before anything can execute.
	ExecutionStrategyReasonDecompositionRequired ExecutionStrategyReason = "decomposition_required"
	// ExecutionStrategyReasonMultiRepository — more than one repository is in
	// scope, so the work cannot be one bounded change in one worktree.
	ExecutionStrategyReasonMultiRepository ExecutionStrategyReason = "multi_repository"
	// ExecutionStrategyReasonSuppliedPlanHierarchy — the caller supplied an
	// existing plan/objective hierarchy, so AO plans rather than decomposes.
	ExecutionStrategyReasonSuppliedPlanHierarchy ExecutionStrategyReason = "supplied_plan_hierarchy"
	// ExecutionStrategyReasonBoundedWork — the declared size/step count fits
	// inside one bounded unit of work.
	ExecutionStrategyReasonBoundedWork ExecutionStrategyReason = "bounded_work"
	// ExecutionStrategyReasonMultiStepProject — the policy default: normal
	// multi-step project work.
	ExecutionStrategyReasonMultiStepProject ExecutionStrategyReason = "multi_step_project"
	// ExecutionStrategyReasonMasterChildDefault — a planned child of a master
	// or autonomous parent, executing as the bounded leaf its planner emitted.
	ExecutionStrategyReasonMasterChildDefault ExecutionStrategyReason = "planned_child_default"
	// ExecutionStrategyReasonLegacyPlannedRun — a pre-model run that owns a
	// durable plan row, so it was and remains a planned objective.
	ExecutionStrategyReasonLegacyPlannedRun ExecutionStrategyReason = "legacy_planned_run"
	// ExecutionStrategyReasonLegacySingleTaskRun — a pre-model run with no
	// plan row: a single bounded chain, which is exactly a task.
	ExecutionStrategyReasonLegacySingleTaskRun ExecutionStrategyReason = "legacy_single_task_run"
	// ExecutionStrategyReasonLegacyPlannedChild — a pre-model child run of a
	// planned parent.
	ExecutionStrategyReasonLegacyPlannedChild ExecutionStrategyReason = "legacy_planned_child"
)

// ExecutionStrategyPolicyVersion is the version of the deterministic
// selection policy implemented by SelectExecutionStrategy. It is stamped into
// every recorded selection so a decision stays replayable after the rules
// change. Bump it whenever the rules below change.
const ExecutionStrategyPolicyVersion = "v1"

// ExecutionStrategyMaxTaskSteps is the largest declared step count that still
// counts as one bounded unit of work. A caller that says "this is one step"
// gets a task; anything more is a plan. Deliberately a named constant rather
// than a literal buried in the rules ("no constantes dispersas").
const ExecutionStrategyMaxTaskSteps = 1

// ExecutionStrategyMaxChildDepth bounds how deep planned decomposition may
// go. A master parent's children are depth 1 and are never themselves master,
// so nothing can decompose without bound. See ChildExecutionStrategy.
const ExecutionStrategyMaxChildDepth = 1

// ExecutionWorkSize is optional caller-supplied size/complexity metadata. It
// is one of the deterministic AUTO signals, never an LLM judgement.
type ExecutionWorkSize string

const (
	// ExecutionWorkSizeUnspecified is "nobody said" and selects nothing on its
	// own.
	ExecutionWorkSizeUnspecified ExecutionWorkSize = ""
	// ExecutionWorkSizeSmall is a bounded change.
	ExecutionWorkSizeSmall ExecutionWorkSize = "small"
	// ExecutionWorkSizeMedium is ordinary multi-step work.
	ExecutionWorkSizeMedium ExecutionWorkSize = "medium"
	// ExecutionWorkSizeLarge is an initiative.
	ExecutionWorkSizeLarge ExecutionWorkSize = "large"
)

// Valid reports whether s is an accepted size value (the unspecified zero
// value included — it is a legitimate "no opinion").
func (s ExecutionWorkSize) Valid() bool {
	switch s {
	case ExecutionWorkSizeUnspecified, ExecutionWorkSizeSmall,
		ExecutionWorkSizeMedium, ExecutionWorkSizeLarge:
		return true
	default:
		return false
	}
}

// NormalizeExecutionWorkSize trims and lower-cases a caller-supplied size,
// leaving anything unrecognised unchanged so the caller can reject it.
func NormalizeExecutionWorkSize(raw string) ExecutionWorkSize {
	return ExecutionWorkSize(strings.ToLower(strings.TrimSpace(raw)))
}

// ExecutionStrategySignals is the bounded, deterministic input to AUTO
// selection. Every field is a fact the caller already knows about the request;
// none of them requires a model call, and SelectExecutionStrategy deliberately
// does not make one. Choosing how to orchestrate must stay cheap, replayable
// and explainable — an LLM here would make the decision unreproducible for
// exactly the runs whose provenance matters most.
type ExecutionStrategySignals struct {
	// ExpectedSteps is the caller's estimate of how many steps the work
	// takes. 0 means "no estimate", which selects nothing on its own.
	ExpectedSteps int `json:"expectedSteps,omitempty"`
	// RequiresDecomposition is the caller stating that the objective cannot
	// be executed until it is broken down.
	RequiresDecomposition bool `json:"requiresDecomposition,omitempty"`
	// RepositoryCount is how many repositories the work spans. 0 and 1 both
	// mean "one repository".
	RepositoryCount int `json:"repositoryCount,omitempty"`
	// MultiWorkstream is the caller asking for several coordinated
	// workstreams to run under one initiative.
	MultiWorkstream bool `json:"multiWorkstream,omitempty"`
	// SuppliedPlanHierarchy is set when the caller already provided a plan or
	// objective hierarchy, so AO plans against it rather than deciding
	// whether to decompose.
	SuppliedPlanHierarchy bool `json:"suppliedPlanHierarchy,omitempty"`
	// Size is optional size/complexity metadata.
	Size ExecutionWorkSize `json:"size,omitempty"`
}

// ExecutionStrategySelection is the durable record of one run's execution
// strategy and how it was decided. It is embedded in WorkflowPolicy and
// therefore frozen into workflow_runs.policy_snapshot at creation, alongside
// the routing/wake/execution snapshots that are already frozen there — one
// place per run holding the full decision-making configuration.
//
// It is evidence, never a substitute for the decision: nothing here is ever
// synthesized to make a run look chosen.
type ExecutionStrategySelection struct {
	// Requested is what the caller asked for, including "auto". Empty for an
	// inherited or recovered selection, which nobody requested.
	Requested RequestedExecutionStrategy `json:"requested,omitempty"`
	// Effective is the strategy this run actually executes under. Once
	// written it is never recomputed — not by a restart, not by a policy
	// change, not by a code change.
	Effective ExecutionStrategy `json:"effective,omitempty"`
	// Source is how Effective was arrived at.
	Source ExecutionStrategySource `json:"source,omitempty"`
	// PolicyVersion is ExecutionStrategyPolicyVersion as of the decision.
	// Recorded for every source, so a decision stays replayable.
	PolicyVersion string `json:"policyVersion,omitempty"`
	// Reason is the stable explanation code, for policy/inherited/recovered
	// selections and for an explicit one alike.
	Reason ExecutionStrategyReason `json:"reason,omitempty"`
	// Signals is the AUTO input this decision was taken from, kept so the
	// decision can be replayed rather than merely re-asserted. Zero-valued
	// for an explicit choice.
	Signals ExecutionStrategySignals `json:"signals,omitempty"`
	// ParentRunID names the parent an inherited selection came from.
	ParentRunID string `json:"parentRunId,omitempty"`
	// Depth is how deep in a planned hierarchy this run sits: 0 for a
	// top-level run, 1 for a planned child. Bounded by
	// ExecutionStrategyMaxChildDepth.
	Depth int `json:"depth,omitempty"`
	// At is when the selection was recorded.
	At time.Time `json:"at,omitempty"`
}

// Recorded reports whether this selection is a real durable decision, as
// opposed to the zero value a run created before this model carries.
func (s ExecutionStrategySelection) Recorded() bool {
	return s.Effective.Valid() && s.Source.Recorded()
}

// Chosen reports whether a person or an API client named this strategy, as
// opposed to AO selecting it.
func (s ExecutionStrategySelection) Chosen() bool {
	return s.Source == ExecutionStrategyExplicit
}

// SelectExecutionStrategy is the whole strategy-selection policy: explicit
// wins outright, and AUTO falls through a short, ordered, deterministic list
// of signals. It is a pure function of its arguments — same inputs, same
// answer, forever — which is what lets a recorded decision be replayed rather
// than merely trusted.
//
// The order matters and is the policy:
//
//  1. an explicit request is honoured, always;
//  2. several coordinated workstreams, or a declared need to decompose, or
//     more than one repository, mean a large initiative -> master;
//  3. a supplied plan/objective hierarchy means AO plans against it ->
//     autonomous;
//  4. a declared small size, or a step count inside
//     ExecutionStrategyMaxTaskSteps, means one bounded change -> task;
//  5. everything else is normal multi-step project work -> autonomous.
//
// Master is never reached by accident: nothing but an explicit request or one
// of rule 2's three deliberate declarations can select it.
func SelectExecutionStrategy(requested RequestedExecutionStrategy, signals ExecutionStrategySignals, now time.Time) ExecutionStrategySelection {
	sel := ExecutionStrategySelection{
		Requested:     requested,
		PolicyVersion: ExecutionStrategyPolicyVersion,
		At:            now,
	}
	if strategy, ok := requested.Explicit(); ok {
		sel.Effective = strategy
		sel.Source = ExecutionStrategyExplicit
		sel.Reason = ExecutionStrategyReasonExplicitRequest
		return sel
	}
	sel.Source = ExecutionStrategyPolicy
	sel.Signals = signals
	switch {
	case signals.MultiWorkstream:
		sel.Effective, sel.Reason = ExecutionStrategyMaster, ExecutionStrategyReasonMultiWorkstream
	case signals.RequiresDecomposition:
		sel.Effective, sel.Reason = ExecutionStrategyMaster, ExecutionStrategyReasonDecompositionRequired
	case signals.RepositoryCount > 1:
		sel.Effective, sel.Reason = ExecutionStrategyMaster, ExecutionStrategyReasonMultiRepository
	case signals.SuppliedPlanHierarchy:
		sel.Effective, sel.Reason = ExecutionStrategyAutonomous, ExecutionStrategyReasonSuppliedPlanHierarchy
	case signals.Size == ExecutionWorkSizeSmall && signals.ExpectedSteps <= ExecutionStrategyMaxTaskSteps:
		sel.Effective, sel.Reason = ExecutionStrategyTask, ExecutionStrategyReasonBoundedWork
	case signals.Size == ExecutionWorkSizeUnspecified && signals.ExpectedSteps > 0 && signals.ExpectedSteps <= ExecutionStrategyMaxTaskSteps:
		sel.Effective, sel.Reason = ExecutionStrategyTask, ExecutionStrategyReasonBoundedWork
	default:
		sel.Effective, sel.Reason = ExecutionStrategyAutonomous, ExecutionStrategyReasonMultiStepProject
	}
	return sel
}

// ChildExecutionStrategy is the strategy a planned child run executes under,
// given its parent's recorded selection.
//
// Two rules, and both are invariants rather than defaults:
//
//   - A child is NEVER master. Recursive decomposition would let one
//     objective fan out without bound, and nothing in AO's planner asks for
//     it; a master parent decomposes once, into workstreams that execute.
//   - A child sits at parent depth + 1 and may not exceed
//     ExecutionStrategyMaxChildDepth, so the hierarchy is bounded by
//     construction rather than by hoping the planner behaves.
//
// The child's own strategy is `task`: AO's planner emits bounded leaf tasks
// with their own acceptance criteria and write intent, which is exactly what
// a task is. §J's "children normally execute as autonomous" describes a
// planner that emits sub-objectives; AO's does not, and recording those
// children as autonomous would name a decomposition that never happens.
func ChildExecutionStrategy(parent ExecutionStrategySelection, parentRunID string, now time.Time) ExecutionStrategySelection {
	depth := parent.Depth + 1
	if depth > ExecutionStrategyMaxChildDepth {
		depth = ExecutionStrategyMaxChildDepth
	}
	return ExecutionStrategySelection{
		Effective:     ExecutionStrategyTask,
		Source:        ExecutionStrategyInherited,
		PolicyVersion: ExecutionStrategyPolicyVersion,
		Reason:        ExecutionStrategyReasonMasterChildDefault,
		ParentRunID:   parentRunID,
		Depth:         depth,
		At:            now,
	}
}
