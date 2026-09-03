import { useState } from "react";
import { Link, createFileRoute } from "@tanstack/react-router";
import { Archive } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { useWorkflowRun } from "../hooks/useWorkflowRun";
import { useWorkflowStatusLabel } from "../hooks/useWorkflowExecutionStatus";
import { useChildTaskRouting } from "../hooks/useChildTaskRouting";
import { WorkflowVerifyDetails } from "../components/workflow-verify-details";
import { WorkflowUsageSection } from "../components/workflow-usage-section";
import { WorkflowQuestionsSection } from "../components/workflow-questions-section";
import { WorkflowCapacityWaitBanner } from "../components/workflow-capacity-wait-banner";
import { WorkflowBranchWaitBanner } from "../components/workflow-branch-wait-banner";
import { WorkflowRoutingSummary } from "../components/workflow-routing-summary";
import { WorkflowIncidentDialog } from "../components/workflow-incident-dialog";
import { WorkflowResumeButton } from "../components/workflow-resume-button";
import { WorkflowRecoveryPanel } from "../components/workflow-recovery-panel";
import {
	translateDynamic,
	WorkflowActivityPanel,
	WorkflowStepIcon,
} from "../components/workflow-activity";
import {
	WorkflowActions,
	WorkflowCompletionSummary,
	WorkflowExecutionLocation,
	WorkflowRepairInline,
	WorkflowStatusPanel,
	WorkflowTechnicalDetails,
	WorkflowTimeline,
	type WorkflowActionHandlers,
} from "../components/workflow-status";
import { WorkflowCommitDialog } from "../components/workflow-commit-dialog";
import { processedTokens } from "../components/workflow-usage-section";
import { formatElapsedCompact } from "../lib/format-time";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { canCancelAndArchive, useCancelAndArchiveWorkflow } from "../hooks/useWorkflowArchive";
import type { components } from "../../api/schema";

export const Route = createFileRoute("/_shell/workflows/$workflowId")({
	component: WorkflowRunRoute,
});

type TranslateFn = (key: string, opts?: Record<string, unknown>) => string;

// The three canonical execution strategies, mapped to their catalog keys.
// Exhaustive on purpose: a strategy the UI has no name for must be a
// compile-time problem, not a raw enum value leaking onto the page.
const executionStrategyKeys = {
	task: "shell.workflowsStrategyTaskLabel",
	autonomous: "shell.workflowsStrategyAutonomousLabel",
	master: "shell.workflowsStrategyMasterLabel",
} as const;

function executionStrategyLabel(t: TranslateFn, strategy: keyof typeof executionStrategyKeys): string {
	return t(executionStrategyKeys[strategy]);
}

function statusLabelText(t: TranslateFn, label: NonNullable<ReturnType<typeof useWorkflowStatusLabel>>): string {
	if (typeof label === "object") {
		return t("shell.workflowsStatusExecutingTask", { current: label.current, total: label.total });
	}
	switch (label) {
		case "planning":
			return t("shell.workflowsStatusPlanning");
		case "reviewing":
			return t("shell.workflowsStatusReviewing");
		case "applying_fixes":
			return t("shell.workflowsStatusApplyingFixes");
		case "verifying":
			return t("shell.workflowsStatusVerifying");
		case "waiting_for_capacity":
			return t("shell.workflowsStatusWaitingForCapacity");
		case "waiting_for_decision":
			return t("shell.workflowsStatusWaitingForDecision");
		case "needs_attention":
			return t("shell.workflowsStatusNeedsAttention");
		case "completed":
			return t("shell.workflowsStatusCompleted");
		default:
			return "";
	}
}

/**
 * The run detail's own "Cancelar y archivar".
 *
 * The same daemon action the Board card triggers: cancel this run and its child
 * runs through the canonical cancellation lifecycle, release the branch, and
 * take the card off the active Board — keeping every durable row. Hidden for an
 * already-archived run (nothing left to archive) and for a run AO is actively
 * driving, which is plain Cancel's job, not this one.
 */
