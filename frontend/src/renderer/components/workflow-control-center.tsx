import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { AlertTriangle, CircleCheck, CircleX, Lightbulb } from "lucide-react";
import type { components } from "../../api/schema";
import { cn } from "../lib/utils";
import { formatDurationCompact } from "../lib/format-time";
import { actionLabelKey, summaryKey } from "../lib/workflow-presentation";
import {
	activeAgent,
	attentionReport,
	compactTimeline,
	fixCycleSummary,
	headlineStatus,
	headlineTone,
	incidentSignals,
	reviewSummary,
	runDurationSeconds,
	secondsSince,
	sessionTree,
	usageDigest,
	verifySummary,
	type AttentionReport,
	type Certainty,
	type EvidenceRow,
	type FixCycleSummary,
	type HeadlineStatus,
	type HeadlineTone,
	type IncidentSignal,
	type ReviewSummary,
	type SessionNode,
	type TimelineChip,
	type UsageDigest,
	type VerifySummary,
	type WorkflowRunDetail,
} from "../lib/workflow-control-center";
import { translateDynamic, WorkflowSpinner } from "./workflow-activity";
import { WorkflowStatusSummary } from "./workflow-status";

/**
 * workflow-control-center.tsx — P8's run header and run anatomy.
 *
 * Two pieces, placed around the action buttons on the run page:
 *
 *  - WorkflowControlCenter (above the actions): what state this workflow is
 *    in, the mode it runs under, what is happening and who is doing it, and —
 *    when it is stopped — what happened, what AO knows, what it could not
 *    determine and what it recommends.
 *  - WorkflowRunAnatomy (below the actions): the workflow and its agent
 *    sessions, what review and Verify concluded, and what it has used.
 *
 * Both render `lib/workflow-control-center.ts` and nothing else. Neither holds
 * a control that changes a run: the actions AO authorises stay with
 * WorkflowActions, so there is still exactly one button per daemon operation.
 * The only interactive thing here is a link to an agent session — a navigation.
 */

type SessionLinkRenderer = (sessionId: string) => ReactNode;
type RunView = components["schemas"]["WorkflowRunView"];

const TONE_CLASS: Record<HeadlineTone, string> = {
	working: "bg-status-working/15 text-status-working",
	review: "bg-status-in-review/15 text-status-in-review",
	attention: "bg-status-needs-you/15 text-status-needs-you",
	failed: "bg-destructive/15 text-destructive",
	done: "bg-success/15 text-success",
	idle: "bg-muted text-muted-foreground",
};

const SPINNING: ReadonlySet<HeadlineStatus> = new Set([
	"planning",
	"running",
	"reviewing",
	"correcting",
	"verifying",
	"integrating",
]);

const STRATEGY_LABEL_KEY = {
	task: "shell.workflowsStrategyTaskLabel",
	autonomous: "shell.workflowsStrategyAutonomousLabel",
	master: "shell.workflowsStrategyMasterLabel",
} as const;

const CERTAINTY_CLASS: Record<Certainty, string> = {
	measured: "border-success/40 text-success",
	modelled: "border-status-in-review/40 text-status-in-review",
	unknown: "border-border text-passive",
};

function agoText(t: TFunction, seconds: number | null): string {
	if (seconds === null) return t("shell.liveness.unknown");
	if (seconds < 60) return t("shell.liveness.secondsAgo", { value: Math.max(0, Math.round(seconds)) });
	if (seconds < 3600) return t("shell.liveness.minutesAgo", { value: Math.round(seconds / 60) });
	return t("shell.liveness.hoursAgo", { value: (seconds / 3600).toFixed(1) });
}

function formatTokens(value: number | undefined, t: TFunction): string {
	return value === undefined ? t("cc.usage.unknown") : value.toLocaleString();
}

function Chip({ children, className, testId }: { children: ReactNode; className?: string; testId?: string }) {
	return (
		<span
			className={cn("inline-flex items-center rounded border border-border px-1.5 py-0.5 text-[11px] text-muted-foreground", className)}
			data-testid={testId}
		>
			{children}
		</span>
	);
}

