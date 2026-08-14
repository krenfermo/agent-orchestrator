import { Link, createFileRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useWorkflowRun, workflowRunIsTerminal } from "../hooks/useWorkflowRun";
import { WorkflowVerifyDetails } from "../components/workflow-verify-details";

export const Route = createFileRoute("/_shell/workflows/$workflowId")({
	component: WorkflowRunRoute,
});

function WorkflowRunRoute() {
	const { t } = useTranslation();
	const { workflowId } = Route.useParams();
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
	} = useWorkflowRun(workflowId);

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

	return (
		<div className="mx-auto flex max-w-2xl flex-col gap-6 p-6">
			{workflow.run.state === "completed" && (
				<div className="rounded-lg border border-green-500/40 bg-green-500/10 p-3 text-sm font-medium text-green-700 dark:text-green-300">
					{t("shell.workflowsCompletedVerified")}
				</div>
			)}
			<div className="flex flex-col gap-1">
				<h1 className="text-lg font-semibold">{workflow.run.objective}</h1>
				<p className="text-sm text-muted-foreground">
					{t("shell.workflowsRunHeader", { projectId: workflow.run.projectId, state: workflow.run.state })}
				</p>
				{workflow.run.nextAction && (
					<p className="text-sm text-muted-foreground">
						{t("shell.workflowsNextAction", { nextAction: workflow.run.nextAction })}
					</p>
				)}
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
			</div>

			{workflow.plan && (
				<section className="flex flex-col gap-3">
					<div>
						<h2 className="text-sm font-semibold">Master Plan</h2>
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
								<span className="text-xs text-muted-foreground">{task.state}</span>
							</div>
							<p className="mt-1 text-sm text-muted-foreground">{task.description}</p>
							{task.dependencies.length > 0 && <p className="mt-2 text-xs text-muted-foreground">Depends on: {task.dependencies.join(", ")}</p>}
							<ul className="mt-2 list-disc space-y-1 pl-5 text-xs text-muted-foreground">{task.acceptanceCriteria.map((criterion) => <li key={criterion}>{criterion}</li>)}</ul>
							<div className="mt-2 text-xs text-muted-foreground">
								{task.verify.commands?.map((check) => <code className="mr-2" key={`${check.command}-${check.args?.join("-")}`}>{[check.command, ...(check.args ?? [])].join(" ")}</code>)}
								{task.verify.files?.map((check) => <code className="mr-2" key={check.path}>{check.path}</code>)}
							</div>
							{task.executionWorkflowId && <Link className="mt-2 inline-block text-xs text-primary underline underline-offset-2" params={{ workflowId: task.executionWorkflowId }} to="/workflows/$workflowId">Execution workflow</Link>}
						</article>
					))}
				</section>
			)}

			<div className="flex flex-col gap-3">
				<h2 className="text-sm font-semibold text-muted-foreground">{t("shell.workflowsSteps")}</h2>
				{workflow.steps.map((step) => (
					<div className="rounded-lg border border-border p-3" key={step.id}>
						<div className="flex items-center justify-between">
							<span className="font-medium">
								{step.ordinal}. {step.kind}
							</span>
							<span className="text-xs text-muted-foreground">{step.state}</span>
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
		</div>
	);
}
