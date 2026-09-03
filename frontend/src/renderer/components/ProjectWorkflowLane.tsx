import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "@tanstack/react-router";
import { AlertTriangle, Archive, Hourglass, Wrench } from "lucide-react";
import {
	needsHumanDecision,
	useProjectBoard,
	type BoardBranchWait,
	type BoardRepair,
	type BoardView,
	type BoardWorkflow,
	type BoardWorkflowTask,
} from "../hooks/useProjectBoard";
import {
	WorkflowProgress,
	WorkflowStageBadge,
	WorkflowStatusSummary,
} from "./workflow-status";
import { actionLabelKey } from "../lib/workflow-presentation";
import {
	canCancelAndArchive,
	useCancelAndArchiveWorkflow,
	useProjectBoardHistory,
} from "../hooks/useWorkflowArchive";
import { ConfirmDialog } from "./ConfirmDialog";
import { formatTimeCompact } from "../lib/format-time";
import { cn } from "../lib/utils";
import {
	isActivePhase,
	phaseBorderClass,
	translateDynamic,
	WorkflowActivityPanel,
	WorkflowPhaseBadge,
	WorkflowStepChecklist,
	WorkflowTaskPlannerBadge,
} from "./workflow-activity";

/**
 * The project Board's workflow lane.
 *
 * It exists because the sessions grid answers a different question. A card
 * there reports what a *session* is doing, and a worker session sits idle for
 * the entire review/fix/verify tail of a workflow — so an actively progressing
 * workflow read as an idle one. This lane reports the workflow itself, from the
 * daemon's own lifecycle projection, and never from session activity state.
 */
export function ProjectWorkflowLane({ projectId }: { projectId: string }) {
	const { t } = useTranslation();
	const navigate = useNavigate();
	// P3-B §14/§20: the four views are FILTERS the daemon applies over one
	// projection, not four different cards. Switching view changes the query,
	// never the way a run is described.
	const [view, setView] = useState<BoardView>("active");
	const { workflows, counts } = useProjectBoard(projectId, view);
	const archive = useCancelAndArchiveWorkflow(projectId);
	// Archived workflows are read whether or not the archived view is open,
	// because their existence is what decides whether this lane appears at all:
	// once the last active workflow is archived there would otherwise be no lane
	// left to reveal the archive from. The query never polls — an archived
	// workflow is finished by definition.
	const { workflows: archived, isLoading: archivedLoading } = useProjectBoardHistory(projectId, true);

	// Nothing active and nothing archived: this lane has never had anything to
	// say about the project, and stays out of the way entirely.
	if (workflows.length === 0 && archived.length === 0 && counts.active === 0) return null;

	const openWorkflow = (workflowId: string) =>
		void navigate({ to: "/workflows/$workflowId", params: { workflowId } });

	const shown = view === "archived" ? archived : workflows;

	return (
		<section
			aria-label={t("board.workflowsAria")}
			className="flex shrink-0 flex-col gap-2 border-b border-border px-3 py-3"
			data-testid="project-workflow-lane"
		>
			<div className="flex items-center justify-between gap-2">
				<h2 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
					{t("board.workflows")}
				</h2>
				<BoardViewTabs archivedCount={archived.length} counts={counts} onSelect={setView} view={view} />
			</div>
			{view === "archived" && archivedLoading ? (
				<p className="text-xs text-muted-foreground">{t("board.archivedLoading")}</p>
			) : shown.length === 0 ? (
				<p className="text-xs text-muted-foreground" data-testid="workflow-view-empty">
					{t(`board.view.${view}Empty` as "board.view.activeEmpty")}
				</p>
			) : (
				<div className="flex flex-col gap-2" data-testid={`workflow-view-${view}`}>
					{shown.map((workflow) => (
						<WorkflowBoardCard
							archived={view === "archived"}
							archiveError={archive.error?.message}
							archivePending={archive.isPending}
							key={workflow.workflowId}
							onArchive={view === "archived" ? undefined : archive.mutateAsync}
							onOpen={() => openWorkflow(workflow.workflowId)}
							onOpenRun={openWorkflow}
							workflow={workflow}
						/>
					))}
				</div>
			)}
		</section>
	);
}