function CertaintyChip({ certainty }: { certainty: Certainty }) {
	const { t } = useTranslation();
	return (
		<span
			className={cn("ml-1.5 inline-flex rounded border px-1 text-[10px] uppercase tracking-wide", CERTAINTY_CLASS[certainty])}
			data-certainty={certainty}
		>
			{t(`cc.certainty.${certainty}` as "cc.certainty.unknown")}
		</span>
	);
}

function Card({ title, children, className, testId }: { title: string; children: ReactNode; className?: string; testId?: string }) {
	return (
		<section className={cn("flex min-w-0 flex-col gap-2 rounded-lg border border-border p-3", className)} data-testid={testId}>
			<h2 className="text-sm font-semibold">{title}</h2>
			{children}
		</section>
	);
}

export function WorkflowHeadlineStatus({ status }: { status: HeadlineStatus }) {
	const { t } = useTranslation();
	return (
		<span
			className={cn(
				"inline-flex shrink-0 items-center gap-1.5 rounded px-2 py-1 text-xs font-semibold uppercase tracking-wide",
				TONE_CLASS[headlineTone(status)],
			)}
			data-status={status}
			data-testid="workflow-headline-status"
		>
			{SPINNING.has(status) ? <WorkflowSpinner className="size-3" label={t("board.inProgress")} /> : null}
			{t(`cc.status.${status}` as "cc.status.running")}
		</span>
	);
}

function ModeChips({ run }: { run: RunView }) {
	const { t } = useTranslation();
	const strategy = run.executionStrategy?.effectiveStrategy;
	return (
		<>
			<Chip className="border-primary/40 text-foreground" testId="workflow-mode-chip">
				{strategy ? t("cc.mode", { mode: t(STRATEGY_LABEL_KEY[strategy]) }) : t("cc.modeUnknown")}
			</Chip>
			<Chip testId="workflow-approval-chip">
				{t(run.executionMode === "autonomous" ? "cc.approval.automatic" : "cc.approval.manual")}
			</Chip>
			{run.reviewDepth ? (
				<Chip>
					{t("cc.reviewDepthChip", {
						depth: translateDynamic(t as TFunction, `wf.reviewDepth.${run.reviewDepth.requestedDepth}`, run.reviewDepth.requestedDepth),
					})}
				</Chip>
			) : null}
			{/* Shown only when a person explicitly turned it on for this run. It is a
			    fact about the run, never a control: P8 exposes no compaction action. */}
			{run.contextEconomy?.sessionCompactionEnabled ? <Chip>{t("cc.compactionOn")}</Chip> : null}
		</>
	);
}

function fixCycleText(cycles: FixCycleSummary, t: TFunction): string {
	if (cycles.spent === null) return t("cc.fixCycle.unknown");
	return cycles.max === null
		? t("cc.fixCycle.noMax", { spent: cycles.spent })
		: t("cc.fixCycle.ofMax", { spent: cycles.spent, max: cycles.max });
}

function agentText(node: SessionNode, t: TFunction): string {
	const who = [node.harness, node.model].filter(Boolean).join(" / ");
	const role = t(`cc.role.${node.role}` as "cc.role.worker");
	return who ? t("cc.fact.agentValue", { role, agent: who }) : role;
}

function Timeline({ chips }: { chips: TimelineChip[] }) {
	const { t } = useTranslation();
	if (chips.length === 0) return null;
	return (
		<div className="flex flex-col gap-1" data-testid="workflow-compact-timeline">
			<span className="text-xs text-muted-foreground">{t("cc.timeline.title")}</span>
			<ol className="flex flex-wrap items-center gap-1 text-xs">
				{chips.map((chip, index) => (
					<li className="flex items-center gap-1" data-phase={chip.phase} key={`${chip.phase}-${chip.at}-${index}`}>
						{index > 0 ? (
							<span aria-hidden="true" className="text-passive">
								{t("cc.timeline.arrow")}
							</span>
						) : null}
						<span
							className={cn(
								"rounded border px-1.5 py-0.5",
								chip.phase === "failed" || chip.phase === "stopped"
									? "border-status-needs-you/40 text-status-needs-you"
									: chip.phase === "completed"
										? "border-success/40 text-success"
										: "border-border text-foreground",
							)}
						>
							{t(`cc.timeline.phase.${chip.phase}` as "cc.timeline.phase.created")}
							{chip.count > 1 ? ` ${t("cc.timeline.count", { n: chip.count })}` : ""}
							{chip.verdict ? ` · ${translateDynamic(t as TFunction, `cc.review.outcome.${chip.verdict}`, chip.verdict)}` : ""}
						</span>
						{chip.failures > 0 ? (
							<span className="text-[11px] text-status-needs-you">{t("cc.timeline.failures", { n: chip.failures })}</span>
						) : null}
					</li>
				))}
			</ol>
		</div>
	);
}

