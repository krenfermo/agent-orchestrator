import { useState } from "react";
import { Link, createFileRoute } from "@tanstack/react-router";
import { Archive } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { useWorkflowRun, workflowRunIsTerminal } from "../hooks/useWorkflowRun";
import { useWorkflowStatusLabel } from "../hooks/useWorkflowExecutionStatus";
import { useChildTaskRouting } from "../hooks/useChildTaskRouting";
import { WorkflowVerifyDetails } from "../components/workflow-verify-details";
import { WorkflowUsageSection } from "../components/workflow-usage-section";
import { WorkflowQuestionsSection } from "../components/workflow-questions-section";
import { WorkflowCapacityWaitBanner } from "../components/workflow-capacity-wait-banner";
import { WorkflowBranchWaitBanner } from "../components/workflow-branch-wait-banner";
import { WorkflowRoutingSummary } from "../components/workflow-routing-summary";
import { WorkflowResumeButton } from "../components/workflow-resume-button";
import {
	translateDynamic,
	WorkflowActivityPanel,
	WorkflowPhaseBadge,
	WorkflowStepIcon,
} from "../components/workflow-activity";
import { processedTokens } from "../components/workflow-usage-section";
import { formatElapsedCompact } from "../lib/format-time";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { canCancelAndArchive, useCancelAndArchiveWorkflow } from "../hooks/useWorkflowArchive";
import type { components } from "../../api/schema";

export const Route = createFileRoute("/_shell/workflows/$workflowId")({
	component: WorkflowRunRoute,
});

type TranslateFn = (key: string, opts?: Record<string, unknown>) => string;

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

	const nonTerminal = !workflowRunIsTerminal(workflow.run.state);
	const isPending = workflow.run.state === "pending";
	const workStep = workflow.steps.find((step) => step.kind === "work");
	const reviewStep = workflow.steps.find((step) => step.kind === "review");
	const canContinueReview = Boolean(
		workStep?.state === "completed" && (reviewStep?.state === "pending" || reviewStep?.state === "ready"),
	);

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
			<WorkflowBranchWaitBanner run={workflow.run} />
			<div className="flex flex-col gap-1">
				<div className="flex items-start gap-2">
					<h1 className="min-w-0 flex-1 text-lg font-semibold">{workflow.run.objective}</h1>
					{/* The header's own "what is this run doing" answer: the same
					    badge (and the same single spinner) the board card uses, so
					    the two surfaces can never disagree. */}
					<WorkflowPhaseBadge className="mt-1" phase={workflow.run.phase} />
				</div>
				<p className="text-sm text-muted-foreground">
					{t("shell.workflowsRunHeader", { projectId: workflow.run.projectId, state: workflow.run.state })}
				</p>
				<p className="text-sm text-muted-foreground">
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
				{canContinueReview && (
					<div>
						<button
							className="rounded border border-border px-3 py-1.5 text-sm disabled:opacity-50"
							disabled={continuing}
							onClick={() => void continueRun()}
							type="button"
						>
							{continuing ? t("shell.workflowsContinuing") : t("shell.workflowsStartReview")}
						</button>
						{continueError && <p className="mt-1 text-sm text-destructive">{continueError}</p>}
					</div>
				)}
				{/* Resume, for a run stopped on something a person has now dealt
				    with. Gated on the backend's authoritative canContinue flag —
				    never on a state string read here — so a terminal or
				    nonrecoverable stop never gets a button that provably does
				    nothing. Hidden when canContinueReview already renders the
				    same POST under its own, more specific label. */}
				{workflow.run.canContinue && !canContinueReview && (
					<WorkflowResumeButton
						attentionWorkflowId={workflow.run.attentionWorkflowId}
						continueError={continueError}
						continueRun={continueRun}
						continuing={continuing}
					/>
				)}
				{nonTerminal && (
					<div>
						<button
							className="rounded border border-border px-3 py-1.5 text-sm disabled:opacity-50"
							disabled={cancelling}
							onClick={() => void cancel()}
							type="button"
						>
							{cancelling ? t("shell.workflowsCancelling") : t("shell.workflowsCancel")}
						</button>
						{cancelError && <p className="mt-1 text-sm text-destructive">{cancelError}</p>}
					</div>
				)}
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

			{workflow.usage && <WorkflowUsageSection usage={workflow.usage} />}
		</div>
	);
}