/**
 * The Board's four views, with the daemon's own counts (§22).
 *
 * The numbers come from the board response, computed by the same rules the
 * cards are, and a repair nested under its origin is not among them: "3
 * workflows need attention" when the truth is one origin and two of its repairs
 * is the misreport those counts exist to prevent. The archived tally is the
 * length of the archived list, which is the only place archived runs live.
 */
function BoardViewTabs({
	view,
	counts,
	archivedCount,
	onSelect,
}: {
	view: BoardView;
	counts: { active: number; needsAttention: number; completed: number };
	archivedCount: number;
	onSelect: (view: BoardView) => void;
}) {
	const { t } = useTranslation();
	const tabs: { id: BoardView; count: number }[] = [
		{ id: "active", count: counts.active },
		{ id: "attention", count: counts.needsAttention },
		{ id: "completed", count: counts.completed },
		{ id: "archived", count: archivedCount },
	];
	return (
		<div className="flex items-center gap-1" data-testid="workflow-board-views" role="tablist">
			{tabs.map((tab) => (
				<button
					aria-selected={view === tab.id}
					className={cn(
						"rounded px-1.5 py-0.5 text-xs",
						view === tab.id ? "bg-muted font-medium text-foreground" : "text-muted-foreground hover:text-foreground",
					)}
					data-testid={`workflow-board-view-${tab.id}`}
					key={tab.id}
					onClick={() => onSelect(tab.id)}
					role="tab"
					type="button"
				>
					{t(`board.view.${tab.id}` as "board.view.active")}
					<span className="ml-1 tabular-nums text-passive">{tab.count}</span>
				</button>
			))}
		</div>
	);
}

/**
 * One Board card — P3-B §28.
 *
 * Everything it says about status comes from `workflow.presentation`, which IS
 * the run detail page's projection: the same stage, the same sentence, the same
 * progression, the same placement, the same recommended action. The card is
 * SHORTER than the page, never different from it, and this component computes
 * no status of its own — there is no phase mapping here, no "is this really
 * blocked" rule, and no Git logic.
 *
 * Exported and free of data fetching/routing so it can be rendered directly
 * against a projection value in tests: the presentation of "what is AO doing"
 * is exactly the part worth pinning down.
 */
