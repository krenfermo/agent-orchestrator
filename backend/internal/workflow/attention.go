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
	// Nonrecoverable marks a stop that POST /continue cannot do anything about,
	// because the remedy is a different action entirely (start a fresh run,
	// retry planning). It is the whole basis of the run detail's authoritative
	// canContinue flag: offering "Reanudar" for one of these is offering a
	// button that provably does nothing.
	Nonrecoverable bool
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
	// ReasonPromptTransportRetry is a fix prompt the terminal transport refused
	// before any of it reached the agent (Checkpoint 8P-E.13C). AO re-sends it
	// itself, within a bounded budget; nobody has a decision to make about
	// "command too long".
	ReasonPromptTransportRetry = fixTransportRetryPhase
	// ReasonReviewerLaunchRetry is a reviewer launch that failed transiently
	// before any reviewer session existed (review_launch_recovery.go). AO closed
	// out the partial review_run, scheduled a durable wake and will launch the
	// reviewer again itself, within a bounded budget — a temporary spawn or
	// runtime failure is not a decision anyone has to make.
	ReasonReviewerLaunchRetry = "reviewer_launch_retry"
	// ReasonVerifyFreshReviewRequired is an authorized verification recovery
	// whose approval no longer describes the workspace (verify_recovery.go). AO
	// re-asks the reviewer about the workspace as it actually stands and then
	// verifies THAT, once, by itself — nobody has a decision to make about
	// "the approval AO is holding is older than the code".
	ReasonVerifyFreshReviewRequired = verifyFreshReviewRequiredPhase
	// ReasonIncidentDiagnosisCapacityWait is an Incident Advisor investigation
	// AO wants to run and currently cannot, because no provider has capacity.
	// Self-remediable by construction: a durable wake is scheduled and AO comes
	// back to it, and nobody has a decision to make about "the provider is rate
	// limited". It never parks the RUN — the run is already stopped for its own
	// reason, and an investigation that cannot start yet is a fact about the
	// investigation, not a second fact about the run.
	ReasonIncidentDiagnosisCapacityWait = "incident_diagnosis_capacity_wait"

	// Human decisions: AO has genuinely stopped and a person must act.
	ReasonFixBudgetExhausted    = "fix_budget_exhausted"
	ReasonVerifyBudgetExhausted = "verify_budget_exhausted"
	ReasonVerifyUnrepairable    = "verify_unrepairable"
	// The three reasons below are verify_context.go's precise replacements for
	// ReasonVerifyUnrepairable when the failure was AO's own verification
	// infrastructure rather than the code: a verifier configuration AO cannot
	// repair, a verifier binary that is not usable, and a verifier that could
	// not be run to completion for an environmental reason. None of them is
	// ever handed to a fix worker, and each names a different thing to do.
	ReasonVerifyConfigInvalid   = "verify_config_invalid"
	ReasonVerifyToolUnavailable = "verify_tool_unavailable"
	ReasonVerifyInfraFailed     = "verify_infrastructure_failed"
	// ReasonVerifyRecoveryExhausted is verify_recovery.go's bound, reached: a
	// person has now reopened this run's terminal verification failure
	// maxVerifyRecoveryAttempts times after correcting something, and it has
	// failed on AO's own infrastructure every single time. The remedy is no
	// longer "continue it again", which is exactly why it needs its own name.
	ReasonVerifyRecoveryExhausted = "verify_recovery_exhausted"
	// ReasonVerifyWorkspaceUnattributable is the refusal half of
	// ReasonVerifyFreshReviewRequired: an authorized verification recovery found
	// the workspace no longer matching the approval, AND could not attribute the
	// difference to this task's own uncommitted work. Distinct from
	// ReasonVerifyUnrepairable because the remedy is distinct — the question is
	// not "why did the checks fail", it is "who changed this worktree" — and
	// because naming it separately is what keeps a plain verify_workspace_changed
	// OUTSIDE a recovery reading exactly as it always did.
	ReasonVerifyWorkspaceUnattributable = "verify_workspace_unattributable"
	ReasonReviewStateAmbiguous          = "review_state_ambiguous"
	// ReasonFixDispatchAmbiguous is a fix cycle whose delivery to the worker
	// session AO could not prove either way after a restart — and only after
	// fix_delivery_recovery.go has exhausted every durable fact that could have
	// proven it. The rows already on disk from before that recovery existed
	// carry this same string, so they keep their meaning.
	ReasonFixDispatchAmbiguous = "fix_dispatch_ambiguous"
	ReasonReviewerLaunchFailed = "reviewer_launch_failed"
	// The four reasons below are review_launch_recovery.go's precise
	// replacements for the flat ReasonReviewerLaunchFailed: a reviewer that
	// could not start because its credentials were rejected, because its CLI is
	// not installed, because the configuration/policy forbids the launch, and
	// because every automatic retry of an otherwise transient failure ran out.
	// Each names a different thing for a person to do, which is the entire
	// point of the vocabulary in this file.
	ReasonReviewerAuthInvalid            = "reviewer_auth_invalid"
	ReasonReviewerBinaryMissing          = "reviewer_binary_missing"
	ReasonReviewerLaunchUnsupported      = "reviewer_launch_unsupported"
	ReasonReviewerLaunchRetriesExhausted = "reviewer_launch_retries_exhausted"
	ReasonFixWorkerBlocked               = "fix_worker_blocked"
	ReasonFixNoVerifiableChange          = "fix_no_verifiable_change"
	// ReasonFixCycleNotStarted is a fix cycle whose prompt provably reached the
	// worker session but which the worker never began: no activity, no turn
	// boundary and no signal postdating the dispatch, for longer than
	// fixCyclePickupTimeout.
	//
	// It is deliberately NOT ReasonFixNoVerifiableChange. That reason asserts
	// the worker ran and produced nothing — a verdict about the work — and AO
	// has no evidence for it here. The two need separate names because they
	// need separate remedies: one is a person reading a diff, the other is a
	// cycle that has simply not been picked up and can be re-delivered.
	ReasonFixCycleNotStarted = "fix_cycle_not_started"
	// ReasonFixPromptNotSubmitted is a fix prompt that provably reached the
	// worker's composer and provably did not leave it. Distinct from
	// ReasonFixCycleNotStarted, which says only that the worker has been silent:
	// here AO can see the text sitting in the draft, so the remedy is a submit,
	// never another send — the payload is already there and re-sending appends a
	// second copy.
	ReasonFixPromptNotSubmitted  = "fix_prompt_not_submitted"
	ReasonPlannerExhausted       = "planner_retries_exhausted"
	ReasonPlannerStartFailed     = "planner_start_failed"
	ReasonPlannerPolicyViolation = "planner_policy_violation"
	ReasonPlannerAmbiguous       = "planner_ambiguous"
	ReasonChildNeedsAttention    = "child_needs_attention"
	ReasonChildFailed            = "child_failed"
	// ReasonTaskParked is the objective's own stop when one or more of its
	// tasks are parked in needs_attention (migration 0130) and nothing else in
	// the plan can move. It is deliberately distinct from
	// ReasonChildNeedsAttention: the child run finished perfectly well, and
	// what is stopped is the integration of its result.
	ReasonTaskParked              = "task_integration_attention"
	ReasonRecoveryInterrupted     = "recovery_interrupted"
	ReasonWorkerDispatchAmbiguous = "worker_dispatch_ambiguous"
	ReasonWorkerBlocked           = "worker_blocked"
	ReasonDispatchFailed          = "dispatch_failed"
	// ReasonWorkerLaunchRetry is a worker spawn that failed transiently before
	// any worker session existed and that AO has already scheduled its own
	// bounded retry for (worker_launch_recovery.go). Self-remediable: it is
	// recorded so the ledger explains the pause, never so a person acts on it,
	// and the run is deliberately NOT parked while it is outstanding.
	ReasonWorkerLaunchRetry = "worker_launch_retry"
	// ReasonWorkerLaunchRetriesExhausted is that same transient failure after
	// every automatic retry has been used. Distinct from ReasonDispatchFailed
	// because the honest thing to tell a person is different: nothing about the
	// provider's configuration is known to be wrong, the launch just would not
	// take.
	ReasonWorkerLaunchRetriesExhausted = "worker_launch_retries_exhausted"
	ReasonCapacityRetryExhausted       = string(domain.WorkflowErrorCapacityExhausted)
	ReasonQuestionHumanRequired        = "question_human_required"

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
	ReasonPlannerRetryScheduled:         {SelfRemediable: true, Phase: PhaseRetrying},
	ReasonPlannerCapacityWait:           {SelfRemediable: true, Phase: PhaseWaitingForCapacity},
	ReasonReviewCapacityRetry:           {SelfRemediable: true, Phase: PhaseWaitingForCapacity},
	ReasonBranchQueued:                  {SelfRemediable: true, Phase: PhaseBlocked},
	ReasonVerifyFixReentry:              {SelfRemediable: true, Phase: PhaseFixing},
	ReasonPromptTransportRetry:          {SelfRemediable: true, Phase: PhaseRetrying},
	ReasonReviewerLaunchRetry:           {SelfRemediable: true, Phase: PhaseRetrying},
	ReasonWorkerLaunchRetry:             {SelfRemediable: true, Phase: PhaseRetrying},
	ReasonIncidentDiagnosisCapacityWait: {SelfRemediable: true, Phase: PhaseWaitingForCapacity},

	ReasonVerifyFreshReviewRequired: {SelfRemediable: true, Phase: PhaseReviewing},

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
	ReasonFixDispatchAmbiguous: {
		HumanAction: "AO could not prove whether the reviewer's findings reached the worker session. Open that session, check whether it received them, then continue this run (AO re-checks its own evidence first and will resume by itself if the answer becomes provable) or cancel it.",
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
	ReasonVerifyConfigInvalid: {
		HumanAction: "AO's verification configuration for this project is not a valid invocation (wrong directory or wrong command). Correct the project's verification commands, then continue this run.",
	},
	ReasonVerifyToolUnavailable: {
		HumanAction: "The verification tool is not installed or not on PATH for the daemon. Install it (or fix PATH), then continue this run.",
	},
	ReasonVerifyInfraFailed: {
		HumanAction: "Verification could not be run to completion on this machine (a runtime or resource failure, not a code defect). Check the host, then continue this run.",
	},
	ReasonVerifyRecoveryExhausted: {
		Nonrecoverable: true,
		HumanAction:    "Verification has been reopened the maximum number of times and still fails on AO's own verification infrastructure rather than on the code. Read the latest verify output, correct the verification configuration or the host, then start a fresh run — or cancel this one.",
	},
	ReasonVerifyWorkspaceUnattributable: {
		HumanAction: "Verification was reopened, but the worktree no longer matches what review approved and AO cannot attribute the difference to this task's own uncommitted work (its branch or its HEAD moved). Inspect the worktree, restore it or commit the intended state, then continue or cancel this run.",
	},
	ReasonReviewStateAmbiguous: {
		HumanAction: "AO could not prove what the review concluded. Inspect the reviewer session, then continue or cancel this run.",
	},
	ReasonReviewerLaunchFailed: {
		HumanAction: "The reviewer could not be launched. Check the reviewer provider's auth and installation, then continue this run.",
	},
	ReasonReviewerAuthInvalid: {
		HumanAction: "The reviewer provider rejected its credentials. Reconnect that provider's profile, then continue this run.",
	},
	ReasonReviewerBinaryMissing: {
		HumanAction: "The reviewer's CLI is not installed or not on PATH. Install it, then continue this run.",
	},
	ReasonReviewerLaunchUnsupported: {
		HumanAction: "The reviewer's configuration is not supported for this launch. Fix the reviewer configuration or the execution policy, then continue this run.",
	},
	ReasonReviewerLaunchRetriesExhausted: {
		HumanAction: "The reviewer failed to launch on every automatic retry. Check the reviewer provider's process/runtime, then continue this run.",
	},
	ReasonFixWorkerBlocked: {
		HumanAction: "The fix worker is waiting on input inside its own session. Answer it in the session, then continue this run.",
	},
	ReasonFixNoVerifiableChange: {
		HumanAction: "The fix worker finished without changing anything AO can verify. Inspect the worktree, then continue or cancel this run.",
	},
	ReasonFixCycleNotStarted: {
		HumanAction: "The reviewer's findings reached the worker session but the worker never started on them. Check that the session is alive and responsive, then continue this run — AO re-checks its own evidence first and re-delivers the cycle by itself when it can prove that is safe.",
	},
	ReasonFixPromptNotSubmitted: {
		HumanAction: "The reviewer's findings are sitting unsubmitted in the worker session's composer. Continue this run and AO will submit what is already there (it never re-sends the text, so the prompt cannot be duplicated); if that keeps failing, open the session and submit it yourself.",
	},
	ReasonPlannerExhausted: {
		Nonrecoverable: true,
		HumanAction:    "The planner failed on every allowed retry. Retry planning, simplify the objective, or switch the planner provider.",
	},
	ReasonPlannerStartFailed: {
		Nonrecoverable: true,
		HumanAction:    "The planner could not be started. Check the planner provider's auth and installation, then retry planning.",
	},
	ReasonPlannerPolicyViolation: {
		Nonrecoverable: true,
		HumanAction:    "The generated plan violated AO's plan policy. Rephrase the objective and retry planning.",
	},
	ReasonPlannerAmbiguous: {
		Nonrecoverable: true,
		HumanAction:    "The planner's state could not be recovered after a restart. Retry planning for this objective.",
	},
	ReasonChildNeedsAttention: {
		HumanAction: "The running task stopped and needs a decision. Open that task's run to see what it is waiting on.",
	},
	ReasonChildFailed: {
		HumanAction: "The running task failed. Open that task's run to see why, then retry or cancel this objective.",
	},
	ReasonTaskParked: {
		HumanAction: "A task passed review and verification but its work could not be integrated automatically. Resolve the conflict named on that task, then resume it.",
	},
	ReasonRecoveryInterrupted: {
		HumanAction: "This step was interrupted by a daemon restart and AO cannot prove where it got to. Inspect it, then continue or cancel this run.",
	},
	ReasonDispatchFailed: {
		HumanAction: "The agent could not be dispatched. Check the provider's auth and installation, then continue this run.",
	},
	ReasonWorkerLaunchRetriesExhausted: {
		HumanAction: "The worker failed to start on every automatic retry, without naming a configuration problem. Check the terminal/runtime and the provider's process, then continue this run — AO reopens the dispatch and starts exactly one worker.",
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
	// Human-owned reasons only, and at most one per run and type however often
	// this is re-entered — see notifyAttentionStop. Unconditional on the write
	// above for the same reason that write is best-effort: the run row is what
	// says the run stopped, the checkpoint only says why, and a person is no
	// less needed when AO failed to write down the explanation.
	c.notifyAttentionStop(ctx, run, reason, detail)
}

// recordAttentionStopOnce is recordAttentionStop for a stop a caller may
// re-derive on every pass (Checkpoint 8P-E.13B).
//
// reconcileMasterTasks runs from the READ path — every GetRun, so every board
// poll — and re-evaluates a completed child's promotion each time. When that
// promotion fails deterministically, the run's stop is the same stop it already
// recorded, and writing it again on each poll produced hundreds of identical
// rows describing one unchanged condition. This records the stop the first
// time, then stays quiet for as long as nothing about it has changed.
//
// The comparison is deliberately against the run's NEWEST checkpoint only, not
// against the whole ledger: any other durable event landing in between means
// something did happen, and this stop is then worth recording again as the
// current state of the run.
func (c *Coordinator) recordAttentionStopOnce(ctx stdctx.Context, run domain.WorkflowRun, stepID *string, reason, detail string) {
	if reason == "" {
		return
	}
	if c.attentionStopIsCurrent(ctx, run, reason, detail) {
		return
	}
	c.recordAttentionStop(ctx, run, stepID, reason, detail)
}

// attentionStopIsCurrent reports whether the run's newest checkpoint already
// says exactly this. A read error answers "no": failing to read is never a
// reason to lose a stop record.
func (c *Coordinator) attentionStopIsCurrent(ctx stdctx.Context, run domain.WorkflowRun, reason, detail string) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil || len(cps) == 0 {
		return false
	}
	newest := cps[0]
	for _, cp := range cps[1:] {
		if !cp.CreatedAt.Before(newest.CreatedAt) {
			newest = cp
		}
	}
	return newest.DurablePhase == reason && newest.NextAction == strings.TrimSpace(detail)
}

