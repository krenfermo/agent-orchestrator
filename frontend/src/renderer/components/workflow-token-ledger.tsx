import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { components } from "../../api/schema";

/**
 * workflow-token-ledger.tsx — P3-E's "Tokens & cost" section.
 *
 * ONE RULE GOVERNS EVERY LINE HERE: nothing rendered may claim more than the
 * backend claimed. Concretely —
 *
 *  - A token figure is printed bare only when `source === "provider_reported"`.
 *    Anything else is prefixed with "~" or shown as "unknown"; the renderer
 *    never upgrades an estimate into a measurement by dropping the marker.
 *  - A cost is printed only when `cost.known`. An unknown cost renders as
 *    "cost unknown", never as $0.00 — "free" and "unpriced" are different
 *    claims and only one of them is true.
 *  - `recorded: false` renders "no usage data recorded", never zeroes.
 *  - The assembled-context block is labeled AO-assembled and estimated, and is
 *    never added to the provider figures. They measure different things.
 *
 * The renderer also computes NO totals. Every number below is read straight off
 * the response; the backend owns the arithmetic, because a session can serve
 * several roles and only the backend knows not to count it twice.
 */

type LedgerResponse = components["schemas"]["ControllersWorkflowUsageLedgerResponse"];
type RoleLine = components["schemas"]["ControllersRoleUsageLineResponse"];
type ModelLine = components["schemas"]["ControllersModelUsageLineResponse"];
type CostResponse = components["schemas"]["ControllersUsageCostResponse"];
type TokensResponse = components["schemas"]["ControllersUsageTokenTotalsResponse"];
type BudgetResponse = components["schemas"]["ControllersUsageBudgetResponse"];
type ContextResponse = components["schemas"]["ControllersWorkflowContextResponse"];
type DynamicsResponse = components["schemas"]["ControllersWorkflowUsageDynamicsResponse"];
type TrajectoryResponse = components["schemas"]["ControllersContextTrajectoryResponse"];
type AdvisoryResponse = components["schemas"]["ControllersUsageAdvisoryResponse"];
type StepUsageResponse = components["schemas"]["ControllersStepUsageResponse"];

const ROLE_LABEL_KEYS: Record<string, string> = {
	planner: "shell.workflowUsage.roleLabel.planner",
	worker: "shell.workflowUsage.roleLabel.worker",
	reviewer: "shell.workflowUsage.roleLabel.reviewer",
	fix_worker: "shell.workflowUsage.roleLabel.fix_worker",
	verify: "shell.workflowUsage.roleLabel.verify",
	decision_resolver: "shell.workflowUsage.roleLabel.decision_resolver",
};

const ADVISORY_LABEL_KEYS: Record<string, string> = {
	duration_above_profile: "shell.usageShape.advisory.duration",
	provider_calls_above_profile: "shell.usageShape.advisory.calls",
	context_growth_above_profile: "shell.usageShape.advisory.growth",
	cost_above_profile: "shell.usageShape.advisory.cost",
	growth_without_progress: "shell.usageShape.advisory.noProgress",
	cache_read_dominant: "shell.usageShape.advisory.cacheDominant",
	context_per_call_above_profile: "shell.usageShape.advisory.contextPerCall",
	coordination_turns_dominant: "shell.usageShape.advisory.coordinationDominant",
	repeated_wait_check_shape: "shell.usageShape.advisory.waitShape",
};

const SOURCE_LABEL_KEYS: Record<string, string> = {
	task_spec: "shell.usageLedger.source.taskSpec",
	project_memory: "shell.usageLedger.source.projectMemory",
	shared_knowledge: "shell.usageLedger.source.sharedKnowledge",
	repo_content: "shell.usageLedger.source.repoContent",
	index_reuse: "shell.usageLedger.source.indexReuse",
	other: "shell.usageLedger.source.other",
};

// Looks up a translation key from a small, finite, hardcoded map. Mirrors the
// same helper in workflow-usage-section.tsx for the same reason: the generated
// TFunction key union cannot express "one of these known keys".
function translate(t: TFunction, key: string): string {
	return (t as unknown as (key: string) => string)(key);
}

function compactNumber(n: number): string {
	if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
	if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
	return n.toLocaleString();
}

/**
 * tokenText renders a token total together with the claim it is entitled to
 * make. A provider-reported count is printed bare; anything derived is marked
 * with "~"; an unknown one is not printed as a number at all.
 */
