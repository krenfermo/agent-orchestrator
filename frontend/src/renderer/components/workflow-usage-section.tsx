import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { components } from "../../api/schema";

// Looks up a translation key computed at runtime from a small, finite,
// hardcoded map (ROLE_KEYS/RECOMMENDATION_KEYS below) — every value that
// reaches this function is one of a handful of keys already declared in
// en.json, but the generated TFunction key union can't express that without
// enumerating them, so the cast is confined to this one helper.
function translate(t: TFunction, key: string): string {
	const untypedT = t as unknown as (key: string) => string;
	return untypedT(key);
}

type WorkflowUsageResponse = components["schemas"]["ControllersWorkflowUsageResponse"];
type RoleUsageResponse = components["schemas"]["ControllersRoleUsageResponse"];
type RoutingUsageResponse = components["schemas"]["ControllersRoutingUsageResponse"];
type SessionLifecycleDecisionResponse = components["schemas"]["ControllersSessionLifecycleDecisionResponse"];

function durationText(ms: number | null | undefined, unknown: string): string {
	if (ms === null || ms === undefined) return unknown;
	if (ms < 1000) return `${ms}ms`;
	return `${(ms / 1000).toFixed(1)}s`;
}

const ROLE_KEYS: Record<string, string> = {
	planner: "shell.workflowUsage.roleLabel.planner",
	worker: "shell.workflowUsage.roleLabel.worker",
	reviewer: "shell.workflowUsage.roleLabel.reviewer",
	fix_worker: "shell.workflowUsage.roleLabel.fix_worker",
	verify: "shell.workflowUsage.roleLabel.verify",
};

const RECOMMENDATION_KEYS: Record<string, string> = {
	REUSE: "shell.workflowUsage.recommendation.REUSE",
	CONSIDER_COMPACTION: "shell.workflowUsage.recommendation.CONSIDER_COMPACTION",
	RECOMMEND_NEW_SESSION: "shell.workflowUsage.recommendation.RECOMMEND_NEW_SESSION",
	UNKNOWN: "shell.workflowUsage.recommendation.UNKNOWN",
};

function RoleRow({ role, unknown }: { role: RoleUsageResponse; unknown: string }) {
	const { t } = useTranslation();
	// The role/recommendation keys are a static, finite lookup table (ROLE_KEYS
	// above), not truly dynamic — the generated TFunction key union can't
	// express "one of these five known keys" without listing them, so the
	// lookup result needs an explicit cast to the key union TS can't infer.
	const label = translate(t, ROLE_KEYS[role.role] ?? role.role);
	const providerModel = [role.provider, role.model].filter(Boolean).join("/") || role.harness || unknown;
	const tokenText = (value: number | null | undefined) => (value === null || value === undefined ? unknown : value.toLocaleString());
	return (
		<div className="rounded border border-border p-2 text-xs">
			<div className="flex items-center justify-between font-medium">
				<span>{label}</span>
				<span className="text-muted-foreground">{providerModel}</span>
			</div>
			<dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 text-muted-foreground">
				<dt>{t("shell.workflowUsage.roleDuration")}</dt>
				<dd>{durationText(role.durationMs, unknown)}</dd>
				{role.role === "verify" && (
					<>
						<dt>{t("shell.workflowUsage.roleChecks")}</dt>
						<dd>{role.verifyChecks ?? unknown}</dd>
					</>
				)}
				<dt>{t("shell.workflowUsage.inputTokens")}</dt>
				<dd>{tokenText(role.usage?.totals?.inputTokens)}</dd>
				<dt>{t("shell.workflowUsage.outputTokens")}</dt>
				<dd>{tokenText(role.usage?.totals?.outputTokens)}</dd>
				<dt>{t("shell.workflowUsage.cachedTokens")}</dt>
				<dd>{tokenText(role.usage?.totals?.cacheReadTokens)}</dd>
			</dl>
		</div>
	);
}

function RoutingRow({ routing }: { routing: RoutingUsageResponse }) {
	const { t } = useTranslation();
	const label = translate(t, ROLE_KEYS[routing.role] ?? routing.role);
	return (
		<div className="rounded border border-border p-2 text-xs">
			<div className="flex items-center justify-between font-medium">
				<span>{label}</span>
				{routing.waiting && (
					<span className="text-amber-600 dark:text-amber-400">{t("shell.workflowUsage.routingWaiting")}</span>
				)}
			</div>
			<dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 text-muted-foreground">
				<dt>{t("shell.workflowUsage.routingPreferred")}</dt>
				<dd>{routing.preferredHarness || t("shell.workflowUsage.unknown")}</dd>
				<dt>{t("shell.workflowUsage.routingSelected")}</dt>
				<dd>{routing.selectedHarness || t("shell.workflowUsage.unknown")}</dd>
				<dt>{t("shell.workflowUsage.routingFallbackUsed")}</dt>
				<dd>{routing.fallbackUsed ? t("shell.workflowUsage.yes") : t("shell.workflowUsage.no")}</dd>
			</dl>
			{routing.reasonCodes && routing.reasonCodes.length > 0 && (
				<ul className="mt-1 list-disc pl-4 text-muted-foreground">
					{routing.reasonCodes.map((reason) => (
						<li key={reason}>{translate(t, `shell.workflowUsage.routingReason.${reason}`)}</li>
					))}
				</ul>
			)}
		</div>
	);
}