export function WorkflowBoardCard({
	workflow,
	onOpen,
	onOpenRun,
	onArchive,
	archivePending,
	archiveError,
	archived,
}: {
	workflow: BoardWorkflow;
	onOpen?: () => void;
	/** Opens any run by id — used by the inline repairs to reach their own run. */
	onOpenRun?: (workflowId: string) => void;
	/** Cancel-and-archive. Absent on an archived card, which has nothing left to stop. */
	onArchive?: (workflowId: string) => Promise<unknown>;
	archivePending?: boolean;
	archiveError?: string;
	archived?: boolean;
}) {
	const { t } = useTranslation();
	const presentation = workflow.presentation;
	// The daemon's own flags. `needsHumanDecision` stays as the fallback for a
	// daemon that predates the projection, and the two agree by construction.
	const human = presentation ? presentation.requiresHuman : needsHumanDecision(workflow);
	const title = objectiveTitle(workflow.objective);
	const active = isActivePhase(workflow.phase);
	// The action never appears for a workflow AO is actively driving: the
	// renderer must not offer to retire something that is still working.
	const showArchiveAction = Boolean(onArchive) && !archived && canCancelAndArchive(workflow);

	return (
		<div
			className={cn("flex flex-col gap-1", archived && "opacity-70")}
			data-archived={archived ? "true" : undefined}
		>
		<button
			// An active card is meant to be findable from across the room: the
			// accent border and tinted surface are the "which one is moving"
			// signal, and the stage badge, spinner and summary below say which
			// kind of movement it is.
			className={cn(
				"flex w-full flex-col gap-2 rounded-lg border bg-surface px-3 py-2.5 text-left transition-colors hover:bg-muted/40",
				human ? "border-warning" : phaseBorderClass(workflow.phase),
				active && "bg-status-working/5 shadow-[inset_2px_0_0_0_var(--color-status-working)]",
			)}
			data-active={active ? "true" : undefined}
			data-testid={`workflow-card-${workflow.workflowId}`}
			onClick={onOpen}
			type="button"
		>
			<div className="flex min-w-0 items-center gap-2">
				<span className="min-w-0 flex-1 truncate text-sm font-medium">{title}</span>
				{presentation ? (
					<WorkflowStageBadge stage={presentation.stage} />
				) : (
					<WorkflowPhaseBadge phase={workflow.phase} />
				)}
			</div>

			{/* §17: a Task's objective can be its whole specification. The card
			    shows the title and says so; the run page shows it in full. */}
			{workflow.objectiveTruncated ? (
				<p className="text-xs text-muted-foreground" data-testid="workflow-spec-hint">
					{t("board.longSpecification")}
				</p>
			) : null}

			{/* The human sentence, from the daemon's summary code. It is the same
			    sentence the run page puts at the top, because it is the same code. */}
			{presentation ? <WorkflowStatusSummary presentation={presentation} /> : null}

			{workflow.tasksTotal > 0 ? <TaskHeadline workflow={workflow} /> : null}

			{/* Stages, never a percentage. The progression REPLACES the raw step
			    checklist when the daemon sends one: they answer the same question,
			    and the projection answers it in the vocabulary the run page uses.
			    A daemon that predates the projection still gets the checklist. */}
			{presentation ? (
				<WorkflowProgress stages={presentation.progress} />
			) : workflow.steps && workflow.steps.length > 0 ? (
				<WorkflowStepChecklist steps={workflow.steps} />
			) : null}

			{/* What AO is doing at this exact moment, which the progression does
			    not say: which step, and on which task. */}
			<WorkflowActivityPanel
				facts={[{ label: t("board.factTask"), value: workflow.currentTaskTitle }]}
				phase={workflow.phase}
			/>

			<PlacementLine workflow={workflow} />

			{workflow.branchWait ? <BranchWaitLine wait={workflow.branchWait} /> : null}

			{/* §6: a repair is AO's own work on THIS run, shown under it rather
			    than as a card of its own, and with none of the origin's buttons. */}
			{workflow.repairs && workflow.repairs.length > 0 ? (
				<RepairInline onOpenRun={onOpenRun} repairs={workflow.repairs} />
			) : null}

			{/* §8: the one thing AO recommends. It is shown only when the daemon
			    both recommends it and offers it enabled — the renderer never
			    invents an action or its availability. */}
			<RecommendedActionHint workflow={workflow} />

			{!presentation && human ? <HumanDecisionNotice workflow={workflow} /> : null}
			{!presentation && !human ? <InternalStatusLine workflow={workflow} /> : null}

			<p className="text-xs text-muted-foreground">
				{[
					workflow.harness || undefined,
					workflow.model || undefined,
					// §11: the workflow's last MEANINGFUL act, not its last write.
					// A heartbeat must not make a run stalled for hours read as
					// active two seconds ago.
					t("board.lastActivity", {
						time: formatTimeCompact(workflow.lastMeaningfulActivityAt || workflow.lastActivityAt),
					}),
				]
					.filter(Boolean)
					.join(" · ")}
			</p>

			<p
				className="font-mono text-[10px] text-muted-foreground/70"
				data-testid="workflow-run-id"
				title={t("board.runIdTitle")}
			>
				{t("board.runId", { runId: workflow.workflowId })}
			</p>

			{workflow.tasks && workflow.tasks.length > 0 ? <ChildTaskList tasks={workflow.tasks} /> : null}
		</button>
			{showArchiveAction && onArchive ? (
				<CancelAndArchiveAction
					error={archiveError}
					onConfirm={() => onArchive(workflow.workflowId)}
					pending={Boolean(archivePending)}
					workflowId={workflow.workflowId}
				/>
			) : null}
		</div>
	);
}

