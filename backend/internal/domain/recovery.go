package domain

import (
	"strings"
	"time"
)

// recovery.go — P1-B's canonical recovery vocabulary.
//
// A stopped run already carries everything needed to decide what to do about
// it: a canonical attention reason, a durable plan state, a frozen execution
// strategy, step/attempt generations, and the review/verify provenance P0
// built. What was missing was a single named answer, so every surface (the
// run page, the Board, an operator's terminal) re-derived one and they could
// disagree.
//
// This file is that answer's vocabulary. It is deliberately a closed set: an
// action AO cannot name is an action AO must not take.

// RecoveryAction is the one thing AO recommends doing about a run.
type RecoveryAction string

const (
	// RecoveryResume means a durable obligation remains and re-entering the
	// ordinary resume path will discharge it. It is the only action that
	// advances a run's own lifecycle.
	RecoveryResume RecoveryAction = "resume"
	// RecoveryReusePlan means an existing durable plan is still authoritative
	// and execution can pick it up without regenerating anything.
	RecoveryReusePlan RecoveryAction = "reuse_plan"
	// RecoveryRegeneratePlan means the existing plan can no longer be trusted
	// to describe this repository, so a new plan revision is required.
	RecoveryRegeneratePlan RecoveryAction = "regenerate_plan"
	// RecoveryRepair means the stop is a repairable technical condition and a
	// bounded Repair Agent may address it.
	RecoveryRepair RecoveryAction = "repair"
	// RecoveryAuthenticate means a provider rejected or demanded credentials.
	// Only the person holding them can fix it.
	RecoveryAuthenticate RecoveryAction = "authenticate"
	// RecoveryInspectRepository means the answer is in the working tree and
	// AO must not guess at it: a dirty worktree, a moved HEAD, a change it
	// cannot attribute.
	RecoveryInspectRepository RecoveryAction = "inspect_repository"
	// RecoveryOperatorAction is the general "a person must decide", used when
	// AO knows the reason and the remedy is neither of the two specific
	// operator actions above.
	RecoveryOperatorAction RecoveryAction = "operator_action"
	// RecoveryRestartRequired means this run cannot be continued and the work
	// has to begin again in a fresh run. It is never chosen because AO is
	// unsure -- only when a bound was genuinely exhausted.
	RecoveryRestartRequired RecoveryAction = "restart_required"
	// RecoveryAbandon means the honest recommendation is to cancel.
	RecoveryAbandon RecoveryAction = "abandon"
	// RecoveryTerminal means the run already ended. Nothing to recover.
	RecoveryTerminal RecoveryAction = "terminal"
	// RecoveryUnrecoverable is the fail-closed answer: AO cannot classify this
	// state from durable facts. Naming the gap is the value; guessing is the
	// failure this whole model exists to prevent.
	RecoveryUnrecoverable RecoveryAction = "unrecoverable"
)

// Valid reports whether a is a recognised recovery action.
func (a RecoveryAction) Valid() bool {
	switch a {
	case RecoveryResume, RecoveryReusePlan, RecoveryRegeneratePlan, RecoveryRepair,
		RecoveryAuthenticate, RecoveryInspectRepository, RecoveryOperatorAction,
		RecoveryRestartRequired, RecoveryAbandon, RecoveryTerminal, RecoveryUnrecoverable:
		return true
	default:
		return false
	}
}

// NeedsHuman reports whether this action is one only a person can take.
func (a RecoveryAction) NeedsHuman() bool {
	switch a {
	case RecoveryAuthenticate, RecoveryInspectRepository, RecoveryOperatorAction,
		RecoveryRestartRequired, RecoveryAbandon, RecoveryUnrecoverable:
		return true
	default:
		return false
	}
}

// PlanReusability is how much of an existing durable plan can still be
// trusted. It is a statement about EVIDENCE, not about convenience.
type PlanReusability string

const (
	// PlanReuseNotApplicable is a run with no plan of its own -- a task-strategy
	// run, or a planned child. Absence of a plan is not a reuse problem.
	PlanReuseNotApplicable PlanReusability = "not_applicable"
	// PlanReuseExact means the durable plan is validated or approved, its hash
	// still matches the plan bytes on disk, and the planner context it was
	// generated from still describes this project. It can be picked up as-is.
	PlanReuseExact PlanReusability = "exact"
	// PlanReuseStaleRevalidatable means a real plan exists but the context it
	// was generated against has moved. It must be revalidated or regenerated
	// before anything executes -- a stale plan must never silently run.
	PlanReuseStaleRevalidatable PlanReusability = "stale_but_revalidatable"
	// PlanReuseNotReusable means there is nothing to reuse: no plan, an
	// invalid or rejected one, or one whose identity cannot be proven.
	PlanReuseNotReusable PlanReusability = "not_reusable"
)