function EvidenceValue({ row, now }: { row: EvidenceRow; now: number }) {
	const { t } = useTranslation();
	if (row.at) return <>{agoText(t as TFunction, secondsSince(row.at, now))}</>;
	if (row.textKey) return <>{translateDynamic(t as TFunction, row.textKey, row.text ?? row.textKey)}</>;
	return <>{row.text}</>;
}

function AttentionBlock({ report, now }: { report: AttentionReport; now: number }) {
	const { t } = useTranslation();
	const tt = t as TFunction;
	const localized = report.summaryCode ? translateDynamic(tt, summaryKey(report.summaryCode), "") : "";
	const what = localized || report.attentionDetail || t("wf.summary.unknown");
	return (
		<section
			className="flex flex-col gap-3 rounded-lg border border-status-needs-you/50 bg-status-needs-you/5 p-3"
			data-testid="workflow-attention-report"
		>
			<h2 className="flex items-center gap-2 text-sm font-semibold">
				<AlertTriangle aria-hidden="true" className="size-4 shrink-0 text-status-needs-you" />
				{t("cc.attention.title")}
			</h2>
			<div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
				<div className="flex flex-col gap-1" data-testid="workflow-attention-what">
					<h3 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">{t("cc.attention.what")}</h3>
					<p className="text-sm">{what}</p>
					{report.causeUndetermined ? (
						<p className="text-sm font-medium text-status-needs-you" data-testid="workflow-attention-undetermined">
							{t("cc.attention.causeUndetermined")}
						</p>
					) : null}
				</div>
				<div className="flex flex-col gap-1">
					<h3 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">{t("cc.attention.action")}</h3>
					<p className="text-sm" data-testid="workflow-attention-action">
						{report.recommendedAction
							? translateDynamic(tt, actionLabelKey(report.recommendedAction), report.recommendedAction)
							: t("cc.attention.noAction")}
					</p>
					{report.automaticAction ? (
						<p className="text-xs text-muted-foreground">
							{t("cc.attention.automatic", {
								action: translateDynamic(tt, `wf.advice.auto.${report.automaticAction}`, report.automaticAction),
							})}
						</p>
					) : null}
					{report.automaticActionBlockedReason ? (
						<p className="text-xs text-muted-foreground">
							{t("cc.attention.automaticBlocked", {
								reason: translateDynamic(
									tt,
									`wf.advice.autoBlocked.${report.automaticActionBlockedReason}`,
									report.automaticActionBlockedReason,
								),
							})}
						</p>
					) : null}
					<p className="text-xs text-muted-foreground">{t("cc.attention.actionsBelow")}</p>
				</div>
				<div className="flex min-w-0 flex-col gap-1" data-testid="workflow-attention-known">
					<h3 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">{t("cc.attention.known")}</h3>
					<dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
						{report.known.map((row) => (
							<div className="contents" key={row.labelKey}>
								<dt>{t(row.labelKey as "cc.evidence.reason")}</dt>
								<dd className={cn("min-w-0 break-all text-foreground", row.mono && "font-mono")}>
									<EvidenceValue now={now} row={row} />
								</dd>
							</div>
						))}
					</dl>
				</div>
				<div className="flex flex-col gap-1">
					<h3 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">{t("cc.attention.unknown")}</h3>
					{report.unknowns.length > 0 ? (
						<ul className="flex list-disc flex-col gap-0.5 pl-4 text-xs text-muted-foreground" data-testid="workflow-attention-unknowns">
							{report.unknowns.map((key) => (
								<li key={key}>{t(key as "cc.unknown.cause")}</li>
							))}
						</ul>
					) : (
						<p className="text-xs text-muted-foreground">{t("cc.attention.nothingUnknown")}</p>
					)}
				</div>
			</div>
		</section>
	);
}