function WorkflowCancelAndArchiveButton({ run }: { run: components["schemas"]["WorkflowRunView"] }) {
	const { t } = useTranslation();
	const [open, setOpen] = useState(false);
	const archive = useCancelAndArchiveWorkflow(run.projectId);
	if (run.archivedAt || !canCancelAndArchive({ state: run.state })) return null;
	return (
		<div>
			<button
				className="flex items-center gap-1.5 rounded border border-border px-3 py-1.5 text-sm disabled:opacity-50"
				data-testid="workflow-detail-archive"
				disabled={archive.isPending}
				onClick={() => setOpen(true)}
				type="button"
			>
				<Archive aria-hidden="true" className="size-3.5 shrink-0" />
				{t("board.cancelAndArchive")}
			</button>
			{archive.error ? <p className="mt-1 text-sm text-destructive">{archive.error.message}</p> : null}
			<ConfirmDialog
				busy={archive.isPending}
				confirmLabel={t("board.cancelAndArchive")}
				description={t("board.cancelAndArchiveExplain")}
				destructive
				error={archive.error?.message ?? null}
				onConfirm={() => archive.mutate(run.id, { onSuccess: () => setOpen(false) })}
				onOpenChange={setOpen}
				open={open}
				title={t("board.cancelAndArchiveConfirm")}
			/>
		</div>
	);
}

function WorkflowRunRoute() {
	const { workflowId } = Route.useParams();
	return <WorkflowRunView workflowId={workflowId} />;
}

