import { createFileRoute, Link, Outlet, useMatchRoute } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useExecutionPolicy } from "../hooks/useExecutionPolicy";
import { useProjectsList } from "../hooks/useProjectsList";
import {
	APPROVAL_POLICIES,
	EXECUTION_STRATEGIES,
	REPAIR_POLICIES,
	useWorkflowRuns,
	type ApprovalPolicy,
	type ExecutionStrategy,
	type RepairPolicy,
} from "../hooks/useWorkflowRuns";
import { useUiStore } from "../stores/ui-store";
import { Button } from "../components/ui/button";
import { Textarea } from "../components/ui/textarea";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "../components/ui/select";

/**
 * MAX_OBJECTIVE_BYTES mirrors domain.MaxWorkflowObjectiveBytes.
 *
 * It is duplicated rather than fetched because the check it drives is a
 * courtesy: the browser tells you before you submit, and the daemon is the
 * authority that refuses. The two must agree, so the value is asserted against
 * the generated OpenAPI schema in the route's test — a limit that drifted
 * would let the UI accept text the daemon rejects, which is exactly the
 * silent-failure shape this change exists to remove.
 */
export const MAX_OBJECTIVE_BYTES = 131072;

/**
 * Below this the counter stays hidden. A specification only becomes something
 * you have to budget once it is genuinely long.
 */
const OBJECTIVE_COUNTER_FROM_BYTES = 2000;