function IncidentAdvisor({ signals, renderSessionLink }: { signals: IncidentSignal[]; renderSessionLink?: SessionLinkRenderer }) {
	const { t } = useTranslation();
	if (signals.length === 0) return null;
	return (
		<Card testId="workflow-incident-advisor" title={t("cc.incident.title")}>
			<p className="text-xs text-muted-foreground">{t("cc.incident.explainer")}</p>
			<ul className="flex flex-col gap-2">
				{signals.map((signal) => (
					<li className="flex gap-2 text-xs" data-signal={signal.id} key={signal.id}>
						<Lightbulb
							aria-hidden="true"
							className={cn("mt-0.5 size-3.5 shrink-0", signal.severity === "warn" ? "text-status-needs-you" : "text-muted-foreground")}
						/>
						<div className="flex min-w-0 flex-col gap-0.5">
							<span className="font-medium text-foreground">{t(`cc.incident.${signal.id}.title` as "cc.incident.worker_quiet.title")}</span>
							<span className="text-muted-foreground">
								{t("cc.incident.evidenceLine", {
									text: t(`cc.incident.${signal.id}.evidence` as "cc.incident.worker_quiet.evidence", signal.params),
								})}
							</span>
							<span className="text-muted-foreground">
								{t("cc.incident.nextLine", { text: t(`cc.incident.${signal.id}.next` as "cc.incident.worker_quiet.next") })}
							</span>
							{signal.sessionId && renderSessionLink ? <span>{renderSessionLink(signal.sessionId)}</span> : null}
						</div>
					</li>
				))}
			</ul>
		</Card>
	);
}

function StructureCard({
	detail,
	tree,
	renderSessionLink,
}: {
	detail: WorkflowRunDetail;
	tree: SessionNode[];
	renderSessionLink?: SessionLinkRenderer;
}) {
	const { t } = useTranslation();
	const tt = t as TFunction;
	return (
		<Card testId="workflow-structure" title={t("cc.tree.title")}>
			<p className="text-xs text-muted-foreground">{t("cc.tree.explainer")}</p>
			<div className="text-xs">
				<p className="break-all font-medium">{t("cc.tree.root", { id: detail.run.id })}</p>
				{tree.length === 0 ? (
					<p className="pl-4 text-muted-foreground">{t("cc.tree.empty")}</p>
				) : (
					<ul className="flex flex-col gap-1 pt-1">
						{tree.map((node, index) => (
							<li
								className={cn("flex min-w-0 items-start gap-2", node.current && "font-medium")}
								data-agent-session={node.agentSession}
								data-role={node.role}
								data-testid="workflow-structure-node"
								key={node.stepId}
							>
								<span aria-hidden="true" className="shrink-0 font-mono text-passive">
									{t(index === tree.length - 1 ? "cc.tree.lastBranch" : "cc.tree.branch")}
								</span>
								<span className="flex min-w-0 flex-col">
									<span className="text-foreground">
										{agentText(node, tt)}
										{node.current ? ` · ${t("cc.tree.current")}` : ""}
									</span>
									<span className="min-w-0 break-all text-muted-foreground">
										{translateDynamic(tt, `board.stepState.${node.state}`, node.state)}
										{" · "}
										{node.agentSession ? (
											node.sessionId ? (
												renderSessionLink ? (
													renderSessionLink(node.sessionId)
												) : (
													<span className="font-mono">{node.sessionId}</span>
												)
											) : (
												t("cc.tree.noSessionRecorded")
											)
										) : (
											t("cc.tree.noAgentSession")
										)}
									</span>
								</span>
							</li>
						))}
					</ul>
				)}
			</div>
		</Card>
	);
}

function ReviewCard({ review }: { review: ReviewSummary | undefined }) {
	const { t } = useTranslation();
	return (
		<Card testId="workflow-review-card" title={t("cc.review.title")}>
			{review ? (
				<>
					<p className="text-sm font-medium" data-outcome={review.outcome} data-testid="workflow-review-outcome">
						{t(`cc.review.outcome.${review.outcome}` as "cc.review.outcome.unknown")}
					</p>
					<dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
						<dt>{t("cc.review.reviewer")}</dt>
						<dd className="text-foreground">{review.reviewer ?? t("cc.review.reviewerUnknown")}</dd>
						<dt>{t("cc.review.fixCycle")}</dt>
						<dd className="text-foreground" data-testid="workflow-review-fix-cycle">
							{fixCycleText(review.fixCycles, t as TFunction)}
						</dd>
					</dl>
					{review.findingsSummary ? (
						<p className="line-clamp-3 whitespace-pre-wrap text-xs text-muted-foreground">{review.findingsSummary}</p>
					) : null}
				</>
			) : (
				<p className="text-xs text-muted-foreground">{t("cc.review.none")}</p>
			)}
			<p className="text-[11px] text-passive">{t("cc.review.vsVerify")}</p>
		</Card>
	);
}