// stopIsHumanOwned reports whether a run parked in needs_attention stopped for
// a reason that names something only a person can do (a HumanAction in the
// registry above). It is the guard maybeScheduleAutonomousHeartbeat and the
// master reconcile need: a stop AO owns keeps its heartbeat instead of going
// silent forever.
//
// It reads only the two durable carriers (newest checkpoint, newest attempt),
// never GetRun, so it is safe to call from inside the reconcile path GetRun
// itself drives.
//
// It is deliberately NOT the negation of "self-remediable". There are three
// kinds of stop, not two: one AO retries by itself, one a person must resolve,
// and one AO could not name at all (unclassifiedStop). ClassifyAttention
// already refuses to bill an unnamed stop to the user — it reports
// AttentionInternal — so the resume machinery must not treat it as a human
// decision either. Before Checkpoint 8P-E.13A.2 it did, by asking only
// "is this self-remediable?", and an unnamed stale stop therefore ended
// autonomy exactly as hard as an exhausted fix budget.
func (c *Coordinator) stopIsHumanOwned(ctx stdctx.Context, run domain.WorkflowRun) bool {
	_, disp, ok := c.stopReason(ctx, run)
	return ok && disp.HumanAction != ""
}

// attentionClearedPhase is the durable phase of the checkpoint clearResolvedStop
// writes. It is deliberately not a canonical attention reason: it records a
// resume, not a stop, and a run that stops again always writes its own reason.
const attentionClearedPhase = "attention_cleared"