/** Bytes, not characters: the limit is in UTF-8 bytes and so is this. */
export function objectiveByteLength(value: string): number {
	return new TextEncoder().encode(value.trim()).length;
}

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
	const { policy: executionPolicy, isLoading: policyLoading } = useExecutionPolicy();
	const openGlobalSettings = useUiStore((state) => state.openGlobalSettings);
	const [projectId, setProjectId] = useState(initialProjectIdFromSearch);
	const [objective, setObjective] = useState("");
	// Byte length, recomputed only when the text changes: TextEncoder on every
	// keystroke of a 100 KB specification would be felt.
	const objectiveBytes = useMemo(() => objectiveByteLength(objective), [objective]);
	const objectiveTooLong = objectiveBytes > MAX_OBJECTIVE_BYTES;
	// P1-A: two independent choices, deliberately not one.
	//
	// Execution strategy is how much orchestration this run gets — a bounded
	// Task, an Autonomous plan, or a Master initiative broken into
	// workstreams. Autonomous is the default for normal project work.
	//
	// Approval is who approves and drives. Checkpoint 8P-D.1's requirement is
	// unchanged and now lives here alone: the user must be able to pick it
	// deliberately for THIS run rather than inherit it invisibly from the
	// global Settings → Execution Policy toggle. `approvalChoice` starts
	// undefined so the radio reflects the stored policy once it loads, and
	// any explicit click wins from then on.
	const [strategy, setStrategy] = useState<ExecutionStrategy>("autonomous");
	const [approvalChoice, setApprovalChoice] = useState<ApprovalPolicy | undefined>(undefined);
	const approvalDefault: ApprovalPolicy = executionPolicy?.autonomousMode ? "automatic" : "manual";
	const approvalPolicy = approvalChoice ?? approvalDefault;
	const strategyLabels: Record<ExecutionStrategy, { label: string; explainer: string }> = {
		task: {
			label: t("shell.workflowsStrategyTaskLabel"),
			explainer: t("shell.workflowsStrategyTaskExplainer"),
		},
		autonomous: {
			label: t("shell.workflowsStrategyAutonomousLabel"),
			explainer: t("shell.workflowsStrategyAutonomousExplainer"),
		},
		master: {
			label: t("shell.workflowsStrategyMasterLabel"),
			explainer: t("shell.workflowsStrategyMasterExplainer"),
		},
	};
	// P1-B: auto-repair is a third independent axis, frozen at creation.
	// "suggest" is the default because a repair writes code, and opting into
	// that unattended should be a decision somebody made.
	const [repairPolicy, setRepairPolicy] = useState<RepairPolicy>("suggest");
	const repairLabels: Record<RepairPolicy, { label: string; explainer: string }> = {
		disabled: {
			label: t("shell.workflowsRepairPolicyDisabledLabel"),
			explainer: t("shell.workflowsRepairPolicyDisabledExplainer"),
		},
		suggest: {
			label: t("shell.workflowsRepairPolicySuggestLabel"),
			explainer: t("shell.workflowsRepairPolicySuggestExplainer"),
		},
		automatic: {
			label: t("shell.workflowsRepairPolicyAutomaticLabel"),
			explainer: t("shell.workflowsRepairPolicyAutomaticExplainer"),
		},
	};
	const approvalLabels: Record<ApprovalPolicy, { label: string; explainer: string }> = {
		automatic: {
			label: t("shell.workflowsApprovalAutomaticLabel"),
			explainer: t("shell.workflowsApprovalAutomaticExplainer"),
		},
		manual: {
			label: t("shell.workflowsApprovalManualLabel"),
			explainer: t("shell.workflowsApprovalManualExplainer"),
		},
	};

	// The preselected project from a deep link may not (yet) be in the loaded
	// list; keep the select controlled either way, it just shows no match.
	const selectedProject = useMemo(() => projects.find((p) => p.id === projectId), [projects, projectId]);

	const onSubmit = (event: React.FormEvent) => {
		event.preventDefault();
		if (!projectId.trim() || !objective.trim()) return;
		void createRun({
			projectId: projectId.trim(),
			objective: objective.trim(),
			strategy,
			approvalPolicy,
			repairPolicy,
		}).then(() => {
			setObjective("");
		});
	};

	const noProjects = !projectsLoading && projects.length === 0;

	return (
		// Owns its own vertical scroll for the same reason the detail route
		// does: the shell hands routes a `min-h-0 flex-1` box with no scroll of
		// its own, so a long workflow list ran off the bottom of the window.
		// See that route for why `[&>*]:shrink-0` is required here.
		<div className="mx-auto flex h-full min-h-0 max-w-2xl flex-col gap-6 overflow-y-auto break-words p-6 [&>*]:shrink-0">
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
						<span className="flex items-baseline justify-between gap-2">
							{t("shell.workflowsObjective")}
							{/* The counter appears only once a specification is long enough for
							    the ceiling to be worth knowing about. Showing "12 / 131072" next
							    to a one-line objective would be noise about a limit nobody is
							    anywhere near. */}
							{objectiveBytes > OBJECTIVE_COUNTER_FROM_BYTES ? (
								<span
									className={
										objectiveTooLong
											? "text-xs font-medium text-destructive"
											: "text-xs text-muted-foreground"
									}
								>
									{t("shell.workflowsObjectiveSize", {
										bytes: objectiveBytes.toLocaleString(),
										max: MAX_OBJECTIVE_BYTES.toLocaleString(),
									})}
								</span>
							) : null}
						</span>
						<Textarea
							aria-invalid={objectiveTooLong || undefined}
							onChange={(event) => setObjective(event.target.value)}
							placeholder={t("shell.workflowsObjectivePlaceholder")}
							rows={8}
							value={objective}
						/>
						{objectiveTooLong ? (
							<span className="text-xs text-destructive">
								{t("shell.workflowsObjectiveTooLong", {
									bytes: objectiveBytes.toLocaleString(),
									max: MAX_OBJECTIVE_BYTES.toLocaleString(),
								})}
							</span>
						) : null}
					</label>
					<fieldset className="flex flex-col gap-2">
						<legend className="text-sm">{t("shell.workflowsStrategy")}</legend>
						{EXECUTION_STRATEGIES.map((value) => (
							<label
								className={`flex cursor-pointer flex-col gap-0.5 rounded border px-3 py-2 text-xs ${
									strategy === value ? "border-primary bg-primary/5" : "border-border bg-muted/40"
								}`}
								key={value}
							>
								<span className="flex items-center gap-2 font-medium text-foreground">
									<input
										checked={strategy === value}
										name="workflow-execution-strategy"
										onChange={() => setStrategy(value)}
										type="radio"
										value={value}
									/>
									{strategyLabels[value].label}
								</span>
								<span className="pl-5 text-muted-foreground">{strategyLabels[value].explainer}</span>
							</label>
						))}
					</fieldset>
					<fieldset className="flex flex-col gap-2" disabled={policyLoading}>
						<legend className="text-sm">{t("shell.workflowsApproval")}</legend>
						{APPROVAL_POLICIES.map((value) => (
							<label
								className={`flex cursor-pointer flex-col gap-0.5 rounded border px-3 py-2 text-xs ${
									approvalPolicy === value ? "border-primary bg-primary/5" : "border-border bg-muted/40"
								}`}
								key={value}
							>
								<span className="flex items-center gap-2 font-medium text-foreground">
									<input
										checked={approvalPolicy === value}
										name="workflow-approval-policy"
										onChange={() => setApprovalChoice(value)}
										type="radio"
										value={value}
									/>
									{approvalLabels[value].label}
								</span>
								<span className="pl-5 text-muted-foreground">{approvalLabels[value].explainer}</span>
							</label>
						))}
					</fieldset>
					<fieldset className="flex flex-col gap-2">
						<legend className="text-sm">{t("shell.workflowsRepairPolicy")}</legend>
						{REPAIR_POLICIES.map((value) => (
							<label
								className={`flex cursor-pointer flex-col gap-0.5 rounded border px-3 py-2 text-xs ${
									repairPolicy === value ? "border-primary bg-primary/5" : "border-border bg-muted/40"
								}`}
								key={value}
							>
								<span className="flex items-center gap-2 font-medium text-foreground">
									<input
										checked={repairPolicy === value}
										name="workflow-repair-policy"
										onChange={() => setRepairPolicy(value)}
										type="radio"
										value={value}
									/>
									{repairLabels[value].label}
								</span>
								<span className="pl-5 text-muted-foreground">{repairLabels[value].explainer}</span>
							</label>
						))}
					</fieldset>
					<button
						className="mt-1 self-start rounded bg-primary px-3 py-1.5 text-sm text-primary-foreground disabled:opacity-50"
						disabled={creating || !projectId.trim() || !objective.trim() || objectiveTooLong}
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
