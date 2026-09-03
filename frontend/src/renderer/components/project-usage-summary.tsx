import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useProjectUsage, type ProjectUsagePeriod } from "../hooks/useProjectUsage";

/**
 * project-usage-summary.tsx — P3-E §13: what this project has spent.
 *
 * Kept to one line plus a period switch. It is a summary, not a billing
 * console: the per-run detail lives on the run page and the enforcement lives
 * in the workflow policy, so nothing here needs a chart.
 *
 * It obeys the same honesty rules as everything else in this checkpoint. An
 * estimated total is marked with "~". A cost with no rate card is not printed
 * as $0.00 — it is not printed at all. And a period with no recorded usage says
 * so in words rather than rendering a row of zeroes.
 */
const PERIODS: ProjectUsagePeriod[] = ["today", "7d", "30d"];

const PERIOD_LABELS: Record<ProjectUsagePeriod, string> = {
	today: "board.usagePeriodToday",
	"7d": "board.usagePeriod7d",
	"30d": "board.usagePeriod30d",
	all: "board.usagePeriodAll",
};

function compact(n: number): string {
	if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
	if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
	return String(n);
}

export function ProjectUsageSummary({ projectId }: { projectId: string }) {
	const { t } = useTranslation();
	const [period, setPeriod] = useState<ProjectUsagePeriod>("7d");
	const { usage } = useProjectUsage(projectId, period);

	if (!usage) return null;

	return (
		<div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground" data-testid="project-usage-summary">
			<div className="flex gap-1">
				{PERIODS.map((option) => (
					<button
						className={`rounded px-1.5 py-0.5 ${option === period ? "bg-muted text-foreground" : ""}`}
						key={option}
						onClick={() => setPeriod(option)}
						type="button"
					>
						{t(PERIOD_LABELS[option] as "board.usagePeriod7d")}
					</button>
				))}
			</div>
			{usage.recorded ? (
				<span data-testid="project-usage-total">
					{t("board.usageProjectTotal", {
						tokens: usage.source === "provider_reported" ? compact(usage.totals.total) : `~${compact(usage.totals.total)}`,
						workflows: usage.workflows,
					})}
					{usage.cost.known ? ` · ${usage.cost.currency || "USD"} ${usage.cost.amount.toFixed(2)}` : ""}
				</span>
			) : (
				<span data-testid="project-usage-empty">{t("board.usageProjectEmpty")}</span>
			)}
		</div>
	);
}