// Exported separately from the route wiring above so tests can render this
// component directly (with a mocked apiClient + real QueryClientProvider)
// without also having to stand up a TanStack Router route match --
// Checkpoint 8P-D.1's regression test for the "Something went wrong!"
// crash renders THIS.
export function WorkflowRunView({ workflowId }: { workflowId: string }) {
	const { t } = useTranslation();
	// The "¿Qué hago?" modal's own open state. It is local because opening the
	// modal is a read with no durable consequence — see the control below.
	const [incidentOpen, setIncidentOpen] = useState(false);
	// The commit-and-continue dialog's own open state. Local because opening it
	// is a read: the daemon runs `git status` and nothing else until the user
	// presses the button inside it.
	const [commitOpen, setCommitOpen] = useState(false);
	const {
		workflow,
		isLoading,
		error,
		cancel,
		cancelling,
		cancelError,
		start,
		starting,
		startError,
		continueRun,
		continuing,
		continueError,
		generatePlan,
		generatingPlan,
		generatePlanError,
		approvePlan,
		approvingPlan,
		approvePlanError,
		rejectPlan,
		rejectingPlan,
		rejectPlanError,
		answerQuestion,
		answeringQuestion,
	} = useWorkflowRun(workflowId);

	// React Rules of Hooks: these must run on every render, including the
	// loading/error/not-found renders below, or the hook count changes
	// between the "no data yet" render and the "data arrived" render and
	// React throws (minified error #310) the moment a real network
	// round-trip separates those two renders -- reproduced via the actual
	// running app, not just synchronous-mock unit tests, in Checkpoint
	// 8P-D.1.
	const childTaskIds = (workflow?.tasks ?? []).map((task) => task.executionWorkflowId).filter((id): id is string => Boolean(id));
	const childTaskRouting = useChildTaskRouting(childTaskIds);
	const statusLabel = useWorkflowStatusLabel(workflow);

	if (isLoading && !workflow) {
		return <p className="p-6 text-sm text-muted-foreground">{t("shell.workflowsLoading")}</p>;
	}
	if (error) {
		return <p className="p-6 text-sm text-destructive">{error}</p>;
	}
	if (!workflow) {
		return <p className="p-6 text-sm text-muted-foreground">{t("shell.workflowsNotFound")}</p>;
	}

	const isPending = workflow.run.state === "pending";
	const workStep = workflow.steps.find((step) => step.kind === "work");

	// Facts for the "working right now" panel. Every one of them is a value the
	// API already returns for this run -- the harness is read the same way the
	// work step reads it (latest attempt first, since a failover changes the
	// harness without changing assignedHarness), and the token total stays
	// Unknown unless the usage record says the number was actually observed.
	const activeStep = workflow.steps.find((step) => step.state === "running");
	const activeAttempts = activeStep?.attempts ?? [];
	const activeHarness = activeAttempts[activeAttempts.length - 1]?.harness || activeStep?.assignedHarness || undefined;
	const activeBranch = activeStep?.branch || workStep?.branch || undefined;
	const observedTokens = workflow.usage ? processedTokens(workflow.usage.metrics) : null;
	const activityFacts = [
		{ label: t("board.factElapsed"), value: formatElapsedCompact(workflow.run.createdAt) },
		{ label: t("board.factAgent"), value: activeHarness },
		{ label: t("board.factBranch"), value: activeBranch },
		{
			label: t("board.factTokens"),
			value: observedTokens === null ? t("board.factUnknown") : observedTokens.toLocaleString(),
		},
	];

	const presentation = workflow.presentation;
	// The action handlers the renderer can actually perform. An action the
	// daemon offers and this map has no entry for renders disabled rather than
	// disappearing: the daemon authorised it, and hiding it would misreport
	// what AO is willing to do. Every entry here is a call the page already
	// made before P3-A -- nothing new is being authorised by the projection.
	const actionHandlers: WorkflowActionHandlers = {
		continue: () => void continueRun(),
		cancel: () => void cancel(),
		commit_and_continue: () => setCommitOpen(true),
		view_changes: () => setCommitOpen(true),
		revalidate_plan: () => void generatePlan(),
		regenerate_plan: () => void generatePlan(),
	};

	return (
		// Checkpoint 8P-E.12: the shell gives every route a `min-h-0 flex-1`
		// box but no vertical scroll of its own, so a workflow with a long
		// objective, a long findings summary or many steps simply overflowed
		// and the bottom of it was unreachable. The route owns its scroll
		// container, same as the board does. `break-words` keeps an
		// unbroken prompt/URL from widening the whole layout instead of
		// wrapping.
		// `[&>*]:shrink-0` is load-bearing, not decoration: in a scrolling
		// flex column the children default to flex-shrink:1 and compress to
		// fit instead of overflowing, so the scrollbar never appears and the
		// content just gets squashed. Pinning the children's size is what
		// makes the overflow real.
		<div className="mx-auto flex h-full min-h-0 max-w-2xl flex-col gap-6 overflow-y-auto break-words p-6 [&>*]:shrink-0">
			{workflow.run.state === "completed" && (
				<div className="rounded-lg border border-green-500/40 bg-green-500/10 p-3 text-sm font-medium text-green-700 dark:text-green-300">
					{t("shell.workflowsCompletedVerified")}
				</div>
			)}
			<WorkflowCapacityWaitBanner run={workflow.run} />
			{/* P1-B §L: the recovery panel. It renders the backend's assessment
			    and offers only the operations that assessment authorises — it
			    never decides for itself whether an action is safe. It shows
			    itself only for a run that is stopped or blocked; a healthy run
			    has no recovery question, and asking for the assessment costs a
			    planner-context probe. */}
			<WorkflowRecoveryPanel run={workflow.run} />
			<WorkflowBranchWaitBanner run={workflow.run} />
			<div className="flex flex-col gap-3">
				<h1 className="min-w-0 flex-1 text-lg font-semibold">{workflow.run.objective}</h1>
				{/* P3-A: the human status is the headline. Stage, the sentence that
				    says what is happening, the one-line "¿qué hago?", and the stage
				    progression — all of it derived by the daemon, so this page and
				    the board card cannot tell two different stories. The technical
				    vocabulary keeps its place further down, in a disclosure. */}
				{presentation ? <WorkflowStatusPanel presentation={presentation} /> : null}
				{presentation ? (
					<WorkflowActions busy={continuing || cancelling} handlers={actionHandlers} presentation={presentation} />
				) : null}
				<p className="text-sm text-muted-foreground">
					{t("shell.workflowsRunHeader", { projectId: workflow.run.projectId, state: workflow.run.state })}
				</p>
				<p className="text-sm text-muted-foreground">
					{/* P1-A: strategy and approval are two facts, shown as two.
					    executionMode is the approval axis (who drives); the
					    strategy is the durable orchestration choice, absent only
					    for a pre-P1-A run the daemon has not reconciled yet. */}
					{workflow.run.executionStrategy && (
						<>
							{t("shell.workflowsStrategyLabel")}: {executionStrategyLabel(t as TranslateFn, workflow.run.executionStrategy.effectiveStrategy)}
							{" · "}
						</>
					)}
					{t("shell.workflowsMode")}:{" "}
					{workflow.run.executionMode === "autonomous"
						? t("shell.workflowsModeAutonomous")
						: t("shell.workflowsModeManual")}
					{statusLabel && <> · {statusLabelText(t as TranslateFn, statusLabel)}</>}
				</p>
				{workflow.run.nextAction && (
					<p className="text-sm text-muted-foreground">
						{t("shell.workflowsNextAction", { nextAction: workflow.run.nextAction })}
					</p>
				)}
				<WorkflowActivityPanel
					detail={statusLabel ? statusLabelText(t as TranslateFn, statusLabel) || undefined : undefined}
					facts={activityFacts}
					phase={workflow.run.phase}
				/>
			</div>

			<div className="flex items-center gap-2">
				{isPending && !workflow.plan && (
					<div>
						<button
							className="rounded border border-border px-3 py-1.5 text-sm disabled:opacity-50"
							disabled={starting}
							onClick={() => void start()}
							type="button"
						>
							{starting ? t("shell.workflowsStarting") : t("shell.workflowsStart")}
						</button>
						{startError && <p className="mt-1 text-sm text-destructive">{startError}</p>}
					</div>
				)}
				{workflow.plan?.status === "pending" && (
					<div>
						<button className="rounded border border-border px-3 py-1.5 text-sm disabled:opacity-50" disabled={generatingPlan} onClick={() => void generatePlan()} type="button">
							{generatingPlan ? "Generating plan…" : "Generate Plan"}
						</button>
						{generatePlanError && <p className="mt-1 text-sm text-destructive">{generatePlanError}</p>}
					</div>
				)}
				{workflow.plan?.status === "validated" && workflow.plan.approvalMode === "manual" && (
					<div>
						<button className="rounded border border-primary bg-primary px-3 py-1.5 text-sm text-primary-foreground disabled:opacity-50" disabled={approvingPlan} onClick={() => void approvePlan()} type="button">
							{approvingPlan ? "Approving…" : "Approve Plan"}
						</button>
						{approvePlanError && <p className="mt-1 text-sm text-destructive">{approvePlanError}</p>}
					</div>
				)}
				{workflow.plan?.approvalMode === "auto" &&
					(workflow.plan.status === "approved" || workflow.plan.status === "validated") && (
						<span className="text-sm text-muted-foreground">{t("shell.workflowsPlanAutoApproved")}</span>
					)}
				{workflow.plan && workflow.plan.status !== "approved" && workflow.plan.status !== "rejected" && (
					<div>
						<button className="rounded border border-border px-3 py-1.5 text-sm disabled:opacity-50" disabled={rejectingPlan} onClick={() => void rejectPlan()} type="button">
							{rejectingPlan ? "Cancelling…" : "Cancel"}
						</button>
						{rejectPlanError && <p className="mt-1 text-sm text-destructive">{rejectPlanError}</p>}
					</div>
				)}
				{/* P3-A: Continue, Resume and Cancel are no longer rendered here.
				    They are offered by WorkflowActions above, from the daemon's own
				    authorisation, so a person is never shown two buttons that make
				    the same POST -- and never shown one AO would refuse. The
				    controls that remain below are the ones the projection does not
				    cover: starting a pending run, the plan approval pair, the
				    incident advisor, and cancel-and-archive. */}
				{continueError && <p className="text-sm text-destructive">{continueError}</p>}
				{/* Resume, for a run stopped on something a person has now dealt
				    with. Gated on the backend's authoritative canContinue flag —
				    never on a state string read here — so a terminal or
				    nonrecoverable stop never gets a button that provably does
				    nothing. Hidden when canContinueReview already renders the
				    same POST under its own, more specific label. */}
				{/* "¿Qué hago?" — offered for any run stopped on a decision, whether
				    or not Reanudar applies. The stops a person most needs help
				    with are precisely the ones Continue cannot resolve, so
				    gating this on canContinue would hide it exactly when it
				    matters. Opening it is a read: the backend deliberately keeps
				    the incident ledger out of the run's derived stop, so asking
				    the question does not change the answer. */}
				{workflow.run.state === "needs_attention" && (
					<button
						className="rounded border border-border px-3 py-1.5 text-sm"
						onClick={() => setIncidentOpen(true)}
						type="button"
					>
						{t("incident.title")}
					</button>
				)}
				{workflow.run.state === "needs_attention" && (
					<WorkflowIncidentDialog onOpenChange={setIncidentOpen} open={incidentOpen} workflowId={workflowId} />
				)}
				{/* The attention hand-off keeps its own control: it does not
				    continue THIS run, it sends the user to the child run that
				    actually stopped, which no generic action can express. */}
				{workflow.run.canContinue && workflow.run.attentionWorkflowId ? (
					<WorkflowResumeButton
						attentionWorkflowId={workflow.run.attentionWorkflowId}
						continueError={continueError}
						continueRun={continueRun}
						continuing={continuing}
					/>
				) : null}
				{cancelError && <p className="text-sm text-destructive">{cancelError}</p>}
				<WorkflowCancelAndArchiveButton run={workflow.run} />
			</div>

			{workflow.plan && (
				<section className="flex flex-col gap-3">
					<div>
						<h2 className="text-sm font-semibold">{t("shell.workflowsMasterPlan")}</h2>
						<p className="text-xs text-muted-foreground">
							{workflow.plan.status} · {workflow.plan.provider || "planner"}/{workflow.plan.model || "default"}
							{workflow.plan.planHash ? ` · ${workflow.plan.planHash.slice(0, 12)}` : ""}
						</p>
					</div>
					{workflow.plan.generated?.summary && <p className="text-sm text-muted-foreground">{workflow.plan.generated.summary}</p>}
					{workflow.tasks?.map((task) => (
						<article className="rounded-lg border border-border p-3" key={task.id}>
							<div className="flex items-center justify-between gap-3">
								<h3 className="font-medium">{task.number}. {task.title}</h3>
								<span className="flex items-center gap-2 text-xs text-muted-foreground">
									{task.executionWorkflowId && childTaskRouting[task.executionWorkflowId] && (
										<span>
											→ {childTaskRouting[task.executionWorkflowId]?.selectedLabel}
											{childTaskRouting[task.executionWorkflowId]?.fallbackUsed &&
												` (${t("shell.routingFallbackTag", "fallback")})`}
										</span>
									)}
									<span>{task.state}</span>
								</span>
							</div>
							<p className="mt-1 text-sm text-muted-foreground">{task.description}</p>
							{task.dependencies.length > 0 && (
							<p className="mt-2 text-xs text-muted-foreground">
								{t("shell.workflowsDependsOn", { dependencies: task.dependencies.join(", ") })}
							</p>
						)}
							<ul className="mt-2 list-disc space-y-1 pl-5 text-xs text-muted-foreground">{task.acceptanceCriteria.map((criterion) => <li key={criterion}>{criterion}</li>)}</ul>
							<div className="mt-2 text-xs text-muted-foreground">
								{task.verify.commands?.map((check) => <code className="mr-2" key={`${check.command}-${check.args?.join("-")}`}>{[check.command, ...(check.args ?? [])].join(" ")}</code>)}
								{task.verify.files?.map((check) => <code className="mr-2" key={check.path}>{check.path}</code>)}
							</div>
							{task.executionWorkflowId && (
								<Link
									className="mt-2 inline-block text-xs text-primary underline underline-offset-2"
									params={{ workflowId: task.executionWorkflowId }}
									to="/workflows/$workflowId"
								>
									{t("shell.workflowsExecutionWorkflow")}
								</Link>
							)}
							{workflow.integrationState?.currentSha && task.state === "completed" && (
								<p className="mt-2 text-xs text-muted-foreground">
									{t("shell.workflowsBasedOnIntegrationRevision", { sha: workflow.integrationState.currentSha.slice(0, 12) })}
								</p>
							)}
						</article>
					))}
				</section>
			)}

			{workflow.integrationState && (
				<section className="flex flex-col gap-2 rounded-lg border border-border p-3">
					<h2 className="text-sm font-semibold">{t("shell.workflowsIntegrationState")}</h2>
					<dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs text-muted-foreground">
						<dt>{t("shell.workflowsIntegrationRevision")}</dt>
						<dd>{workflow.integrationState.currentSha ? workflow.integrationState.currentSha.slice(0, 12) : "—"}</dd>
						<dt>{t("shell.workflowsTasksIntegrated")}</dt>
						<dd>{workflow.integrationState.tasksIntegrated}</dd>
						{workflow.integrationState.latestTaskId && (
							<>
								<dt>{t("shell.workflowsLatestIntegratedTask")}</dt>
								<dd>{workflow.integrationState.latestTaskId}</dd>
							</>
						)}
						<dt>{t("shell.workflowsIntegrationStatus")}</dt>
						<dd>{workflow.integrationState.status}{workflow.integrationState.errorClass ? ` · ${workflow.integrationState.errorClass}` : ""}</dd>
					</dl>
				</section>
			)}

			<div className="flex flex-col gap-3">
				<h2 className="text-sm font-semibold text-muted-foreground">{t("shell.workflowsSteps")}</h2>
				{workflow.steps.map((step) => (
					<div className="rounded-lg border border-border p-3" key={step.id}>
						<div className="flex items-center justify-between gap-2">
							<span className="flex min-w-0 items-center gap-2 font-medium">
								<WorkflowStepIcon state={step.state} />
								{step.ordinal}. {step.kind}
							</span>
							<span className="shrink-0 text-xs text-muted-foreground">
								{translateDynamic(t as TFunction, `board.stepState.${step.state}`, step.state)}
							</span>
						</div>
						{step.kind === "work" && (
							<dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-2 gap-y-1 border-t border-border pt-2 text-xs text-muted-foreground">
								{(() => {
									// Checkpoint 8H: the harness actually in use is the latest
									// attempt's harness, not the (unused) assignedHarness column —
									// a failover changes provider without changing that field.
									const attempts = step.attempts;
									const currentHarness =
										attempts.length > 0
											? attempts[attempts.length - 1].harness || step.assignedHarness || "codex"
											: step.assignedHarness || "codex";
									const harnessChain = attempts
										.map((a) => a.harness)
										.filter((h): h is string => Boolean(h))
										.filter((h, i, arr) => i === 0 || arr[i - 1] !== h);
									const failedOver = harnessChain.length > 1;
									const failoverReason = failedOver
										? (attempts.find((a) => a.outcome === "failed" && a.harness === harnessChain[0])?.errorClass ?? "")
										: "";
									return (
										<>
											<dt>{t("shell.workflowsCurrentAgent")}</dt>
											<dd>{currentHarness}</dd>
											{failedOver && (
												<>
													<dt>{t("shell.workflowsFailoverLabel")}</dt>
													<dd>
														{t("shell.workflowsFailover", {
															from: harnessChain[0],
															to: harnessChain[harnessChain.length - 1],
															reason: failoverReason,
														})}
													</dd>
												</>
											)}
										</>
									);
								})()}
								{step.sessionId && (
									<>
										<dt>{t("shell.workflowsSession")}</dt>
										<dd>
											<Link
												className="text-primary underline underline-offset-2"
												params={{ sessionId: step.sessionId }}
												to="/sessions/$sessionId"
											>
												{step.sessionId}
											</Link>
										</dd>
									</>
								)}
								{step.branch && (
									<>
										<dt>{t("shell.workflowsBranch")}</dt>
										<dd>{step.branch}</dd>
									</>
								)}
								{step.headSha && (
									<>
										<dt>{t("shell.workflowsHeadSha")}</dt>
										<dd className="font-mono">{step.headSha}</dd>
									</>
								)}
								{step.nextAction && (
									<>
										<dt>{t("shell.workflowsNextActionLabel")}</dt>
										<dd>{step.nextAction}</dd>
									</>
								)}
							</dl>
						)}
						{step.kind === "review" && step.reviewPolicy && (
							<dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-2 gap-y-1 border-t border-border pt-2 text-xs text-muted-foreground">
								<dt>{t("shell.workflowsReviewPolicyLabel")}</dt>
								<dd>
									{step.reviewPolicy.decision === "skipped"
										? t("shell.workflowsReviewPolicySkipped")
										: t("shell.workflowsReviewPolicyRequired")}
								</dd>
								{step.reviewPolicy.reasons && step.reviewPolicy.reasons.length > 0 && (
									<>
										<dt>{t("shell.workflowsReviewPolicyReason")}</dt>
										<dd>{step.reviewPolicy.reasons.join(", ")}</dd>
									</>
								)}
							</dl>
						)}
						{step.kind === "review" && (step.reviewRunId || step.reviewer) && (
							<dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-2 gap-y-1 border-t border-border pt-2 text-xs text-muted-foreground">
								<dt>{t("shell.workflowsReviewer")}</dt>
								<dd>{step.reviewer || "claude-code"}</dd>
								{step.target && (
									<>
										<dt>{t("shell.workflowsTarget")}</dt>
										<dd className="font-mono">{step.target}</dd>
									</>
								)}
								{step.verdict && (
									<>
										<dt>{t("shell.workflowsVerdict")}</dt>
										<dd>{step.verdict}</dd>
									</>
								)}
								{step.findingsSummary && (
									<>
										<dt>{t("shell.workflowsFindings")}</dt>
										<dd className="whitespace-pre-wrap">{step.findingsSummary}</dd>
									</>
								)}
								{step.nextAction && (
									<>
										<dt>{t("shell.workflowsNextActionLabel")}</dt>
										<dd>{step.nextAction}</dd>
									</>
								)}
							</dl>
						)}
						{step.routing && <WorkflowRoutingSummary routing={step.routing} />}
						{step.kind === "verify" && step.verification && <WorkflowVerifyDetails result={step.verification} />}
						{step.attempts.length > 0 && (
							<ul className="mt-2 flex flex-col gap-1 border-t border-border pt-2">
								{step.attempts.map((attempt) => (
									<li className="text-xs text-muted-foreground" key={attempt.id}>
										{attempt.outcome
											? t("shell.workflowsAttemptLineWithOutcome", {
													number: attempt.attemptNumber,
													outcome: attempt.outcome,
													startedAt: new Date(attempt.startedAt).toLocaleString(),
												})
											: t("shell.workflowsAttemptLine", {
													number: attempt.attemptNumber,
													startedAt: new Date(attempt.startedAt).toLocaleString(),
												})}
										{attempt.harness && ` · ${attempt.harness}`}
										{attempt.errorClass && ` · ${attempt.errorClass}`}
									</li>
								))}
							</ul>
						)}
					</div>
				))}
			</div>

			{workflow.questions && workflow.questions.length > 0 && (
				<WorkflowQuestionsSection questions={workflow.questions} onAnswer={answerQuestion} answering={answeringQuestion} />
			)}

			{/* §26: what a finished run actually produced, so nobody has to
			    reconstruct it from the step list -- and, for a direct-branch run,
			    a plain statement that there is nothing left to integrate. */}
			{presentation ? (
				<WorkflowCompletionSummary
					commitSha={workflow.steps.find((step) => step.kind === "work")?.headSha}
					presentation={presentation}
					verdict={workflow.steps.find((step) => step.kind === "review")?.verdict}
					verificationPassed={workflow.steps.find((step) => step.kind === "verify")?.verification?.passed}
				/>
			) : null}
			{/* §5/§6: AO's own repair, inside the run it is repairing, with the
			    audit trail one link away rather than hidden. */}
			<WorkflowRepairInline
				renderLink={(runId, label) => (
					<Link
						className="text-xs text-primary underline underline-offset-2"
						params={{ workflowId: runId }}
						to="/workflows/$workflowId"
					>
						{label}
					</Link>
				)}
				repair={workflow.run.repair}
			/>
			{/* §13: where AO is working, always visible, never only in a log. */}
			{presentation ? (
				<WorkflowExecutionLocation presentation={presentation} projectId={workflow.run.projectId} />
			) : null}
			{/* §15: the bounded human timeline. */}
			{presentation ? <WorkflowTimeline events={presentation.timeline} /> : null}
			{/* §3: the technical account, present in full and ranked below the
			    human one. */}
			{presentation ? <WorkflowTechnicalDetails presentation={presentation} /> : null}

			{workflow.usage && <WorkflowUsageSection usage={workflow.usage} />}
			<WorkflowCommitDialog onOpenChange={setCommitOpen} open={commitOpen} workflowId={workflowId} />
		</div>
	);
}
