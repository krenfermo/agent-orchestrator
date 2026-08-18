import { createFileRoute, Link, Outlet, useMatchRoute } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useExecutionPolicy } from "../hooks/useExecutionPolicy";
import { useProjectsList } from "../hooks/useProjectsList";
import { useWorkflowRuns } from "../hooks/useWorkflowRuns";
import { useUiStore } from "../stores/ui-store";
import { Button } from "../components/ui/button";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "../components/ui/select";

export const Route = createFileRoute("/_shell/workflows")({
	component: WorkflowsListRoute,
});

function WorkflowsListRoute() {
	const matchRoute = useMatchRoute();
	if (matchRoute({ to: "/workflows/$workflowId", fuzzy: true })) {
		return <Outlet />;
	}
	return <WorkflowsList />;
}

function initialProjectIdFromSearch(): string {
	if (typeof window === "undefined") return "";
	return new URLSearchParams(window.location.search).get("projectId") ?? "";
}

export function WorkflowsList() {
	const { t } = useTranslation();
	const { runs, isLoading, error, createRun, creating, createError } = useWorkflowRuns();
	const { projects, isLoading: projectsLoading } = useProjectsList();
	const { policy: executionPolicy } = useExecutionPolicy();
	const autonomous = executionPolicy?.autonomousMode ?? false;
	const openGlobalSettings = useUiStore((state) => state.openGlobalSettings);
	const [projectId, setProjectId] = useState(initialProjectIdFromSearch);
	const [objective, setObjective] = useState("");

	// The preselected project from a deep link may not (yet) be in the loaded
	// list; keep the select controlled either way, it just shows no match.
	const selectedProject = useMemo(() => projects.find((p) => p.id === projectId), [projects, projectId]);

	const onSubmit = (event: React.FormEvent) => {
		event.preventDefault();
		if (!projectId.trim() || !objective.trim()) return;
		void createRun({ projectId: projectId.trim(), objective: objective.trim() }).then(() => {
			setObjective("");
		});
	};

	const noProjects = !projectsLoading && projects.length === 0;

	return (
		<div className="mx-auto flex max-w-2xl flex-col gap-6 p-6">
			<h1 className="text-lg font-semibold">{t("shell.workflows")}</h1>

			{noProjects ? (
				<div className="flex flex-col gap-3 rounded-lg border border-border p-4">
					<p className="text-sm text-muted-foreground">{t("shell.workflowsNoProjects")}</p>
					<Button
						type="button"
						variant="outline"
						className="self-start"
						onClick={() => openGlobalSettings("projects")}
					>
						{t("shell.workflowsGoToSettings")}
					</Button>
				</div>
			) : (
				<form className="flex flex-col gap-3 rounded-lg border border-border p-4" onSubmit={onSubmit}>
					<label className="flex flex-col gap-1 text-sm">
						{t("shell.workflowsProjectLabel")}
						<Select value={projectId} onValueChange={setProjectId}>
							<SelectTrigger className="w-full" aria-label={t("shell.workflowsProjectLabel")}>
								<SelectValue placeholder={t("shell.workflowsSelectProjectPlaceholder")}>
									{selectedProject ? selectedProject.name : undefined}
								</SelectValue>
							</SelectTrigger>
							<SelectContent>
								{projects.map((project) => (
									<SelectItem key={project.id} value={project.id}>
										<div className="flex flex-col">
											<span>{project.name}</span>
											<span className="text-xs text-muted-foreground">{project.repo || project.path}</span>
										</div>
									</SelectItem>
								))}
							</SelectContent>
						</Select>
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
					<div className="rounded border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
						<span className="font-medium text-foreground">
							{autonomous
								? t("shell.workflowsModeAutonomousLabel")
								: t("shell.workflowsModeManualLabel")}
						</span>
						<p className="mt-0.5">
							{autonomous
								? t("shell.workflowsModeAutonomousExplainer")
								: t("shell.workflowsModeManualExplainer")}
						</p>
					</div>
					<button
						className="mt-1 self-start rounded bg-primary px-3 py-1.5 text-sm text-primary-foreground disabled:opacity-50"
						disabled={creating || !projectId.trim() || !objective.trim()}
						type="submit"
					>
						{creating ? t("shell.workflowsCreating") : t("shell.workflowsCreate")}
					</button>
					{createError && <p className="text-sm text-destructive">{createError}</p>}
				</form>
			)}

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
