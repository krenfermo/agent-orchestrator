import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../ui/tabs";
import { IntelligenceArchitecture } from "./IntelligenceArchitecture";
import { IntelligenceContext } from "./IntelligenceContext";
import { IntelligenceGraph } from "./IntelligenceGraph";
import { IntelligenceMemory } from "./IntelligenceMemory";
import { IntelligenceOverview } from "./IntelligenceOverview";
import { IntelligenceSearch } from "./IntelligenceSearch";
import { IntelligenceStateBadge } from "./IntelligenceStateBadge";
import { useIntelligenceOverview } from "../../hooks/useProjectIntelligence";
import type { MessageKey } from "../../i18n/messages";

// Project → Intelligence.
//
// The tabs are ordered the way somebody actually approaches a codebase they do
// not know: what does AO know about this at all (Overview), what shape is it
// (Architecture), how does this bit connect (Graph), what has AO written down
// (Memory), where is the thing I am looking for (Search), and what would AO
// tell an agent about it (Context).
//
// Seeding is shared: clicking a module in Architecture or a symbol in Search
// moves you to the Graph tab already looking at it, because "what is this" and
// "what does it touch" is one question asked twice.

const TABS: { id: string; label: MessageKey }[] = [
	{ id: "overview", label: "intelligence.tab.overview" },
	{ id: "architecture", label: "intelligence.tab.architecture" },
	{ id: "graph", label: "intelligence.tab.graph" },
	{ id: "memory", label: "intelligence.tab.memory" },
	{ id: "search", label: "intelligence.tab.search" },
	{ id: "context", label: "intelligence.tab.context" },
];

export function ProjectIntelligenceView({ projectId }: { projectId: string }) {
	const { t } = useTranslation();
	const [tab, setTab] = useState<string>("overview");
	const [seed, setSeed] = useState("");
	const { repos } = useIntelligenceOverview(projectId);

	// The project's headline state is the worst of its repositories': one stale
	// repository makes the project's picture stale, and rolling that up to
	// "ready" would be the reassuring answer rather than the true one.
	const worst =
		repos.find((r) => r.state === "failed") ??
		repos.find((r) => r.state === "stale") ??
		repos.find((r) => r.state === "indexing") ??
		repos.find((r) => r.state === "pending") ??
		repos[0];

	const inspect = (term: string) => {
		setSeed(term);
		setTab("graph");
	};

	return (
		<div className="flex h-full min-h-0 flex-col gap-3 p-4" data-testid="project-intelligence">
			<div className="flex flex-wrap items-center gap-2">
				<h1 className="text-base font-medium">{t("intelligence.title")}</h1>
				{worst ? (
					<span data-testid="intelligence-headline-state">
						<IntelligenceStateBadge state={worst.state} />
					</span>
				) : null}
				<span className="text-xs text-muted-foreground">{t("intelligence.subtitle")}</span>
			</div>

			<Tabs value={tab} onValueChange={setTab} className="flex min-h-0 flex-1 flex-col gap-3">
				<TabsList>
					{TABS.map((tab) => (
						<TabsTrigger key={tab.id} value={tab.id}>
							{t(tab.label)}
						</TabsTrigger>
					))}
				</TabsList>

				<div className="min-h-0 flex-1 overflow-y-auto">
					<TabsContent value="overview">
						<IntelligenceOverview projectId={projectId} />
					</TabsContent>
					<TabsContent value="architecture">
						<IntelligenceArchitecture projectId={projectId} onInspect={inspect} />
					</TabsContent>
					<TabsContent value="graph">
						<IntelligenceGraph projectId={projectId} seed={seed} onSeedChange={setSeed} />
					</TabsContent>
					<TabsContent value="memory">
						<IntelligenceMemory projectId={projectId} />
					</TabsContent>
					<TabsContent value="search">
						<IntelligenceSearch projectId={projectId} onInspect={inspect} />
					</TabsContent>
					<TabsContent value="context">
						<IntelligenceContext projectId={projectId} />
					</TabsContent>
				</div>
			</Tabs>
		</div>
	);
}