// clearResolvedStop un-parks a run whose needs_attention is now stale, and is
// Checkpoint 8P-E.13A.2's answer to the deadlock this file's vocabulary made
// visible but did not resolve.
//
// The situation it exists for: a run parks in needs_attention on something AO
// remediates itself (a queued branch, and — for rows written before the branch
// wait became a Waiting state — any legacy misfiling of the same), AO then
// genuinely remediates it, and nothing ever writes the run row back. That
// matters far beyond cosmetics, because needs_attention is not a state the
// forward transitions can leave: ValidWorkflowRunTransition allows
// needs_attention -> running only, so observeWorkStep's completion transition
// (-> waiting) is silently dropped as invalid and the run stays stopped with a
// completed work step underneath it.
//
// The rule is narrow on purpose. Clearing requires BOTH:
//
//  1. the caller has just proven forward progress — this function is only ever
//     called from a site that has already made the dispatch/observation
//     succeed, never speculatively from a poll; and
//  2. the recorded stop is not a human decision. A stop with a HumanAction
//     (fix_budget_exhausted, dirty_worktree, an auth failure, a child that
//     needs a decision) is left exactly where it is.
//
// evidence is the caller's one-line statement of what it proved, recorded on
// the checkpoint so the resume is auditable rather than a state change nobody
// can account for afterwards.
func (c *Coordinator) clearResolvedStop(ctx stdctx.Context, run domain.WorkflowRun, evidence string) domain.WorkflowRun {
	if run.State != domain.WorkflowRunNeedsAttention {
		return run
	}
	reason, disp, ok := c.stopReason(ctx, run)
	if ok && disp.HumanAction != "" {
		return run
	}
	if !ok || reason == "" {
		reason = unclassifiedStop
	}
	return c.unparkRun(ctx, run, reason, evidence)
}

