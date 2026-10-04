import { useState } from "react";
import { Search } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { MessageKey } from "../../i18n/messages";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { useIntelligenceSearch } from "../../hooks/useProjectIntelligence";

// The Search tab.
//
// Every row says which authority produced it — a durable fact somebody's work
// wrote, or a symbol the indexer parsed. That label is not decoration: the two
// are different kinds of claim, and a result list that blurred them would let
// a stale note borrow a parser's certainty.
//
// Memory rows carry a second label for the same reason at one level down: a
// fact a verified workflow established and a fact AO inferred from directory
// naming are both "memory", and only one of them is settled.

// EVIDENCE_LABEL maps the wire vocabulary to its message key. The mapping is a
// table of literal keys rather than an interpolated string so the message keys
// stay statically checkable, and so an unrecognised class from a newer daemon
// renders as nothing rather than as a missing translation.
const EVIDENCE_LABEL = {
	derived: "intelligence.memory.evidence.derived",
	observed: "intelligence.memory.evidence.observed",
	user_provided: "intelligence.memory.evidence.userProvided",
	workflow_verified: "intelligence.memory.evidence.workflowVerified",
} as const satisfies Record<string, MessageKey>;

function evidenceLabel(evidenceClass: string | undefined): MessageKey | undefined {
	if (!evidenceClass) return undefined;
	return (EVIDENCE_LABEL as Record<string, MessageKey>)[evidenceClass];
}

export function IntelligenceSearch({
	projectId,
	onInspect,
}: {
	projectId: string;
	onInspect?: (symbol: string) => void;
}) {
	const { t } = useTranslation();
	const [draft, setDraft] = useState("");
	const [term, setTerm] = useState("");
	const { result, isLoading, error } = useIntelligenceSearch(projectId, term);

	return (
		<div className="space-y-3 p-1">
			<form
				className="flex items-center gap-2"
				onSubmit={(e) => {
					e.preventDefault();
					setTerm(draft.trim());
				}}
			>
				<div className="relative flex-1">
					<Search aria-hidden="true" className="absolute left-2 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
					<Input
						aria-label={t("intelligence.search.label")}
						className="pl-8"
						placeholder={t("intelligence.search.placeholder")}
						value={draft}
						onChange={(e) => setDraft(e.target.value)}
					/>
				</div>
				<Button size="sm" type="submit">
					{t("intelligence.search.submit")}
				</Button>
			</form>

			{!term ? (
				<div className="rounded-lg border border-border p-6 text-sm text-muted-foreground" data-testid="search-idle">
					{t("intelligence.search.idle")}
				</div>
			) : error ? (
				<div className="p-4 text-sm text-destructive">{error}</div>
			) : isLoading ? (
				<div className="p-4 text-sm text-muted-foreground">{t("intelligence.search.searching")}</div>
			) : !result || result.hits.length === 0 ? (
				<div className="rounded-lg border border-border p-6 text-sm text-muted-foreground" data-testid="search-empty">
					{t("intelligence.search.empty")}
				</div>
			) : (
				<>
					<div className="text-xs text-muted-foreground" data-testid="search-counts">
						{t("intelligence.search.counts", {
							total: result.hits.length,
							memory: result.memoryHits,
							graph: result.symbolHits,
						})}
						{result.truncated ? t("intelligence.search.andMore") : ""}
					</div>
					<ul className="space-y-1.5">
						{result.hits.map((hit, i) => (
							<li
								key={`${hit.kind}-${hit.title}-${i}`}
								className="rounded-lg border border-border p-2.5"
								data-testid="search-hit"
							>
								<div className="flex flex-wrap items-baseline gap-2">
									<Badge variant={hit.kind === "memory" ? "accent" : "outline"}>
										{hit.kind === "memory"
											? t("intelligence.search.fromMemory")
											: t("intelligence.search.fromGraph")}
									</Badge>
									{hit.kind === "symbol" && onInspect ? (
										<button
											type="button"
											className="cursor-pointer text-sm font-medium underline-offset-2 hover:underline"
											onClick={() => onInspect(hit.title)}
										>
											{hit.title}
										</button>
									) : (
										<span className="text-sm font-medium">{hit.title}</span>
									)}
									{hit.symbolKind ? <Badge variant="outline">{hit.symbolKind}</Badge> : null}
									{hit.memoryType ? <Badge variant="outline">{hit.memoryType}</Badge> : null}
									{hit.state && hit.state !== "valid" ? (
										<Badge variant="warning">{hit.state}</Badge>
									) : null}
									{/*
									 * P4-H: a memory row also says how strong a
									 * claim it is. Labelling only WHICH authority
									 * produced a row leaves the two very
									 * different kinds of memory row — one AO read
									 * out of a file, one AO inferred from naming —
									 * looking equally settled.
									 */}
									{hit.kind === "memory" && evidenceLabel(hit.evidenceClass) ? (
										<Badge
											variant={hit.evidenceClass === "derived" ? "warning" : "outline"}
											data-testid="search-hit-evidence"
										>
											{t(evidenceLabel(hit.evidenceClass) as MessageKey)}
										</Badge>
									) : null}
								</div>
								{hit.detail ? (
									<div className="mt-1 truncate text-xs text-muted-foreground" title={hit.detail}>
										{hit.detail}
									</div>
								) : null}
								<div className="mt-1 flex flex-wrap gap-x-3 text-xs text-muted-foreground">
									{hit.path ? (
										<span>
											{hit.path}
											{hit.line ? `:${hit.line}` : ""}
										</span>
									) : null}
									{hit.sourceCommit ? (
										<span>{t("intelligence.graph.atCommit", { commit: hit.sourceCommit.slice(0, 12) })}</span>
									) : null}
								</div>
							</li>
						))}
					</ul>
				</>
			)}
		</div>
	);
}
