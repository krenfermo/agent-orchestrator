import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "@tanstack/react-router";
import { AlertTriangle, Archive, Hourglass } from "lucide-react";
import {
	needsHumanDecision,
	useProjectBoard,
	type BoardBranchWait,
	type BoardWorkflow,
	type BoardWorkflowTask,
} from "../hooks/useProjectBoard";
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
	const { workflows } = useProjectBoard(projectId);
	const [showArchived, setShowArchived] = useState(false);
	const archive = useCancelAndArchiveWorkflow(projectId);
	// Archived workflows are read whether or not the history is open, because
	// their existence is what decides whether this lane appears at all: once the
	// last active workflow is archived there would otherwise be no lane left to
	// reveal the archive from. The query never polls — an archived workflow is
	// finished by definition — so this is one request per project, not a second
	// board on a timer. They are never mixed into the active list: the point of
	// archiving is that the active area stops competing for attention with
	// incidents that are over.
	const { workflows: archived, isLoading: archivedLoading } = useProjectBoardHistory(projectId, true);

	// Nothing active and nothing archived: this lane has never had anything to
	// say about the project, and stays out of the way entirely.
	if (workflows.length === 0 && archived.length === 0) return null;

	const openWorkflow = (workflowId: string) =>
		void navigate({ to: "/workflows/$workflowId", params: { workflowId } });

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
				<button
					aria-pressed={showArchived}
					className="text-xs text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
					data-testid="workflow-archived-toggle"
					onClick={() => setShowArchived((open) => !open)}
					type="button"
				>
					{showArchived ? t("board.hideArchived") : t("board.showArchived")}
				</button>
			</div>
			<div className="flex flex-col gap-2">
				{workflows.map((workflow) => (
					<WorkflowBoardCard
						archiveError={archive.error?.message}
						archivePending={archive.isPending}
						key={workflow.workflowId}
						onArchive={archive.mutateAsync}
						onOpen={() => openWorkflow(workflow.workflowId)}
						workflow={workflow}
					/>
				))}
			</div>
			{showArchived ? (
				<div className="flex flex-col gap-2" data-testid="workflow-archived-list">
					<h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
						{t("board.archivedHeading")}
					</h3>
					{archivedLoading ? (
						<p className="text-xs text-muted-foreground">{t("board.archivedLoading")}</p>
					) : archived.length === 0 ? (
						<p className="text-xs text-muted-foreground">{t("board.archivedEmpty")}</p>
					) : (
						archived.map((workflow) => (
							<WorkflowBoardCard
								archived
								key={workflow.workflowId}
								onOpen={() => openWorkflow(workflow.workflowId)}
								workflow={workflow}
							/>
						))
					)}
				</div>
			) : null}
		</section>
	);
}

/**
 * One Board card. Exported and free of data fetching/routing so it can be
 * rendered directly against a projection value in tests — the presentation of
 * "what is AO doing" is exactly the part worth pinning down.
 */
export function WorkflowBoardCard({
	workflow,
	onOpen,
	onArchive,
	archivePending,
	archiveError,
	archived,
}: {
	workflow: BoardWorkflow;
	onOpen?: () => void;
	/** Cancel-and-archive. Absent on an archived card, which has nothing left to stop. */
	onArchive?: (workflowId: string) => Promise<unknown>;
	archivePending?: boolean;
	archiveError?: string;
	archived?: boolean;
}) {
	const { t } = useTranslation();
	const human = needsHumanDecision(workflow);
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
			// signal, and the phase badge, spinner and activity block below say
			// which kind of movement it is.
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
				<WorkflowPhaseBadge phase={workflow.phase} />
			</div>

			{workflow.tasksTotal > 0 ? <TaskHeadline workflow={workflow} /> : null}

			{workflow.steps && workflow.steps.length > 0 ? <WorkflowStepChecklist steps={workflow.steps} /> : null}

			{/* The card footer already carries harness/model/last activity, so the
			    panel here adds only what is specific to this moment. */}
			<WorkflowActivityPanel
				facts={[{ label: t("board.factTask"), value: workflow.currentTaskTitle }]}
				phase={workflow.phase}
			/>

			{workflow.branchWait ? <BranchWaitLine wait={workflow.branchWait} /> : null}

			{human ? <HumanDecisionNotice workflow={workflow} /> : <InternalStatusLine workflow={workflow} />}

			<p className="text-xs text-muted-foreground">
				{[
					workflow.harness || undefined,
					workflow.model || undefined,
					t("board.lastActivity", { time: formatTimeCompact(workflow.lastActivityAt) }),
				]
					.filter(Boolean)
					.join(" · ")}
			</p>

			<p
				className="font-mono text-[10px] text-muted-foreground/70"
				data-testid="workflow-run-id"
				title="Autonomous workflow run ID"
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

