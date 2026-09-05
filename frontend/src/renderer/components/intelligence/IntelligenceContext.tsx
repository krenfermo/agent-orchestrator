import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Badge } from "../ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import { Tabs, TabsList, TabsTrigger } from "../ui/tabs";
import { useIntelligenceContext, type ContextRole } from "../../hooks/useProjectIntelligence";
import type { MessageKey } from "../../i18n/messages";

// The Context tab: what AO would actually hand an agent.
//
// This is INPUT observability. Everything shown is AO's own construction from
// AO's own durable rows — the facts it selected and the graph evidence it
// retrieved. Nothing a model produced appears here, which is precisely why it
// is safe to show.
//
// The measurement vocabulary is "selected" and "avoided", never "saved". AO
// cannot see what the coding harness reads inside the worktree, so it cannot
// know what its context prevented anybody from reading; a savings number would
// be an invented baseline. The copy below says so in as many words, because a
// number without its caveat gets quoted without it.

const ROLES: { id: ContextRole; label: MessageKey }[] = [
	{ id: "planner", label: "intelligence.role.planner" },
	{ id: "worker", label: "intelligence.role.worker" },
	{ id: "reviewer", label: "intelligence.role.reviewer" },
	{ id: "repair", label: "intelligence.role.repair" },
];

function bytes(n: number) {
	if (n < 1024) return `${n} B`;
	if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
	return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

function Metric({ label, value, hint }: { label: string; value: string; hint?: string }) {
	return (
		<div>
			<div className="text-xs text-muted-foreground">{label}</div>
			<div className="font-medium tabular-nums">{value}</div>
			{hint ? <div className="text-[11px] text-muted-foreground">{hint}</div> : null}
		</div>
	);
}

export function IntelligenceContext({ projectId }: { projectId: string }) {
	const { t } = useTranslation();
	const [role, setRole] = useState<ContextRole>("planner");
	const { preview, isLoading, error } = useIntelligenceContext(projectId, role);

	return (
		<div className="space-y-3 p-1">
			<Tabs value={role} onValueChange={(v) => setRole(v as ContextRole)}>
				<TabsList>
					{ROLES.map((r) => (
						<TabsTrigger key={r.id} value={r.id}>
							{t(r.label)}
						</TabsTrigger>
					))}
				</TabsList>
			</Tabs>

			{error ? (
				<div className="p-4 text-sm text-destructive">{error}</div>
			) : isLoading ? (
				<div className="p-4 text-sm text-muted-foreground">{t("intelligence.context.assembling")}</div>
			) : !preview ? null : preview.empty ? (
				<div className="rounded-lg border border-border p-6 text-sm text-muted-foreground" data-testid="context-empty">
					{t("intelligence.context.empty")}
					{preview.fallbackReason
						? t("intelligence.context.emptyReason", { reason: preview.fallbackReason })
						: ""}
				</div>
			) : (
				<>
					<Card data-testid="context-metrics">
						<CardHeader className="space-y-0.5">
							<CardTitle className="text-sm">{t("intelligence.context.weighs")}</CardTitle>
							<div className="text-xs text-muted-foreground">
								{t("intelligence.context.weighsHint")}
							</div>
						</CardHeader>
						<CardContent className="grid grid-cols-2 gap-3 sm:grid-cols-4">
							<Metric
								label={t("intelligence.context.factsSelected")}
								value={t("intelligence.context.ofCandidates", {
									selected: preview.selectedItems,
									candidates: preview.candidateItems,
								})}
							/>
							<Metric label={t("intelligence.context.contextSelected")} value={bytes(preview.selectedBytes)} />
							<Metric
								label={t("intelligence.context.estimatedTokens")}
								value={String(preview.estimatedTokens)}
								hint={t("intelligence.context.anEstimate")}
							/>
							<Metric
								label={t("intelligence.context.contextAvoided")}
								value={t("intelligence.context.factsCount", { count: preview.droppedItems })}
								hint={t("intelligence.context.summaryOnlyCount", { count: preview.droppedToSummary })}
							/>
							<Metric label={t("intelligence.context.withheld")} value={String(preview.staleExcluded)} />
							<Metric
								label={t("intelligence.context.graphSymbols")}
								value={t("intelligence.context.ofCandidates", {
									selected: preview.graph.selectedSymbols,
									candidates: preview.graph.consideredSymbols,
								})}
							/>
							<Metric
								label={t("intelligence.context.graphRelations")}
								value={t("intelligence.context.ofCandidates", {
									selected: preview.graph.selectedEdges,
									candidates: preview.graph.consideredEdges,
								})}
							/>
							<Metric
								label={t("intelligence.context.graphContext")}
								value={bytes(preview.graph.bytes)}
								hint={t("intelligence.context.approxTokens", { count: preview.graph.estimatedTokens })}
							/>
						</CardContent>
					</Card>

					{preview.sourcesReused?.length ? (
						<Card>
							<CardHeader className="space-y-0.5">
								<CardTitle className="text-sm">{t("intelligence.context.reusedTitle")}</CardTitle>
								<div className="text-xs text-muted-foreground">
									{t("intelligence.context.reusedHint")}
								</div>
							</CardHeader>
							<CardContent className="flex flex-wrap gap-1.5">
								{preview.sourcesReused.map((path) => (
									<Badge key={path} variant="outline">
										{path}
									</Badge>
								))}
							</CardContent>
						</Card>
					) : null}

					{preview.sections.map((section) => (
						<Card key={section.title} data-testid="context-section">
							<CardHeader>
								<CardTitle className="text-sm">
									{t("intelligence.context.sectionCount", {
										title: section.title,
										count: section.items.length,
									})}
								</CardTitle>
							</CardHeader>
							<CardContent>
								<ul className="space-y-2">
									{section.items.map((item, i) => (
										<li key={i} className="border-b border-border/60 pb-2 last:border-0 last:pb-0">
											<div className="flex flex-wrap items-baseline gap-2">
												<span className="min-w-0 flex-1 text-sm">{item.summary}</span>
												{!item.bodyIncluded ? (
													<Badge variant="neutral">{t("intelligence.context.summaryOnly")}</Badge>
												) : null}
												{item.state && item.state !== "valid" ? (
													<Badge variant="warning">{item.state}</Badge>
												) : null}
											</div>
											<div className="mt-0.5 flex flex-wrap gap-x-3 text-xs text-muted-foreground">
												{item.reason ? (
													<span>{t("intelligence.context.selectedBecause", { reason: item.reason })}</span>
												) : null}
												{item.sourcePaths?.length ? <span>{item.sourcePaths.slice(0, 2).join(", ")}</span> : null}
											</div>
										</li>
									))}
								</ul>
							</CardContent>
						</Card>
					))}

					{preview.graph.architecture ? (
						<Card>
							<CardHeader>
								<CardTitle className="text-sm">{t("intelligence.context.structureGiven")}</CardTitle>
							</CardHeader>
							<CardContent>
								<pre className="overflow-x-auto whitespace-pre-wrap text-xs text-muted-foreground">
									{preview.graph.architecture}
								</pre>
							</CardContent>
						</Card>
					) : null}
				</>
			)}
		</div>
	);
}
