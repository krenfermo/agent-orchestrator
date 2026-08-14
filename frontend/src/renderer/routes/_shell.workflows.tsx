import { createFileRoute, Link } from "@tanstack/react-router";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useWorkflowRuns } from "../hooks/useWorkflowRuns";

export const Route = createFileRoute("/_shell/workflows")({
	component: WorkflowsListRoute,
});

function WorkflowsListRoute() {
	const { t } = useTranslation();
	const { runs, isLoading, error, createRun, creating, createError } = useWorkflowRuns();
	const [projectId, setProjectId] = useState("");
	const [objective, setObjective] = useState("");

	const onSubmit = (event: React.FormEvent) => {
		event.preventDefault();
		if (!projectId.trim() || !objective.trim()) return;
		void createRun({ projectId: projectId.trim(), objective: objective.trim() }).then(() => {
			setObjective("");
		});
	};

	return (
		<div className="mx-auto flex max-w-2xl flex-col gap-6 p-6">
			<h1 className="text-lg font-semibold">{t("shell.workflows")}</h1>

			<form className="flex flex-col gap-2 rounded-lg border border-border p-4" onSubmit={onSubmit}>
				<label className="flex flex-col gap-1 text-sm">
					{t("shell.workflowsProjectId")}
					<input
						className="rounded border border-border bg-background px-2 py-1"
						onChange={(event) => setProjectId(event.target.value)}
						placeholder={t("shell.workflowsProjectIdPlaceholder")}
						value={projectId}
					/>
				</label>
				<label className="flex flex-col gap-1 text-sm">
					{t("shell.workflowsObjective")}
					<input
						className="rounded border border-border bg-background px-2 py-1"
						onChange={(event) => setObjective(event.target.value)}
						placeholder={t("shell.workflowsObjectivePlaceholder")}
						value={objective}
					/>
				</label>
				<button
					className="mt-1 self-start rounded bg-primary px-3 py-1.5 text-sm text-primary-foreground disabled:opacity-50"
					disabled={creating || !projectId.trim() || !objective.trim()}
					type="submit"
				>
					{creating ? t("shell.workflowsCreating") : t("shell.workflowsCreate")}
				</button>
				{createError && <p className="text-sm text-destructive">{createError}</p>}
			</form>

			{isLoading && <p className="text-sm text-muted-foreground">{t("shell.workflowsLoading")}</p>}
			{error && <p className="text-sm text-destructive">{error}</p>}

			<ul className="flex flex-col gap-2">
				{runs.map((run) => (
					<li className="rounded-lg border border-border p-3" key={run.id}>
						<Link
							className="flex flex-col gap-1"
							params={{ workflowId: run.id }}
							to="/workflows/$workflowId"
						>
							<span className="font-medium">{run.objective}</span>
							<span className="text-xs text-muted-foreground">
								{t("shell.workflowsRunMeta", {
									projectId: run.projectId,
									state: run.state,
									createdAt: new Date(run.createdAt).toLocaleString(),
								})}
							</span>
						</Link>
					</li>
				))}
				{!isLoading && runs.length === 0 && (
					<li className="text-sm text-muted-foreground">{t("shell.workflowsNone")}</li>
				)}
			</ul>
		</div>
	);
}
