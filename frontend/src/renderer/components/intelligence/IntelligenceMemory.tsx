import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import type { MessageKey } from "../../i18n/messages";
import { Badge, type BadgeVariant } from "../ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import { useProjectMemoryItems, type MemoryItem } from "../../hooks/useProjectMemoryItems";

// The Memory tab: the durable facts AO holds, grouped by what KIND of claim
// each one is.
//
// The grouping is the point. "This project uses X convention" (written from a
// repository derivation), "this task discovered Y" (written from an outcome)
// and "this module does Z" (written from the graph) are different kinds of
// claim with different reliability, and a flat list would let the weakest
// borrow the credibility of the strongest. Provenance is shown on every row
// for the same reason.

const STATE_VARIANT: Record<string, BadgeVariant> = {
	valid: "success",
	stale: "warning",
	invalidated: "error",
	rebuilding: "accent",
};

type Group = { id: string; titleKey: MessageKey; hintKey: MessageKey; test: (item: MemoryItem) => boolean };

// Ordered most- to least-durable, which is also the order somebody reading
// this page would want them in.
const GROUPS: Group[] = [
	{
		id: "project-facts",
		titleKey: "intelligence.memory.projectFacts",
		hintKey: "intelligence.memory.projectFactsHint",
		test: (i) => i.provenanceKind === "repo_derivation" && i.scope !== "module",
	},
	{
		id: "architecture",
		titleKey: "intelligence.memory.architecture",
		hintKey: "intelligence.memory.architectureHint",
		test: (i) => i.scope === "module" || i.type === "module" || i.type === "architecture",
	},
	{
		id: "graph",
		titleKey: "intelligence.memory.graph",
		hintKey: "intelligence.memory.graphHint",
		test: (i) => i.scope === "symbol" || i.scope === "file",
	},
	{
		id: "workflow",
		titleKey: "intelligence.memory.workflow",
		hintKey: "intelligence.memory.workflowHint",
		test: (i) => i.provenanceKind === "task_outcome" || i.provenanceKind === "workflow_knowledge",
	},
];

function groupOf(item: MemoryItem): string {
	for (const group of GROUPS) {
		if (group.test(item)) return group.id;
	}
	return "project-facts";
}

function MemoryRow({ item }: { item: MemoryItem }) {
	const { t } = useTranslation();
	return (
		<li className="border-b border-border/60 py-2 last:border-0" data-testid="memory-item">
			<div className="flex flex-wrap items-baseline gap-2">
				<span className="min-w-0 flex-1 text-sm">{item.summary}</span>
				<Badge variant={STATE_VARIANT[item.state] ?? "neutral"}>{item.state}</Badge>
				{item.type ? <Badge variant="outline">{item.type}</Badge> : null}
			</div>
			<div className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
				{item.sourcePaths?.length ? <span>{item.sourcePaths.slice(0, 3).join(", ")}</span> : null}
				{item.sourceCommit ? (
					<span>{t("intelligence.graph.atCommit", { commit: item.sourceCommit.slice(0, 12) })}</span>
				) : null}
				{item.provenanceKind ? (
					<span>{t("intelligence.memory.via", { kind: item.provenanceKind.replace(/_/g, " ") })}</span>
				) : null}
				<span>{t("intelligence.memory.confidence", { percent: Math.round(item.confidence * 100) })}</span>
				{item.updatedAt ? <span>{new Date(item.updatedAt).toLocaleString()}</span> : null}
				{!item.servable ? (
					<span className="text-warning">{t("intelligence.memory.withheld")}</span>
				) : null}
			</div>
		</li>
	);
}

export function IntelligenceMemory({ projectId, repoPath }: { projectId: string; repoPath?: string }) {
	const { t } = useTranslation();
	const { items, isLoading, error } = useProjectMemoryItems(projectId, repoPath);
	const [query, setQuery] = useState("");

	const filtered = useMemo(() => {
		const term = query.trim().toLowerCase();
		if (!term) return items;
		return items.filter(
			(item) =>
				item.summary.toLowerCase().includes(term) ||
				item.type.toLowerCase().includes(term) ||
				(item.sourcePaths ?? []).some((p) => p.toLowerCase().includes(term)),
		);
	}, [items, query]);

	const grouped = useMemo(() => {
		const out = new Map<string, MemoryItem[]>();
		for (const item of filtered) {
			const id = groupOf(item);
			out.set(id, [...(out.get(id) ?? []), item]);
		}
		return out;
	}, [filtered]);

	if (isLoading) return <div className="p-4 text-sm text-muted-foreground">{t("intelligence.loading")}</div>;
	if (error) return <div className="p-4 text-sm text-destructive">{error}</div>;
	if (items.length === 0) {
		return (
			<div className="p-4 text-sm text-muted-foreground" data-testid="memory-empty">
				{t("intelligence.memory.empty")}
			</div>
		);
	}

	return (
		<div className="space-y-3 p-1">
			<input
				aria-label={t("intelligence.memory.filterLabel")}
				className="h-control-form w-full rounded-md border border-border bg-background px-3 text-sm"
				placeholder={t("intelligence.memory.filterPlaceholder")}
				value={query}
				onChange={(e) => setQuery(e.target.value)}
			/>
			{GROUPS.map((group) => {
				const rows = grouped.get(group.id) ?? [];
				if (rows.length === 0) return null;
				return (
					<Card key={group.id} data-testid={`memory-group-${group.id}`}>
						<CardHeader className="space-y-0.5">
							<CardTitle className="text-sm">
								{t(group.titleKey)}{" "}
								<span className="text-muted-foreground">({rows.length})</span>
							</CardTitle>
							<div className="text-xs text-muted-foreground">{t(group.hintKey)}</div>
						</CardHeader>
						<CardContent>
							<ul>
								{rows.map((item) => (
									<MemoryRow key={item.id} item={item} />
								))}
							</ul>
						</CardContent>
					</Card>
				);
			})}
		</div>
	);
}