const VERIFY_VISIBLE_CHECKS = 5;

function VerifyCard({ verify }: { verify: VerifySummary | undefined }) {
	const { t } = useTranslation();
	if (!verify) {
		return (
			<Card testId="workflow-verify-card" title={t("cc.verify.title")}>
				<p className="text-xs text-muted-foreground">{t("cc.verify.none")}</p>
			</Card>
		);
	}
	const visible = verify.checks.slice(0, VERIFY_VISIBLE_CHECKS);
	const hidden = verify.checks.length - visible.length;
	return (
		<Card testId="workflow-verify-card" title={t("cc.verify.title")}>
			<p className="text-sm font-medium" data-outcome={verify.outcome} data-testid="workflow-verify-outcome">
				{t(`cc.verify.outcome.${verify.outcome}` as "cc.verify.outcome.unknown")}
			</p>
			{verify.checks.length > 0 ? (
				<>
					<p className="text-xs text-muted-foreground">
						{t("cc.verify.checks", { passed: verify.checks.length - verify.failedCount, failed: verify.failedCount })}
					</p>
					<ul className="flex flex-col gap-0.5 text-xs">
						{visible.map((check, index) => (
							<li className="flex min-w-0 items-start gap-1.5" key={`${check.label}-${index}`}>
								{check.passed ? (
									<CircleCheck aria-hidden="true" className="mt-0.5 size-3.5 shrink-0 text-success" />
								) : (
									<CircleX aria-hidden="true" className="mt-0.5 size-3.5 shrink-0 text-destructive" />
								)}
								<span className="flex min-w-0 flex-col">
									<span className="truncate font-mono text-foreground" title={check.label}>
										{check.label}
									</span>
									{!check.passed && (check.reason || (check.exitCode !== undefined && check.exitCode !== null)) ? (
										<span className="text-muted-foreground">
											{[
												check.exitCode !== undefined && check.exitCode !== null ? t("cc.verify.exitCode", { code: check.exitCode }) : "",
												check.reason ?? "",
											]
												.filter(Boolean)
												.join(" · ")}
										</span>
									) : null}
								</span>
							</li>
						))}
					</ul>
					{hidden > 0 ? <p className="text-xs text-muted-foreground">{t("cc.verify.more", { n: hidden })}</p> : null}
				</>
			) : null}
			{verify.infraFailure ? (
				<p className="text-xs text-status-needs-you">
					{t("cc.verify.infra", { kind: verify.infraFailure.kind, detail: verify.infraFailure.detail })}
				</p>
			) : null}
			{verify.triggeredFix ? (
				<p className="text-xs text-foreground" data-testid="workflow-verify-triggered-fix">
					{t("cc.verify.triggeredFix")}
				</p>
			) : null}
			{verify.reusedCheckCount ? (
				<p className="text-xs text-muted-foreground">{t("cc.verify.reused", { n: verify.reusedCheckCount })}</p>
			) : null}
			{verify.errorClass || verify.stopReason ? (
				<p className="break-all font-mono text-[11px] text-passive">
					{[verify.errorClass, verify.stopReason].filter(Boolean).join(" · ")}
				</p>
			) : null}
			<p className="text-[11px] text-passive">{t("cc.verify.detailBelow")}</p>
		</Card>
	);
}

