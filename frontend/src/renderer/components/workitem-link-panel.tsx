import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink, Loader2, SquareKanban, Unlink } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";
import { Badge, type BadgeVariant } from "./ui/badge";
import { Button } from "./ui/button";
import { Input } from "./ui/input";

type WorkItemLink = components["schemas"]["WorkItemLinkResponse"];
type WorkItemsHealth = components["schemas"]["WorkItemsHealthResponse"];

/**
 * The Plane link for one workflow run or planned task (P4-E §6).
 *
 * It renders NOTHING at all when the project has no connection, which is the
 * common case and the reason this can sit on every run page without clutter:
 * an integration nobody configured should not occupy a line explaining that it
 * is not configured.
 *
 * Three states it is careful to distinguish, because collapsing any two would
 * mislead somebody:
 *
 * - **Not linked** — an input and a Link button. Nothing has gone wrong.
 * - **Linked and current** — the item's key, title and the provider's own state
 *   name, with a way out to Plane.
 * - **Linked and stale** — the same, from cache, badged as such with the reason.
 *   The panel must not go blank when Plane is down; that reads as AO losing the
 *   link rather than as an outage.
 *
 * RBAC is rendered from the daemon's answer, not guessed: the read routes
 * answer 404 for a project the caller cannot reach (so the panel disappears),
 * and a link attempt the caller is not permitted returns the daemon's own
 * error, which is shown as-is.
 */
export function WorkItemLinkPanel({
	projectId,
	scope,
	scopeId,
}: {
	projectId: string;
	scope: "run" | "task";
	scopeId: string;
}) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const linksKey = ["workitems", "links", projectId] as const;
	const healthKey = ["workitems", "health", projectId] as const;

	const [reference, setReference] = useState("");
	const [error, setError] = useState<string | null>(null);

	// Health decides whether this panel exists at all, and it is the cheap
	// read: it makes no provider call.
	const health = useQuery({
		queryKey: healthKey,
		enabled: hasTrustedApiBaseUrl() && Boolean(projectId),
		queryFn: async (): Promise<WorkItemsHealth> => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/projects/{id}/workitems/health", {
				credentials: "include",
				params: { path: { id: projectId } },
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
		retry: false,
	});

	const links = useQuery({
		queryKey: linksKey,
		enabled: Boolean(health.data?.enabled),
		queryFn: async (): Promise<WorkItemLink[]> => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/projects/{id}/workitems/links", {
				credentials: "include",
				params: { path: { id: projectId }, query: { live: true } },
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data.links ?? [];
		},
	});

	const link = useMutation({
		mutationFn: async () => {
			const { data, error: apiError } = await apiClient.POST("/api/v1/projects/{id}/workitems/links", {
				credentials: "include",
				params: { path: { id: projectId } },
				body: { scope, scopeId, reference: reference.trim(), syncEnabled: true },
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
		onSuccess: () => {
			setError(null);
			setReference("");
			void queryClient.invalidateQueries({ queryKey: linksKey });
		},
		onError: (e: Error) => setError(e.message),
	});

	const unlink = useMutation({
		mutationFn: async (linkId: string) => {
			const { error: apiError } = await apiClient.DELETE(
				"/api/v1/projects/{id}/workitems/links/{linkId}",
				{ credentials: "include", params: { path: { id: projectId, linkId } } },
			);
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			void queryClient.invalidateQueries({ queryKey: linksKey });
		},
		onError: (e: Error) => setError(e.message),
	});

	// No connection, or a project this caller cannot reach: render nothing.
	// health.error covers the 404 a denied project answers, which is the same
	// answer a nonexistent one gives — so a caller learns nothing either way.
	if (!health.data?.enabled || health.error) return null;

	const current = (links.data ?? []).find((l) => l.scope === scope && l.scopeId === scopeId);

	return (
		<section className="rounded-lg border border-border p-3" data-testid="workitem-link-panel">
			<div className="flex flex-wrap items-center gap-2">
				<SquareKanban aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />
				<h2 className="text-sm font-medium">{t("workitems.panel.title")}</h2>
				{health.data.degraded ? (
					<Badge variant="warning" data-testid="workitem-panel-degraded">
						{t("workitems.status.degraded")}
					</Badge>
				) : null}
				{health.data.pending > 0 ? (
					<Badge variant="neutral" data-testid="workitem-panel-pending">
						{t("workitems.panel.pending", { count: health.data.pending })}
					</Badge>
				) : null}
			</div>

			{links.isLoading ? (
				<div className="mt-2 flex items-center gap-2 text-sm text-muted-foreground">
					<Loader2 aria-hidden="true" className="size-3.5 animate-spin" />
					{t("workitems.linksLoading")}
				</div>
			) : current ? (
				<LinkedItem item={current} onUnlink={() => unlink.mutate(current.id)} busy={unlink.isPending} />
			) : (
				<form
					className="mt-2 flex flex-wrap items-center gap-2"
					onSubmit={(e) => {
						e.preventDefault();
						if (reference.trim()) link.mutate();
					}}
				>
					<Input
						aria-label={t("workitems.panel.referenceLabel")}
						className="h-8 max-w-56 flex-1"
						placeholder={t("workitems.panel.referencePlaceholder")}
						value={reference}
						onChange={(e) => setReference(e.target.value)}
					/>
					<Button size="sm" type="submit" disabled={link.isPending || !reference.trim()}>
						{link.isPending ? t("workitems.panel.linking") : t("workitems.panel.link")}
					</Button>
				</form>
			)}

			{error ? (
				<p className="mt-2 text-sm text-destructive" data-testid="workitem-panel-error">
					{error}
				</p>
			) : null}
		</section>
	);
}

const SYNC_VARIANT: Record<string, BadgeVariant> = {
	ready: "success",
	deferred: "neutral",
	done: "outline",
	unknown: "neutral",
};

function LinkedItem({
	item,
	onUnlink,
	busy,
}: {
	item: WorkItemLink;
	onUnlink: () => void;
	busy: boolean;
}) {
	const { t } = useTranslation();
	return (
		<div className="mt-2" data-testid="workitem-linked">
			<div className="flex flex-wrap items-baseline gap-2">
				<span className="font-mono text-xs">{item.externalItemKey || item.externalItemId}</span>
				<span className="min-w-0 flex-1 text-sm">{item.title || t("workitems.untitled")}</span>
				{item.stateName || item.state ? (
					<Badge variant={SYNC_VARIANT[item.readiness ?? "unknown"] ?? "neutral"}>
						{item.stateName || item.state}
					</Badge>
				) : null}
				{/* A cached row says so rather than passing itself off as current. */}
				{item.stale ? <Badge variant="warning">{t("workitems.stale")}</Badge> : null}
				{!item.syncEnabled ? <Badge variant="neutral">{t("workitems.muted")}</Badge> : null}
			</div>
			<div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
				{item.liveError ? <span className="text-warning">{item.liveError}</span> : null}
				{item.url ? (
					<a
						className="inline-flex items-center gap-1 underline-offset-2 hover:underline"
						href={item.url}
						target="_blank"
						rel="noreferrer"
					>
						<ExternalLink aria-hidden="true" className="size-3" />
						{t("workitems.open")}
					</a>
				) : null}
				<button
					type="button"
					className="inline-flex cursor-pointer items-center gap-1 underline-offset-2 hover:underline disabled:opacity-50"
					disabled={busy}
					onClick={onUnlink}
				>
					<Unlink aria-hidden="true" className="size-3" />
					{t("workitems.unlink")}
				</button>
			</div>
		</div>
	);
}
