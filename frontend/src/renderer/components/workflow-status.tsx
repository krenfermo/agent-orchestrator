import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { AlertTriangle, Check, CircleDot, CircleSlash, Minus, Wrench } from "lucide-react";
import {
	guidanceKey,
	isActiveStage,
	orderedActions,
	placementLabelKey,
	stageLabelKey,
	stageTone,
	summaryKey,
	timelineKey,
	type PresentationAction,
	type PresentationActionId,
	type PresentationProgressStage,
	type WorkflowPresentation,
} from "../lib/workflow-presentation";
import { translateDynamic, WorkflowSpinner } from "./workflow-activity";
import { cn } from "../lib/utils";

/**
 * workflow-status.tsx — P3-A's human status surface.
 *
 * Everything here renders the daemon's `presentation` projection and nothing
 * else. There is no lifecycle derivation, no Git logic and no second opinion
 * about what a run is doing: if the daemon says the stage is "correcting", this
 * says "corrigiendo", and if the daemon offers no action, this offers none.
 *
 * The organising rule of the whole file is §3's: the human sentence is the
 * title, and the technical vocabulary — reason codes, phases, generations —
 * lives underneath in a disclosure that is always present and never the first
 * thing read. Nothing is hidden; it is ranked.
 */

const TONE_BADGE_CLASS = {
	working: "bg-status-working/15 text-status-working",
	review: "bg-status-in-review/15 text-status-in-review",
	attention: "bg-status-needs-you/15 text-status-needs-you",
	failed: "bg-destructive/15 text-destructive",
	done: "bg-success/15 text-success",
	idle: "bg-muted text-muted-foreground",
} as const;

/**
 * The stage badge: the two-second answer to "what is AO doing".
 *
 * §29 — never color alone. The stage name is always spelled out, and an active
 * stage additionally carries the one spinner the renderer owns.
 */
export function WorkflowStageBadge({ stage, className }: { stage: string | undefined; className?: string }) {
	const { t } = useTranslation();
	if (!stage) return null;
	return (
		<span
			className={cn(
				"inline-flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide",
				TONE_BADGE_CLASS[stageTone(stage)],
				className,
			)}
			data-testid="workflow-stage-badge"
		>
			{isActiveStage(stage) ? <WorkflowSpinner className="size-3" label={t("board.inProgress")} /> : null}
			{translateDynamic(t as TFunction, stageLabelKey(stage), stage)}
		</span>
	);
}

/**
 * The sentence a person reads first, plus the single "¿qué hago?" line.
 *
 * The summary falls back through three levels, and each one still says
 * something true: the localized copy for this summary code, then the daemon's
 * own English sentence about the remedy, then the generic "AO stopped" copy
 * that points at the technical detail below. A code the renderer has never seen
 * degrades to less polished, never to blank.
 */
export function WorkflowStatusSummary({ presentation }: { presentation: WorkflowPresentation }) {
	const { t } = useTranslation();
	const localized = translateDynamic(t as TFunction, summaryKey(presentation.summaryCode), "");
	const summary = localized || presentation.technical.attentionDetail || t("wf.summary.unknown");
	return (
		<div className="flex flex-col gap-1" data-testid="workflow-status-summary">
			<p className="text-sm font-medium text-foreground">{summary}</p>
			<p className="text-sm text-muted-foreground" data-testid="workflow-status-guidance">
				{t(guidanceKey(presentation) as "wf.guidance.none")}
			</p>
		</div>
	);
}

const PROGRESS_ICON = {
	completed: Check,
	current: CircleDot,
	blocked: AlertTriangle,
	skipped: CircleSlash,
	future: Minus,
} as const;

const PROGRESS_CLASS = {
	completed: "text-success",
	current: "font-medium text-status-working",
	blocked: "font-medium text-status-needs-you",
	skipped: "text-passive line-through",
	future: "text-passive",
} as const;

