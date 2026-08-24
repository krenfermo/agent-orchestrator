import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { AlertTriangle, Check, Circle, CircleDot, CircleX, Loader2 } from "lucide-react";
import type { components } from "../../api/schema";
import { cn } from "../lib/utils";
import { Badge, type BadgeVariant } from "./ui/badge";

export type WorkflowPhase = components["schemas"]["WorkflowRunView"]["phase"];
export type WorkflowStepProgress = components["schemas"]["WorkflowStepProgressView"];
export type WorkflowStepState = WorkflowStepProgress["state"];

/**
 * Looks up a key the type system cannot know statically (a phase, a step kind,
 * a step state) and falls back to the raw value when no translation exists —
 * so a vocabulary the daemon grows before the renderer does shows the real
 * value rather than a dangling key. Mirrors workflow-capacity-wait-banner's
 * own reasonLabel.
 */
export function translateDynamic(t: TFunction, key: string, fallback: string): string {
	const untypedT = t as unknown as (key: string) => string;
	const label = untypedT(key);
	return label === key ? fallback : label;
}

/**
 * The phases in which AO is genuinely executing something right now.
 *
 * This is the single definition the whole renderer animates from: a spinner
 * appears here and nowhere else. A run that is queued, waiting on a branch or
 * on capacity, stopped for a person, or finished is *not* active — animating
 * those was the original complaint from the other direction, work that looks
 * busy while nothing is moving.
 */
const ACTIVE_PHASES: ReadonlySet<string> = new Set(["planning", "running", "reviewing", "fixing", "verifying"]);

export function isActivePhase(phase: string | undefined): boolean {
	return ACTIVE_PHASES.has(phase ?? "");
}

/**
 * Semantic tone per phase, from the status tokens the design system already
 * defines (`status-working` blue, `status-in-review` amber, `status-needs-you`
 * orange, `success` green, `destructive` red). No new colors are introduced
 * here, and failure is deliberately no longer painted with the same orange as
 * "needs attention": one is a dead run, the other is a live one waiting on a
 * person.
 */
export type PhaseTone = "working" | "review" | "attention" | "failed" | "done" | "idle";

export function phaseTone(phase: string): PhaseTone {
	switch (phase) {
		case "reviewing":
			return "review";
		case "planning":
		case "running":
		case "fixing":
		case "verifying":
			return "working";
		case "needs_attention":
			return "attention";
		case "failed":
			return "failed";
		case "completed":
			return "done";
		default:
			return "idle";
	}
}

const TONE_BADGE_CLASS: Record<PhaseTone, string> = {
	working: "bg-status-working/15 text-status-working",
	review: "bg-status-in-review/15 text-status-in-review",
	attention: "bg-status-needs-you/15 text-status-needs-you",
	failed: "bg-destructive/15 text-destructive",
	done: "bg-success/15 text-success",
	idle: "bg-muted text-muted-foreground",
};

const TONE_BORDER_CLASS: Record<PhaseTone, string> = {
	working: "border-status-working/60",
	review: "border-status-in-review/60",
	attention: "border-status-needs-you/60",
	failed: "border-destructive/50",
	done: "border-success/40",
	idle: "border-border",
};

export function phaseBadgeClass(phase: string): string {
	return TONE_BADGE_CLASS[phaseTone(phase)];
}

export function phaseBorderClass(phase: string): string {
	return TONE_BORDER_CLASS[phaseTone(phase)];
}

/**
 * The one spinner in the workflow UI.
 *
 * `motion-reduce:animate-none` is not decoration: a user who asked the OS for
 * reduced motion still needs to know the run is live, so the icon stays (as a
 * static ring) and the accessible label carries the meaning instead of the
 * animation. Pass `label` when the spinner is the only thing announcing the
 * state; leave it off when neighbouring text already says it, so a screen
 * reader is not told "in progress" three times per card.
 */
export function WorkflowSpinner({ className, label }: { className?: string; label?: string }) {
	const icon = (
		<Loader2
			aria-hidden="true"
			className={cn("size-3.5 shrink-0 animate-spin motion-reduce:animate-none", className)}
		/>
	);
	if (!label) return icon;
	return (
		<span aria-label={label} className="inline-flex" data-testid="workflow-spinner" role="status">
			{icon}
		</span>
	);
}

/**
 * The phase badge: the two-second answer to "what is this run doing".
 *
 * Never color alone — the phase name is always spelled out next to the tone,
 * and an active run additionally carries the spinner.
 */
export function WorkflowPhaseBadge({ phase, className }: { phase: WorkflowPhase | undefined; className?: string }) {
	const { t } = useTranslation();
	const active = isActivePhase(phase);
	// A record written before the daemon projected a phase has none. Showing
	// nothing is honest; showing the literal "undefined" is not.
	if (!phase) return null;
	return (
		<span
			className={cn(
				"inline-flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide",
				phaseBadgeClass(phase),
				className,
			)}
			data-testid="workflow-phase-badge"
		>
			{active ? <WorkflowSpinner className="size-3" label={t("board.inProgress")} /> : null}
			{translateDynamic(t, `board.phase.${phase}`, phase)}
		</span>
	);
}

/**
 * Plan / Work / Review / Fix / Verify, in order, with the state of each.
 * The never-executed `advance` step is already excluded by the daemon, so a
 * completed run reads as complete rather than five of six.
 */
