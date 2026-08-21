package workflow

import (
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// Phase is the derived lifecycle vocabulary the Board and the workflow detail
// page both render. It is never a stored column: every value below is derived
// at read time from durable rows, exactly as docs/workflow-lifecycle-mapping.md
// §5 specifies. That document is the contract; this file is its implementation,
// and the evaluation order in DerivePhase mirrors its table row for row.
type Phase string

// The thirteen derived lifecycle phases.
const (
	PhaseCancelled          Phase = "cancelled"
	PhaseFailed             Phase = "failed"
	PhaseCompleted          Phase = "completed"
	PhaseNeedsAttention     Phase = "needs_attention"
	PhaseBlocked            Phase = "blocked"
	PhaseWaitingForCapacity Phase = "waiting_for_capacity"
	PhaseWaiting            Phase = "waiting"
	PhasePlanning           Phase = "planning"
	PhaseReviewing          Phase = "reviewing"
	PhaseFixing             Phase = "fixing"
	PhaseVerifying          Phase = "verifying"
	PhaseRunning            Phase = "running"
	PhaseQueued             Phase = "queued"
)

// Terminal reports whether a phase can never change again.
func (p Phase) Terminal() bool {
	return p == PhaseCompleted || p == PhaseFailed || p == PhaseCancelled
}

// Attention is Checkpoint 8P-E.12 §2's separation of the two things AO used to
// conflate under one "needs attention" banner.
//
// The distinction is behavioural, not cosmetic: AttentionInternal means AO has
// noticed something and can still act on it by itself, so interrupting a human
// would be a false alarm. AttentionHuman means AO genuinely cannot proceed
// safely without a decision only a person can make. Only the latter may ever
// put the UI into "Te necesita" / Waiting for decision.
//
// A review that requested changes AO is about to fix is the canonical example
// of the first kind, and was the specific misreport this checkpoint exists to
// end: it is progress, not a request for help.
type Attention string

// The three attention classifications.
const (
	AttentionNone     Attention = ""
	AttentionInternal Attention = "ao_internal"
	AttentionHuman    Attention = "human_decision"
)

// Lifecycle is the full derived projection of one run, shared by the run detail
// endpoint and the Board. Every field is either copied from a durable row or
// derived by the rules in the mapping document — nothing here is estimated, and
// an unknown value stays at its zero value rather than being guessed.
type Lifecycle struct {
	Phase     Phase
	Attention Attention
	// AttentionReason is the machine-readable kind of the stop, taken from the
	// durable carrier that produced it (a checkpoint's durable_phase, an
	// attempt's error_class, or a question's classification). Empty when
	// Attention is AttentionNone, and — deliberately — empty when a run is in
	// needs_attention with no recorded reason at all, because the mapping
	// document forbids synthesizing one.
	AttentionReason string
	// AttentionAction is the concrete thing a human has to do. Populated only
	// for AttentionHuman, and only for the reasons where AO actually knows the
	// remedy.
	AttentionAction string
	// WaitReason mirrors the soonest open wake's reason (the wait taxonomy).
	WaitReason string
	NextWakeAt *time.Time
	// LastActivityAt is the newest durable timestamp AO has for this run: its
	// latest checkpoint if it has one, otherwise the run row's own updated_at.
	// This is what the Board shows as "Last activity", and it is deliberately
	// NOT the worker session's activity timestamp — an idle worker during a
	// review is not an idle workflow.
	LastActivityAt time.Time
}

// StepProgress is one entry in the Board's Plan/Work/Review/Fix/Verify
// checklist. Kind and State come straight from workflow_steps.
type StepProgress struct {
	Kind  domain.WorkflowStepKind
	State domain.WorkflowStepState
}

// TaskProgress summarizes a master run's workflow_tasks fan-out.
type TaskProgress struct {
	Total     int
	Completed int
	Running   int
	Blocked   int
	Eligible  int
	Cancelled int
	// CurrentNumber/CurrentTitle/CurrentRunID describe the task currently
	// running, when one is. CurrentNumber is the task's own
	// workflow_tasks.ordinal, so "Task 2 of 7" is a fact, not an index.
	CurrentNumber int64
	CurrentTitle  string
	CurrentRunID  string
}

// humanDecisionReasons maps a durable stop to the remedy a person must apply.
// Membership in this map is the whole definition of HUMAN_DECISION_REQUIRED for
// checkpoint-carried stops: a durable_phase absent from it is, by construction,
// something AO either handles itself or has no advice about.
var humanDecisionReasons = map[string]string{
	"dirty_worktree":                        "Commit, stash or discard the local changes in the target repository, then continue this run.",
	"autonomous_local_commit_failed":        "Resolve the failed local commit in the working repository; the branch stays locked to this run until you do.",
	"autonomous_local_commit_deferred":      "Approve the local commit, or change the project's local-commit policy.",
	"worker_dispatch_ambiguous":             "Confirm whether the worker session actually produced work, then continue or cancel this run.",
	"review_dispatch_ambiguous":             "Confirm the state of the review, then continue or cancel this run.",
	"fix_dispatch_ambiguous":                "Confirm the state of the fix, then continue or cancel this run.",
	"work_provider_failure_needs_attention": "Every configured provider attempt failed. Check provider auth/capacity, then continue this run.",
	"master_integration_promotion_failed":   "Resolve the integration conflict for the completed task, then continue this run.",
}

// humanDecisionErrorClasses are the attempt-level error classes that mean AO
// has stopped for good rather than for now. Everything absent from this set —
// rate_limited, capacity_exhausted, transient, review_changes_requested — is a
// condition AO retries or fixes on its own.
var humanDecisionErrorClasses = map[domain.WorkflowErrorClass]string{
	domain.WorkflowErrorFixBudgetExhausted:   "The review/fix budget is exhausted. Review the remaining findings and decide how to proceed.",
	domain.WorkflowErrorAuth:                 "Provider authentication failed. Reconnect the provider profile, then continue this run.",
	domain.WorkflowErrorBinaryMissing:        "The provider CLI is not installed or not on PATH. Install it, then continue this run.",
	domain.WorkflowErrorAmbiguousWorkerState: "AO could not prove whether the work happened. Inspect the session, then continue or cancel this run.",
}

// LifecycleInput is everything DeriveLifecycle needs. It is a plain value so the
// derivation stays a pure function that can be exercised without a store.
type LifecycleInput struct {
	Detail RunDetail
	// Questions is the run's durable question list (any state). Empty is a
	// valid, meaningful value: it means the run has never asked anything.
	Questions []domain.WorkflowQuestion
}

// DeriveLifecycle projects one run onto the UI lifecycle vocabulary.
//
// The order below is the mapping document's order and must stay that way:
// terminal run states are decided before any wait, so a cancelled run never
// renders as "waiting", and a completed run with a lingering unanswered
// question renders "completed" rather than "waiting for decision" (§8.4 — a
// durable terminal state is a stronger fact than a question that no longer
// blocks anything).
func DeriveLifecycle(in LifecycleInput) Lifecycle {
	d := in.Detail
	life := Lifecycle{
		WaitReason:     d.WaitReason,
		NextWakeAt:     d.NextWakeAt,
		LastActivityAt: lastActivity(d),
	}
	life.Phase = derivePhase(d)
	life.Attention, life.AttentionReason, life.AttentionAction = classifyAttention(d, in.Questions, life.Phase)
	return life
}

func derivePhase(d RunDetail) Phase {
	switch d.Run.State {
	case domain.WorkflowRunCancelled:
		return PhaseCancelled
	case domain.WorkflowRunFailed:
		return PhaseFailed
	case domain.WorkflowRunCompleted:
		return PhaseCompleted
	case domain.WorkflowRunNeedsAttention:
		return PhaseNeedsAttention
	}

	if d.Run.State == domain.WorkflowRunWaiting {
		// Blocked is a wait on another workflow, never a provider problem.
		// Either signal counts: the branch-lock wake is best-effort, so a run
		// can carry the checkpoint with no wake row at all.
		if d.WaitReason == string(wake.ReasonBranchLock) || d.LatestCheckpointPhase == branchWaitPhase {
			return PhaseBlocked
		}
		if isCapacityWaitReason(d.WaitReason) {
			return PhaseWaitingForCapacity
		}
		// §8.8: a reviewer capacity stall parks the run with no wake at all.
		// Keyed on the wake alone this would read as a plain wait while it is
		// genuinely a provider-capacity one.
		if d.WaitReason == "" && d.LatestCheckpointPhase == reviewCapacityRetryDurablePhase {
			return PhaseWaitingForCapacity
		}
	}

	// A master run's own phase comes from its plan and task fan-out; its
	// active child's step kind is layered on top by the Board, not here.
	if d.Plan != nil {
		switch d.Plan.Status {
		case domain.WorkflowPlanPending, domain.WorkflowPlanRunning, domain.WorkflowPlanValidated:
			if d.WaitReason == string(wake.ReasonPlannerCapacity) {
				return PhaseWaitingForCapacity
			}
			return PhasePlanning
		}
		progress := DeriveTaskProgress(d.Tasks)
		if progress.Running > 0 {
			return PhaseRunning
		}
		if progress.Total > 0 && progress.Eligible == 0 && progress.Blocked > 0 {
			return PhaseBlocked
		}
		if d.Run.State == domain.WorkflowRunWaiting {
			return PhaseWaiting
		}
		if d.Run.State == domain.WorkflowRunPending {
			return PhaseQueued
		}
		return PhaseRunning
	}

	// Single-task run: the first non-terminal step names the phase.
	switch activeStepKind(d) {
	case domain.WorkflowStepPlan:
		if d.Run.State == domain.WorkflowRunPending {
			return PhaseQueued
		}
		return PhasePlanning
	case domain.WorkflowStepReview:
		return PhaseReviewing
	case domain.WorkflowStepFix:
		return PhaseFixing
	case domain.WorkflowStepVerify:
		return PhaseVerifying
	case domain.WorkflowStepWork:
		if d.Run.State == domain.WorkflowRunPending {
			return PhaseQueued
		}
		if d.Run.State == domain.WorkflowRunWaiting {
			return PhaseWaiting
		}
		return PhaseRunning
	}

	if d.Run.State == domain.WorkflowRunPending {
		return PhaseQueued
	}
	if d.Run.State == domain.WorkflowRunWaiting {
		return PhaseWaiting
	}
	return PhaseRunning
}

// activeStepKind names the step that is actually happening.
//
// A step in `running` always wins, even over an earlier step that is merely
// `waiting`. That ordering is what makes the fix cycle legible: a
// changes_requested verdict deliberately rests the review step at `waiting`
// while the fix step runs, so scanning in plain ordinal order would report
// "reviewing" throughout every fix AO applies.
//
// The `advance` step is skipped entirely: it is seeded but never executed
// (§8.2), so treating it as active would make every successfully verified run
// look like it still had work left.
func activeStepKind(d RunDetail) domain.WorkflowStepKind {
	for _, s := range d.Steps {
		if s.Step.Kind != domain.WorkflowStepAdvance && s.Step.State == domain.WorkflowStepRunning {
			return s.Step.Kind
		}
	}
	for _, s := range d.Steps {
		if s.Step.Kind == domain.WorkflowStepAdvance || s.Step.State.Terminal() {
			continue
		}
		if s.Step.Kind == domain.WorkflowStepFix && s.Step.State == domain.WorkflowStepPending {
			// A pending fix step is the normal resting state of a run whose
			// review has not asked for changes; it does not mean "fixing".
			continue
		}
		return s.Step.Kind
	}
	return ""
}

// classifyAttention is Phase 2's whole point: it decides whether a stop belongs
// to the user or to AO.
//
// It never infers "human" from the run state alone. A run in needs_attention
// with no recorded reason is reported as needs_attention with an empty reason —
// the mapping document's explicit rule against synthesizing one — rather than
// being escalated into a decision request the user cannot act on.
func classifyAttention(d RunDetail, questions []domain.WorkflowQuestion, phase Phase) (Attention, string, string) {
	if phase.Terminal() {
		return AttentionNone, "", ""
	}

	// A question classified human_required is the one carrier that outranks the
	// run state: it is a real, answerable request addressed to the user.
	for _, q := range questions {
		if q.State == domain.QuestionStateHumanRequired {
			return AttentionHuman, "question_human_required", questionAction(q)
		}
	}

	if phase != PhaseNeedsAttention {
		// Everything short of needs_attention that AO is actively handling —
		// a changes_requested rest, a capacity wait with a scheduled retry, a
		// branch queue — is internal. It is surfaced so the user can see what
		// AO is doing, never as a request for help.
		switch phase {
		case PhaseWaitingForCapacity, PhaseBlocked:
			return AttentionInternal, phase.String(), ""
		case PhaseFixing:
			return AttentionInternal, "review_changes_requested", ""
		}
		return AttentionNone, "", ""
	}

	reason := d.LatestCheckpointPhase
	if action, ok := humanDecisionReasons[reason]; ok {
		return AttentionHuman, reason, action
	}
	if class := latestErrorClass(d); class != "" {
		if action, ok := humanDecisionErrorClasses[class]; ok {
			return AttentionHuman, string(class), action
		}
		// A typed error class AO does not have a remedy for is still the
		// user's call to make — AO has already stopped.
		return AttentionHuman, string(class), ""
	}
	if reason != "" {
		return AttentionHuman, reason, ""
	}
	// needs_attention with nothing recorded. Truthful, unhelpful, and correct:
	// see the mapping document's "never synthesize a reason".
	return AttentionHuman, "", ""
}

// String renders a phase for reason codes and the wire format.
func (p Phase) String() string { return string(p) }

func questionAction(q domain.WorkflowQuestion) string {
	text := strings.TrimSpace(q.QuestionText)
	if text == "" {
		return "Answer the open question for this workflow."
	}
	return text
}

// latestErrorClass returns the error class of the newest attempt that recorded
// one, scanning steps in reverse so the most recent step wins.
func latestErrorClass(d RunDetail) domain.WorkflowErrorClass {
	var class domain.WorkflowErrorClass
	var at time.Time
	for _, s := range d.Steps {
		for _, a := range s.Attempts {
			if a.ErrorClass == "" {
				continue
			}
			stamp := a.StartedAt
			if a.FinishedAt != nil {
				stamp = *a.FinishedAt
			}
			if class == "" || !stamp.Before(at) {
				class, at = a.ErrorClass, stamp
			}
		}
	}
	return class
}

func lastActivity(d RunDetail) time.Time {
	latest := d.Run.UpdatedAt
	if d.LatestCheckpointAt.After(latest) {
		latest = d.LatestCheckpointAt
	}
	for _, s := range d.Steps {
		if s.Step.UpdatedAt.After(latest) {
			latest = s.Step.UpdatedAt
		}
	}
	return latest
}

// isCapacityWaitReason mirrors the renderer's denylist
// (frontend/src/renderer/lib/workflow-wake-reason.ts): every wake reason except
// the autonomous heartbeat and the branch lock is a capacity-shaped wait, so a
// reason added later reads as a capacity wait rather than going unlabeled.
func isCapacityWaitReason(reason string) bool {
	switch reason {
	case "", string(wake.ReasonAutonomousProgress), string(wake.ReasonBranchLock):
		return false
	default:
		return true
	}
}

// DeriveTaskProgress counts a master run's tasks by state and names the one
// currently running. Progress is deliberately computed from workflow_tasks, not
// from a step count: the `advance` step never executes, so any step-based
// percentage would read 5/6 on a fully completed run (§8.2).
func DeriveTaskProgress(tasks []domain.WorkflowTask) TaskProgress {
	var p TaskProgress
	p.Total = len(tasks)
	for _, t := range tasks {
		switch t.State {
		case domain.WorkflowTaskCompleted:
			p.Completed++
		case domain.WorkflowTaskRunning:
			p.Running++
			p.CurrentNumber, p.CurrentTitle = t.Ordinal, t.Title
			if t.ExecutionRunID != nil {
				p.CurrentRunID = *t.ExecutionRunID
			}
		case domain.WorkflowTaskBlocked:
			p.Blocked++
		case domain.WorkflowTaskEligible:
			p.Eligible++
		case domain.WorkflowTaskCancelled:
			p.Cancelled++
		}
	}
	return p
}

// DeriveStepProgress projects a run's steps onto the Board checklist, dropping
// the never-executed `advance` step so the checklist shows only steps that can
// actually happen.
func DeriveStepProgress(d RunDetail) []StepProgress {
	out := make([]StepProgress, 0, len(d.Steps))
	for _, s := range d.Steps {
		if s.Step.Kind == domain.WorkflowStepAdvance {
			continue
		}
		out = append(out, StepProgress{Kind: s.Step.Kind, State: s.Step.State})
	}
	return out
}