/**
 * Where AO is working, and whether anything is left to integrate (§15/§16).
 *
 * Read from the run's FROZEN placement, never from the project's default: a run
 * that AO placed on the current branch inside a project configured for isolated
 * worktrees must read "current branch", because that is where its work is. The
 * integration line uses the daemon's five-value answer, so a direct-branch run
 * says "nothing to integrate" instead of a generic "merge pending".
 */
function PlacementLine({ workflow }: { workflow: BoardWorkflow }) {
	const { t } = useTranslation();
	const placement = workflow.presentation?.placement;
	if (!placement) return null;
	const parts = [
		translateDynamic(t, `wf.placement.${placement.type}`, placement.type),
		placement.executionBranch || undefined,
	].filter(Boolean) as string[];
	return (
		<p className="truncate text-xs text-muted-foreground" data-testid="workflow-placement">
			<span className="font-mono">{parts.join(" · ")}</span>
			{/* §15: the daemon's five-value answer, so a direct-branch run says
			    "nothing to integrate" instead of a generic "merge pending". */}
			{placement.integration && placement.integration !== "not_required" ? (
				<span
					className={cn("ml-2", placement.integration === "failed" ? "text-destructive" : "text-warning")}
					data-testid="workflow-integration"
				>
					{translateDynamic(t, `wf.integration.${placement.integration}`, placement.integration)}
				</span>
			) : null}
		</p>
	);
}

/**
 * The automatic repairs of this run, inline (§6).
 *
 * A repair carries "attempt N of M", its own stage and its outcome, and a link
 * to its full run for anyone who wants the technical view. It deliberately has
 * no Resume/Repair of its own: those belong to the origin, and offering a
 * second copy is the duplicate remedy §5 forbids.
 */
function RepairInline({
	repairs,
	onOpenRun,
}: {
	repairs: BoardRepair[];
	onOpenRun?: (workflowId: string) => void;
}) {
	const { t } = useTranslation();
	return (
		<div className="flex flex-col gap-0.5 rounded border border-border bg-muted/30 px-2 py-1.5" data-testid="workflow-repair-inline">
			<span className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
				<Wrench aria-hidden="true" className="size-3 shrink-0" />
				{t("wf.repair.title")}
			</span>
			{repairs.map((repair) => (
				<div className="flex items-center gap-2 text-xs" data-testid={`workflow-repair-${repair.workflowId}`} key={repair.workflowId}>
					<span className="text-muted-foreground">
						{t("wf.repair.attempt", { attempt: repair.attempt, budget: repair.budget })}
					</span>
					<span className="text-foreground">{translateDynamic(t, `wf.stage.${repair.stage}`, repair.stage)}</span>
					{repair.failed ? <span className="text-destructive">{t("board.repairFailed")}</span> : null}
					{repair.succeeded ? <span className="text-success">{t("board.repairSucceeded")}</span> : null}
					{onOpenRun ? (
						<span
							className="ml-auto shrink-0 cursor-pointer text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
							data-testid={`workflow-repair-open-${repair.workflowId}`}
							onClick={(event) => {
								// The card itself is a button; opening the repair must
								// not also open the origin behind it.
								event.stopPropagation();
								onOpenRun(repair.workflowId);
							}}
							role="link"
							tabIndex={-1}
						>
							{t("wf.repair.viewDetails")}
						</span>
					) : null}
				</div>
			))}
		</div>
	);
}

/**
 * The recommended action, as a label rather than a second control (§8/§9).
 *
 * The Board says WHAT AO recommends and sends you to the run to do it. It never
 * performs the action from here, which is what keeps the two surfaces from
 * being able to disagree about whether it is allowed: the run page renders the
 * same offered actions with the same enabled flags, and the daemon refuses a
 * stale one idempotently either way. A recommendation the daemon offers
 * disabled is not shown at all — the Board must never present an action the
 * detail page would refuse.
 */