export function WorkflowStepChecklist({ steps }: { steps: WorkflowStepProgress[] }) {
	const { t } = useTranslation();
	return (
		<ul className="flex flex-wrap items-center gap-x-3 gap-y-1" data-testid="workflow-step-checklist">
			{steps.map((step) => (
				<li className="flex items-center gap-1 text-xs" key={step.kind}>
					<WorkflowStepIcon state={step.state} />
					<span className={stepLabelClass(step.state)}>
						{translateDynamic(t, `board.step.${step.kind}`, step.kind)}
					</span>
					{/* The icon is decorative; the state has to reach a screen reader as text. */}
					<span className="sr-only">{translateDynamic(t, `board.stepState.${step.state}`, step.state)}</span>
				</li>
			))}
		</ul>
	);
}

function stepLabelClass(state: WorkflowStepState): string {
	switch (state) {
		case "running":
			return "font-medium text-status-working";
		case "failed":
			return "font-medium text-destructive";
		case "completed":
			return "text-muted-foreground";
		default:
			return "text-passive";
	}
}

/**
 * check = done, spinner = running, hollow ring = not started, warning = failed.
 * Shape carries the state as much as color does, so the checklist survives both
 * a monochrome reading and a reduced-motion one.
 */
export function WorkflowStepIcon({ state }: { state: WorkflowStepState }) {
	switch (state) {
		case "completed":
			return <Check aria-hidden="true" className="size-3.5 shrink-0 text-success" />;
		case "running":
			return <WorkflowSpinner className="text-status-working" />;
		case "waiting":
		case "ready":
			return <CircleDot aria-hidden="true" className="size-3.5 shrink-0 text-status-working" />;
		case "failed":
			return <AlertTriangle aria-hidden="true" className="size-3.5 shrink-0 text-destructive" />;
		case "cancelled":
			return <CircleX aria-hidden="true" className="size-3.5 shrink-0 text-passive" />;
		default:
			return <Circle aria-hidden="true" className="size-3.5 shrink-0 text-passive" />;
	}
}

export type ActivityFact = {
	label: string;
	/** Already-formatted display value. Never a fabricated zero — see below. */
	value: string | null | undefined;
};

/**
 * "Working right now": the compact block that says what AO is doing this
 * second, and on what.
 *
 * Renders only for a genuinely active phase, so it can never dress up a wait
 * as work. Facts are optional and drop out entirely when the underlying value
 * is absent — an unmeasured token total shows the Unknown the rest of the app
 * already uses, never a 0 that reads as "nothing was spent".
 */
export function WorkflowActivityPanel({
	phase,
	detail,
	facts = [],
}: {
	phase: WorkflowPhase | undefined;
	detail?: string;
	facts?: ActivityFact[];
}) {
	const { t } = useTranslation();
	if (!phase || !isActivePhase(phase)) return null;
	const shown = facts.filter((fact) => fact.value !== null && fact.value !== undefined && fact.value !== "");
	return (
		<section
			aria-live="polite"
			className="flex flex-col gap-1 rounded-md border border-status-working/40 bg-status-working/10 px-2 py-1.5"
			data-testid="workflow-activity-panel"
			role="status"
		>
			<span className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wide text-status-working">
				<WorkflowSpinner />
				{t("board.workingNow")}
			</span>
			<span className="text-xs text-foreground">
				{detail ?? translateDynamic(t, `board.activityDetail.${phase}`, translateDynamic(t, `board.phase.${phase}`, phase))}
			</span>
			{shown.length > 0 ? (
				<dl className="grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground">
					{shown.map((fact) => (
						<div className="contents" key={fact.label}>
							<dt>{fact.label}</dt>
							<dd className="truncate">{fact.value}</dd>
						</div>
					))}
				</dl>
			) : null}
		</section>
	);
}

/**
 * A planned task's planner-level status, as one pill.
 *
 * It is the vocabulary parallel execution created and the task state alone
 * cannot express: two tasks that are both "running" are a very different
 * picture depending on whether they are running *together*, and a task that is
 * "running" while its work sits in the integration queue is not running at all.
 * The seven values come from the daemon's own projection (workflow.TaskPlannerStatus)
 * — the renderer classifies nothing here, it only labels.
 *
 * Absent for a task with nothing planner-level to say, which is the ordinary
 * case: the row keeps rendering its phase or state exactly as before, and this
 * badge is additive rather than a replacement.
 */
export type WorkflowTaskPlannerStatus = NonNullable<
	NonNullable<components["schemas"]["WorkflowBoardTaskView"]["planner"]>["status"]
>;

/**
 * Tone per status. Color is rare and meaningful here (DESIGN.md → Color): only
 * the two outcomes a person may have to act on, and the one that is finished,
 * get any at all. Every wait is deliberately neutral — a queue is not a
 * problem, and painting it like one is how a board stops being readable.
 */
const PLANNER_STATUS_VARIANT: Record<WorkflowTaskPlannerStatus, BadgeVariant> = {
	running_in_parallel: "neutral",
	waiting_for_dependency: "outline",
	waiting_for_conflict: "warning",
	ready_to_integrate: "outline",
	integrating: "neutral",
	conflict: "error",
	integrated: "success",
};

export function WorkflowTaskPlannerBadge({
	status,
	className,
}: {
	status: WorkflowTaskPlannerStatus | undefined;
	className?: string;
}) {
	const { t } = useTranslation();
	if (!status) return null;
	return (
		<Badge
			className={cn("h-4 px-1.5 text-[10px] font-medium", className)}
			data-testid={`workflow-task-planner-${status}`}
			variant={PLANNER_STATUS_VARIANT[status] ?? "neutral"}
		>
			{/* A status the daemon grows before the renderer does still shows its
			    real value rather than a dangling translation key. */}
			{translateDynamic(t, `board.plannerStatus.${status}`, status)}
		</Badge>
	);
}
