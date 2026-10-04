import { useState } from "react";
import { AlertTriangle, RefreshCw, RotateCcw } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "../ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogFooter,
	DialogHeader,
	DialogTitle,
} from "../ui/dialog";
import { IntelligenceStateBadge } from "./IntelligenceStateBadge";
import {
	useIntelligenceOverview,
	useIntelligenceSync,
	type IntelligenceRepoStatus,
} from "../../hooks/useProjectIntelligence";

// The Overview tab: is AO's picture of this project current, and if not, why.
//
// The two buttons are deliberately framed as repairs rather than as the normal
// way to use the feature. Indexing is automatic — the reconciler keeps every
// project current without anybody opening this screen — so the copy says so,
// and neither button is the primary action.

function shortSHA(sha?: string) {
	return sha ? sha.slice(0, 12) : "—";
}

function duration(ms: number) {
	if (!ms) return "—";
	if (ms < 1000) return `${ms} ms`;
	return `${(ms / 1000).toFixed(1)} s`;
}

function Stat({ label, value, hint }: { label: string; value: string; hint?: string }) {
	return (
		<div className="min-w-0">
			<div className="text-xs text-muted-foreground">{label}</div>
			<div className="truncate font-medium tabular-nums" title={hint ?? value}>
				{value}
			</div>
		</div>
	);
}

function RepoCard({
	repo,
	projectId,
}: {
	repo: IntelligenceRepoStatus;
	projectId: string;
}) {
	const { t } = useTranslation();
	const { sync, rebuild, isSyncing, isRebuilding, error } = useIntelligenceSync(projectId);
	const [confirmRebuild, setConfirmRebuild] = useState(false);

	return (
		<Card data-testid="intelligence-repo-card">
			<CardHeader className="flex-row items-center justify-between gap-3 space-y-0">
				<div className="min-w-0">
					<CardTitle className="truncate text-sm" title={repo.repoPath}>
						{repo.repoPath}
					</CardTitle>
					{repo.backend ? (
						<div className="text-xs text-muted-foreground">
							{t("intelligence.overview.indexedBy", { backend: repo.backend })}
						</div>
					) : null}
				</div>
				<IntelligenceStateBadge state={repo.state} />
			</CardHeader>
			<CardContent className="space-y-4">
				{repo.drift ? (
					<div
						className="flex items-start gap-2 rounded-md bg-warning/10 p-2.5 text-xs text-warning-foreground"
						data-testid="intelligence-drift"
					>
						<AlertTriangle aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
						<span>{repo.drift}</span>
					</div>
				) : null}
				{repo.lastError ? (
					<div
						className="rounded-md bg-destructive/10 p-2.5 text-xs text-destructive"
						data-testid="intelligence-last-error"
					>
						{repo.lastError}
					</div>
				) : null}

				<div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
					<Stat label={t("intelligence.overview.files")} value={String(repo.files)} />
					<Stat label={t("intelligence.overview.symbols")} value={String(repo.symbols)} />
					<Stat label={t("intelligence.overview.relations")} value={String(repo.edges)} />
					<Stat label={t("intelligence.overview.memoryFacts")} value={String(repo.memoryItems)} />
					<Stat label={t("intelligence.overview.indexedCommit")} value={shortSHA(repo.indexedCommit)} hint={repo.indexedCommit} />
					<Stat label={t("intelligence.overview.repoCommit")} value={shortSHA(repo.headCommit)} hint={repo.headCommit} />
					<Stat label={t("intelligence.overview.lastSync")} value={repo.lastSyncKind || "—"} />
					<Stat label={t("intelligence.overview.took")} value={duration(repo.lastMillis)} />
				</div>

				{repo.lastSyncKind ? (
					<div className="text-xs text-muted-foreground">
						{t("intelligence.overview.lastPass", { parsed: repo.filesParsed })}
						{repo.filesReused > 0 ? t("intelligence.overview.reused", { reused: repo.filesReused }) : ""}
						{repo.filesRemoved > 0 ? t("intelligence.overview.removed", { removed: repo.filesRemoved }) : ""}
						{repo.updatedAt ? ` · ${new Date(repo.updatedAt).toLocaleString()}` : ""}
					</div>
				) : null}

				{error ? <div className="text-xs text-destructive">{error}</div> : null}

				<div className="flex items-center gap-2">
					<Button
						size="sm"
						variant="outline"
						disabled={isSyncing || isRebuilding}
						onClick={() => void sync(repo.repoPath)}
					>
						<RefreshCw aria-hidden="true" className={isSyncing ? "animate-spin" : undefined} />
						{isSyncing ? t("intelligence.overview.syncing") : t("intelligence.overview.syncNow")}
					</Button>
					<Button
						size="sm"
						variant="ghost"
						disabled={isSyncing || isRebuilding}
						onClick={() => setConfirmRebuild(true)}
					>
						<RotateCcw aria-hidden="true" />
						{t("intelligence.overview.rebuild")}
					</Button>
					<span className="text-xs text-muted-foreground">
						{t("intelligence.overview.automatic")}
					</span>
				</div>
			</CardContent>

			<Dialog open={confirmRebuild} onOpenChange={setConfirmRebuild}>
				<DialogContent>
					<DialogHeader>
						<DialogTitle>{t("intelligence.overview.rebuildTitle")}</DialogTitle>
						<DialogDescription>{t("intelligence.overview.rebuildBody")}</DialogDescription>
					</DialogHeader>
					<DialogFooter>
						<Button variant="ghost" onClick={() => setConfirmRebuild(false)}>
							{t("intelligence.cancel")}
						</Button>
						<Button
							variant="primary"
							onClick={() => {
								setConfirmRebuild(false);
								void rebuild(repo.repoPath);
							}}
						>
							{t("intelligence.overview.rebuild")}
						</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>
		</Card>
	);
}

export function IntelligenceOverview({ projectId }: { projectId: string }) {
	const { t } = useTranslation();
	const { repos, isLoading, error } = useIntelligenceOverview(projectId);

	if (isLoading) {
		return <div className="p-4 text-sm text-muted-foreground">{t("intelligence.loading")}</div>;
	}
	if (error) {
		return (
			<div className="p-4 text-sm text-destructive" data-testid="intelligence-error">
				{error}
			</div>
		);
	}
	if (repos.length === 0) {
		return (
			<div className="p-4 text-sm text-muted-foreground">
				{t("intelligence.overview.noRepos")}
			</div>
		);
	}
	return (
		<div className="space-y-3 p-1">
			{repos.map((repo) => (
				<RepoCard key={repo.repoId} repo={repo} projectId={projectId} />
			))}
		</div>
	);
}