function UsageCard({ usage }: { usage: UsageDigest }) {
	const { t } = useTranslation();
	const tt = t as TFunction;
	const cost =
		usage.cost.certainty === "unknown" || usage.cost.amount === undefined
			? t("cc.usage.unknown")
			: `${usage.cost.currency ?? ""} ${usage.cost.amount.toFixed(2)}`.trim();
	return (
		<Card testId="workflow-usage-digest" title={t("cc.usage.title")}>
			<dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-xs text-muted-foreground sm:grid-cols-[auto_1fr_auto_1fr]">
				<dt>{t("cc.usage.calls")}</dt>
				<dd className="text-foreground">
					{usage.calls === undefined ? t("cc.usage.unknown") : usage.calls.toLocaleString()}
					<CertaintyChip certainty={usage.context} />
				</dd>
				<dt>{t("cc.usage.context")}</dt>
				<dd className="text-foreground">
					{usage.context === "unknown"
						? t("cc.usage.unknown")
						: t("cc.usage.contextValue", {
								current: formatTokens(usage.contextCurrent, tt),
								peak: formatTokens(usage.contextPeak, tt),
							})}
					<CertaintyChip certainty={usage.context} />
				</dd>
				<dt>{t("cc.usage.growth")}</dt>
				<dd className="text-foreground">
					{usage.context === "unknown" ? t("cc.usage.unknown") : formatTokens(usage.contextGrowth, tt)}
					<CertaintyChip certainty={usage.context} />
				</dd>
				<dt>{t("cc.usage.tokens")}</dt>
				<dd className="text-foreground" data-testid="workflow-usage-tokens">
					{usage.tokens === "unknown"
						? t("cc.usage.unknown")
						: t("cc.usage.tokensValue", {
								input: formatTokens(usage.inputTokens, tt),
								output: formatTokens(usage.outputTokens, tt),
							})}
					<CertaintyChip certainty={usage.tokens} />
				</dd>
				<dt>{t("cc.usage.cost")}</dt>
				<dd className="text-foreground" data-testid="workflow-usage-cost">
					{cost}
					<CertaintyChip certainty={usage.cost.certainty} />
				</dd>
				{usage.assembledContextEstimate !== undefined ? (
					<>
						<dt>{t("cc.usage.assembled")}</dt>
						<dd className="text-foreground">
							{usage.assembledContextEstimate.toLocaleString()}
							<CertaintyChip certainty="modelled" />
						</dd>
					</>
				) : null}
				{usage.budgetState ? (
					<>
						<dt>{t("cc.usage.budget")}</dt>
						<dd className="text-foreground">
							{t("cc.usage.budgetValue", { percent: usage.budgetPercent ?? 0, state: usage.budgetState })}
						</dd>
					</>
				) : null}
				{usage.advisories.length > 0 ? (
					<>
						<dt>{t("cc.usage.advisories")}</dt>
						<dd className="text-foreground">{usage.advisories.length}</dd>
					</>
				) : null}
			</dl>
			{usage.cost.unpricedModels.length > 0 ? (
				<p className="text-xs text-muted-foreground">{t("cc.usage.unpriced", { models: usage.cost.unpricedModels.join(", ") })}</p>
			) : null}
			{/* No P7 shadow economics here on purpose: there is no HTTP readback
			    for it, and a modelled saving must never sit next to measured
			    numbers as if it were one. */}
			<p className="text-[11px] text-passive">{t("cc.usage.detailBelow")}</p>
		</Card>
	);
}

/**
 * The control-center header for one workflow run.
 *
 * `now` is injectable so a test can pin the clocks; the page passes nothing and
 * the clocks are read on each render, which the run query's existing poll
 * already triggers — this adds no timer and no request of its own.
 */
