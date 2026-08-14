import { Link, createFileRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useWorkflowRun, workflowRunIsTerminal } from "../hooks/useWorkflowRun";

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
				{isPending && (
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
								<dt>{t("shell.workflowsHarness")}</dt>
								<dd>{step.assignedHarness || "codex"}</dd>
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
