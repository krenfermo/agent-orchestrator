import { AlertTriangle, ExternalLink, GitPullRequest, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge, type BadgeVariant } from "../ui/badge";
import { Button } from "../ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import { aoBridge } from "../../lib/bridge";
import {
	useProjectGitHub,
	type GitHubAvailability,
	type GitHubIssue,
	type GitHubPullRequest,
	type GitHubRepository,
} from "../../hooks/useProjectIntelligence";
import type { MessageKey } from "../../i18n/messages";

// Project → Intelligence → GitHub (P4-F).
//
// This is a READ of somebody else's system, and the design follows from that.
//
// It is not a GitHub client. There is no merging, no commenting, no approving:
// every row that has a URL opens GitHub in the system browser, because the
// place to act on a pull request is the place that owns it. What AO adds is the
// join GitHub cannot make — the branch in the checkout on this machine beside
// the branch on the server, and the pull request for THAT branch surfaced first
// rather than buried in a list.
//
// The local and remote halves are shown side by side and never merged into one
// number. "2 ahead, 1 behind" is a fact about the last fetch; the server's tip
// is a fact about now; collapsing them would produce a figure that is true of
// neither.
//
// Degraded is rendered, not hidden. A missing token, a rate limit and an
// outage each say what happened and what is still true — the local half needs
// no token and survives all three — because a panel that goes blank teaches
// people to stop opening it.

const AVAILABILITY_VARIANT: Record<GitHubAvailability, BadgeVariant> = {
	ready: "success",
	degraded: "warning",
	unavailable: "neutral",
};

const AVAILABILITY_LABEL: Record<GitHubAvailability, MessageKey> = {
	ready: "intelligence.github.state.ready",
	degraded: "intelligence.github.state.degraded",
	unavailable: "intelligence.github.state.unavailable",
};

// GitHub's own review and check vocabulary, mapped to colour once.
const REVIEW_VARIANT: Record<string, BadgeVariant> = {
	approved: "success",
	changes_requested: "error",
	review_required: "warning",
};

const CHECKS_VARIANT: Record<string, BadgeVariant> = {
	passing: "success",
	failing: "error",
	pending: "warning",
};

function shortSHA(sha?: string) {
	return sha ? sha.slice(0, 12) : "—";
}