const LIFECYCLE_ACTION_KEYS: Record<string, string> = {
	reuse: "shell.workflowUsage.lifecycle.action.reuse",
	compact: "shell.workflowUsage.lifecycle.action.compact",
	new_session: "shell.workflowUsage.lifecycle.action.new_session",
	unknown: "shell.workflowUsage.lifecycle.action.unknown",
};

function SessionLifecycleRow({ decision }: { decision: SessionLifecycleDecisionResponse }) {
	const { t } = useTranslation();
	const label = translate(t, LIFECYCLE_ACTION_KEYS[decision.action] ?? decision.action);
	return (
		<div className="rounded border border-border p-2 text-xs">
			<div className="flex items-center justify-between font-medium">
				<span>{label}</span>
				{decision.role && <span className="text-muted-foreground">{decision.role}</span>}
			</div>
			{(decision.fromSessionId || decision.toSessionId) && (
				<dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 text-muted-foreground">
					{decision.fromSessionId && (
						<>
							<dt>{t("shell.workflowUsage.lifecycle.fromSession")}</dt>
							<dd>{decision.fromSessionId}</dd>
						</>
					)}
					{decision.toSessionId && decision.toSessionId !== decision.fromSessionId && (
						<>
							<dt>{t("shell.workflowUsage.lifecycle.toSession")}</dt>
							<dd>{decision.toSessionId}</dd>
						</>
					)}
					{decision.contextPackHash && (
						<>
							<dt>{t("shell.workflowUsage.lifecycle.contextPackHash")}</dt>
							<dd className="truncate" title={decision.contextPackHash}>
								{decision.contextPackHash.slice(0, 12)}
							</dd>
						</>
					)}
				</dl>
			)}
			{decision.reasons && decision.reasons.length > 0 && (
				<ul className="mt-1 list-disc pl-4 text-muted-foreground">
					{decision.reasons.map((reason) => (
						<li key={reason}>{translate(t, `shell.workflowUsage.lifecycle.reason.${reason}`)}</li>
					))}
				</ul>
			)}
		</div>
	);
}

type TaskMetrics = WorkflowUsageResponse["metrics"];

/**
 * Total tokens the providers actually processed for this run: input + output +
 * the cached input replayed on every turn.
 *
 * Returns null — never 0 — unless every component is a real observed value.
 * A partial sum would read as a smaller number than the truth, which is worse
 * than admitting the total is unknown. Checkpoint 8P-E.12 §9: this is the one
 * aggregate today's telemetry supports honestly. Tool-call volume and context
 * replay beyond the cached-token count are NOT measurable from what AO records
 * per session, and are deliberately absent here rather than approximated.
 */
function processedTokens(metrics: TaskMetrics): number | null {
	if (metrics.tokensCertainty !== "actual") return null;
	const { inputTokens, outputTokens, cachedTokens } = metrics;
	if (inputTokens === null || inputTokens === undefined) return null;
	if (outputTokens === null || outputTokens === undefined) return null;
	return inputTokens + outputTokens + (cachedTokens ?? 0);
}

/**
 * A display threshold, not a limit: nothing in AO stops or throttles a run
 * because of it. It exists so a very large number is accompanied by the reason
 * it got large (long review/fix loops replay the whole context every cycle)
 * instead of sitting there as an opaque figure the user has to interpret alone.
 */
const HIGH_PROCESSED_TOKENS = 2_000_000;

function TokenUsageNotice({ metrics }: { metrics: TaskMetrics }) {
	const { t } = useTranslation();
	const total = processedTokens(metrics);
	if (total === null) {
		return <p className="mt-2 text-muted-foreground">{t("shell.workflowUsage.tokensNotMeasured")}</p>;
	}
	if (total < HIGH_PROCESSED_TOKENS) return null;
	return (
		<p className="mt-2 rounded border border-warning/50 bg-warning/10 px-2 py-1.5 text-warning" role="note">
			{t("shell.workflowUsage.highUsageWarning", {
				total: total.toLocaleString(),
				cycles: metrics.fixCycles + metrics.reviewRuns,
			})}
		</p>
	);
}

/**
 * WorkflowUsageSection is Checkpoint 8J's minimal dashboard addition: per-
 * role provider/model/duration/usage, task-level metrics, and the
 * advisory-only session-refresh recommendation. It never triggers any
 * action — the recommendation is display-only.
 */