/**
 * The visible progression (§2).
 *
 * Stages, never a percentage: AO has a bounded set of stages and a known
 * current one, and a number derived from those would be a fabrication. Each
 * entry carries an icon, a label AND a screen-reader state, so the four
 * distinctions a person needs (done / now / next / stuck) survive without
 * color.
 */
export function WorkflowProgress({ stages }: { stages: readonly PresentationProgressStage[] | undefined }) {
	const { t } = useTranslation();
	if (!stages || stages.length === 0) return null;
	return (
		<ol className="flex flex-wrap items-center gap-x-3 gap-y-1" data-testid="workflow-progress">
			{stages.map((entry) => {
				const Icon = PROGRESS_ICON[entry.state];
				return (
					<li
						className={cn("flex items-center gap-1 text-xs", PROGRESS_CLASS[entry.state])}
						data-progress-state={entry.state}
						data-progress-stage={entry.stage}
						key={entry.stage}
					>
						<Icon aria-hidden="true" className="size-3.5 shrink-0" />
						<span className={entry.optional && entry.state === "future" ? "opacity-70" : undefined}>
							{translateDynamic(t as TFunction, stageLabelKey(entry.stage), entry.stage)}
						</span>
						<span className="sr-only">{t(`wf.progress.${entry.state}` as "wf.progress.future")}</span>
					</li>
				);
			})}
		</ol>
	);
}

export type WorkflowActionHandlers = Partial<Record<PresentationActionId, () => void>>;

/**
 * The offered actions (§4).
 *
 * A disabled action is rendered with its reason rather than removed, because
 * "why can't I repair this" is a question the UI should answer and a missing
 * button does not. An action the renderer has no handler for is disabled with
 * no reason rather than silently dropped — the daemon authorised it, and
 * hiding it would misreport what AO is willing to do.
 */
export function WorkflowActions({
	presentation,
	handlers,
	busy,
}: {
	presentation: WorkflowPresentation;
	handlers: WorkflowActionHandlers;
	busy?: boolean;
}) {
	const { t } = useTranslation();
	const actions = orderedActions(presentation.actions);
	if (actions.length === 0) return null;
	return (
		<div className="flex flex-wrap items-center gap-2" data-testid="workflow-actions">
			{actions.map((action) => (
				<WorkflowActionButton action={action} busy={busy} handler={handlers[action.id]} key={action.id} t={t as TFunction} />
			))}
		</div>
	);
}

function WorkflowActionButton({
	action,
	handler,
	busy,
	t,
}: {
	action: PresentationAction;
	handler: (() => void) | undefined;
	busy: boolean | undefined;
	t: TFunction;
}) {
	const label = translateDynamic(t, `wf.action.${action.id}`, action.id);
	const reason = action.disabledReason
		? translateDynamic(t, `wf.disabled.${action.disabledReason}`, action.disabledReason)
		: "";
	const disabled = Boolean(busy) || !action.enabled || !handler;
	return (
		<span className="flex flex-col gap-0.5">
			<button
				className={cn(
					"rounded border px-3 py-1.5 text-sm disabled:opacity-50",
					action.primary
						? "border-primary bg-primary text-primary-foreground"
						: "border-border",
				)}
				data-action-id={action.id}
				data-testid={`workflow-action-${action.id}`}
				disabled={disabled}
				onClick={handler}
				title={reason || undefined}
				type="button"
			>
				{label}
			</button>
			{reason ? <span className="text-xs text-muted-foreground">{reason}</span> : null}
		</span>
	);
}

/**
 * Where AO is working (§13).
 *
 * Always visible during execution, so nobody has to open a log to find out
 * which repository and branch their work is happening in. `chosenBy` is shown
 * because §10 requires an automatic decision to be visible as one — and because
 * a person who explicitly picked "current branch" should be able to see that
 * AO agreed.
 */
