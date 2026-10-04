package domain

import (
	"strings"
	"time"
)

// capacity.go — P1-C's runtime capacity model.
//
// # This is not the capacity AO already had
//
// `CapacityState` / `agent_health_events` / workflow's capacity probe answer a
// different question: "is this provider harness usable right now" (its CLI is
// installed, its credentials work, its rate limit is clear). That is an
// ELIGIBILITY signal about a provider, it is per-(harness, user, profile), and
// it decides WHICH provider a role routes to.
//
// This file answers "may AO start one more runtime at all". It is about the
// machine, not the vendor: tmux sessions, agent processes, and the finite
// number of them a laptop can host before everything gets slower and nothing
// finishes. Before P1-C there was no answer -- every dispatch site launched
// whatever it needed, and the only bound in the system was an incidental one
// (a master objective serialises its tasks unless the project is in
// smart-parallel mode).
//
// The two are deliberately separate and compose: a dispatch must be routable
// (provider capacity) AND admitted (runtime capacity). Neither substitutes for
// the other, and merging them would have made "the machine is full" and "your
// Claude subscription is rate limited" the same string.

// ExecutionKind is the closed vocabulary of runtime AO launches and therefore
// meters. Membership is the entire definition of "a thing that occupies a
// runtime slot".
type ExecutionKind string

const (
	// ExecutionKindPlanner is an objective planner invocation.
	ExecutionKindPlanner ExecutionKind = "planner"
	// ExecutionKindWorker is a worker session for a work step.
	ExecutionKindWorker ExecutionKind = "worker"
	// ExecutionKindReviewer is an independent reviewer session.
	ExecutionKindReviewer ExecutionKind = "reviewer"
	// ExecutionKindRepair is a P1-B Repair Agent's run. It is its own class
	// rather than another worker so a repair storm cannot consume the whole
	// worker budget, and so repairs can be bounded and prioritised separately.
	ExecutionKindRepair ExecutionKind = "repair"
)

// Valid reports whether k is a metered execution kind.
func (k ExecutionKind) Valid() bool {
	switch k {
	case ExecutionKindPlanner, ExecutionKindWorker, ExecutionKindReviewer, ExecutionKindRepair:
		return true
	default:
		return false
	}
}

// ExecutionKinds is the enumeration, in a stable order, for reporting.
func ExecutionKinds() []ExecutionKind {
	return []ExecutionKind{ExecutionKindPlanner, ExecutionKindWorker, ExecutionKindReviewer, ExecutionKindRepair}
}

// NormalizeExecutionKind trims and lower-cases a caller-supplied kind, leaving
// anything unrecognised unchanged so the caller can reject it.
func NormalizeExecutionKind(raw string) ExecutionKind {
	return ExecutionKind(strings.ToLower(strings.TrimSpace(raw)))
}

// CapacityClaimState is where one claim sits.
//
// Three states, and the middle one is the whole point: a claim exists BEFORE
// it holds capacity, so the queue is durable rather than an in-memory list the
// daemon forgets when it restarts.
type CapacityClaimState string

const (
	// CapacityClaimQueued means the work is admitted to the queue and is
	// waiting for a slot. It occupies no capacity.
	CapacityClaimQueued CapacityClaimState = "queued"
	// CapacityClaimHeld means this claim owns a slot. No runtime may be
	// launched without one.
	CapacityClaimHeld CapacityClaimState = "held"
	// CapacityClaimReleased is terminal. The slot is free and the row remains
	// as evidence of what held it and why it ended.
	CapacityClaimReleased CapacityClaimState = "released"
)

// Valid reports whether s is a persistable claim state.
func (s CapacityClaimState) Valid() bool {
	switch s {
	case CapacityClaimQueued, CapacityClaimHeld, CapacityClaimReleased:
		return true
	default:
		return false
	}
}

// Terminal reports whether the claim can still change.
func (s CapacityClaimState) Terminal() bool { return s == CapacityClaimReleased }

// CapacityClaim is one durable authorization to occupy a runtime slot.
//
// It is the single answer to "who is allowed to be running right now", and it
// is deliberately relational rather than another opaque checkpoint payload:
// the scheduler has to COUNT held claims inside the same write that grants the
// next one, and counting rows in a ledger of JSON blobs is neither atomic nor
// indexable.
type CapacityClaim struct {
	ID   string
	Kind ExecutionKind
	// State is queued / held / released.
	State CapacityClaimState
	// WorkflowRunID, WorkflowStepID and TaskID place the claim in the
	// lifecycle. StepID/TaskID are empty for kinds that have neither (a
	// planner claim belongs to the run).
	WorkflowRunID  string
	WorkflowStepID string
	TaskID         string
	// LifecycleGeneration is the step's dispatch generation at the moment the
	// claim was minted. It is the fence: a claim carrying an older generation
	// than the step's current one describes a launch the lifecycle has moved
	// past, and may neither hold capacity nor release a newer claim.
	LifecycleGeneration int64
	// DispatchKey is the launch intent's identity, and it is UNIQUE. One
	// intended launch gets one claim, however many times reconciliation,
	// a wake, a restart or a double click re-derives it. This is what makes
	// "duplicate reconcile does not double-claim" a property of the schema.
	DispatchKey string
	// OwnerID and ProjectID are the scopes AO already represents reliably.
	// They exist so scoped limits are a configuration change rather than a
	// schema change; P1-C enforces global, per-kind and per-workflow bounds
	// only, and does not invent unfinished multitenancy.
	OwnerID   UserID
	ProjectID string
	// RuntimeHandle and RuntimeInstanceID record which runtime this claim
	// actually paid for, once one exists. InstanceID is the immutable
	// incarnation (tmux's `$N`), never a reusable name -- it is what lets GC
	// tell a claim's own runtime from a stranger that later took its name.
	RuntimeHandle     string
	RuntimeInstanceID string
	// Priority orders the queue. Lower runs first.
	Priority int64
	// EnqueuedAt orders within a priority, and is what makes the policy FIFO
	// rather than arbitrary.
	EnqueuedAt time.Time
	HeldAt     *time.Time
	ReleasedAt *time.Time
	// ReleaseReason is AO's own vocabulary for why the slot came back.
	ReleaseReason string
	UpdatedAt     time.Time
}