// unparkRun performs the needs_attention -> running write and its audit
// checkpoint. Split out from clearResolvedStop so clearMirroredChildStop can
// reuse the exact same write for the one human-owned reason that is a mirror
// of someone else's state rather than a decision of this run's own.
func (c *Coordinator) unparkRun(ctx stdctx.Context, run domain.WorkflowRun, reason, evidence string) domain.WorkflowRun {
	now := c.clock()
	if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID,
		domain.WorkflowRunNeedsAttention, domain.WorkflowRunRunning, now); err != nil {
		if c.log != nil {
			c.log.Warn("workflow: clearing a resolved stop failed", "run", run.ID, "reason", reason, "err", err)
		}
		return run
	}
	run.State = domain.WorkflowRunRunning
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		ProjectID:      run.ProjectID,
		NextAction:     "resumed automatically: " + evidence + " (cleared stop: " + reason + ")",
		DurablePhase:   attentionClearedPhase,
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: recording a cleared stop failed", "run", run.ID, "reason", reason, "err", err)
	}
	return run
}

// reconcileMirroredChildStop is the single authoritative rule for the one
// attention reason a master run does not own: ReasonChildNeedsAttention is a
// MIRROR of a child's state, recorded so the parent does not go silent while
// its task is stopped, and it is therefore only ever as true as the child's
// state is RIGHT NOW.
//
// Before this rule the mirror was historical: reconcileMasterTasksOnce wrote it
// when a child stopped on a human decision and cleared it only from inside the
// "this child can advance" branch, which a parent could stop reaching at all —
// the mirror is a human-owned reason, so it killed the parent's own autonomous
// heartbeat (maybeScheduleAutonomousHeartbeat), and the heartbeat was the only
// thing that would ever have called back to notice the child had recovered.
// The result is the incident this rule exists to remove: child running, parent
// durably "Te necesita — child_needs_attention", every later task blocked, and
// nothing short of a human clicking Continue on the PARENT to break the tie.
//
// The rule is: derive the mirror from the children's current durable states, on
// every reconcile pass.
//
//   - No child of this plan is currently stopped on a decision a person has to
//     make => the mirror is false => unpark the parent.
//   - Any child still stopped on a human-owned reason => the mirror is still
//     true => leave the parent exactly where it is.
//   - Any child terminally FAILED => not this rule's business. A failure is not
//     a recovery, and reconcileMasterTasksOnce's own failure branch supersedes
//     the mirror with ReasonChildFailed. Never clear it here.
//
// Everything else about the parent is untouched. A parent stopped for any other
// reason — a failed integration, an exhausted planner, a dirty worktree, a
// child that failed — does not match the reason check and is left alone; this
// is the whole guarantee that "unrelated parent attention is never cleared".
//
// Idempotent by construction: the clear moves the run out of needs_attention
// and writes an attention_cleared checkpoint, so a repeat pass fails both the
// state check and the reason check and does nothing at all.
func (c *Coordinator) reconcileMirroredChildStop(ctx stdctx.Context, run domain.WorkflowRun, tasks []domain.WorkflowTask) domain.WorkflowRun {
	if run.State != domain.WorkflowRunNeedsAttention {
		return run
	}
	if !c.stopIsMirroredChildStop(ctx, run) {
		return run
	}
	for _, task := range tasks {
		if task.ExecutionRunID == nil {
			continue
		}
		child, ok, err := c.store.GetWorkflowRun(ctx, *task.ExecutionRunID)
		if err != nil || !ok {
			// Cannot prove the mirror is stale, so do not act on the guess.
			// Failing to read is never a licence to un-park a stopped run.
			return run
		}
		if child.State == domain.WorkflowRunFailed {
			return run
		}
		if child.State == domain.WorkflowRunNeedsAttention && c.stopIsHumanOwned(ctx, child) {
			return run
		}
	}
	return c.unparkRun(ctx, run, ReasonChildNeedsAttention,
		"no task of this objective is waiting on a decision any more")
}

