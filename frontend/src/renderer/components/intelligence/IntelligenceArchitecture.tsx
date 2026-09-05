import { useTranslation } from "react-i18next";
import type { MessageKey } from "../../i18n/messages";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import { Badge } from "../ui/badge";
import { useIntelligenceArchitecture } from "../../hooks/useProjectIntelligence";

// The Architecture tab.
//
// Every fact rendered here came out of the indexed graph. Nothing is inferred,
// summarised by a model, or filled in when the data is thin: a project whose
// graph names no services shows no services section, rather than a plausible
// one. That is the difference between a structural summary and a guess, and it
// is the whole reason this tab is trustworthy enough to act on.

type Group = { id: string; titleKey: MessageKey; hintKey: MessageKey; values: string[] };

// The shape the code graph's own Architecture struct serialises to. Read
// defensively: this is the graph's vocabulary, not the renderer's, and a
// backend that adds a category should not break the tab.
function groupsFrom(architecture: Record<string, unknown> | undefined): Group[] {
	if (!architecture) return [];
	const pick = (key: string): string[] => {
		const raw = architecture[key];
		if (Array.isArray(raw)) {
			return raw
				.map((entry) =>
					typeof entry === "string"
						? entry
						: entry && typeof entry === "object" && "name" in entry
							? String((entry as { name: unknown }).name)
							: "",
				)
				.filter(Boolean);
		}
		return [];
	};
	const candidates: Group[] = [
		{ id: "modules", titleKey: "intelligence.architecture.modules", hintKey: "intelligence.architecture.modulesHint", values: pick("modules") },
		{ id: "entryPoints", titleKey: "intelligence.architecture.entryPoints", hintKey: "intelligence.architecture.entryPointsHint", values: pick("entryPoints") },
		{ id: "services", titleKey: "intelligence.architecture.services", hintKey: "intelligence.architecture.servicesHint", values: pick("services") },
		{ id: "controllers", titleKey: "intelligence.architecture.controllers", hintKey: "intelligence.architecture.controllersHint", values: pick("controllers") },
		{ id: "storage", titleKey: "intelligence.architecture.storage", hintKey: "intelligence.architecture.storageHint", values: pick("storage") },
		{ id: "endpoints", titleKey: "intelligence.architecture.endpoints", hintKey: "intelligence.architecture.endpointsHint", values: pick("endpoints") },
		{ id: "tables", titleKey: "intelligence.architecture.tables", hintKey: "intelligence.architecture.tablesHint", values: pick("tables") },
	];
	return candidates.filter((group) => group.values.length > 0);
}

export function IntelligenceArchitecture({
	projectId,
	repoPath,
	onInspect,
}: {
	projectId: string;
	repoPath?: string;
	onInspect?: (term: string) => void;
}) {
	const { t } = useTranslation();
	const { architecture, rendered, isLoading, error } = useIntelligenceArchitecture(projectId, repoPath);

	if (isLoading) return <div className="p-4 text-sm text-muted-foreground">{t("intelligence.loading")}</div>;
	if (error) return <div className="p-4 text-sm text-destructive">{error}</div>;

	const groups = groupsFrom(architecture);
	if (groups.length === 0 && !rendered) {
		return (
			<div className="p-4 text-sm text-muted-foreground" data-testid="architecture-empty">
				{t("intelligence.architecture.empty")}
			</div>
		);
	}

	return (
		<div className="space-y-3 p-1" data-testid="architecture-groups">
			{groups.map((group) => (
				<Card key={group.id}>
					<CardHeader className="space-y-0.5">
						<CardTitle className="text-sm">{t(group.titleKey)}</CardTitle>
						<div className="text-xs text-muted-foreground">{t(group.hintKey)}</div>
					</CardHeader>
					<CardContent className="flex flex-wrap gap-1.5">
						{group.values.map((value) => (
							<button
								key={value}
								type="button"
								className="cursor-pointer"
								onClick={() => onInspect?.(value)}
								title={onInspect ? t("intelligence.architecture.inspect", { name: value }) : undefined}
							>
								<Badge variant="outline">{value}</Badge>
							</button>
						))}
					</CardContent>
				</Card>
			))}
			{groups.length === 0 && rendered ? (
				// The structured form is missing but the build did render a text
				// summary. Showing it is strictly better than showing nothing, and
				// it is the same facts a dispatch is given.
				<Card>
					<CardHeader>
						<CardTitle className="text-sm">{t("intelligence.architecture.structure")}</CardTitle>
					</CardHeader>
					<CardContent>
						<pre className="overflow-x-auto whitespace-pre-wrap text-xs text-muted-foreground">
							{rendered}
						</pre>
					</CardContent>
				</Card>
			) : null}
		</div>
	);
}
