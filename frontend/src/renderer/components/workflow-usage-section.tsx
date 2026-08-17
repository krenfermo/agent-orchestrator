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
				</dl>
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