export function WorkflowExecutionLocation({
	presentation,
	projectId,
}: {
	presentation: WorkflowPresentation;
	projectId: string;
}) {
	const { t } = useTranslation();
	const placement = presentation.placement;
	return (
		<section className="flex flex-col gap-2 rounded-lg border border-border p-3" data-testid="workflow-location">
			<h2 className="text-sm font-semibold">{t("wf.section.location")}</h2>
			<dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs text-muted-foreground">
				<dt>{t("wf.location.project")}</dt>
				<dd className="truncate">{projectId}</dd>
				{placement ? (
					<>
						<dt>{t("wf.location.repository")}</dt>
						<dd className="truncate font-mono" title={placement.repoPath}>
							{placement.repoPath || t("wf.location.unknownValue")}
						</dd>
						<dt>{t("wf.location.placement")}</dt>
						<dd>
							{translateDynamic(t as TFunction, placementLabelKey(placement.type), placement.type)}
							{" · "}
							{t(`wf.location.chosenBy.${placement.chosenBy}` as "wf.location.chosenBy.automatic")}
						</dd>
						<dt>{t("wf.location.branch")}</dt>
						<dd className="truncate font-mono">{placement.executionBranch || t("wf.location.unknownValue")}</dd>
						{placement.worktreePath ? (
							<>
								<dt>{t("wf.location.worktree")}</dt>
								<dd className="truncate font-mono" title={placement.worktreePath}>
									{placement.worktreePath}
								</dd>
							</>
						) : null}
						{/*
						 * §9: the merge target is shown only when there is something to
						 * integrate. A direct-branch run's target IS its execution
						 * branch, and printing "will be integrated into feat/x" for work
						 * that is already on feat/x is the confusion this rule removes.
						 */}
						{placement.integrationRequired && placement.mergeTarget ? (
							<>
								<dt>{t("wf.location.mergeTarget")}</dt>
								<dd className="truncate font-mono">{placement.mergeTarget}</dd>
							</>
						) : null}
						{placement.integratedSha ? (
							<>
								<dt>{t("wf.location.integratedSha")}</dt>
								<dd className="truncate font-mono">{placement.integratedSha.slice(0, 12)}</dd>
							</>
						) : null}
					</>
				) : (
					<>
						<dt>{t("wf.location.placement")}</dt>
						<dd>{t("wf.location.placementUnknown")}</dd>
					</>
				)}
			</dl>
		</section>
	);
}

/**
 * The bounded activity timeline (§15).
 *
 * It reports what happened, in order, with the technical qualifier beside the
 * line rather than as the line. Heartbeats and reconcile passes never reach it:
 * the daemon does not send them, precisely so this cannot become the log the
 * page exists to replace.
 */
export function WorkflowTimeline({ events }: { events: readonly { at: string; kind: string; detail?: string }[] | undefined }) {
	const { t } = useTranslation();
	if (!events || events.length === 0) return null;
	return (
		<section className="flex flex-col gap-2 rounded-lg border border-border p-3" data-testid="workflow-timeline">
			<h2 className="text-sm font-semibold">{t("wf.section.timeline")}</h2>
			<ol className="flex flex-col gap-1">
				{events.map((event, index) => (
					<li className="flex gap-2 text-xs text-muted-foreground" key={`${event.kind}-${event.at}-${index}`}>
						<time className="shrink-0 font-mono" dateTime={event.at}>
							{new Date(event.at).toLocaleTimeString()}
						</time>
						<span className="text-foreground">
							{translateDynamic(t as TFunction, timelineKey(event.kind), event.kind)}
						</span>
						{event.detail ? <span className="truncate">{event.detail}</span> : null}
					</li>
				))}
			</ol>
		</section>
	);
}

/**
 * The technical account (§3).
 *
 * Always present, never first. Every code the old UI put in the title is here
 * verbatim — reason, phase, run state, wait reason, error class, both
 * generations, the repair run — so an operator diagnosing a stop loses nothing
 * by the human sentence being on top.
 */