// stopIsMirroredChildStop reports whether a stopped run's current durable stop
// is the child mirror rather than a decision of its own. Two callers need this
// exact distinction: reconcileMirroredChildStop, to know what it may clear, and
// maybeScheduleAutonomousHeartbeat, to know that this one human-owned reason
// must NOT end the parent's heartbeat — the person acts on the child, and the
// parent has to keep watching in order to notice when they have.
func (c *Coordinator) stopIsMirroredChildStop(ctx stdctx.Context, run domain.WorkflowRun) bool {
	reason, _, ok := c.stopReason(ctx, run)
	return ok && reason == ReasonChildNeedsAttention
}

// clearIntegrationStop releases a master run from an integration failure it has
// since recovered from (Checkpoint 8P-E.13B). Like clearMirroredChildStop it is
// only ever called from a site that has just PROVEN the condition is gone — the
// promotion the stop was about has now succeeded — never speculatively, and it
// touches exactly one reason: a master stopped for anything else is left alone.
func (c *Coordinator) clearIntegrationStop(ctx stdctx.Context, run domain.WorkflowRun) domain.WorkflowRun {
	if run.State != domain.WorkflowRunNeedsAttention {
		return run
	}
	reason, _, ok := c.stopReason(ctx, run)
	if !ok || reason != masterIntegrationFailureDurablePhase {
		return run
	}
	return c.unparkRun(ctx, run, reason, "the completed task was integrated successfully")
}