function RecommendedActionHint({ workflow }: { workflow: BoardWorkflow }) {
	const { t } = useTranslation();
	const presentation = workflow.presentation;
	const recommended = presentation?.recommendedAction;
	if (!presentation || !recommended) return null;
	const offered = (presentation.actions ?? []).find((action) => action.id === recommended);
	if (!offered || !offered.enabled) return null;
	return (
		<p className="text-xs font-medium text-foreground" data-testid="workflow-recommended-action">
			{t("board.recommends", { action: translateDynamic(t, actionLabelKey(recommended), recommended) })}
		</p>
	);
}

/**
 * "Cancelar y archivar", with the confirmation its consequences deserve.
 *
 * The dialog says both halves out loud, because they are easy to confuse and
 * only one of them is irreversible: execution stops (this workflow and any
 * child workflows are cancelled and the branch lock is released), and the
 * history is kept (every run, step, attempt, checkpoint and review record stays
 * on disk, readable under "Mostrar archivados").
 */
function CancelAndArchiveAction({
	workflowId,
	onConfirm,
	pending,
	error,
}: {
	workflowId: string;
	onConfirm: () => Promise<unknown>;
	pending: boolean;
	error?: string;
}) {
	const { t } = useTranslation();
	const [open, setOpen] = useState(false);

	return (
		<>
			<div className="flex justify-end">
				<button
					className="flex items-center gap-1 rounded px-2 py-1 text-xs text-muted-foreground hover:bg-muted/50 hover:text-foreground"
					data-testid={`workflow-archive-${workflowId}`}
					onClick={() => setOpen(true)}
					type="button"
				>
					<Archive aria-hidden="true" className="size-3.5 shrink-0" />
					{t("board.cancelAndArchive")}
				</button>
			</div>
			<ConfirmDialog
				busy={pending}
				confirmLabel={t("board.cancelAndArchive")}
				description={t("board.cancelAndArchiveExplain")}
				destructive
				error={error ?? null}
				onConfirm={() => {
					// The dialog stays open on failure so the error is readable
					// next to the action that produced it.
					void onConfirm()
						.then(() => setOpen(false))
						.catch(() => {});
				}}
				onOpenChange={setOpen}
				open={open}
				title={t("board.cancelAndArchiveConfirm")}
			/>
		</>
	);
}

/** The objective's first line: a workflow's name is the thing it was asked to do. */
function objectiveTitle(objective: string): string {
	const first = objective.split("\n", 1)[0]?.trim();
	return first && first.length > 0 ? first : objective.trim();
}

function TaskHeadline({ workflow }: { workflow: BoardWorkflow }) {
	const { t } = useTranslation();
	// "Task 2 of 7" comes from the running task's own ordinal, so it stays
	// correct even when tasks complete out of ordinal order.
	const position = workflow.currentTaskOrdinal ?? 0;
	return (
		<p className="text-xs text-muted-foreground">
			{position > 0
				? t("board.taskOfTotal", { current: position, total: workflow.tasksTotal })
				: t("board.tasksCompletedOfTotal", { done: workflow.tasksCompleted, total: workflow.tasksTotal })}
			{workflow.currentTaskTitle ? ` — ${workflow.currentTaskTitle}` : ""}
		</p>
	);
}

/**
 * The branch a card is queued on (Checkpoint 8P-E.13A).
 *
 * "Blocked" alone was never enough: it could mean a branch about to be handed
 * over by a workflow that is nearly done, or one held by a workflow that
 * stopped and is waiting on a person. Both rendered identically, and only the
 * second one is anybody's problem. The daemon resolves which it is
 * (`autoResume`), so the card can say it.
 */