export function tokenText(t: TFunction, tokens: TokensResponse | undefined, source: string): string {
	if (!tokens || source === "unknown" || source === "") return t("shell.usageLedger.unknown");
	const formatted = compactNumber(tokens.total);
	return source === "provider_reported" ? formatted : `~${formatted}`;
}

/** costText never renders an unknown cost as zero. */
export function costText(t: TFunction, cost: CostResponse | undefined): string {
	if (!cost || !cost.known) return t("shell.usageLedger.costUnknown");
	const amount = `${cost.currency || "USD"} ${cost.amount.toFixed(2)}`;
	return cost.unpricedModels && cost.unpricedModels.length > 0
		? t("shell.usageLedger.costPartial", { amount, models: cost.unpricedModels.join(", ") })
		: amount;
}

function BudgetMeter({ budget }: { budget: BudgetResponse }) {
	const { t } = useTranslation();
	// "unset" is not "0% used". Nobody configured a ceiling, so no meter is
	// drawn at all — an empty bar would imply a limit that does not exist.
	if (budget.state === "unset") return null;
	const percent = budget.tokenPercent ?? budget.costPercent;
	const tone =
		budget.state === "exhausted"
			? "border-destructive/50 bg-destructive/10 text-destructive"
			: budget.state === "warning"
				? "border-warning/50 bg-warning/10 text-warning"
				: "border-border text-muted-foreground";
	return (
		<div className={`rounded border px-2 py-1.5 text-xs ${tone}`} role="note">
			<span className="font-medium">
				{budget.state === "exhausted"
					? t("shell.usageLedger.budgetExhausted")
					: budget.state === "warning"
						? t("shell.usageLedger.budgetWarning")
						: t("shell.usageLedger.budgetOk")}
			</span>
			{percent !== null && percent !== undefined ? (
				<span className="ml-1">{t("shell.usageLedger.budgetPercent", { percent: Math.round(percent) })}</span>
			) : null}
			{budget.state === "exhausted" ? (
				<p className="mt-1">{t("shell.usageLedger.budgetExhaustedDetail")}</p>
			) : null}
		</div>
	);
}

function RoleTable({ roles }: { roles: RoleLine[] }) {
	const { t } = useTranslation();
	if (roles.length === 0) return null;
	return (
		<table className="w-full text-xs" data-testid="usage-role-table">
			<thead className="text-muted-foreground">
				<tr>
					<th className="text-left font-normal">{t("shell.usageLedger.colRole")}</th>
					<th className="text-left font-normal">{t("shell.usageLedger.colModel")}</th>
					<th className="text-right font-normal">{t("shell.usageLedger.colTokens")}</th>
					<th className="text-right font-normal">{t("shell.usageLedger.colCost")}</th>
				</tr>
			</thead>
			<tbody>
				{roles.map((role) => (
					<tr key={`${role.role}-${role.cycle}-${role.attemptId}`}>
						<td className="py-0.5">
							{translate(t, ROLE_LABEL_KEYS[role.role] ?? "shell.usageLedger.roleUnknown")}
							{role.repair && role.cycle > 0 ? (
								<span className="ml-1 text-muted-foreground">
									{t("shell.usageLedger.repairCycle", { cycle: role.cycle })}
								</span>
							) : null}
						</td>
						<td className="py-0.5 text-muted-foreground">{role.model || role.provider || "—"}</td>
						<td className="py-0.5 text-right">{tokenText(t, role.tokens, role.source)}</td>
						<td className="py-0.5 text-right text-muted-foreground">{costText(t, role.cost)}</td>
					</tr>
				))}
			</tbody>
		</table>
	);
}

function ModelTable({ models }: { models: ModelLine[] }) {
	const { t } = useTranslation();
	if (models.length === 0) return null;
	return (
		<table className="w-full text-xs" data-testid="usage-model-table">
			<thead className="text-muted-foreground">
				<tr>
					<th className="text-left font-normal">{t("shell.usageLedger.colProvider")}</th>
					<th className="text-left font-normal">{t("shell.usageLedger.colModel")}</th>
					<th className="text-right font-normal">{t("shell.usageLedger.colTokens")}</th>
					<th className="text-right font-normal">{t("shell.usageLedger.colCost")}</th>
				</tr>
			</thead>
			<tbody>
				{models.map((model) => (
					<tr key={`${model.provider}-${model.model}`}>
						<td className="py-0.5">{model.provider || "—"}</td>
						<td className="py-0.5 text-muted-foreground">{model.model}</td>
						<td className="py-0.5 text-right">{tokenText(t, model.tokens, model.source)}</td>
						<td className="py-0.5 text-right text-muted-foreground">{costText(t, model.cost)}</td>
					</tr>
				))}
			</tbody>
		</table>
	);
}