// Reusable reports whether execution may proceed on this plan with no further
// human or planner step. Only `exact` qualifies, on purpose.
func (r PlanReusability) Reusable() bool { return r == PlanReuseExact }

// RepairMode is the frozen auto-repair policy for one run.
type RepairMode string

const (
	// RepairModeDisabled means AO never starts a Repair Agent for this run.
	RepairModeDisabled RepairMode = "disabled"
	// RepairModeSuggest means AO surfaces the repair action and waits for an
	// operator. This is the default -- a repair writes code, and opting into
	// that automatically should be a decision somebody made.
	RepairModeSuggest RepairMode = "suggest"
	// RepairModeAutomatic means AO may start a repair itself, and ONLY for a
	// condition class explicitly marked repairable.
	RepairModeAutomatic RepairMode = "automatic"
)

// Valid reports whether m is a recognised repair mode.
func (m RepairMode) Valid() bool {
	switch m {
	case RepairModeDisabled, RepairModeSuggest, RepairModeAutomatic:
		return true
	default:
		return false
	}
}

// NormalizeRepairMode trims and lower-cases a caller-supplied mode, leaving
// anything unrecognised unchanged so the caller can reject it rather than
// silently running under a policy nobody chose.
func NormalizeRepairMode(raw string) RepairMode {
	return RepairMode(strings.ToLower(strings.TrimSpace(raw)))
}

// RepairPolicyVersion versions the repair policy and eligibility rules, so a
// decision stays explainable after they change.
const RepairPolicyVersion = "v1"

// DefaultMaxRepairCycles bounds how many Repair Agents ONE run may spend on
// its stops before AO stops offering and escalates to a person.
//
// Two, deliberately: a repairable failure is worth one automatic attempt and
// one more after the first attempt's own evidence, and a third identical
// attempt discovers nothing the second did not. It is a per-run budget rather
// than a per-condition one so a run cannot cycle through condition classes to
// buy itself unlimited repairs.
const DefaultMaxRepairCycles = 2

// RepairPolicySnapshot is the frozen auto-repair policy embedded in a run's
// WorkflowPolicy, alongside Routing/Wake/Execution/Strategy. Frozen for the
// same reason those are: a Settings change must never alter what an in-flight
// run is allowed to do to itself, and a restart must never change the answer.
type RepairPolicySnapshot struct {
	Version string     `json:"version,omitempty"`
	Mode    RepairMode `json:"mode,omitempty"`
	// MaxRepairCycles is this run's repair budget. Zero means "use
	// DefaultMaxRepairCycles" -- see WorkflowPolicy.EffectiveRepairPolicy.
	MaxRepairCycles int       `json:"maxRepairCycles,omitempty"`
	At              time.Time `json:"at,omitempty"`
}

// Recorded reports whether this snapshot is a real frozen decision rather than
// the zero value a run created before P1-B carries.
func (s RepairPolicySnapshot) Recorded() bool { return s.Mode.Valid() && s.Version != "" }

// DefaultRepairPolicy is what a run freezes when nobody said otherwise:
// suggest, with the default budget. It never launches anything by itself.
func DefaultRepairPolicy(now time.Time) RepairPolicySnapshot {
	return RepairPolicySnapshot{
		Version:         RepairPolicyVersion,
		Mode:            RepairModeSuggest,
		MaxRepairCycles: DefaultMaxRepairCycles,
		At:              now,
	}
}

// RepairEligibility is the deterministic answer to "may a Repair Agent be
// pointed at this stop at all".
type RepairEligibility string

const (
	// RepairEligible means the stop is a technical condition whose remedy is a
	// bounded code change, and AO holds the evidence describing it.
	RepairEligible RepairEligibility = "eligible"
	// RepairIneligible means AO recognises the stop and it is NOT repairable:
	// a provenance gap, a credential, a permission, a destructive ambiguity, a
	// policy refusal. Naming these is the safety property.
	RepairIneligible RepairEligibility = "ineligible"
	// RepairBudgetExhausted means the condition is repairable but this run has
	// spent its repair budget. Escalates to a person.
	RepairBudgetExhausted RepairEligibility = "budget_exhausted"
	// RepairPolicyDisabled means repair is eligible but the run's frozen
	// policy forbids it.
	RepairPolicyDisabled RepairEligibility = "policy_disabled"
	// RepairUnknownCondition is the fail-closed default: AO cannot name what
	// stopped this run, so it will not aim a code-writing agent at it.
	RepairUnknownCondition RepairEligibility = "unknown_condition"
)

