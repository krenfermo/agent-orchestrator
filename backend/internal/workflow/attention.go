package workflow

import (
	stdctx "context"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// This file is Checkpoint 8P-E.13's single canonical answer to one question:
// "when AO has stopped, whose problem is it?"
//
// Before this file the answer was spread across three places that disagreed —
// lifecycle.go's two lookup maps, the ad-hoc durable_phase string each stop
// site happened to write, and the run state itself — and the default when all
// three came up empty was AttentionHuman with no reason and no action. That
// default produced the exact dead end this checkpoint exists to remove: a
// Board card saying "Te necesita" with nothing a person could actually do.
//
// The rules here are:
//
//  1. Every stop is named by a canonical reason code. A stop site records that
//     code as its checkpoint's durable_phase (see recordAttentionStop), so the
//     reason survives restart, is greppable, and never has to be re-derived
//     from prose.
//  2. Every canonical reason has exactly one AttentionDisposition, below.
//  3. human_decision requires a non-empty reason AND a non-empty action. A
//     disposition with no action is not a human decision by construction —
//     ClassifyAttention cannot express one.

// AttentionDisposition is what AO knows about one canonical stop reason: who
// owns it, and — when it is the user's — precisely what they have to do.
type AttentionDisposition struct {
	// SelfRemediable means AO can still make progress on this by itself
	// (a scheduled retry, a fix cycle, a queued branch). It must never reach
	// the user as a request for a decision.
	SelfRemediable bool
	// HumanAction is the concrete thing a person has to do. It is the whole
	// licence to classify a stop as human_decision: a reason with no action is
	// a reason AO has no advice about, and pretending otherwise is what
	// produced "needs attention" cards nobody could act on.
	HumanAction string
	// Phase overrides the derived lifecycle phase for a self-remediable stop,
	// so a run AO is retrying reads as "retrying" rather than the durable
	// run-state's flat "needs_attention". Empty means "keep the derived
	// phase".
	Phase Phase
}

// Canonical attention reason codes. These are the exact strings written into
// workflow_checkpoints.durable_phase by recordAttentionStop, and the exact
// strings the Board renders as `attentionReason`. They are deliberately a
// closed vocabulary: a stop site that wants a new one adds it here, which is
// also where it is forced to answer "and what does the user do about it?".
const (
	// Self-remediable: AO is still working on these.
	ReasonPlannerRetryScheduled = "planner_retry_scheduled"
	ReasonPlannerCapacityWait   = "planner_capacity_wait"
	ReasonReviewCapacityRetry   = reviewCapacityRetryDurablePhase
	ReasonBranchQueued          = branchWaitPhase
	ReasonVerifyFixReentry      = "verify_fix_reentry"

	// Human decisions: AO has genuinely stopped and a person must act.
	ReasonFixBudgetExhausted      = "fix_budget_exhausted"
	ReasonVerifyBudgetExhausted   = "verify_budget_exhausted"
	ReasonVerifyUnrepairable      = "verify_unrepairable"
	ReasonReviewStateAmbiguous    = "review_state_ambiguous"
	ReasonReviewerLaunchFailed    = "reviewer_launch_failed"
	ReasonFixWorkerBlocked        = "fix_worker_blocked"
	ReasonFixNoVerifiableChange   = "fix_no_verifiable_change"
	ReasonPlannerExhausted        = "planner_retries_exhausted"
	ReasonPlannerStartFailed      = "planner_start_failed"
	ReasonPlannerPolicyViolation  = "planner_policy_violation"
	ReasonPlannerAmbiguous        = "planner_ambiguous"
	ReasonChildNeedsAttention     = "child_needs_attention"
	ReasonChildFailed             = "child_failed"
	ReasonRecoveryInterrupted     = "recovery_interrupted"
	ReasonWorkerDispatchAmbiguous = "worker_dispatch_ambiguous"
	ReasonWorkerBlocked           = "worker_blocked"
	ReasonDispatchFailed          = "dispatch_failed"
	ReasonCapacityRetryExhausted  = string(domain.WorkflowErrorCapacityExhausted)
	ReasonQuestionHumanRequired   = "question_human_required"

	// unclassifiedStop is the honest label for a run durably parked in
	// needs_attention with no canonical reason recorded anywhere — an
	// older row, or a stop site not yet migrated to recordAttentionStop. It is
	// deliberately NOT a human decision: AO does not know what happened, so it
	// has no business telling a person it is their turn. See
	// ClassifyAttention's closing branch.
	unclassifiedStop = "unclassified_stop"
)

// attentionDispositions is the registry ClassifyAttention consults. Membership
// is the entire definition of "AO knows what this stop is".
var attentionDispositions = map[string]AttentionDisposition{
	// ---- Self-remediable ---------------------------------------------------
	ReasonPlannerRetryScheduled: {SelfRemediable: true, Phase: PhaseRetrying},
	ReasonPlannerCapacityWait:   {SelfRemediable: true, Phase: PhaseWaitingForCapacity},
	ReasonReviewCapacityRetry:   {SelfRemediable: true, Phase: PhaseWaitingForCapacity},
	ReasonBranchQueued:          {SelfRemediable: true, Phase: PhaseBlocked},
	ReasonVerifyFixReentry:      {SelfRemediable: true, Phase: PhaseFixing},

	// ---- Human decisions ---------------------------------------------------
	"dirty_worktree": {
		HumanAction: "Commit, stash or discard the local changes in the target repository, then continue this run.",
	},
	"autonomous_local_commit_failed": {
		HumanAction: "Resolve the failed local commit in the working repository; the branch stays locked to this run until you do.",
	},
	"autonomous_local_commit_deferred": {
		HumanAction: "Approve the local commit, or change the project's local-commit policy.",
	},
	ReasonWorkerDispatchAmbiguous: {
		HumanAction: "Confirm whether the worker session actually produced work, then continue or cancel this run.",
	},
	"review_dispatch_ambiguous": {
		HumanAction: "Confirm the state of the review, then continue or cancel this run.",
	},
	"fix_dispatch_ambiguous": {
		HumanAction: "Confirm the state of the fix, then continue or cancel this run.",
	},
	"work_provider_failure_needs_attention": {
		HumanAction: "Every configured provider attempt failed. Check provider auth/capacity, then continue this run.",
	},
	masterIntegrationFailureDurablePhase: {
		HumanAction: "Resolve the integration conflict for the completed task, then continue this run.",
	},
	ReasonFixBudgetExhausted: {
		HumanAction: "The review/fix budget is exhausted and the reviewer is still requesting changes. Read the latest findings and either raise the fix budget, apply the changes yourself, or cancel this run.",
	},
	ReasonVerifyBudgetExhausted: {
		HumanAction: "Verification kept failing after every automatic fix attempt. Read the verify output and either fix it yourself, adjust the verification commands, or cancel this run.",
	},
	ReasonVerifyUnrepairable: {
		HumanAction: "Verification failed for a reason no fix cycle can repair (its environment or its target changed under it). Inspect the worktree, then continue or cancel this run.",
	},
	ReasonReviewStateAmbiguous: {
		HumanAction: "AO could not prove what the review concluded. Inspect the reviewer session, then continue or cancel this run.",
	},
	ReasonReviewerLaunchFailed: {
		HumanAction: "The reviewer could not be launched. Check the reviewer provider's auth and installation, then continue this run.",
	},
	ReasonFixWorkerBlocked: {
		HumanAction: "The fix worker is waiting on input inside its own session. Answer it in the session, then continue this run.",
	},
	ReasonFixNoVerifiableChange: {
		HumanAction: "The fix worker finished without changing anything AO can verify. Inspect the worktree, then continue or cancel this run.",
	},
	ReasonPlannerExhausted: {
		HumanAction: "The planner failed on every allowed retry. Retry planning, simplify the objective, or switch the planner provider.",
	},
	ReasonPlannerStartFailed: {
		HumanAction: "The planner could not be started. Check the planner provider's auth and installation, then retry planning.",
	},
	ReasonPlannerPolicyViolation: {
		HumanAction: "The generated plan violated AO's plan policy. Rephrase the objective and retry planning.",
	},
	ReasonPlannerAmbiguous: {
		HumanAction: "The planner's state could not be recovered after a restart. Retry planning for this objective.",
	},
	ReasonChildNeedsAttention: {
		HumanAction: "The running task stopped and needs a decision. Open that task's run to see what it is waiting on.",
	},
	ReasonChildFailed: {
		HumanAction: "The running task failed. Open that task's run to see why, then retry or cancel this objective.",
	},
	ReasonRecoveryInterrupted: {
		HumanAction: "This step was interrupted by a daemon restart and AO cannot prove where it got to. Inspect it, then continue or cancel this run.",
	},
	ReasonDispatchFailed: {
		HumanAction: "The agent could not be dispatched. Check the provider's auth and installation, then continue this run.",
	},
	ReasonWorkerBlocked: {
		HumanAction: "The worker is waiting on input inside its own session (often an interactive trust or auth prompt). Answer it in the session, then continue this run.",
	},
	ReasonCapacityRetryExhausted: {
		HumanAction: "Every automatic retry ran out while the provider was still at capacity. Wait and continue this run, switch provider, or cancel it.",
	},
}

// attentionErrorClasses maps an attempt-level error class onto the same
// canonical vocabulary, for stops whose only durable carrier is an attempt row.
// Everything absent from this map — rate_limited, capacity_exhausted,
// transient, review_changes_requested — is a condition AO retries or fixes on
// its own and must never surface as a human decision.
var attentionErrorClasses = map[domain.WorkflowErrorClass]AttentionDisposition{
	domain.WorkflowErrorFixBudgetExhausted: attentionDispositions[ReasonFixBudgetExhausted],
	domain.WorkflowErrorAuth: {
		HumanAction: "Provider authentication failed. Reconnect the provider profile, then continue this run.",
	},
	domain.WorkflowErrorBinaryMissing: {
		HumanAction: "The provider CLI is not installed or not on PATH. Install it, then continue this run.",
	},
	domain.WorkflowErrorAmbiguousWorkerState: {
		HumanAction: "AO could not prove whether the work happened. Inspect the session, then continue or cancel this run.",
	},
	domain.WorkflowErrorWorkerTerminatedUnexpectedly: {
		HumanAction: "The worker session ended with no verifiable change. Inspect the worktree, then continue or cancel this run.",
	},
	domain.WorkflowErrorAgentStartFailed: {
		HumanAction: "The agent process launched but never got started (often an interactive trust prompt, a login, or a launch error). Open the session to see where it stopped, then continue or cancel this run.",
	},
	domain.WorkflowErrorPromptDeliveryFailed: {
		HumanAction: "AO could not deliver the prompt to the worker session. Check the session is still alive, then continue or cancel this run.",
	},
	domain.WorkflowErrorReviewerLaunchFailed: attentionDispositions[ReasonReviewerLaunchFailed],
	domain.WorkflowErrorIntegrationFailed:    attentionDispositions[masterIntegrationFailureDurablePhase],
	domain.WorkflowErrorCapacityExhausted: {
		HumanAction: "Every automatic retry ran out while the provider was still at capacity. Wait and continue this run, switch provider, or cancel it.",
	},
}

// AttentionVerdict is ClassifyAttention's whole output: the classification, the
// canonical reason, the human action (empty unless Attention is
// AttentionHuman), and an optional phase override for a self-remediable stop.
type AttentionVerdict struct {
	Attention Attention
	Reason    string
	Action    string
	Phase     Phase
}

// ClassifyAttention is Checkpoint 8P-E.13 Phase 2's single canonical decision
// function. Every attention classification in AO goes through it.
//
// The invariant it enforces, and the reason it is a function rather than three
// scattered map lookups:
//
//	Attention == AttentionHuman  =>  Reason != "" AND Action != ""
//
// There is no code path through this function that can return AttentionHuman
// with either field empty. A stop AO cannot name, or can name but has no
// remedy for, comes back as AttentionInternal — truthful about the fact that
// AO stopped, honest about the fact that it has nothing to ask.
//
// Evaluation order matters and mirrors docs/workflow-lifecycle-mapping.md §5:
// terminal first, then a real question addressed to the user, then the run's
// own durable stop record.
func ClassifyAttention(d RunDetail, questions []domain.WorkflowQuestion, phase Phase) AttentionVerdict {
	if phase.Terminal() {
		return AttentionVerdict{Attention: AttentionNone}
	}

	// A question classified human_required outranks the run state: it is a
	// real, answerable request addressed to the user, and it always carries its
	// own action (the question text itself).
	for _, q := range questions {
		if q.State == domain.QuestionStateHumanRequired {
			return AttentionVerdict{
				Attention: AttentionHuman,
				Reason:    ReasonQuestionHumanRequired,
				Action:    questionAction(q),
			}
		}
	}

	if phase != PhaseNeedsAttention {
		// Everything short of needs_attention that AO is actively handling — a
		// changes_requested rest, a capacity wait with a scheduled retry, a
		// branch queue, a scheduled retry — is internal. Surfaced so the user
		// can see what AO is doing, never as a request for help.
		switch phase {
		case PhaseWaitingForCapacity, PhaseBlocked, PhaseRetrying:
			return AttentionVerdict{Attention: AttentionInternal, Reason: phase.String()}
		case PhaseFixing:
			return AttentionVerdict{Attention: AttentionInternal, Reason: "review_changes_requested"}
		}
		return AttentionVerdict{Attention: AttentionNone}
	}

	if reason, disp, ok := resolveAttentionReason(d); ok {
		if disp.SelfRemediable {
			return AttentionVerdict{
				Attention: AttentionInternal,
				Reason:    reason,
				Phase:     disp.Phase,
			}
		}
		if disp.HumanAction != "" {
			return AttentionVerdict{
				Attention: AttentionHuman,
				Reason:    reason,
				Action:    disp.HumanAction,
			}
		}
		// Named, but AO has no remedy to offer. Report the name; do not
		// escalate a stop the user has not been told how to resolve.
		return AttentionVerdict{Attention: AttentionInternal, Reason: reason}
	}

	// needs_attention with nothing canonical recorded. The pre-8P-E.13 code
	// returned AttentionHuman with an empty reason here; that is the dead end
	// this checkpoint removes.
	return AttentionVerdict{Attention: AttentionInternal, Reason: unclassifiedStop}
}

// resolveAttentionReason finds the canonical reason for a stopped run, in the
// order the durable carriers are trustworthy: the newest checkpoint's
// durable_phase (written deliberately by a stop site), then the newest
// attempt's error_class (written by the dispatch/observation machinery).
//
// A durable_phase that is not a canonical reason (e.g. "review_observed", the
// generic phase every review observation writes) is deliberately NOT accepted:
// it names what AO was doing, not why it stopped, and surfacing it as an
// attention reason is how the Board ended up showing "review_observed" as
// though it were something a person could act on.
func resolveAttentionReason(d RunDetail) (string, AttentionDisposition, bool) {
	if disp, ok := attentionDispositions[d.LatestCheckpointPhase]; ok {
		return d.LatestCheckpointPhase, disp, true
	}
	if class := latestErrorClass(d); class != "" {
		if disp, ok := attentionErrorClasses[class]; ok {
			return string(class), disp, true
		}
	}
	// Legacy carrier, for runs that stopped BEFORE this checkpoint shipped.
	//
	// Until 8P-E.13, an exhausted fix budget recorded itself as the bare
	// next_action literal "human_attention" — a word naming an audience, not a
	// reason — and tried to carry its error class on the review step's latest
	// attempt row. Review dispatch never creates attempt rows, so that class
	// was written to nothing. The result was wf-3220567f: a run with 23
	// checkpoints, none of which said why it stopped.
	//
	// Those rows are durable and still on disk. Recognising the one literal the
	// old code wrote lets an already-stranded run explain itself on the next
	// read, instead of needing a data migration to rewrite history. Nothing
	// writes this literal any more, so the branch is read-only compatibility.
	if strings.TrimSpace(d.NextAction) == legacyFixBudgetNextAction {
		return ReasonFixBudgetExhausted, attentionDispositions[ReasonFixBudgetExhausted], true
	}
	return "", AttentionDisposition{}, false
}

// legacyFixBudgetNextAction is the pre-8P-E.13 next_action literal described in
// resolveAttentionReason.
const legacyFixBudgetNextAction = "human_attention"

// recordAttentionStop writes the durable record that makes a needs_attention
// run explainable: one append-only checkpoint whose durable_phase is a
// canonical reason code from the registry above, and whose next_action carries
// the human-readable detail of this particular occurrence.
//
// Every site that moves a run into needs_attention calls this. It is the whole
// mechanism by which the Phase 2 invariant becomes achievable at read time:
// ClassifyAttention can only name a stop that a stop site bothered to name.
//
// Best-effort by design — matching scheduleWake's own "observers don't invent
// failures" convention. A run that stops AND fails to record why is strictly
// better off than one that fails to stop.
func (c *Coordinator) recordAttentionStop(ctx stdctx.Context, run domain.WorkflowRun, stepID *string, reason, detail string) {
	if reason == "" {
		return
	}
	var step *string
	if stepID != nil && *stepID != "" {
		v := *stepID
		step = &v
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: step,
		ProjectID:      run.ProjectID,
		NextAction:     strings.TrimSpace(detail),
		DurablePhase:   reason,
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      c.clock(),
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: recording attention stop failed", "run", run.ID, "reason", reason, "err", err)
	}
}

// stopIsSelfRemediable reports whether a run currently parked in
// needs_attention is one AO can still drive forward by itself — the guard
// maybeScheduleAutonomousHeartbeat needs so a retryable stop keeps its
// heartbeat instead of going silent forever.
//
// It reads only the two durable carriers (newest checkpoint, newest attempt),
// never GetRun, so it is safe to call from inside the reconcile path GetRun
// itself drives.
func (c *Coordinator) stopIsSelfRemediable(ctx stdctx.Context, run domain.WorkflowRun) bool {
	d := RunDetail{Run: run}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if !cp.CreatedAt.Before(d.LatestCheckpointAt) {
			d.LatestCheckpointPhase = cp.DurablePhase
			d.LatestCheckpointAt = cp.CreatedAt
		}
	}
	if steps, serr := c.store.ListWorkflowSteps(ctx, run.ID); serr == nil {
		for _, s := range steps {
			attempts, aerr := c.store.ListWorkflowAttempts(ctx, s.ID)
			if aerr != nil {
				return false
			}
			d.Steps = append(d.Steps, StepDetail{Step: s, Attempts: attempts})
		}
	}
	_, disp, ok := resolveAttentionReason(d)
	return ok && disp.SelfRemediable
}
