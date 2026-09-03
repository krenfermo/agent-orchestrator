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
	// Recovery is P1-B's canonical recommendation for this stop: the ONE thing
	// AO advises doing about it. The zero value means "derive it" -- a
	// self-remediable stop resumes, a nonrecoverable one needs a restart, and
	// anything else is an operator action (see recoveryActionFor).
	//
	// It is on the disposition rather than computed elsewhere because this
	// table is already the single place a stop site is forced to answer "and
	// what does the user do about it?". Adding a second table would recreate
	// exactly the three-way disagreement this file was written to end.
	Recovery domain.RecoveryAction
	// Repairable marks a stop whose remedy is a bounded code change AO could
	// aim a Repair Agent at.
	//
	// The default is false, and that default is the safety property. A stop
	// nobody has explicitly marked repairable -- including every stop AO cannot
	// classify at all -- can never have a code-writing agent pointed at it. The
	// classes that must NEVER be repaired (unprovable provenance, unknown
	// approved HEAD, credentials, external permissions, destructive ambiguity,
	// policy refusals, repository identity) are excluded by construction rather
	// than by a deny-list that could fall out of date.
	Repairable bool
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
	// ReasonVerifyFixUnavailable is a repairable verification failure with
	// budget left that AO nevertheless cannot hand to a fix worker, because
	// something structural prevents the fix cycle from ever running.
	//
	// It exists so that condition converges instead of parking. Before it,
	// finishVerifyFailure took the re-entry branch on the strength of the
	// failure being repairable alone, wrote its verify_fix_reentry checkpoint
	// and left the run at `waiting` — and when the dispatcher then refused, the
	// run rested forever on a verify step that renders as live work (incident
	// wf-170b16ce). Distinct from ReasonVerifyBudgetExhausted because the
	// budget is NOT the problem and raising it would change nothing, and
	// distinct from ReasonVerifyUnrepairable because the failure itself is
	// perfectly repairable — what is missing is the machinery to repair it.
	ReasonVerifyFixUnavailable = "verify_fix_unavailable"
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
	// ReasonVerifyApprovedHeadUnprovable is the ONE fact whose absence makes
	// every other question about a changed workspace unanswerable: AO cannot
	// name the commit the approved review target was read at.
	//
	// It is deliberately distinct from ReasonVerifyWorkspaceUnattributable,
	// which says "AO looked and could not attribute the change". Here AO never
	// got as far as looking: a moved history and uncommitted work cannot be
	// told apart without the approval's own commit, so the classification stops
	// before it starts. The two need separate names because they need separate
	// remedies -- this one has an explicit operator recovery
	// (RecoverUnprovableApprovedHead), and that one does not.
	ReasonVerifyApprovedHeadUnprovable = "verify_approved_head_unprovable"
	ReasonReviewStateAmbiguous         = "review_state_ambiguous"
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
	ReasonFixPromptNotSubmitted = "fix_prompt_not_submitted"
	// ReasonFixGenerationUnprovable is durable fix-cycle dispatch state that AO
	// cannot map onto exactly one dispatch generation: two different generations
	// recorded against one cycle, a claim token that disagrees with the
	// pre-delivery record it should match, a generation-less delivery whose
	// findings or review run are not the ones this pass derived.
	//
	// It is deliberately NOT ReasonFixDispatchAmbiguous. That reason says "AO
	// cannot prove whether the prompt arrived" — a question about the session.
	// This one says "AO cannot prove which dispatch the state on disk belongs
	// to" — a question about the ledger, which no amount of looking at the
	// worker will answer. Fail-closed and inert: nothing is sent, no attempt is
	// opened, no state advances, and no retry is scheduled, because nothing AO
	// can do by itself would change the answer. See markFixGenerationUnprovable.
	ReasonFixGenerationUnprovable = "fix_generation_unprovable"
	ReasonPlannerExhausted        = "planner_retries_exhausted"
	ReasonPlannerStartFailed      = "planner_start_failed"
	ReasonPlannerPolicyViolation  = "planner_policy_violation"
	ReasonPlannerAmbiguous        = "planner_ambiguous"
	// ReasonPlannerResultInconsistent is F2's class, and it is deliberately
	// NOT ReasonPlannerAmbiguous. Ambiguous means the objective genuinely did
	// not say enough and a person must decide. This means the provider
	// answered, was billed for answering, and AO received something that
	// provably is not that answer -- a schema-shaped placeholder accepted while
	// 17.5k output tokens of real plan were lost. Nothing about the objective
	// is unclear and there is nothing for a person to decide; the right
	// response is to ask the planner again, which is why this class routes
	// through the bounded planner retry and only names a human once that is
	// exhausted.
	ReasonPlannerResultInconsistent = "planner_result_inconsistent"
	// ReasonObjectivePlanRecovered is CP1's healer speaking: an objective run
	// that had no plan row got one, and the approval mode that row should have
	// carried was never recorded anywhere. It is NOT a failure — the run works
	// again — but it is a substitution of somebody's choice, so it is on the
	// ledger with the action that undoes it.
	ReasonObjectivePlanRecovered = "objective_plan_recovered"
	ReasonChildNeedsAttention    = "child_needs_attention"
	ReasonChildFailed            = "child_failed"
	// ReasonTaskParked is the objective's own stop when one or more of its
	// tasks are parked in needs_attention (migration 0130) and nothing else in
	// the plan can move. It is deliberately distinct from
	// ReasonChildNeedsAttention: the child run finished perfectly well, and
	// what is stopped is the integration of its result.
	ReasonTaskParked          = "task_integration_attention"
	ReasonRecoveryInterrupted = "recovery_interrupted"
	// ReasonRecoveryUnreconcilable is a run whose durable state AO could not
	// reconcile against its runtime at all -- the runtime would not answer, or
	// answered with something AO refuses to act on (a pane that will not name
	// its process, a session it cannot prove it owns).
	//
	// It is deliberately not self-remediable. The whole point of parking the
	// run is that re-driving it produced the same unprovable answer every time,
	// and something outside AO has to change before it can produce a different
	// one. Parking is scoped to this run: every other run reconciles normally.
	ReasonRecoveryUnreconcilable  = "recovery_unreconcilable"
	ReasonWorkerDispatchAmbiguous = "worker_dispatch_ambiguous"
	// ReasonWorkerWorkspaceUnreadable is a worker whose turn AO can PROVE
	// finished — the provider's own turn receipt for this dispatch — and whose
	// repository AO could not read, so what the turn produced is unknown.
	//
	// It is deliberately NOT ReasonWorkerDispatchAmbiguous (P3-D §1). That
	// reason means AO cannot say what the worker did or whether it ran at all;
	// here AO knows exactly what the worker did with its turn and is missing
	// one specific observation, of one specific directory. The remedies are
	// nothing alike: this one is fixed by making the workspace readable again
	// and re-entering, and the run resumes on the evidence it was always going
	// to be decided on. Sending a person to "confirm whether the worker
	// produced work" instead sends them to answer a question AO could answer
	// itself the moment the path comes back.
	ReasonWorkerWorkspaceUnreadable = "worker_workspace_unreadable"
	// ReasonWorkerTurnProducedNothing is the other half of that split: the turn
	// receipt is there, the workspace WAS read, and there is nothing in it.
	//
	// Also not ReasonWorkerDispatchAmbiguous, and for the opposite reason —
	// nothing here is ambiguous. Both facts are in hand and they disagree with
	// each other: the provider says it finished, and the tree says it did
	// nothing. That is a judgement about the work, which is a person's, and the
	// sentence they get should say so rather than ask them to establish a fact
	// AO already established.
	ReasonWorkerTurnProducedNothing = "worker_turn_produced_nothing"
	ReasonWorkerBlocked             = "worker_blocked"
	// ReasonProviderDialogUnreadable is a worker sitting on an interactive
	// prompt that AO holds a durable answer for and CANNOT READ well enough to
	// deliver it: an unrecognised layout, or a screen that never settled.
	//
	// It is deliberately not ReasonWorkerBlocked, and it is emphatically not
	// treated as the prompt being gone. Worker-blocked says "somebody has to
	// answer this"; here the answer already exists and AO is the one that
	// cannot act. Telling a person to go and decide something AO decided
	// minutes ago wastes their time and hides the real fault, which is that
	// AO's reading of this provider's screen has stopped matching the provider.
	//
	// Bounded: it is raised only after the retry window has passed with every
	// observation inconclusive. Before that the run stays as it was and AO
	// keeps looking, because a half-drawn repaint resolves itself.
	ReasonProviderDialogUnreadable = "provider_dialog_unreadable"
	// ReasonReadOnlyWorkspaceMutated is a task the plan declared read-only
	// (domain.WorkflowWriteIntentReadOnly) whose worktree changed anyway,
	// measured against the workspace fingerprint recorded at dispatch.
	//
	// It is deliberately NOT ReasonWorkerDispatchAmbiguous. That reason means AO
	// could not prove what happened; here AO proved exactly what happened and
	// the two fingerprints are on the ledger. The remedy is different too: the
	// question is not "did the worker do anything", it is "this change was not
	// supposed to exist -- do you want it?".
	ReasonReadOnlyWorkspaceMutated = "read_only_workspace_mutated"
	ReasonDispatchFailed           = "dispatch_failed"
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
	// The three provider-preflight reasons are declared in
	// provider_preflight.go, next to the classes they mirror, and registered in
	// the dispositions table below. They name the one thing AO can now detect
	// BEFORE spending a dispatch: a provider that would have stopped at an
	// interactive prompt nobody was there to answer.

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
		Recovery:    domain.RecoveryInspectRepository,
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
	ReasonProviderDialogUnreadable: {
		HumanAction: "AO decided this question automatically but cannot read the prompt the agent is showing, so it has not sent the answer. Open that session and choose the option AO recorded, then continue this run.",
	},
	ReasonWorkerWorkspaceUnreadable: {
		HumanAction: "The worker finished its turn and AO could not read its workspace to see what it produced. Check that the run's repository path still exists and is readable, then continue this run — AO re-reads it and resumes on its own evidence.",
	},
	ReasonWorkerTurnProducedNothing: {
		HumanAction: "The worker reported its turn finished and left no change in its workspace. Decide whether that is correct for this task, then continue this run or cancel it.",
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
		// Repairable: the reviewer is still naming concrete changes and the
		// remedy is a bounded code change AO holds the findings for.
		Repairable:  true,
		HumanAction: "The review/fix budget is exhausted and the reviewer is still requesting changes. Read the latest findings and either raise the fix budget, apply the changes yourself, or cancel this run.",
	},
	ReasonVerifyBudgetExhausted: {
		// Repairable: deterministic checks are failing and their output says
		// how. This is the build/test failure class P1-B §E names first.
		Repairable:  true,
		HumanAction: "Verification kept failing after every automatic fix attempt. Read the verify output and either fix it yourself, adjust the verification commands, or cancel this run.",
	},
	ReasonVerifyUnrepairable: {
		HumanAction: "Verification failed for a reason no fix cycle can repair (its environment or its target changed under it). Inspect the worktree, then continue or cancel this run.",
	},
	ReasonVerifyFixUnavailable: {
		// Repairable in the same sense ReasonVerifyBudgetExhausted is: the
		// verify output says what is wrong. The difference is that AO cannot
		// dispatch the worker that would act on it, so the person is told what
		// blocked the cycle rather than being invited to raise a budget that is
		// not the constraint.
		Repairable:  true,
		HumanAction: "Verification failed and AO could not hand the findings to a fix worker (the detail names what blocked it). Read the verify output and either fix it yourself or cancel this run.",
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
	ReasonVerifyApprovedHeadUnprovable: {
		Recovery:    domain.RecoveryInspectRepository,
		HumanAction: "AO cannot prove which commit the approved review was given for, and could not recover it from this branch's history, so it will not verify against that approval. Inspect the worktree; if the work in it is what you want reviewed, recover this run's review provenance — AO discards the unlocatable approval and asks for exactly one fresh review of what is there now.",
	},
	ReasonVerifyWorkspaceUnattributable: {
		Recovery:    domain.RecoveryInspectRepository,
		HumanAction: "Verification was reopened, but the worktree no longer matches what review approved and AO cannot attribute the difference to this task's own uncommitted work (its branch or its HEAD moved). Inspect the worktree, restore it or commit the intended state, then continue or cancel this run.",
	},
	ReasonReviewStateAmbiguous: {
		HumanAction: "AO could not prove what the review concluded. Inspect the reviewer session, then continue or cancel this run.",
	},
	ReasonRecoveryUnreconcilable: {
		HumanAction: "AO could not reconcile this run against its runtime (its session or reviewer pane cannot be classified, so AO will neither adopt nor kill it). Check whether that session is still running and close it out, then continue or cancel this run.",
	},
	ReasonReviewerLaunchFailed: {
		HumanAction: "The reviewer could not be launched. Check the reviewer provider's auth and installation, then continue this run.",
	},
	ReasonReviewerAuthInvalid: {
		Recovery:    domain.RecoveryAuthenticate,
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
		// Repairable: the findings are known and nothing was written against
		// them. A repair agent has an unambiguous, bounded job.
		Repairable:  true,
		HumanAction: "The fix worker finished without changing anything AO can verify. Inspect the worktree, then continue or cancel this run.",
	},
	ReasonFixCycleNotStarted: {
		HumanAction: "The reviewer's findings reached the worker session but the worker never started on them. Check that the session is alive and responsive, then continue this run — AO re-checks its own evidence first and re-delivers the cycle by itself when it can prove that is safe.",
	},
	ReasonFixGenerationUnprovable: {
		HumanAction: "AO cannot tell which fix dispatch the durable state for this cycle belongs to, so it will not send the findings, open a fix attempt or advance the run on it. The checkpoint names exactly what disagrees. Inspect the worker session to see whether it already has these findings, then continue this run if it does not, or cancel it.",
	},
	ReasonFixPromptNotSubmitted: {
		HumanAction: "The reviewer's findings are sitting unsubmitted in the worker session's composer. Continue this run and AO will submit what is already there (it never re-sends the text, so the prompt cannot be duplicated); if that keeps failing, open the session and submit it yourself.",
	},
	ReasonPlannerExhausted: {
		Nonrecoverable: true,
		Recovery:       domain.RecoveryRegeneratePlan,
		HumanAction:    "The planner failed on every allowed retry. Retry planning, simplify the objective, or switch the planner provider.",
	},
	ReasonPlannerStartFailed: {
		Nonrecoverable: true,
		HumanAction:    "The planner could not be started. Check the planner provider's auth and installation, then retry planning.",
	},
	ReasonPlannerPolicyViolation: {
		Nonrecoverable: true,
		Recovery:       domain.RecoveryRegeneratePlan,
		HumanAction:    "The generated plan violated AO's plan policy. Rephrase the objective and retry planning.",
	},
	ReasonPlannerAmbiguous: {
		Nonrecoverable: true,
		Recovery:       domain.RecoveryRegeneratePlan,
		HumanAction:    "The planner's state could not be recovered after a restart. Reopen planning for this objective (AO will not do it on its own), or cancel it.",
	},
	ReasonObjectivePlanRecovered: {
		HumanAction: "This objective was created without its plan row and AO recreated it. The approval mode could not be recovered and defaults to manual: start planning yourself, or recreate the objective if it was meant to run autonomously.",
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
	ReasonReadOnlyWorkspaceMutated: {
		Recovery:    domain.RecoveryInspectRepository,
		HumanAction: "This task was planned as read-only, but its worktree changed while it ran. Inspect the change (the checkpoint names the dispatch-time and current workspace fingerprints), then keep it and continue this run, or revert it and continue.",
	},
	ReasonCapacityRetryExhausted: {
		HumanAction: "Every automatic retry ran out while the provider was still at capacity. Wait and continue this run, switch provider, or cancel it.",
	},
	ReasonProviderAuthRequired: {
		Recovery:    domain.RecoveryAuthenticate,
		HumanAction: "The provider reported that its credentials are not usable, so an unattended launch would have stopped at a login prompt. Sign that provider profile in, then continue this run.",
	},
	ReasonProviderWorkspaceTrustRequired: {
		HumanAction: "The provider has no recorded trust for this workspace, so an unattended launch would have stopped at its \"do you trust this folder?\" prompt. Trust the directory through the provider's own configuration (never by answering the prompt for an agent), then continue this run.",
	},
	// P1-B: automatic repair is spent for this run. It is a real stop with a
	// real remedy, so it is registered here like any other -- and it is
	// deliberately NOT repairable, because the whole point of the escalation is
	// that repairing is no longer on the table.
	repairEscalatedPhase: {
		Recovery:    domain.RecoveryOperatorAction,
		HumanAction: "AO has used every automatic repair this run is allowed. Read the latest repair run's review and verification output, then apply the change yourself, raise the repair budget for a fresh run, or cancel this one.",
	},
	ReasonProviderPreflightFailed: {
		HumanAction: "The provider would have asked the operator something before it could work — usually a permission mode that cannot run unattended. Correct the provider configuration, then continue this run.",
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
	WorkflowErrorProviderAuthRequired:           attentionDispositions[ReasonProviderAuthRequired],
	WorkflowErrorProviderWorkspaceTrustRequired: attentionDispositions[ReasonProviderWorkspaceTrustRequired],
	WorkflowErrorProviderPreflightFailed:        attentionDispositions[ReasonProviderPreflightFailed],
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
	// The STOP AUTHORITY first: the newest checkpoint that actually stopped this
	// run, which is not the same thing as the newest checkpoint. A run parked on
	// reviewer_launch_failed that has since re-read the same verdict 302 times
	// is still parked on reviewer_launch_failed. See checkpoint_authority.go.
	if d.StopAuthorityPhase != "" {
		if disp, ok := attentionDispositions[d.StopAuthorityPhase]; ok {
			return d.StopAuthorityPhase, disp, true
		}
	}
	// A RunDetail built by hand -- every caller that constructs one without
	// folding a ledger, including much of this package's own test surface --
	// carries no stop authority, and for those the newest phase is all there is.
	// It is a strict fallback, never an override: a folded detail whose stop was
	// cleared must not be re-explained by the phase that cleared it.
	if !d.CheckpointsFolded {
		if disp, ok := attentionDispositions[d.LatestCheckpointPhase]; ok {
			return d.LatestCheckpointPhase, disp, true
		}
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
	c.recordAttentionStopWithState(ctx, run, stepID, reason, detail, "{}")
}

// recordAttentionStopWithState is recordAttentionStop with a caller-supplied
// retry_state payload, for a stop that carries machine-readable evidence about
// what produced it (today: planner attempt evidence -- see
// planner_evidence.go). It rides on the SAME checkpoint row the stop already
// writes rather than a row of its own, so nothing that reads a run's newest
// checkpoint starts seeing a diagnostic where it expects a stop.
func (c *Coordinator) recordAttentionStopWithState(ctx stdctx.Context, run domain.WorkflowRun, stepID *string, reason, detail, retryState string) {
	if reason == "" {
		return
	}
	if strings.TrimSpace(retryState) == "" {
		retryState = "{}"
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
		RetryState:     retryState,
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
	// P3-C §3: if this run's own frozen policy authorizes AO to repair this
	// condition without asking, make that happen on a timer rather than on the
	// next daemon restart. scheduleAutoRecoveryWake re-checks the policy and
	// the condition itself and is a no-op for every stop that is not both
	// repairable and automatic, so registering the call here costs nothing for
	// the stops it does not apply to. Unconditional on the checkpoint write
	// above for the same reason the notification is: the run stopped either
	// way, and a failure to write down why must not also cost it its recovery.
	c.scheduleAutoRecoveryWake(ctx, run, reason)
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

// stopIsSelfRemediable reports whether a run parked in needs_attention stopped
// for a reason AO is POSITIVELY known to be handling itself (a scheduled retry,
// a capacity wait, a queued branch, a fresh review AO asked for).
//
// It is deliberately not the negation of stopIsHumanOwned. There are three
// kinds of stop — AO's, the user's, and one AO could not name at all — and this
// function answers only the first, so an unnameable stop answers "no". That
// asymmetry is the whole of the parent-attention fix: clearing a mirror
// requires proof the child is moving, while holding it requires nothing but the
// child still being stopped.
func (c *Coordinator) stopIsSelfRemediable(ctx stdctx.Context, run domain.WorkflowRun) bool {
	_, disp, ok := c.stopReason(ctx, run)
	return ok && disp.SelfRemediable
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
		// Checkpoint 8P-E.24 (incident wf-f0efac7e): the mirror is cleared only
		// when the child has DURABLY left needs_attention, or when AO can
		// POSITIVELY prove the stop it is still in is one AO remediates itself.
		//
		// The old test was the opposite way round — hold only if the child's
		// stop is human-owned — and "human-owned" is derived from the child's
		// NEWEST checkpoint. Any row landing on the child after its stop (a
		// worker observation, an incident record, a wake) makes that lookup come
		// back empty for one pass, which read as "not human-owned" and unparked
		// the parent. The next pass re-derived the stop and parked it again.
		// That is exactly the flap the incident recorded: child_needs_attention,
		// attention_cleared eight seconds later, child_needs_attention again.
		//
		// Not being able to name a child's stop is not evidence the child has
		// recovered. A child still sitting in needs_attention holds the mirror.
		if child.State == domain.WorkflowRunNeedsAttention && !c.stopIsSelfRemediable(ctx, child) {
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
	// The same fold every projection uses, so this answer and the Board's are
	// the same answer. It excludes the incident ledger and the other
	// bookkeeping rows for the reason they have always been excluded — they
	// describe a stop, they are never one, and counting them would rewrite a
	// run's reason as a side effect of opening the modal — and it additionally
	// refuses to let an OBSERVATION displace the stop, which is what 302
	// re-readings of one approved verdict did to wf-c4c84f52. It also carries
	// resolveAttentionReason's legacy next_action carrier forward. See
	// checkpoint_authority.go.
	applyCheckpointAuthority(&d, cps)
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

// recordChildAttentionStopOnce is recordAttentionStopOnce for the parent's
// mirror of a child's stop (Checkpoint 8P-E.24).
//
// recordAttentionStopOnce compares only against the run's NEWEST checkpoint, so
// any unrelated row landing on the parent between two reconcile passes — an
// incident record, a wake note, another task's integration — makes the same
// unchanged child stop look new and write another identical row. Over a night
// of polling that is thousands of rows describing one condition, and it is what
// made wf-f0efac7e's ledger unreadable.
//
// This looks back through the parent's ledger to the last point at which the
// mirror could have MEANINGFULLY changed: an attention_cleared (the mirror was
// released) or a different attention reason (the parent stopped for something
// else). If the same child mirror, with the same detail, is still standing
// after that point, nothing is written.
func (c *Coordinator) recordChildAttentionStopOnce(ctx stdctx.Context, run domain.WorkflowRun, reason, detail string) {
	if reason == "" {
		return
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		// Failing to read is never a reason to lose a stop record.
		c.recordAttentionStop(ctx, run, nil, reason, detail)
		return
	}
	want := strings.TrimSpace(detail)
	// Newest first: the first row that either matches or invalidates decides.
	for i := len(cps) - 1; i >= 0; i-- {
		cp := cps[i]
		if cp.DurablePhase == reason && cp.NextAction == want {
			return
		}
		if cp.DurablePhase == attentionClearedPhase {
			break
		}
		if _, isReason := attentionDispositions[cp.DurablePhase]; isReason {
			// The parent stopped for a different reason since; this mirror is
			// news again.
			break
		}
	}
	c.recordAttentionStop(ctx, run, nil, reason, detail)
}