// Allowed reports whether a Repair Agent may be created for this eligibility.
func (e RepairEligibility) Allowed() bool { return e == RepairEligible }

// RepairIntent is the durable record of one Repair Agent: what it is repairing,
// on whose authority, under which generation, and how far it may reach.
//
// It is written to the append-only workflow_checkpoints ledger, the same way
// the Incident Advisor's records are and for the same reasons (see
// workflow/incident.go): an intent belongs in the same timeline as the stop
// that caused it, and it must not require a migration to land.
type RepairIntent struct {
	// ID is this intent's stable identity, derived from the run and the repair
	// generation so a replay computes the same one.
	ID string `json:"id"`
	// WorkflowRunID is the run that stopped. TargetRunID is the run whose
	// obligation is actually being repaired -- the same run for task and
	// autonomous strategies, and the AFFECTED CHILD for a master objective.
	WorkflowRunID string `json:"workflowRunId"`
	TargetRunID   string `json:"targetRunId"`
	// TargetStepID is the step whose failure is being repaired, when one is
	// identifiable.
	TargetStepID string `json:"targetStepId,omitempty"`
	// ConditionReason is the canonical attention reason that made this run
	// repairable. It is the eligibility decision's whole input.
	ConditionReason string `json:"conditionReason"`
	// EvidenceDigest is a stable digest of the failure being repaired. Two
	// intents with the same digest are about the same failure, which is what
	// makes "one failure must not create unbounded repair agents" checkable
	// rather than hoped for.
	EvidenceDigest string `json:"evidenceDigest"`
	// Generation is this run's repair generation, 1-based. It is the CAS fence:
	// a repair carrying a generation older than the run's current one may not
	// mutate anything.
	Generation int `json:"generation"`
	// LifecycleGeneration pins the target step's dispatch generation at the
	// moment the intent was created. A repair whose lifecycle generation is
	// behind the step's current one is stale by definition -- the thing it was
	// repairing has already moved.
	LifecycleGeneration int64 `json:"lifecycleGeneration,omitempty"`
	// ProjectID and Scope bound where the repair may write.
	ProjectID string      `json:"projectId"`
	Scope     RepairScope `json:"scope"`
	// AcceptanceCriteria is what "repaired" means, written by AO from the
	// failure -- never proposed by the agent that will be judged against it.
	AcceptanceCriteria []string `json:"acceptanceCriteria,omitempty"`
	// Strategy is the execution strategy the repair run itself executes under.
	// Always `task`: a repair is bounded work, and a repair that decomposes is
	// a repair nobody can reason about.
	Strategy ExecutionStrategy `json:"strategy"`
	// RepairRunID is the run created to carry out the repair, once it exists.
	RepairRunID string `json:"repairRunId,omitempty"`
	// AuthorizedBy names the person who approved it, or is empty for a
	// policy-authorized automatic repair under RepairModeAutomatic.
	AuthorizedBy string `json:"authorizedBy,omitempty"`
	// PolicyVersion and Mode record the policy in force at the decision.
	PolicyVersion string     `json:"policyVersion,omitempty"`
	Mode          RepairMode `json:"mode,omitempty"`
	At            time.Time  `json:"at,omitempty"`
}

// RepairScope is the mutation boundary of one repair. It is recorded rather
// than assumed so a reader can tell what a repair was permitted to touch even
// after the run has ended.
type RepairScope struct {
	// AllowedPaths, when non-empty, are the only paths the repair is expected
	// to change. Empty means the target run's own working tree, unrestricted
	// within it -- which is still a boundary, because the repair runs in that
	// tree and nowhere else.
	AllowedPaths []string `json:"allowedPaths,omitempty"`
	// WriteIntent is always mutating for a repair: a repair that changes
	// nothing has not repaired anything.
	WriteIntent WorkflowWriteIntent `json:"writeIntent,omitempty"`
	// SiblingsUntouched records the master-strategy invariant: repairing one
	// child must not rewrite its siblings.
	SiblingsUntouched bool `json:"siblingsUntouched,omitempty"`
}