export function WorkflowControlCenter({
	detail,
	renderSessionLink,
	now = Date.now(),
}: {
	detail: WorkflowRunDetail;
	renderSessionLink?: SessionLinkRenderer;
	now?: number;
}) {
	const { t } = useTranslation();
	const tt = t as TFunction;
	const run = detail.run;
	const presentation = detail.presentation;
	const status = headlineStatus(detail);
	const agent = activeAgent(detail);
	const cycles = fixCycleSummary(detail);
	const report = attentionReport(detail);
	const signals = incidentSignals(detail);
	const liveness = run.workerLiveness;
	const silent = liveness?.observed ? (liveness.silentForSeconds ?? secondsSince(liveness.lastSignalAt, now)) : null;
	const duration = runDurationSeconds(detail, now);
	const tech = presentation?.technical;
	const showCycles = detail.steps.some((step) => step.kind === "review" || step.kind === "fix");

	return (
		<div className="flex flex-col gap-3" data-testid="workflow-control-center">
			<section
				className={cn(
					"flex flex-col gap-3 rounded-lg border p-3",
					headlineTone(status) === "attention" ? "border-status-needs-you/50" : "border-border",
				)}
			>
				<div className="flex flex-wrap items-center gap-2">
					<span
						className="text-[10px] font-semibold uppercase tracking-wide text-passive"
						data-testid="workflow-kind-label"
						title={t("cc.workflowDefinition")}
					>
						{t("cc.kind.workflow")}
					</span>
					<WorkflowHeadlineStatus status={status} />
					<ModeChips run={run} />
				</div>
				{presentation ? <WorkflowStatusSummary presentation={presentation} /> : null}
				<dl
					className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs text-muted-foreground sm:grid-cols-[auto_1fr_auto_1fr]"
					data-testid="workflow-control-facts"
				>
					<dt>{t("cc.fact.currentStep")}</dt>
					<dd className="text-foreground" data-testid="workflow-fact-step">
						{agent
							? t("cc.fact.stepValue", { ordinal: agent.ordinal, role: t(`cc.role.${agent.role}` as "cc.role.worker") })
							: t("cc.fact.noCurrentStep")}
					</dd>
					<dt>{t("cc.fact.whoWorks")}</dt>
					<dd className="min-w-0 break-all text-foreground" data-testid="workflow-fact-agent">
						{agent ? (
							<>
								{agent.agentSession ? agentText(agent, tt) : t("cc.tree.noAgentSession")}
								{agent.sessionId && renderSessionLink ? <> · {renderSessionLink(agent.sessionId)}</> : null}
							</>
						) : (
							t("cc.fact.nobody")
						)}
					</dd>
					<dt>{t("cc.fact.lastSignal")}</dt>
					<dd className="text-foreground" data-testid="workflow-fact-signal">
						{liveness?.observed ? agoText(tt, silent) : t("cc.fact.noSignal")}
					</dd>
					<dt>{t("cc.fact.lastTransition")}</dt>
					<dd className="text-foreground">
						{liveness?.observed ? agoText(tt, secondsSince(liveness.lastTransitionAt, now)) : t("shell.liveness.unknown")}
					</dd>
					<dt>{t("cc.fact.lastEvent")}</dt>
					<dd className="min-w-0 break-all text-foreground">
						{tech?.lastEventAt
							? `${agoText(tt, secondsSince(tech.lastEventAt, now))}${tech.lastEventPhase ? ` · ${tech.lastEventPhase}` : ""}`
							: t("shell.liveness.unknown")}
					</dd>
					<dt>{t("cc.fact.duration")}</dt>
					<dd className="text-foreground" data-testid="workflow-fact-duration">
						{duration === null ? t("shell.liveness.unknown") : formatDurationCompact(duration)}
					</dd>
					{showCycles ? (
						<>
							<dt>{t("cc.fact.fixCycle")}</dt>
							<dd className="text-foreground" data-testid="workflow-fact-fix-cycle">
								{fixCycleText(cycles, tt)}
							</dd>
						</>
					) : null}
				</dl>
				<Timeline chips={compactTimeline(presentation?.timeline)} />
				<p className="break-all font-mono text-[11px] text-passive" data-testid="workflow-technical-line">
					{t("cc.technical", { state: run.state, phase: run.phase ?? "-", stage: presentation?.stage ?? run.stage ?? "-" })}
				</p>
			</section>
			{report ? <AttentionBlock now={now} report={report} /> : null}
			<IncidentAdvisor renderSessionLink={renderSessionLink} signals={signals} />
		</div>
	);
}

/** The run's structure and outcomes: workflow and its sessions, review, Verify, usage. */
export function WorkflowRunAnatomy({
	detail,
	renderSessionLink,
}: {
	detail: WorkflowRunDetail;
	renderSessionLink?: SessionLinkRenderer;
}) {
	const usage = usageDigest(detail.usage);
	return (
		<div className="flex flex-col gap-3" data-testid="workflow-run-anatomy">
			<div className="grid grid-cols-1 gap-3 lg:grid-cols-3">
				<StructureCard detail={detail} renderSessionLink={renderSessionLink} tree={sessionTree(detail)} />
				<ReviewCard review={reviewSummary(detail)} />
				<VerifyCard verify={verifySummary(detail)} />
			</div>
			{usage ? <UsageCard usage={usage} /> : null}
		</div>
	);
}