function BranchWaitLine({ wait }: { wait: BoardBranchWait }) {
	const { t } = useTranslation();
	const parts = [t("board.branchWaitBranch", { branch: wait.branch })];
	if (wait.heldByWorkflowRunId) {
		parts.push(t("board.branchWaitHeldBy", { workflowId: wait.heldByWorkflowRunId }));
	}
	return (
		<div
			className="flex flex-col gap-0.5 rounded border border-border bg-muted/40 px-2 py-1.5"
			data-testid="workflow-branch-wait"
		>
			{/* Ownership of a branch, not execution: no spinner, no accent, and
			    its own label, so a queued run never reads as a working one. */}
			<span className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
				<Hourglass aria-hidden="true" className="size-3 shrink-0" />
				{t("shell.workflowsWaitingForBranch")}
			</span>
			<p className="text-xs text-muted-foreground">{parts.join(" · ")}</p>
			{wait.repoPath ? (
				<p className="truncate text-[10px] text-muted-foreground/70" title={wait.repoPath}>
					{wait.repoPath}
				</p>
			) : null}
			{wait.autoResume ? null : <p className="text-xs text-warning">{t("board.branchWaitBlocked")}</p>}
		</div>
	);
}

/**
 * The "Te necesita" state. Reached only when the daemon classified the stop as
 * human_decision — a review that asked for changes AO is about to apply never
 * lands here.
 */
function HumanDecisionNotice({ workflow }: { workflow: BoardWorkflow }) {
	const { t } = useTranslation();
	return (
		<div className="flex flex-col gap-0.5 rounded border border-warning/50 bg-warning/10 px-2 py-1.5" role="note">
			<span className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-warning">
				{/* Icon + word: the state must survive a monochrome reading. */}
				<AlertTriangle aria-hidden="true" className="size-3.5 shrink-0" />
				{t("board.needsYou")}
			</span>
			{workflow.attentionReason ? (
				<span className="text-xs text-foreground">
					{t("board.reasonLabel", { reason: workflow.attentionReason })}
				</span>
			) : (
				<span className="text-xs text-muted-foreground">{t("board.noRecordedReason")}</span>
			)}
			{workflow.attentionAction ? (
				<span className="text-xs text-muted-foreground">{workflow.attentionAction}</span>
			) : null}
		</div>
	);
}

/**
 * What AO is handling by itself. Shown so the user can see the wait is real and
 * scheduled, never as a request for a decision.
 */
function InternalStatusLine({ workflow }: { workflow: BoardWorkflow }) {
	const { t } = useTranslation();
	if (workflow.attention !== "ao_internal") return null;
	const parts: string[] = [];
	if (workflow.waitReason) parts.push(t("board.waitReasonLabel", { reason: workflow.waitReason }));
	if (workflow.nextWakeAt) parts.push(t("board.nextRetry", { time: formatTimeCompact(workflow.nextWakeAt) }));
	if (parts.length === 0) parts.push(t("board.aoIsHandlingThis"));
	return <p className="text-xs text-muted-foreground">{parts.join(" · ")}</p>;
}

function ChildTaskList({ tasks }: { tasks: BoardWorkflowTask[] }) {
	const { t } = useTranslation();
	return (
		<ol className="flex flex-col gap-0.5" data-testid="workflow-child-tasks">
			{tasks.map((task) => (
				<li className="flex items-center gap-2 text-xs" key={task.ordinal}>
					<span className="w-4 shrink-0 text-right text-passive">{task.ordinal}.</span>
					<span className="min-w-0 flex-1 truncate text-muted-foreground">{task.title}</span>
					{/* The planner status sits BEFORE the phase/state rather than
					    replacing it: "Waiting for conflict" says why the task is not
					    moving, and "blocked" still says what it is. Absent for a task
					    with nothing planner-level to say, which leaves the row exactly
					    as it was. */}
					<WorkflowTaskPlannerBadge status={task.planner?.status} />
					<span className="shrink-0 text-passive">
						{task.phase
							? translateDynamic(t, `board.phase.${task.phase}`, task.phase)
							: translateDynamic(t, `board.taskState.${task.state}`, task.state)}
					</span>
				</li>
			))}
		</ol>
	);
}

