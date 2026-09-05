import { useMemo, useRef, useState, type PointerEvent as ReactPointerEvent } from "react";
import { Search, ZoomIn, ZoomOut, Maximize2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import {
	useIntelligenceSubgraph,
	type IntelligenceSubgraphEdge,
	type IntelligenceSubgraphNode,
} from "../../hooks/useProjectIntelligence";

// The Graph tab.
//
// THE RULE THIS COMPONENT EXISTS TO OBEY: never render the whole graph. Real
// projects have tens of thousands of symbols and hundreds of thousands of
// relations, and a view that tried to draw them would hang the renderer before
// it taught anybody anything. So the view is always a bounded neighbourhood of
// one seed, the daemon enforces the ceilings, and when a walk hits one the
// truncation is SHOWN rather than hidden — a graph that silently drops half its
// edges teaches something false about the codebase.
//
// The layout is concentric by depth: the seed in the middle, one ring per hop.
// It is deliberately not a force simulation. A force layout on a bounded
// neighbourhood spends animation frames to arrive at an arrangement that is
// no more informative than "how far is this from what I asked about", and the
// ring encodes exactly that, deterministically, with no settling time.

const WIDTH = 900;
const HEIGHT = 560;
const RING = 150;

type Placed = IntelligenceSubgraphNode & { x: number; y: number };

function layout(nodes: IntelligenceSubgraphNode[]): Placed[] {
	const byDepth = new Map<number, IntelligenceSubgraphNode[]>();
	for (const node of nodes) {
		const ring = byDepth.get(node.depth) ?? [];
		ring.push(node);
		byDepth.set(node.depth, ring);
	}
	const placed: Placed[] = [];
	const cx = WIDTH / 2;
	const cy = HEIGHT / 2;
	for (const [depth, ring] of [...byDepth.entries()].sort((a, b) => a[0] - b[0])) {
		if (depth === 0 && ring.length === 1) {
			placed.push({ ...ring[0], x: cx, y: cy });
			continue;
		}
		const radius = RING * (depth === 0 ? 0.55 : depth);
		ring.forEach((node, i) => {
			const angle = (2 * Math.PI * i) / ring.length - Math.PI / 2;
			placed.push({ ...node, x: cx + radius * Math.cos(angle), y: cy + radius * Math.sin(angle) });
		});
	}
	return placed;
}

const KIND_FILL: Record<string, string> = {
	func: "var(--color-accent)",
	method: "var(--color-accent)",
	type: "var(--color-primary)",
	struct: "var(--color-primary)",
	interface: "var(--color-primary)",
};

function fillFor(kind: string) {
	return KIND_FILL[kind.toLowerCase()] ?? "var(--color-muted-foreground)";
}

export function IntelligenceGraph({
	projectId,
	repoPath,
	seed,
	onSeedChange,
}: {
	projectId: string;
	repoPath?: string;
	seed: string;
	onSeedChange: (next: string) => void;
}) {
	const { t } = useTranslation();
	const [draft, setDraft] = useState(seed);
	const [depth, setDepth] = useState(1);
	const [nodeKind, setNodeKind] = useState("");
	const [edgeKind, setEdgeKind] = useState("");
	const [selected, setSelected] = useState<string | null>(null);
	const [zoom, setZoom] = useState(1);
	const [pan, setPan] = useState({ x: 0, y: 0 });
	const dragging = useRef<{ x: number; y: number } | null>(null);

	const { subgraph, seeded, isLoading, error } = useIntelligenceSubgraph(projectId, {
		symbol: seed || undefined,
		depth,
		nodeKinds: nodeKind || undefined,
		edgeKinds: edgeKind || undefined,
		repoPath,
	});

	const placed = useMemo(() => layout(subgraph?.nodes ?? []), [subgraph?.nodes]);
	const byKey = useMemo(() => new Map(placed.map((node) => [node.key, node])), [placed]);
	const nodeKinds = useMemo(
		() => [...new Set((subgraph?.nodes ?? []).map((n) => n.kind).filter(Boolean))].sort(),
		[subgraph?.nodes],
	);
	const edgeKinds = useMemo(
		() => [...new Set((subgraph?.edges ?? []).map((e) => e.kind).filter(Boolean))].sort(),
		[subgraph?.edges],
	);

	const selectedNode = selected ? byKey.get(selected) : undefined;
	const relations = useMemo(() => {
		if (!selected || !subgraph) return { incoming: [], outgoing: [] };
		const incoming: IntelligenceSubgraphEdge[] = [];
		const outgoing: IntelligenceSubgraphEdge[] = [];
		for (const edge of subgraph.edges) {
			if (edge.to === selected) incoming.push(edge);
			if (edge.from === selected) outgoing.push(edge);
		}
		return { incoming, outgoing };
	}, [selected, subgraph]);

	const onPointerDown = (e: ReactPointerEvent<SVGSVGElement>) => {
		dragging.current = { x: e.clientX - pan.x, y: e.clientY - pan.y };
		e.currentTarget.setPointerCapture(e.pointerId);
	};
	const onPointerMove = (e: ReactPointerEvent<SVGSVGElement>) => {
		if (!dragging.current) return;
		setPan({ x: e.clientX - dragging.current.x, y: e.clientY - dragging.current.y });
	};
	const onPointerUp = () => {
		dragging.current = null;
	};

	return (
		<div className="space-y-3 p-1">
			<form
				className="flex flex-wrap items-center gap-2"
				onSubmit={(e) => {
					e.preventDefault();
					onSeedChange(draft.trim());
				}}
			>
				<div className="relative min-w-56 flex-1">
					<Search aria-hidden="true" className="absolute left-2 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
					<Input
						aria-label={t("intelligence.graph.symbolLabel")}
						className="pl-8"
						placeholder={t("intelligence.graph.symbolPlaceholder")}
						value={draft}
						onChange={(e) => setDraft(e.target.value)}
					/>
				</div>
				<Button size="sm" type="submit">
					{t("intelligence.graph.explore")}
				</Button>
				<label className="flex items-center gap-1.5 text-xs text-muted-foreground">
					{t("intelligence.graph.depth")}
					<select
						aria-label={t("intelligence.graph.depthLabel")}
						className="h-control-md rounded-md border border-border bg-background px-1.5 text-xs"
						value={depth}
						onChange={(e) => setDepth(Number(e.target.value))}
					>
						<option value={1}>{t("intelligence.graph.oneHop")}</option>
						<option value={2}>{t("intelligence.graph.twoHops")}</option>
					</select>
				</label>
				{nodeKinds.length > 1 ? (
					<label className="flex items-center gap-1.5 text-xs text-muted-foreground">
						{t("intelligence.graph.nodes")}
						<select
							aria-label={t("intelligence.graph.nodeKindLabel")}
							className="h-control-md rounded-md border border-border bg-background px-1.5 text-xs"
							value={nodeKind}
							onChange={(e) => setNodeKind(e.target.value)}
						>
							<option value="">{t("intelligence.graph.allKinds")}</option>
							{nodeKinds.map((kind) => (
								<option key={kind} value={kind}>
									{kind}
								</option>
							))}
						</select>
					</label>
				) : null}
				{edgeKinds.length > 1 ? (
					<label className="flex items-center gap-1.5 text-xs text-muted-foreground">
						{t("intelligence.graph.relations")}
						<select
							aria-label={t("intelligence.graph.edgeKindLabel")}
							className="h-control-md rounded-md border border-border bg-background px-1.5 text-xs"
							value={edgeKind}
							onChange={(e) => setEdgeKind(e.target.value)}
						>
							<option value="">{t("intelligence.graph.allRelations")}</option>
							{edgeKinds.map((kind) => (
								<option key={kind} value={kind}>
									{kind}
								</option>
							))}
						</select>
					</label>
				) : null}
				<div className="ml-auto flex items-center gap-1">
					<Button size="sm" variant="ghost" type="button" aria-label={t("intelligence.graph.zoomOut")} onClick={() => setZoom((z) => Math.max(0.4, z - 0.2))}>
						<ZoomOut aria-hidden="true" />
					</Button>
					<Button size="sm" variant="ghost" type="button" aria-label={t("intelligence.graph.zoomIn")} onClick={() => setZoom((z) => Math.min(2.5, z + 0.2))}>
						<ZoomIn aria-hidden="true" />
					</Button>
					<Button
						size="sm"
						variant="ghost"
						type="button"
						aria-label={t("intelligence.graph.resetView")}
						onClick={() => {
							setZoom(1);
							setPan({ x: 0, y: 0 });
						}}
					>
						<Maximize2 aria-hidden="true" />
					</Button>
				</div>
			</form>

			{!seeded ? (
				<div className="rounded-lg border border-border p-6 text-sm text-muted-foreground" data-testid="graph-needs-seed">
					{t("intelligence.graph.needsSeed")}
				</div>
			) : error ? (
				<div className="p-4 text-sm text-destructive">{error}</div>
			) : isLoading ? (
				<div className="p-4 text-sm text-muted-foreground">{t("intelligence.loading")}</div>
			) : !subgraph || subgraph.nodes.length === 0 ? (
				<div className="rounded-lg border border-border p-6 text-sm text-muted-foreground" data-testid="graph-empty">
					{t("intelligence.graph.noMatch", { seed })}
				</div>
			) : (
				<>
					<div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
						<span data-testid="graph-counts">
							{t("intelligence.graph.counts", {
								nodes: subgraph.nodes.length,
								edges: subgraph.edges.length,
							})}
						</span>
						{subgraph.truncated ? (
							<Badge variant="warning" data-testid="graph-truncated">
								{t("intelligence.graph.truncated")}
							</Badge>
						) : null}
						{subgraph.indexedCommit ? (
							<span>{t("intelligence.graph.atCommit", { commit: subgraph.indexedCommit.slice(0, 12) })}</span>
						) : null}
					</div>

					<div className="overflow-hidden rounded-lg border border-border bg-background">
						<svg
							role="img"
							aria-label={t("intelligence.graph.canvasLabel")}
							viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
							className="h-[560px] w-full cursor-grab touch-none active:cursor-grabbing"
							onPointerDown={onPointerDown}
							onPointerMove={onPointerMove}
							onPointerUp={onPointerUp}
							onPointerLeave={onPointerUp}
						>
							<g transform={`translate(${pan.x} ${pan.y}) scale(${zoom})`}>
								{subgraph.edges.map((edge, i) => {
									const from = byKey.get(edge.from);
									const to = byKey.get(edge.to);
									if (!from || !to) return null;
									return (
										<line
											key={`${edge.kind}-${edge.from}-${edge.to}-${i}`}
											x1={from.x}
											y1={from.y}
											x2={to.x}
											y2={to.y}
											stroke="var(--color-border)"
											strokeWidth={selected && (edge.from === selected || edge.to === selected) ? 2 : 1}
											opacity={selected && edge.from !== selected && edge.to !== selected ? 0.25 : 0.9}
										/>
									);
								})}
								{placed.map((node) => (
									<g
										key={node.key}
										transform={`translate(${node.x} ${node.y})`}
										className="cursor-pointer"
										onClick={(e) => {
											e.stopPropagation();
											setSelected(node.key);
										}}
										onDoubleClick={(e) => {
											e.stopPropagation();
											// Expanding a neighbour re-seeds the walk on it, which
											// keeps the view bounded rather than accumulating.
											onSeedChange(node.name);
											setDraft(node.name);
										}}
									>
										<circle
											r={node.depth === 0 ? 9 : 6}
											fill={fillFor(node.kind ?? "")}
											stroke={selected === node.key ? "var(--color-foreground)" : "transparent"}
											strokeWidth={2}
										/>
										<text
											x={11}
											y={4}
											className="fill-foreground text-[11px]"
											opacity={selected && selected !== node.key ? 0.5 : 1}
										>
											{node.name}
										</text>
									</g>
								))}
							</g>
						</svg>
					</div>

					{selectedNode ? (
						<div className="rounded-lg border border-border p-3 text-sm" data-testid="graph-selection">
							<div className="flex flex-wrap items-baseline gap-2">
								<span className="font-medium">{selectedNode.name}</span>
								{selectedNode.kind ? <Badge variant="outline">{selectedNode.kind}</Badge> : null}
								<span className="text-xs text-muted-foreground">
									{selectedNode.path}
									{selectedNode.line ? `:${selectedNode.line}` : ""}
								</span>
							</div>
							{selectedNode.signature ? (
								<pre className="mt-2 overflow-x-auto text-xs text-muted-foreground">{selectedNode.signature}</pre>
							) : null}
							<div className="mt-2 grid gap-2 sm:grid-cols-2">
								<div>
									<div className="text-xs font-medium">
										{t("intelligence.graph.reachedBy", { count: relations.incoming.length })}
									</div>
									<ul className="mt-1 space-y-0.5 text-xs text-muted-foreground">
										{relations.incoming.slice(0, 8).map((edge, i) => (
											<li key={i}>
												{edge.kind} ← {byKey.get(edge.from)?.name ?? edge.from}
											</li>
										))}
										{relations.incoming.length === 0 ? <li>{t("intelligence.graph.nothingInView")}</li> : null}
									</ul>
								</div>
								<div>
									<div className="text-xs font-medium">
										{t("intelligence.graph.reaches", { count: relations.outgoing.length })}
									</div>
									<ul className="mt-1 space-y-0.5 text-xs text-muted-foreground">
										{relations.outgoing.slice(0, 8).map((edge, i) => (
											<li key={i}>
												{edge.kind} → {byKey.get(edge.to)?.name ?? edge.to}
											</li>
										))}
										{relations.outgoing.length === 0 ? <li>{t("intelligence.graph.nothingInView")}</li> : null}
									</ul>
								</div>
							</div>
							<div className="mt-2 text-xs text-muted-foreground">
								{t("intelligence.graph.doubleClickHint")}
							</div>
						</div>
					) : null}
				</>
			)}
		</div>
	);
}
