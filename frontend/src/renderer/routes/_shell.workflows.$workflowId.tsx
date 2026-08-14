import { createFileRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useWorkflowRun, workflowRunIsTerminal } from "../hooks/useWorkflowRun";

export const Route = createFileRoute("/_shell/workflows/$workflowId")({
	component: WorkflowRunRoute,
});

function WorkflowRunRoute() {
	const { t } = useTranslation();
	const { workflowId } = Route.useParams();
	const { workflow, isLoading, error, cancel, cancelling, cancelError } = useWorkflowRun(workflowId);

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

	return (
		<div className="mx-auto flex max-w-2xl flex-col gap-6 p-6">
			<div className="flex flex-col gap-1">
				<h1 className="text-lg font-semibold">{workflow.run.objective}</h1>
				<p className="text-sm text-muted-foreground">
					{t("shell.workflowsRunHeader", { projectId: workflow.run.projectId, state: workflow.run.state })}
				</p>
			</div>

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