/**
 * UnobservableNotice names the roles that ran but have not reported their spend.
 *
 * Since P3-E's completion pass this is a PENDING state, not an architectural
 * one: a reviewer, a decision resolver and the planner can all be metered now.
 * A role listed here is a surface whose provider report has not arrived, which
 * makes the totals a lower bound — and saying so is the whole point, because a
 * partial total that looks complete is the one thing this section must not do.
 */
function UnobservableNotice({ roles }: { roles: RoleLine[] }) {
	const { t } = useTranslation();
	if (roles.length === 0) return null;
	const names = Array.from(
		new Set(roles.map((r) => translate(t, ROLE_LABEL_KEYS[r.role] ?? "shell.usageLedger.roleUnknown"))),
	).join(", ");
	return (
		<p className="rounded border border-border bg-muted/40 px-2 py-1.5 text-xs text-muted-foreground" role="note">
			{t("shell.usageLedger.unobservable", { roles: names })}
		</p>
	);
}

function ContextBlock({ context }: { context: ContextResponse }) {
	const { t } = useTranslation();
	return (
		<div className="rounded-lg border border-border p-3 text-xs" data-testid="usage-context-block">
			<h3 className="font-medium">{t("shell.usageLedger.contextTitle")}</h3>
			<p className="mt-1 text-muted-foreground">{t("shell.usageLedger.contextBasis")}</p>
			<dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 text-muted-foreground">
				<dt>{t("shell.usageLedger.contextAssembled")}</dt>
				<dd>{t("shell.usageLedger.estimatedTokens", { value: compactNumber(context.estimatedAssembledTokens) })}</dd>
				<dt>{t("shell.usageLedger.contextDispatches")}</dt>
				<dd>{context.dispatches}</dd>
				{context.memory.mode ? (
					<>
						<dt>{t("shell.usageLedger.memoryMode")}</dt>
						<dd>{context.memory.mode}</dd>
						<dt>{t("shell.usageLedger.memoryProvider")}</dt>
						<dd>{context.memory.provider}</dd>
						<dt>{t("shell.usageLedger.memoryGeneration")}</dt>
						<dd>{context.memory.generation}</dd>
						<dt>{t("shell.usageLedger.memoryContribution")}</dt>
						<dd>
							{t("shell.usageLedger.estimatedTokens", { value: compactNumber(context.memory.estimatedPackTokens) })}
						</dd>
						<dt>{t("shell.usageLedger.memoryReused")}</dt>
						<dd>{t("shell.usageLedger.itemsOfCandidates", {
							selected: context.memory.sharedSelected,
							candidates: context.memory.sharedCandidates,
						})}</dd>
					</>
				) : null}
			</dl>

			{context.bySource && context.bySource.length > 0 ? (
				<dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 text-muted-foreground">
					{context.bySource.map((line) => (
						<div className="contents" key={line.source}>
							<dt>{translate(t, SOURCE_LABEL_KEYS[line.source] ?? "shell.usageLedger.source.other")}</dt>
							<dd>{t("shell.usageLedger.estimatedTokens", { value: compactNumber(line.estimatedTokens) })}</dd>
						</div>
					))}
				</dl>
			) : null}

			{/* The saving claim, and the words it is allowed to use. This is
			    context AO did not ASSEMBLE, measured against a real baseline; it
			    is NOT a claim that the provider billed that many fewer tokens.
			    With no baseline, nothing is shown — not zero. */}
			{context.avoidedComparable ? (
				<p className="mt-2 text-muted-foreground">
					{t("shell.usageLedger.contextAvoided", {
						value: compactNumber(context.estimatedAvoidedTokens),
					})}
				</p>
			) : (
				<p className="mt-2 text-muted-foreground">{t("shell.usageLedger.contextNoBaseline")}</p>
			)}

			{!context.complete ? (
				<p className="mt-2 text-muted-foreground">{t("shell.usageLedger.contextPartial")}</p>
			) : null}
		</div>
	);
}

