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

const ROLE_LABEL_KEYS: Record<string, string> = {
	planner: "shell.workflowUsage.roleLabel.planner",
	worker: "shell.workflowUsage.roleLabel.worker",
	reviewer: "shell.workflowUsage.roleLabel.reviewer",
	fix_worker: "shell.workflowUsage.roleLabel.fix_worker",
	verify: "shell.workflowUsage.roleLabel.verify",
	decision_resolver: "shell.workflowUsage.roleLabel.decision_resolver",
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