export function WorkflowTechnicalDetails({ presentation }: { presentation: WorkflowPresentation }) {
	const { t } = useTranslation();
	const tech = presentation.technical;
	const rows: [string, string][] = [];
	const push = (key: string, value: string | number | undefined | null) => {
		if (value === undefined || value === null || value === "" || value === 0) return;
		rows.push([key, String(value)]);
	};
	push("wf.technical.summaryCode", presentation.summaryCode);
	push("wf.technical.phase", tech.phase);
	push("wf.technical.runState", tech.runState);
	push("wf.technical.attention", tech.attention);
	push("wf.technical.attentionReason", tech.attentionReason);
	push("wf.technical.waitReason", tech.waitReason);
	push("wf.technical.errorClass", tech.errorClass);
	push("wf.technical.placementGeneration", tech.placementGeneration);
	push("wf.technical.lifecycleGeneration", tech.lifecycleGeneration);
	push("wf.technical.repairRun", tech.repairRunId);
	push("wf.technical.nextWake", tech.nextWakeAt ? new Date(tech.nextWakeAt).toLocaleString() : "");
	// P3-D §24: WHICH execution this status is about. Every value is served
	// already composed — the attempt, its provider, the session it owns, the
	// authority AO grants it — so nothing here is inferred in the renderer, and
	// a fact the daemon does not hold simply does not render.
	push(
		"wf.technical.attempt",
		tech.attemptId ? `${tech.attemptId}${tech.attemptNumber ? ` (#${tech.attemptNumber})` : ""}` : "",
	);
	push("wf.technical.provider", tech.provider);
	push("wf.technical.session", tech.sessionId);
	push("wf.technical.authority", tech.authority);
	push("wf.technical.dispatchedAt", tech.dispatchedAt ? new Date(tech.dispatchedAt).toLocaleString() : "");
	push(
		"wf.technical.lastEvent",
		tech.lastEventPhase
			? `${tech.lastEventPhase}${tech.lastEventAt ? ` · ${new Date(tech.lastEventAt).toLocaleString()}` : ""}`
			: "",
	);
	if (rows.length === 0 && !tech.attentionDetail) return null;
	return (
		<details className="rounded-lg border border-border p-3" data-testid="workflow-technical">
			<summary className="cursor-pointer text-sm font-semibold">{t("wf.section.technical")}</summary>
			{tech.attentionDetail ? (
				<p className="mt-2 text-xs text-muted-foreground">{tech.attentionDetail}</p>
			) : null}
			<dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs text-muted-foreground">
				{rows.map(([key, value]) => (
					<div className="contents" key={key}>
						<dt>{t(key as "wf.technical.phase")}</dt>
						<dd className="truncate font-mono">{value}</dd>
					</div>
				))}
			</dl>
		</details>
	);
}

/**
 * The whole status header: stage, sentence, guidance, progression.
 *
 * One component so the four cannot drift apart on different screens, and so a
 * surface that wants "the human status of this run" asks for exactly that.
 */
export function WorkflowStatusPanel({ presentation }: { presentation: WorkflowPresentation }) {
	const tone = stageTone(presentation.stage);
	return (
		<section
			className={cn(
				"flex flex-col gap-3 rounded-lg border p-3",
				tone === "attention" ? "border-status-needs-you/50" : "border-border",
			)}
			data-testid="workflow-status-panel"
		>
			<div className="flex items-start gap-2">
				<WorkflowStageBadge className="mt-0.5" stage={presentation.stage} />
				<div className="min-w-0 flex-1">
					<WorkflowStatusSummary presentation={presentation} />
				</div>
				{presentation.automaticActionActive && !presentation.requiresHuman ? (
					<Wrench aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-status-working" />
				) : null}
			</div>
			<WorkflowProgress stages={presentation.progress} />
		</section>
	);
}

/**
 * The automatic repair, shown inside the run it is repairing (§5/§6).
 *
 * A repair is AO's own work on this run, so it belongs in this run's page
 * rather than as a second, unexplained card somewhere else. What a person needs
 * is the two facts they cannot get from the stage badge — which attempt of how
 * many, and where the detail is — and the audit trail stays reachable through
 * the link rather than being hidden.
 *
 * It renders only when a repair generation actually exists. A run that has
 * never needed one shows nothing, not a zeroed row.
 */
export function WorkflowRepairInline({
	repair,
	renderLink,
}: {
	repair:
		| {
				active: boolean;
				attempt: number;
				budget: number;
				runId?: string;
				exhausted: boolean;
				quiescenceReason?: string;
		  }
		| undefined;
	renderLink: (runId: string, label: string) => React.ReactNode;
}) {
	const { t } = useTranslation();
	if (!repair || repair.attempt === 0) return null;
	return (
		<section className="flex flex-col gap-1 rounded-lg border border-border p-3" data-testid="workflow-repair">
			<h2 className="flex items-center gap-2 text-sm font-semibold">
				<Wrench aria-hidden="true" className="size-4 shrink-0" />
				{t("wf.repair.title")}
			</h2>
			<p className="text-xs text-muted-foreground">
				{t("wf.repair.attempt", { attempt: repair.attempt, budget: repair.budget })}
				{repair.exhausted ? ` · ${t("wf.repair.exhausted")}` : null}
			</p>
			{repair.runId ? renderLink(repair.runId, t("wf.repair.viewDetails")) : null}
			{repair.quiescenceReason ? (
				<details className="text-xs text-muted-foreground">
					<summary className="cursor-pointer">{t("wf.section.technical")}</summary>
					<p className="mt-1 font-mono">{repair.quiescenceReason}</p>
				</details>
			) : null}
		</section>
	);
}

/**
 * The completion summary (§26).
 *
 * What a person wants after a run finishes is whether it is really done and
 * whether anything is left for them — not a step list they have to reconstruct
 * that from. Every value here is already on the run's detail response; nothing
 * new is computed, and a fact AO does not have is omitted rather than guessed.
 */
export function WorkflowCompletionSummary({
	presentation,
	verdict,
	verificationPassed,
	commitSha,
}: {
	presentation: WorkflowPresentation;
	verdict: string | undefined;
	verificationPassed: boolean | undefined;
	commitSha: string | undefined;
}) {
	const { t } = useTranslation();
	if (presentation.stage !== "completed") return null;
	const placement = presentation.placement;
	return (
		<section className="flex flex-col gap-2 rounded-lg border border-success/40 bg-success/5 p-3" data-testid="workflow-completion">
			<h2 className="text-sm font-semibold">{t("wf.completed.title")}</h2>
			<dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs text-muted-foreground">
				{verdict ? (
					<>
						<dt>{t("wf.completed.review")}</dt>
						<dd>{verdict}</dd>
					</>
				) : null}
				{verificationPassed !== undefined ? (
					<>
						<dt>{t("wf.completed.verification")}</dt>
						<dd>{verificationPassed ? t("wf.completed.passed") : t("wf.completed.notPassed")}</dd>
					</>
				) : null}
				{commitSha ? (
					<>
						<dt>{t("wf.completed.commit")}</dt>
						<dd className="font-mono">{commitSha.slice(0, 12)}</dd>
					</>
				) : null}
				{placement ? (
					<>
						<dt>{t("wf.location.placement")}</dt>
						<dd>{translateDynamic(t as TFunction, placementLabelKey(placement.type), placement.type)}</dd>
						<dt>{t("wf.completed.integration")}</dt>
						{/*
						 * §9 again, at the one place it matters most: a finished
						 * direct-branch run is told there is nothing to integrate,
						 * rather than being asked whether to merge work that is
						 * already on the branch the user chose.
						 */}
						<dd>
							{placement.integratedSha
								? t("wf.completed.integrationDone")
								: placement.integrationRequired
									? t("wf.completed.integrationPending")
									: t("wf.completed.integrationNotRequired")}
						</dd>
					</>
				) : null}
			</dl>
		</section>
	);
}