/**
 * durationText renders a second count as the coarsest unit that still says
 * something. It never renders "0s" for an unknown duration: null is passed
 * through as the unknown label, because a run whose clock AO could not read is
 * not a run that took no time.
 */
function durationText(t: TFunction, seconds: number | null | undefined): string {
	if (seconds === null || seconds === undefined) return t("shell.usageLedger.unknown");
	if (seconds < 60) return t("shell.usageShape.seconds", { value: Math.round(seconds) });
	if (seconds < 3600) return t("shell.usageShape.minutes", { value: Math.round(seconds / 60) });
	return t("shell.usageShape.hours", { value: (seconds / 3600).toFixed(1) });
}

/**
 * AdvisoryList renders what AO has to say about a run's SHAPE.
 *
 * These never change what AO does — no dispatch is refused and no run is
 * parked because of one — so they are styled as notes, and the informational
 * ones (cache-read dominance is the current example) are visibly not warnings.
 * Reading 98% cache read as a fault would be the opposite of the point: it is
 * the arithmetic of an agentic loop, and it is shown so a large bill is not
 * left to be interpreted alone.
 */
function AdvisoryList({ warnings }: { warnings: AdvisoryResponse[] }) {
	const { t } = useTranslation();
	if (warnings.length === 0) return null;
	return (
		<ul className="flex flex-col gap-1" data-testid="usage-shape-advisories">
			{warnings.map((warning) => (
				<li
					className={
						warning.severity === "warn"
							? "rounded border border-warning/50 bg-warning/10 px-2 py-1 text-warning"
							: "rounded border border-border px-2 py-1 text-muted-foreground"
					}
					key={warning.code}
					role="note"
				>
					{translate(t, ADVISORY_LABEL_KEYS[warning.code] ?? "shell.usageShape.advisory.unknown")}
					<span className="ml-1">
						{t("shell.usageShape.advisoryNumbers", {
							observed: warning.observed.toLocaleString(),
							threshold: warning.threshold.toLocaleString(),
						})}
					</span>
				</li>
			))}
		</ul>
	);
}

/** TrajectoryLine renders one context series: how many calls, and both ends. */
function TrajectoryLine({ trajectory }: { trajectory: TrajectoryResponse }) {
	const { t } = useTranslation();
	if (!trajectory.observable) return <span>{t("shell.usageShape.notObservable")}</span>;
	return (
		<span>
			{t("shell.usageShape.contextRange", {
				first: compactNumber(trajectory.firstContextTokens),
				last: compactNumber(trajectory.lastContextTokens),
				growth: compactNumber(trajectory.growthTokens),
			})}
		</span>
	);
}

/** StepTable answers "which step cost the money". */
function StepTable({ steps }: { steps: StepUsageResponse[] }) {
	const { t } = useTranslation();
	if (steps.length === 0) return null;
	return (
		<div className="flex flex-col gap-1" data-testid="usage-shape-steps">
			<h4 className="font-medium">{t("shell.usageShape.stepsTitle")}</h4>
			{steps.map((step) => (
				<div className="flex items-baseline justify-between gap-2" key={`${step.workflowStepId}-${step.cycle}`}>
					<span className="truncate text-muted-foreground">
						{translate(t, ROLE_LABEL_KEYS[step.role] ?? "shell.usageShape.stepUnknownRole")}
						{step.cycle > 0 ? t("shell.usageShape.stepCycle", { cycle: step.cycle }) : ""}
					</span>
					<span className="shrink-0">
						{t("shell.usageShape.stepFigures", {
							calls: step.trajectory.providerCalls,
							tokens: tokenText(t, step.tokens, step.source),
							cost: costText(t, step.cost),
						})}
					</span>
				</div>
			))}
		</div>
	);
}

/**
 * DynamicsBlock is the SHAPE of a run's cost, beside its total.
 *
 * The total is what the ledger above already says and it explains nothing on
 * its own: $23 for a one-field fix is 193 calls against a conversation that
 * grew from 54k to 324k, and no sum contains that. These three figures — call
 * count, both ends of the context, elapsed — are the whole explanation, and
 * they are also the only two levers there are (the number of turns, and how
 * fast the context grows).
 */