function openExternally(url?: string) {
	if (url) void aoBridge.app.openExternal(url);
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

/** A row that opens GitHub in the system browser rather than inside AO. */
function ExternalRow({ url, label, children }: { url?: string; label: string; children: React.ReactNode }) {
	if (!url) return <div className="min-w-0 flex-1">{children}</div>;
	return (
		<button
			type="button"
			aria-label={label}
			onClick={() => openExternally(url)}
			className="flex min-w-0 flex-1 items-center gap-2 text-left hover:underline"
		>
			<span className="min-w-0 flex-1">{children}</span>
			<ExternalLink aria-hidden="true" className="size-3.5 shrink-0 text-muted-foreground" />
		</button>
	);
}

function RepositoryCard({ repo }: { repo: GitHubRepository }) {
	const { t } = useTranslation();
	const branch = repo.detached ? t("intelligence.github.detached") : repo.currentBranch || "—";
	// Only stated when non-zero: "0 ahead, 0 behind" is a line spent saying
	// nothing happened.
	const position =
		repo.ahead || repo.behind
			? t("intelligence.github.aheadBehind", {
					ahead: repo.ahead,
					behind: repo.behind,
					upstream: repo.upstreamRef || t("intelligence.github.upstream"),
				})
			: t("intelligence.github.inSync");

	return (
		<Card data-testid="github-repository">
			<CardHeader className="flex-row items-center justify-between gap-3 space-y-0">
				<div className="min-w-0">
					<CardTitle className="truncate text-sm">
						<ExternalRow url={repo.url} label={t("intelligence.github.openRepository")}>
							{repo.repo || t("intelligence.github.noRepository")}
						</ExternalRow>
					</CardTitle>
					{repo.originUrl ? (
						<div className="truncate text-xs text-muted-foreground" title={repo.originUrl}>
							{repo.originUrl}
						</div>
					) : null}
				</div>
				<div className="flex shrink-0 items-center gap-1.5">
					{repo.private ? (
						<Badge variant="outline">{t("intelligence.github.private")}</Badge>
					) : null}
					{repo.archived ? (
						<Badge variant="warning">{t("intelligence.github.archived")}</Badge>
					) : null}
					{/* An SSH host alias is worth naming: it is the reason this
					    repository is recognized at all, and silence about it made
					    the previous failure impossible to diagnose. */}
					{repo.aliasResolved ? (
						<Badge variant="outline" data-testid="github-alias-resolved">
							{t("intelligence.github.aliasResolved")}
						</Badge>
					) : null}
				</div>
			</CardHeader>
			<CardContent className="space-y-3">
				<div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
					<Stat label={t("intelligence.github.branch")} value={branch} />
					<Stat label={t("intelligence.github.defaultBranch")} value={repo.defaultBranch || "—"} />
					<Stat label={t("intelligence.github.head")} value={shortSHA(repo.headSha)} hint={repo.headSha} />
					<Stat
						label={t("intelligence.github.remoteHead", { ref: repo.remoteHeadRef || "—" })}
						value={shortSHA(repo.remoteHeadSha)}
						hint={repo.remoteHeadSha}
					/>
				</div>
				<div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
					<span data-testid="github-sync-state">{position}</span>
					{repo.dirty ? (
						<Badge variant="warning" data-testid="github-dirty">
							{t("intelligence.github.dirty")}
						</Badge>
					) : null}
				</div>
				{repo.recentCommits?.length ? (
					<div className="space-y-1" data-testid="github-recent-commits">
						<div className="text-xs text-muted-foreground">{t("intelligence.github.recentCommits")}</div>
						{repo.recentCommits.slice(0, 5).map((commit) => (
							<div key={commit.sha} className="flex min-w-0 gap-2 text-xs">
								<code className="shrink-0 text-muted-foreground">{commit.sha.slice(0, 8)}</code>
								<span className="truncate">{commit.subject}</span>
							</div>
						))}
					</div>
				) : null}
			</CardContent>
		</Card>
	);
}

function PullRequestRow({ pr, highlight }: { pr: GitHubPullRequest; highlight?: boolean }) {
	const { t } = useTranslation();
	const checks = pr.checksSummary ? CHECKS_VARIANT[pr.checksSummary] : undefined;
	return (
		<div
			className={highlight ? "rounded-md border border-primary/40 bg-primary/5 p-2.5" : "p-2.5"}
			data-testid={highlight ? "github-current-pr" : "github-pull-request"}
		>
			<div className="flex items-center gap-2">
				<GitPullRequest aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />
				<ExternalRow url={pr.url} label={t("intelligence.github.openPullRequest", { number: pr.number })}>
					<span className="truncate text-sm">
						<span className="text-muted-foreground">#{pr.number}</span> {pr.title}
					</span>
				</ExternalRow>
			</div>
			<div className="mt-1.5 flex flex-wrap items-center gap-1.5 pl-6">
				{pr.draft ? <Badge variant="neutral">{t("intelligence.github.draft")}</Badge> : null}
				{pr.reviewDecision ? (
					<Badge variant={REVIEW_VARIANT[pr.reviewDecision] ?? "neutral"} data-testid="github-review-decision">
						{pr.reviewDecision}
					</Badge>
				) : null}
				{pr.checksSummary ? (
					<Badge variant={checks ?? "neutral"} data-testid="github-checks">
						{t("intelligence.github.checks", {
							summary: pr.checksSummary,
							passed: pr.checksPassed,
							failed: pr.checksFailed,
						})}
					</Badge>
				) : null}
				{pr.mergeable ? <Badge variant="outline">{pr.mergeable}</Badge> : null}
				{pr.baseBranch ? (
					<span className="text-xs text-muted-foreground">
						{t("intelligence.github.into", { base: pr.baseBranch })}
					</span>
				) : null}
			</div>
			{pr.failingChecks?.length ? (
				<div className="mt-1 pl-6 text-xs text-destructive" data-testid="github-failing-checks">
					{t("intelligence.github.failing", { checks: pr.failingChecks.join(", ") })}
				</div>
			) : null}
		</div>
	);
}

function IssueRow({ issue }: { issue: GitHubIssue }) {
	const { t } = useTranslation();
	return (
		<div className="p-2.5" data-testid="github-issue">
			<ExternalRow url={issue.url} label={t("intelligence.github.openIssue", { number: issue.number })}>
				<span className="truncate text-sm">
					<span className="text-muted-foreground">#{issue.number}</span> {issue.title}
				</span>
			</ExternalRow>
			<div className="mt-1.5 flex flex-wrap items-center gap-1.5">
				{issue.state ? <Badge variant="outline">{issue.state}</Badge> : null}
				{issue.milestone ? <Badge variant="neutral">{issue.milestone}</Badge> : null}
				{issue.labels?.slice(0, 4).map((label) => (
					<Badge key={label} variant="neutral">
						{label}
					</Badge>
				))}
				{issue.assignees?.length ? (
					<span className="text-xs text-muted-foreground">
						{t("intelligence.github.assigned", { who: issue.assignees.join(", ") })}
					</span>
				) : null}
			</div>
			{issue.linkedPrs?.length ? (
				<div className="mt-1 text-xs text-muted-foreground" data-testid="github-linked-prs">
					{t("intelligence.github.linkedPrs", {
						numbers: issue.linkedPrs.map((pr) => `#${pr.number}`).join(", "),
					})}
				</div>
			) : null}
		</div>
	);
}

export function IntelligenceGitHub({ projectId }: { projectId: string }) {
	const { t } = useTranslation();
	const {
		github,
		availability,
		repository,
		pullRequests,
		currentPR,
		issues,
		isLoading,
		isRefreshing,
		refresh,
		error,
	} = useProjectGitHub(projectId);

	if (isLoading) {
		return <div className="p-4 text-sm text-muted-foreground">{t("intelligence.loading")}</div>;
	}
	// A request that failed is not a GitHub outage: the daemon or the caller's
	// access is the problem, and it needs a different fix from a rate limit.
	if (error) {
		return (
			<div className="p-4 text-sm text-destructive" data-testid="github-request-error">
				{error}
			</div>
		);
	}

	const otherPRs = pullRequests.filter((pr) => pr.number !== currentPR?.number);

	return (
		<div className="space-y-3 p-1" data-testid="intelligence-github">
			<div className="flex flex-wrap items-center gap-2">
				<Badge variant={AVAILABILITY_VARIANT[availability]} data-testid="github-availability">
					{t(AVAILABILITY_LABEL[availability])}
				</Badge>
				{github?.stale ? (
					<Badge variant="warning" data-testid="github-stale">
						{t("intelligence.github.stale")}
					</Badge>
				) : null}
				{!github?.authenticated ? (
					<Badge variant="outline" data-testid="github-unauthenticated">
						{t("intelligence.github.noToken")}
					</Badge>
				) : null}
				<div className="flex-1" />
				{github?.observedAt ? (
					<span className="text-xs text-muted-foreground">
						{t("intelligence.github.observedAt", {
							when: new Date(github.observedAt).toLocaleString(),
						})}
					</span>
				) : null}
				<Button size="sm" variant="outline" disabled={isRefreshing} onClick={() => void refresh()}>
					<RefreshCw aria-hidden="true" className={isRefreshing ? "animate-spin" : undefined} />
					{t("intelligence.github.refresh")}
				</Button>
			</div>

			{github?.detail ? (
				<div
					className="flex items-start gap-2 rounded-md bg-warning/10 p-2.5 text-xs text-warning-foreground"
					data-testid="github-degraded"
				>
					<AlertTriangle aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
					<span>{github.detail}</span>
				</div>
			) : null}

			{repository ? <RepositoryCard repo={repository} /> : null}

			<Card>
				<CardHeader className="space-y-0">
					<CardTitle className="text-sm">{t("intelligence.github.pullRequests")}</CardTitle>
				</CardHeader>
				<CardContent className="divide-y divide-border p-0">
					{currentPR ? <PullRequestRow pr={currentPR} highlight /> : null}
					{otherPRs.map((pr) => (
						<PullRequestRow key={pr.number} pr={pr} />
					))}
					{!currentPR && otherPRs.length === 0 ? (
						<div className="p-3 text-sm text-muted-foreground">
							{t("intelligence.github.noPullRequests")}
						</div>
					) : null}
				</CardContent>
			</Card>

			<Card>
				<CardHeader className="space-y-0">
					<CardTitle className="text-sm">{t("intelligence.github.issues")}</CardTitle>
				</CardHeader>
				<CardContent className="divide-y divide-border p-0">
					{issues.map((issue) => (
						<IssueRow key={issue.number} issue={issue} />
					))}
					{issues.length === 0 ? (
						<div className="p-3 text-sm text-muted-foreground">{t("intelligence.github.noIssues")}</div>
					) : null}
				</CardContent>
			</Card>
		</div>
	);
}
