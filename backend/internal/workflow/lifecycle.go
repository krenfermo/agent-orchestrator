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

// The fourteen derived lifecycle phases.
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
	// PhaseRetrying is Checkpoint 8P-E.13's missing vocabulary: AO has hit a
	// failure it is allowed to retry, has a bounded retry durably scheduled,
	// and is neither running nor asking for help. Before this phase existed
	// every such state had to borrow either "waiting" (which hid the failure)
	// or "needs_attention" (which invented a human decision) — the planner
	// timeout misreport this checkpoint exists to end.
	PhaseRetrying Phase = "retrying"
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
	// CanContinue is the authoritative answer to "would POST /continue do
	// anything for this run right now". The frontend must not re-derive it: the
	// rules are step states, run state and the stop's own disposition, all of
	// which live here, and a second implementation in React would drift.
	CanContinue bool
	// AttentionWorkflowID names the run a human should actually act on when this
	// run's stop is a MIRROR of another run's (a master reflecting its child).
	// Empty for a stop the run owns itself. It is what lets the objective's
	// "child_needs_attention" card send the user — and a Continue — to the exact
	// child, instead of to the parent that merely reported it.
	AttentionWorkflowID string
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
	// Failed counts tasks whose child run ended failed/cancelled (Checkpoint
	// 8P-E.13). A non-zero value is why a master run with no running task is
	// nonetheless not going to finish.
	Failed int
	// NeedsAttention counts tasks parked on something only a person can decide
	// — today, an integration conflict (migration 0130). It is kept separate
	// from Failed on purpose: the work is very likely fine and one human
	// decision releases it, which is a different card and a different action
	// from a task that ended badly.
	NeedsAttention int
	// CurrentNumber/CurrentTitle/CurrentRunID describe the task currently
	// running, when one is. CurrentNumber is the task's own
	// workflow_tasks.ordinal, so "Task 2 of 7" is a fact, not an index.
	CurrentNumber int64
	CurrentTitle  string
	CurrentRunID  string
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
	verdict := ClassifyAttention(d, in.Questions, life.Phase)
	life.Attention, life.AttentionReason, life.AttentionAction = verdict.Attention, verdict.Reason, verdict.Action
	// Checkpoint 8P-E.13: a self-remediable stop reports the phase of what AO
	// is actually doing about it (retrying, waiting for capacity, queued behind
	// a branch) rather than the durable run state's flat "needs_attention".
	// The run row is unchanged — this is a derivation, exactly like every other
	// value in this struct.
	if verdict.Phase != "" {
		life.Phase = verdict.Phase
	}
	life.CanContinue = canContinueRun(d, life.Phase)
	if verdict.Reason == ReasonChildNeedsAttention || verdict.Reason == ReasonChildFailed {
		life.AttentionWorkflowID = DeriveTaskProgress(d.Tasks).CurrentRunID
	}
	return life
}

// canContinueRun reports whether POST /continue can advance this run.
//
// Three rules, in order:
//
//  1. A terminal run can never be continued. ContinueRun itself answers
//     ErrAlreadyTerminal, so a button for it is a button that only ever errors.
//  2. A stopped run can be continued unless its own recorded stop says the
//     remedy is something else entirely (Nonrecoverable — "start a fresh run",
//     "retry planning"). This is the recoverable-needs_attention case the
//     Reanudar control exists for.
//  3. A run that is NOT stopped has exactly one meaningful continue: the
//     explicit "the work step finished, hand off to review" step. Everything
//     else in flight is either already moving or has a durable wake that will
//     move it, and offering Continue there invites a person to intervene in
//     something AO is handling — which is the misreport the lifecycle work has
//     spent several checkpoints removing.
func canContinueRun(d RunDetail, phase Phase) bool {
	if phase.Terminal() {
		return false
	}
	switch d.Run.State {
	case domain.WorkflowRunCompleted, domain.WorkflowRunFailed, domain.WorkflowRunCancelled:
		return false
	case domain.WorkflowRunNeedsAttention:
		_, disp, ok := resolveAttentionReason(d)
		return !ok || !disp.Nonrecoverable
	}
	return reviewHandoffPending(d)
}

// reviewHandoffPending reports the one non-stopped state a manual continue
// legitimately advances: a completed work step with its review step still
// waiting to be dispatched.
func reviewHandoffPending(d RunDetail) bool {
	var workDone, reviewPending bool
	for _, s := range d.Steps {
		switch s.Step.Kind {
		case domain.WorkflowStepWork:
			workDone = s.Step.State == domain.WorkflowStepCompleted
		case domain.WorkflowStepReview:
			reviewPending = s.Step.State == domain.WorkflowStepPending || s.Step.State == domain.WorkflowStepReady
		}
	}
	return workDone && reviewPending
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
			// Checkpoint 8P-E.13: a planner parked by retryPlanOrFail is
			// reported as retrying, not as the plain "planning" it would
			// otherwise borrow. Either signal counts, for the same reason the
			// branch-lock case above accepts either: the wake write is
			// best-effort, so the checkpoint can exist with no wake row at all.
			if d.WaitReason == string(wake.ReasonTransientRetry) || d.LatestCheckpointPhase == ReasonPlannerRetryScheduled {
				return PhaseRetrying
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
		case domain.WorkflowTaskFailed:
			p.Failed++
		case domain.WorkflowTaskNeedsAttention:
			p.NeedsAttention++
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