function DynamicsBlock({ dynamics }: { dynamics: DynamicsResponse }) {
	const { t } = useTranslation();
	if (!dynamics.recorded) {
		return (
			<div className="rounded border border-border p-2" data-testid="usage-shape">
				<h3 className="font-medium">{t("shell.usageShape.title")}</h3>
				<p className="mt-1 text-muted-foreground">{t("shell.usageShape.notRecorded")}</p>
			</div>
		);
	}
	const trajectory = dynamics.trajectory;
	return (
		<div className="flex flex-col gap-2 rounded border border-border p-2" data-testid="usage-shape">
			<h3 className="font-medium">{t("shell.usageShape.title")}</h3>
			<dl className="grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 text-muted-foreground">
				<dt>{t("shell.usageShape.providerCalls")}</dt>
				<dd data-testid="usage-shape-calls">{trajectory.providerCalls.toLocaleString()}</dd>
				<dt>{t("shell.usageShape.context")}</dt>
				<dd data-testid="usage-shape-context">
					<TrajectoryLine trajectory={trajectory} />
				</dd>
				<dt>{t("shell.usageShape.peak")}</dt>
				<dd>{compactNumber(trajectory.peakContextTokens)}</dd>
				<dt>{t("shell.usageShape.elapsed")}</dt>
				<dd>{durationText(t, trajectory.elapsedSeconds)}</dd>
			</dl>
			{/* A series with events AO could not place in time is a LOWER BOUND,
			    and says so rather than implying it saw every call. */}
			{trajectory.unplaceableEvents > 0 ? (
				<p className="text-muted-foreground">
					{t("shell.usageShape.lowerBound", { count: trajectory.unplaceableEvents })}
				</p>
			) : null}
			<StepTable steps={dynamics.steps ?? []} />
			<AdvisoryList warnings={dynamics.warnings ?? []} />
		</div>
	);
}

/**
 * WorkflowTokenLedger renders the canonical per-run token and cost answer.
 */
export function WorkflowTokenLedger({ ledger }: { ledger: LedgerResponse }) {
	const { t } = useTranslation();

	if (!ledger.recorded) {
		return (
			<div className="rounded-lg border border-border p-3 text-xs" data-testid="usage-ledger">
				<h3 className="font-medium">{t("shell.usageLedger.title")}</h3>
				{/* An absence, not a zero: a run created before usage accounting,
				    or one whose provider AO cannot meter. */}
				<p className="mt-1 text-muted-foreground">{t("shell.usageLedger.notRecorded")}</p>
				<UnobservableNotice roles={ledger.unobservable ?? []} />
			</div>
		);
	}

	return (
		<div className="flex flex-col gap-2 rounded-lg border border-border p-3 text-xs" data-testid="usage-ledger">
			<div className="flex items-baseline justify-between">
				<h3 className="font-medium">{t("shell.usageLedger.title")}</h3>
				<span className="text-sm font-semibold" data-testid="usage-ledger-total">
					{tokenText(t, ledger.totals, ledger.source)} · {costText(t, ledger.cost)}
				</span>
			</div>

			{ledger.repairTokens.total > 0 ? (
				<p className="text-muted-foreground" data-testid="usage-repair-split">
					{t("shell.usageLedger.baseAndRepair", {
						base: compactNumber(ledger.baseTokens.total),
						repair: compactNumber(ledger.repairTokens.total),
					})}
				</p>
			) : null}

			<BudgetMeter budget={ledger.budget} />

			{/* The SHAPE, directly under the total it explains. */}
			{ledger.dynamics ? <DynamicsBlock dynamics={ledger.dynamics} /> : null}

			<RoleTable roles={ledger.roles} />
			<ModelTable models={ledger.models} />
			<UnobservableNotice roles={ledger.unobservable ?? []} />

			{ledger.children && ledger.children.length > 0 ? (
				<p className="text-muted-foreground">
					{t("shell.usageLedger.familyTotal", {
						count: ledger.children.length,
						value: compactNumber(ledger.familyTotals.total),
					})}
				</p>
			) : null}

			{ledger.approximateEvents > 0 ? (
				<p className="text-muted-foreground">
					{t("shell.usageLedger.approximateAttribution", {
						approximate: ledger.approximateEvents,
						total: ledger.totalEvents,
					})}
				</p>
			) : null}

			{ledger.cost.known ? (
				<p className="text-muted-foreground">
					{t("shell.usageLedger.costProvenance", {
						source: ledger.cost.pricingSource,
						version: ledger.cost.pricingVersion,
					})}
				</p>
			) : null}

			{ledger.context ? <ContextBlock context={ledger.context} /> : null}
		</div>
	);
}