// stopReason resolves the canonical reason for a stopped run from its durable
// carriers alone. stopIsHumanOwned is the boolean view of it; branch-lock
// retention (branch_lock_recovery.go) needs the reason itself, so it can say
// which stop is holding a branch rather than merely that one is.
func (c *Coordinator) stopReason(ctx stdctx.Context, run domain.WorkflowRun) (string, AttentionDisposition, bool) {
	d := RunDetail{Run: run}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		return "", AttentionDisposition{}, false
	}
	for _, cp := range cps {
		// Checkpoint 8P-E.18: the incident ledger describes a stop, it is never
		// itself one. Its rows are the newest thing on a run the moment anyone
		// asks "what do I do", so counting them here would rewrite the run's
		// stop reason to `unclassified_stop` as a side effect of opening the
		// modal — changing the Board, the incident's own signature, and every
		// later comparison against it. Skipping them is what keeps asking about
		// a stop free of consequences for the stop.
		if isIncidentLedgerPhase(cp.DurablePhase) {
			continue
		}
		// NextAction carries resolveAttentionReason's legacy carrier (the
		// pre-8P-E.13 "human_attention" literal). Without it, a run stranded by
		// the old fix-budget code reads as unclassified here while the very
		// same lookup on the run detail page names it — and branch-lock
		// retention would then have to guess about a stop AO can actually
		// explain.
		if cp.NextAction != "" {
			d.NextAction = cp.NextAction
		}
		if !cp.CreatedAt.Before(d.LatestCheckpointAt) {
			d.LatestCheckpointPhase = cp.DurablePhase
			d.LatestCheckpointAt = cp.CreatedAt
		}
	}
	if steps, serr := c.store.ListWorkflowSteps(ctx, run.ID); serr == nil {
		for _, s := range steps {
			attempts, aerr := c.store.ListWorkflowAttempts(ctx, s.ID)
			if aerr != nil {
				return "", AttentionDisposition{}, false
			}
			d.Steps = append(d.Steps, StepDetail{Step: s, Attempts: attempts})
		}
	}
	return resolveAttentionReason(d)
}
