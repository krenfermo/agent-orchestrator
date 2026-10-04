import { useMemo, useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { MessageKey } from "../../i18n/messages";
import { Badge, type BadgeVariant } from "../ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import { useProjectMemoryItems, type MemoryItem } from "../../hooks/useProjectMemoryItems";
import { useProjectMemoryProvenance } from "../../hooks/useProjectMemoryProvenance";

// The Memory tab: the durable facts AO holds, grouped by what KIND of claim
// each one is.
//
// The grouping is the point. "This project uses X convention" (written from a
// repository derivation), "this task discovered Y" (written from an outcome)
// and "authorisation is decided in these three packages" (AO's own reading of
// a directory census) are different kinds of claim with different reliability,
// and a flat list would let the weakest borrow the credibility of the
// strongest.
//
// P4-H adds the two things that make that more than a layout decision:
//
//   - Every row carries its EVIDENCE CLASS. A fact AO copied out of a file and
//     a fact AO concluded from naming look the same in a list, and rendering
//     them identically is how a plausible guess becomes a premise nobody
//     rechecks. Derived rows say so, in the row.
//   - Every row can be EXPANDED to its evidence: the body, every source path,
//     the commit, the promotion authority. That is where a derived fact states
//     how it was determined — including, for the auth model, that AO has not
//     verified it. A label a reader cannot check is not much better than no
//     label.

const STATE_VARIANT: Record<string, BadgeVariant> = {
	valid: "success",
	stale: "warning",
	invalidated: "error",
	rebuilding: "accent",
};

// An inference is the one class that gets a colour. The others are neutral
// because they are the ordinary case; `derived` is the one a reader has to
// weigh differently, so it is the one that has to catch the eye.
const EVIDENCE_VARIANT: Record<string, BadgeVariant> = {
	derived: "warning",
	observed: "outline",
	user_provided: "accent",
	workflow_verified: "success",
};

const EVIDENCE_LABEL: Record<string, MessageKey> = {
	derived: "intelligence.memory.evidence.derived",
	observed: "intelligence.memory.evidence.observed",
	user_provided: "intelligence.memory.evidence.userProvided",
	workflow_verified: "intelligence.memory.evidence.workflowVerified",
};

type Group = { id: string; titleKey: MessageKey; hintKey: MessageKey; test: (item: MemoryItem) => boolean };

// The P4-H high-level types, which are the ones somebody opens this tab to
// read. Kept as a set rather than a prefix test because the vocabulary is open
// and a type this build does not know must fall through to a group rather than
// be mistaken for one of these.
const HIGH_LEVEL = new Set([
	"architecture",
	"entry_point",
	"runtime_surface",
	"persistence",
	"auth_model",
	"integration",
	"testing_surface",
	"config_surface",
	"deployment",
	"project_overview",
]);

// Ordered most- to least-durable, which is also the order somebody reading
// this page would want them in.
const GROUPS: Group[] = [
	{
		id: "shape",
		titleKey: "intelligence.memory.shape",
		hintKey: "intelligence.memory.shapeHint",
		test: (i) => HIGH_LEVEL.has(i.type),
	},
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
		test: (i) => i.scope === "module" || i.type === "module",
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

// The expanded evidence for one fact, fetched only when it is opened.
function MemoryEvidence({ projectId, item }: { projectId: string; item: MemoryItem }) {
	const { t } = useTranslation();
	const { provenance, isLoading, error } = useProjectMemoryProvenance(projectId, item.id);

	if (isLoading) {
		return <div className="mt-2 text-xs text-muted-foreground">{t("intelligence.loading")}</div>;
	}
	if (error) {
		return <div className="mt-2 text-xs text-destructive">{error}</div>;
	}
	const body = provenance?.item?.content ?? "";
	const paths = provenance?.item?.sourcePaths ?? item.sourcePaths ?? [];
	const metadata = Object.entries(provenance?.item?.metadata ?? item.metadata ?? {});

	return (
		<div className="mt-2 space-y-2 rounded-md border border-border/60 bg-muted/30 p-2.5" data-testid="memory-evidence">
			{body ? (
				<pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words text-xs leading-relaxed">
					{body}
				</pre>
			) : null}
			{paths.length > 0 ? (
				<div>
					<div className="text-xs font-medium">{t("intelligence.memory.derivedFrom")}</div>
					<ul className="mt-0.5 space-y-0.5">
						{paths.map((path) => (
							<li key={path} className="break-all font-mono text-xs text-muted-foreground">
								{path}
							</li>
						))}
					</ul>
				</div>
			) : null}
			{item.stateReason ? (
				<div className="text-xs text-warning">{item.stateReason}</div>
			) : null}
			{item.authorityReason ? (
				<div className="text-xs text-warning">{item.authorityReason}</div>
			) : null}
			{metadata.length > 0 ? (
				<div className="flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
					{metadata.map(([key, value]) => (
						<span key={key} className="font-mono">
							{key}={value}
						</span>
					))}
				</div>
			) : null}
			{provenance?.relations?.length ? (
				<div>
					<div className="text-xs font-medium">{t("intelligence.memory.relations")}</div>
					<ul className="mt-0.5 space-y-0.5">
						{provenance.relations.map((rel) => (
							<li key={rel.id} className="break-all font-mono text-xs text-muted-foreground">
								{rel.fromKey} → {rel.kind} → {rel.toKey}
							</li>
						))}
					</ul>
				</div>
			) : null}
		</div>
	);
}

function MemoryRow({ projectId, item }: { projectId: string; item: MemoryItem }) {
	const { t } = useTranslation();
	const [open, setOpen] = useState(false);
	const evidenceKey = item.evidenceClass ? EVIDENCE_LABEL[item.evidenceClass] : undefined;

	return (
		<li className="border-b border-border/60 py-2 last:border-0" data-testid="memory-item">
			<div className="flex flex-wrap items-baseline gap-2">
				<button
					type="button"
					aria-expanded={open}
					className="flex min-w-0 flex-1 cursor-pointer items-baseline gap-1.5 text-left"
					onClick={() => setOpen((was) => !was)}
				>
					{open ? (
						<ChevronDown aria-hidden="true" className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
					) : (
						<ChevronRight aria-hidden="true" className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
					)}
					<span className="min-w-0 text-sm">{item.summary}</span>
				</button>
				<Badge variant={STATE_VARIANT[item.state] ?? "neutral"}>{item.state}</Badge>
				{evidenceKey ? (
					<Badge
						variant={EVIDENCE_VARIANT[item.evidenceClass ?? ""] ?? "outline"}
						data-testid="memory-evidence-class"
					>
						{t(evidenceKey)}
					</Badge>
				) : null}
				{item.type ? <Badge variant="outline">{item.type}</Badge> : null}
			</div>
			<div className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 pl-5 text-xs text-muted-foreground">
				{item.sourcePaths?.length ? <span>{item.sourcePaths.slice(0, 3).join(", ")}</span> : null}
				{item.sourceCommit ? (
					<span>{t("intelligence.graph.atCommit", { commit: item.sourceCommit.slice(0, 12) })}</span>
				) : null}
				{item.provenanceKind ? (
					<span>{t("intelligence.memory.via", { kind: item.provenanceKind.replace(/_/g, " ") })}</span>
				) : null}
				<span>{t("intelligence.memory.confidence", { percent: Math.round(item.confidence * 100) })}</span>
				{item.updatedAt ? <span>{new Date(item.updatedAt).toLocaleString()}</span> : null}
				{!item.servable ? <span className="text-warning">{t("intelligence.memory.withheld")}</span> : null}
			</div>
			{open ? (
				<div className="pl-5">
					<MemoryEvidence projectId={projectId} item={item} />
				</div>
			) : null}
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
								{t(group.titleKey)} <span className="text-muted-foreground">({rows.length})</span>
							</CardTitle>
							<div className="text-xs text-muted-foreground">{t(group.hintKey)}</div>
						</CardHeader>
						<CardContent>
							<ul>
								{rows.map((item) => (
									<MemoryRow key={item.id} projectId={projectId} item={item} />
								))}
							</ul>
						</CardContent>
					</Card>
				);
			})}
		</div>
	);
}