export function WorkflowUsageSection({ usage }: { usage: WorkflowUsageResponse }) {
	const { t } = useTranslation();
	const unknown = t("shell.workflowUsage.unknown");
	const tokenText = (value: number | null | undefined) => (value === null || value === undefined ? unknown : value.toLocaleString());

	return (
		<section className="flex flex-col gap-3">
			<h2 className="text-sm font-semibold text-muted-foreground">{t("shell.workflowUsage.title")}</h2>

			<div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
				{usage.roles.map((role) => (
					<RoleRow key={`${role.role}-${role.stepKind}`} role={role} unknown={unknown} />
				))}
			</div>

			{usage.routing && usage.routing.length > 0 && (
				<div className="flex flex-col gap-2">
					<h3 className="text-xs font-medium text-muted-foreground">{t("shell.workflowUsage.routingTitle")}</h3>
					<div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
						{usage.routing.map((routing) => (
							<RoutingRow key={`${routing.role}-${routing.stepKind}`} routing={routing} />
						))}
					</div>
				</div>
			)}

			{usage.sessionLifecycle && usage.sessionLifecycle.decisions && usage.sessionLifecycle.decisions.length > 0 && (
				<div className="flex flex-col gap-2">
					<h3 className="text-xs font-medium text-muted-foreground">{t("shell.workflowUsage.lifecycle.title")}</h3>
					<dl className="grid grid-cols-2 gap-x-2 gap-y-0.5 text-xs text-muted-foreground sm:grid-cols-5">
						<dt>{t("shell.workflowUsage.lifecycle.sessionsCreated")}</dt>
						<dd>{usage.sessionLifecycle.sessionsCreated}</dd>
						<dt>{t("shell.workflowUsage.lifecycle.sessionsReused")}</dt>
						<dd>{usage.sessionLifecycle.sessionsReused}</dd>
						<dt>{t("shell.workflowUsage.lifecycle.sessionsCompacted")}</dt>
						<dd>{usage.sessionLifecycle.sessionsCompacted}</dd>
						<dt>{t("shell.workflowUsage.lifecycle.contextPacksCreated")}</dt>
						<dd>{usage.sessionLifecycle.contextPacksCreated}</dd>
						<dt>{t("shell.workflowUsage.lifecycle.sessionSwitches")}</dt>
						<dd>{usage.sessionLifecycle.sessionSwitches}</dd>
					</dl>
					<div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
						{usage.sessionLifecycle.decisions.map((decision, i) => (
							<SessionLifecycleRow key={`${decision.action}-${decision.role}-${decision.createdAt ?? i}`} decision={decision} />
						))}
					</div>
				</div>
			)}

			<div className="rounded-lg border border-border p-3 text-xs">
				<h3 className="font-medium">{t("shell.workflowUsage.taskSummary")}</h3>
				<dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 text-muted-foreground">
					<dt>{t("shell.workflowUsage.attempts")}</dt>
					<dd>{usage.metrics.attempts}</dd>
					<dt>{t("shell.workflowUsage.fixCycles")}</dt>
					<dd>{usage.metrics.fixCycles}</dd>
					<dt>{t("shell.workflowUsage.reviewRuns")}</dt>
					<dd>{usage.metrics.reviewRuns}</dd>
					<dt>{t("shell.workflowUsage.reviewsSkipped")}</dt>
					<dd>{usage.metrics.reviewsSkipped ? t("shell.workflowUsage.yes") : t("shell.workflowUsage.no")}</dd>
					<dt>{t("shell.workflowUsage.verifyDuration")}</dt>
					<dd>{durationText(usage.metrics.verifyDurationMs, unknown)}</dd>
					<dt>{t("shell.workflowUsage.inputTokensTotal")}</dt>
					<dd>{tokenText(usage.metrics.inputTokens)}</dd>
					<dt>{t("shell.workflowUsage.outputTokensTotal")}</dt>
					<dd>{tokenText(usage.metrics.outputTokens)}</dd>
					<dt>{t("shell.workflowUsage.cachedTokensTotal")}</dt>
					<dd>{tokenText(usage.metrics.cachedTokens)}</dd>
					<dt>{t("shell.workflowUsage.processedTokensTotal")}</dt>
					<dd>{tokenText(processedTokens(usage.metrics))}</dd>
				</dl>
				<TokenUsageNotice metrics={usage.metrics} />
			</div>

			<div className="rounded-lg border border-border p-3 text-xs">
				<h3 className="font-medium">{t("shell.workflowUsage.advisoryTitle")}</h3>
				<p className="mt-1 font-medium text-foreground">
					{translate(t, RECOMMENDATION_KEYS[usage.advisory.recommendation] ?? "shell.workflowUsage.recommendation.UNKNOWN")}
				</p>
				<p className="mt-1 text-muted-foreground">{usage.advisory.reason}</p>
				{usage.advisory.signals && usage.advisory.signals.length > 0 && (
					<ul className="mt-1 list-disc pl-4 text-muted-foreground">
						{usage.advisory.signals.map((signal) => (
							<li key={signal}>{signal}</li>
						))}
					</ul>
				)}
			</div>
		</section>
	);
}