// Holding reports whether this claim currently occupies a slot.
func (c CapacityClaim) Holding() bool { return c.State == CapacityClaimHeld }

// Capacity claim priorities. Lower is scheduled first.
//
// Only two bands exist, on purpose. A scheduler with many priorities needs a
// policy for every pair of them, and AO has exactly one justified distinction.
const (
	// CapacityPriorityRepair is a modest boost for P1-B Repair Agents.
	//
	// The justification is that a repair is the only execution whose PURPOSE is
	// to unblock a run that is already stopped: every moment it waits, a stopped
	// workflow stays stopped. It is a boost and not a reservation -- repairs
	// still compete for the same global slots, are capped by their own
	// per-kind limit (1 by default), and are bounded per run by P1-B's repair
	// budget. Those three bounds together are why a repair queue cannot starve
	// ordinary work.
	CapacityPriorityRepair int64 = 50
	// CapacityPriorityNormal is everything else.
	CapacityPriorityNormal int64 = 100
)

// PriorityForKind is the whole priority policy.
func PriorityForKind(kind ExecutionKind) int64 {
	if kind == ExecutionKindRepair {
		return CapacityPriorityRepair
	}
	return CapacityPriorityNormal
}

// CapacityLimits is the configured bound on concurrent runtimes.
//
// Defaults are chosen from what AO actually did before P1-C rather than from a
// theory: a project in the default isolated-worktree mode ran one task at a
// time per objective, with a reviewer alongside it and occasionally a planner,
// so a handful of concurrent runtimes is the observed working set. They are
// deliberately modest -- the failure this exists to prevent is a laptop with
// twelve agent processes on it, where everything is slower and nothing
// finishes.
type CapacityLimits struct {
	// Global bounds all held claims of every kind.
	Global int
	// PerKind bounds each execution kind independently. A kind absent from the
	// map is bounded only by Global.
	PerKind map[ExecutionKind]int
	// PerWorkflow bounds how many slots ONE workflow run may hold at once. It
	// is the fairness rule: it is what stops a master objective with twenty
	// runnable children from occupying every slot until it finishes.
	PerWorkflow int
}

// Default capacity limits. Every one of them is overridable by configuration.
const (
	DefaultCapacityGlobal      = 6
	DefaultCapacityWorkers     = 4
	DefaultCapacityReviewers   = 3
	DefaultCapacityPlanners    = 2
	DefaultCapacityRepairs     = 1
	DefaultCapacityPerWorkflow = 2
)

// DefaultCapacityLimits returns the shipped defaults.
func DefaultCapacityLimits() CapacityLimits {
	return CapacityLimits{
		Global: DefaultCapacityGlobal,
		PerKind: map[ExecutionKind]int{
			ExecutionKindWorker:   DefaultCapacityWorkers,
			ExecutionKindReviewer: DefaultCapacityReviewers,
			ExecutionKindPlanner:  DefaultCapacityPlanners,
			ExecutionKindRepair:   DefaultCapacityRepairs,
		},
		PerWorkflow: DefaultCapacityPerWorkflow,
	}
}

// LimitFor returns the effective bound for one kind, and the global bound.
// A non-positive per-kind limit means "bounded only by Global"; a non-positive
// Global means the limits are unconfigured and Normalize should have run.
func (l CapacityLimits) LimitFor(kind ExecutionKind) int {
	if n, ok := l.PerKind[kind]; ok && n > 0 {
		return n
	}
	return l.Global
}

// Normalize fills in any unset bound from the defaults, so a partially
// configured limit set can never read as "zero slots, nothing may ever run".
// A misconfiguration should look like a default, never like a deadlock.
func (l CapacityLimits) Normalize() CapacityLimits {
	defaults := DefaultCapacityLimits()
	out := CapacityLimits{Global: l.Global, PerWorkflow: l.PerWorkflow, PerKind: map[ExecutionKind]int{}}
	if out.Global <= 0 {
		out.Global = defaults.Global
	}
	if out.PerWorkflow <= 0 {
		out.PerWorkflow = defaults.PerWorkflow
	}
	for _, kind := range ExecutionKinds() {
		if n, ok := l.PerKind[kind]; ok && n > 0 {
			out.PerKind[kind] = n
			continue
		}
		out.PerKind[kind] = defaults.PerKind[kind]
	}
	return out
}

// CapacityUsage is one kind's live meter, for the status API.
type CapacityUsage struct {
	Kind   ExecutionKind
	Limit  int
	Held   int
	Queued int
}

// SchedulerSnapshot is the whole scheduler's observable state.
//
// Named for the scheduler rather than for "capacity" because
// domain.CapacitySnapshot already means the provider-health projection -- the
// other capacity (see this file's header). Two things called CapacitySnapshot
// in one package is exactly the confusion this model has to avoid.
type SchedulerSnapshot struct {
	Limits      CapacityLimits
	Global      CapacityUsage
	PerKind     []CapacityUsage
	HeldClaims  []CapacityClaim
	QueuedFirst []CapacityClaim
	ObservedAt  time.Time
}
