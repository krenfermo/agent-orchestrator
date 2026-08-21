import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { useNavigate } from "@tanstack/react-router";
import { AlertTriangle, Check, CircleDot, Circle, Loader2 } from "lucide-react";
import {
	needsHumanDecision,
	useProjectBoard,
	type BoardBranchWait,
	type BoardPhase,
	type BoardStepProgress,
	type BoardWorkflow,
	type BoardWorkflowTask,
} from "../hooks/useProjectBoard";
import { formatTimeCompact } from "../lib/format-time";
import { cn } from "../lib/utils";

/**
 * Looks up a key the type system cannot know statically (a phase, a step kind,
 * a task state) and falls back to the raw value when no translation exists —
 * so a vocabulary the daemon grows before the renderer does shows the real
 * value rather than a dangling key. Mirrors workflow-capacity-wait-banner's
 * own reasonLabel.
 */
function translate(t: TFunction, key: string, fallback: string): string {
	const untypedT = t as unknown as (key: string) => string;
	const label = untypedT(key);
	return label === key ? fallback : label;
}

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

	if (workflows.length === 0) return null;

	return (
		<section
			aria-label={t("board.workflowsAria")}
			className="flex shrink-0 flex-col gap-2 border-b border-border px-3 py-3"
			data-testid="project-workflow-lane"
		>
			<h2 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
				{t("board.workflows")}
			</h2>
			<div className="flex flex-col gap-2">
				{workflows.map((workflow) => (
					<WorkflowBoardCard
						key={workflow.workflowId}
						onOpen={() =>
							void navigate({ to: "/workflows/$workflowId", params: { workflowId: workflow.workflowId } })
						}
						workflow={workflow}
					/>
				))}
			</div>
		</section>
	);
}

/**
 * One Board card. Exported and free of data fetching/routing so it can be
 * rendered directly against a projection value in tests — the presentation of
 * "what is AO doing" is exactly the part worth pinning down.
 */
export function WorkflowBoardCard({ workflow, onOpen }: { workflow: BoardWorkflow; onOpen?: () => void }) {
	const { t } = useTranslation();
	const human = needsHumanDecision(workflow);
	const title = objectiveTitle(workflow.objective);

	return (
		<button
			className={cn(
				"flex w-full flex-col gap-2 rounded-lg border bg-surface px-3 py-2.5 text-left transition-colors hover:bg-muted/40",
				human ? "border-warning" : "border-border",
			)}
			data-testid={`workflow-card-${workflow.workflowId}`}
			onClick={onOpen}
			type="button"
		>
			<div className="flex min-w-0 items-center gap-2">
				<span className="min-w-0 flex-1 truncate text-sm font-medium">{title}</span>
				<PhaseBadge phase={workflow.phase} />
			</div>

			{workflow.tasksTotal > 0 ? <TaskHeadline workflow={workflow} /> : null}

			{workflow.steps && workflow.steps.length > 0 ? <StepChecklist steps={workflow.steps} /> : null}

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

			<p className="font-mono text-[10px] text-muted-foreground/70" data-testid="workflow-run-id">
				{t("board.runId", { runId: workflow.workflowId })}
			</p>

			{workflow.tasks && workflow.tasks.length > 0 ? <ChildTaskList tasks={workflow.tasks} /> : null}
		</button>
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
 * Plan / Work / Review / Fix / Verify, in order, with the state of each.
 * The never-executed `advance` step is already excluded by the daemon, so a
 * completed run reads as complete rather than five of six.
 */
function StepChecklist({ steps }: { steps: BoardStepProgress[] }) {
	const { t } = useTranslation();
	return (
		<ul className="flex flex-wrap items-center gap-x-3 gap-y-1" data-testid="workflow-step-checklist">
			{steps.map((step) => (
				<li className="flex items-center gap-1 text-xs" key={step.kind}>
					<StepIcon state={step.state} />
					<span className={step.state === "running" ? "font-medium" : "text-muted-foreground"}>
						{translate(t, `board.step.${step.kind}`, step.kind)}
					</span>
					{/* The icon is decorative; the state has to reach a screen reader as text. */}
					<span className="sr-only">{translate(t, `board.stepState.${step.state}`, step.state)}</span>
				</li>
			))}
		</ul>
	);
}

function StepIcon({ state }: { state: BoardStepProgress["state"] }) {
	switch (state) {
		case "completed":
			return <Check aria-hidden="true" className="size-3.5 text-success" />;
		case "running":
			return <Loader2 aria-hidden="true" className="size-3.5 animate-spin text-primary" />;
		case "waiting":
		case "ready":
			return <CircleDot aria-hidden="true" className="size-3.5 text-primary" />;
		case "failed":
			return <AlertTriangle aria-hidden="true" className="size-3.5 text-warning" />;
		default:
			return <Circle aria-hidden="true" className="size-3.5 text-passive" />;
	}
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
		<div className="flex flex-col gap-0.5" data-testid="workflow-branch-wait">
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
			<span className="text-xs font-semibold uppercase tracking-wide text-warning">
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
							? translate(t, `board.phase.${task.phase}`, task.phase)
							: translate(t, `board.taskState.${task.state}`, task.state)}
					</span>
				</li>
			))}
		</ol>
	);
}

function PhaseBadge({ phase }: { phase: BoardPhase }) {
	const { t } = useTranslation();
	return (
		<span
			className={cn(
				"shrink-0 rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide",
				phaseToneClass(phase),
			)}
			data-testid="workflow-phase-badge"
		>
			{translate(t, `board.phase.${phase}`, phase)}
		</span>
	);
}

function phaseToneClass(phase: BoardPhase): string {
	switch (phase) {
		case "needs_attention":
		case "failed":
			return "bg-warning/15 text-warning";
		case "completed":
			return "bg-success/15 text-success";
		case "cancelled":
			return "bg-muted text-muted-foreground";
		case "waiting":
		case "waiting_for_capacity":
		// "retrying" reads as a wait, not a warning: AO hit something it is
		// allowed to retry, has the retry scheduled, and needs nothing from the
		// user. Toning it like needs_attention would recreate the false alarm
		// Checkpoint 8P-E.13 introduced the phase to remove.
		case "retrying":
		case "blocked":
		case "queued":
			return "bg-muted text-muted-foreground";
		default:
			return "bg-primary/15 text-primary";
	}
}
