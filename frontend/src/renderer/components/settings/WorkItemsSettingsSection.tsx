import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink, Loader2, Unlink } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { components } from "../../../api/schema";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { Badge, type BadgeVariant } from "../ui/badge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "../ui/select";
import { SettingsSection } from "./SettingsSection";

type WorkItemsConfig = components["schemas"]["WorkItemsConfigResponse"];
type WorkItemLink = components["schemas"]["WorkItemLinkResponse"];
type WorkItemsProviderProject = components["schemas"]["WorkItemsProviderProject"];

/**
 * Project settings → Planning (P4-E). The external work-management connection,
 * and the items this project's work is linked to.
 *
 * Three things this component is careful about, because getting any of them
 * wrong would misrepresent something:
 *
 * - **The credential is write-only.** The response has no token field at all,
 *   so there is nothing to render. What is shown is whether one is stored, and
 *   whether it came from the environment rather than the database — which is
 *   what somebody wondering why clearing the field changed nothing needs told.
 * - **Degraded is shown, not hidden.** A connection that is switched on and not
 *   working says so, and a link the provider could not answer for renders from
 *   its cache marked stale rather than vanishing. A planning panel that goes
 *   blank when the provider is down looks like AO lost the data.
 * - **AO is canonical, and the copy says so.** Nothing here suggests the
 *   external board drives AO. The one-directional arrow in the wording is the
 *   product promise this whole phase rests on.
 */
export function WorkItemsSettingsSection({ projectId }: { projectId: string }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const configKey = ["workitems", "config", projectId] as const;
	const linksKey = ["workitems", "links", projectId] as const;

	const [workspace, setWorkspace] = useState<string | null>(null);
	const [baseURL, setBaseURL] = useState<string | null>(null);
	const [token, setToken] = useState("");
	const [error, setError] = useState<string | null>(null);
	const [tested, setTested] = useState<string | null>(null);

	const config = useQuery({
		queryKey: configKey,
		queryFn: async (): Promise<WorkItemsConfig> => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/projects/{id}/workitems", {
				credentials: "include",
				params: { path: { id: projectId } },
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
	});

	const links = useQuery({
		queryKey: linksKey,
		queryFn: async () => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/projects/{id}/workitems/links", {
				credentials: "include",
				params: { path: { id: projectId }, query: { live: true } },
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data.links ?? [];
		},
		enabled: Boolean(config.data?.enabled),
	});

	// The provider's projects are only listable once a workspace and a
	// credential exist, so the picker asks for them only then — a failing
	// request behind an empty form would render as a broken connection.
	const providerProjects = useQuery({
		queryKey: ["workitems", "provider-projects", projectId, config.data?.workspace] as const,
		queryFn: async (): Promise<WorkItemsProviderProject[]> => {
			const { data, error: apiError } = await apiClient.GET(
				"/api/v1/projects/{id}/workitems/projects",
				{ credentials: "include", params: { path: { id: projectId } } },
			);
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data.projects ?? [];
		},
		enabled: Boolean(config.data?.workspace) && Boolean(config.data?.tokenConfigured),
		retry: false,
	});

	const save = useMutation({
		mutationFn: async (update: components["schemas"]["WorkItemsConfigUpdate"]) => {
			const { data, error: apiError } = await apiClient.PUT("/api/v1/projects/{id}/workitems", {
				credentials: "include",
				params: { path: { id: projectId } },
				body: update,
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
		onSuccess: (data) => {
			setError(null);
			setToken("");
			queryClient.setQueryData(configKey, data);
			void queryClient.invalidateQueries({ queryKey: linksKey });
		},
		onError: (e: Error) => setError(e.message),
	});

	const test = useMutation({
		mutationFn: async () => {
			const { data, error: apiError } = await apiClient.POST("/api/v1/projects/{id}/workitems/test", {
				credentials: "include",
				params: { path: { id: projectId } },
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
		onSuccess: (data) => {
			setError(null);
			setTested(t("workitems.testOk", { workspace: data.workspace, count: data.projects }));
			void queryClient.invalidateQueries({ queryKey: configKey });
			void queryClient.invalidateQueries({ queryKey: ["workitems", "provider-projects", projectId] });
		},
		onError: (e: Error) => {
			setTested(null);
			setError(e.message);
		},
	});

	const unlink = useMutation({
		mutationFn: async (linkId: string) => {
			const { error: apiError } = await apiClient.DELETE(
				"/api/v1/projects/{id}/workitems/links/{linkId}",
				{ credentials: "include", params: { path: { id: projectId, linkId } } },
			);
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => void queryClient.invalidateQueries({ queryKey: linksKey }),
		onError: (e: Error) => setError(e.message),
	});

	if (config.isLoading) {
		return (
			<SettingsSection title={t("workitems.title")}>
				<div className="flex items-center gap-2 text-sm text-muted-foreground">
					<Loader2 aria-hidden="true" className="size-4 animate-spin" />
					{t("workitems.loading")}
				</div>
			</SettingsSection>
		);
	}
	if (config.error) {
		return (
			<SettingsSection title={t("workitems.title")}>
				<div className="text-sm text-destructive">{apiErrorMessage(config.error)}</div>
			</SettingsSection>
		);
	}

	const current = config.data;
	const workspaceValue = workspace ?? current?.workspace ?? "";
	const baseURLValue = baseURL ?? current?.baseUrl ?? "";
	const canEnable =
		Boolean(workspaceValue) &&
		Boolean(current?.externalProjectId) &&
		(Boolean(current?.tokenConfigured) || token.trim() !== "");

	return (
		<SettingsSection title={t("workitems.title")}>
			<div className="space-y-4" data-testid="workitems-settings">
				<p className="text-xs text-muted-foreground">{t("workitems.description")}</p>
				<ConnectionStatus config={current} />

				<label className="block space-y-1">
					<span className="text-sm font-medium">{t("workitems.baseUrlLabel")}</span>
					<Input
						aria-label={t("workitems.baseUrlLabel")}
						placeholder="https://api.plane.so"
						value={baseURLValue}
						onChange={(e) => setBaseURL(e.target.value)}
					/>
					<span className="text-xs text-muted-foreground">{t("workitems.baseUrlHint")}</span>
				</label>

				<label className="block space-y-1">
					<span className="text-sm font-medium">{t("workitems.workspaceLabel")}</span>
					<Input
						aria-label={t("workitems.workspaceLabel")}
						placeholder="acme"
						value={workspaceValue}
						onChange={(e) => setWorkspace(e.target.value)}
					/>
				</label>

				<label className="block space-y-1">
					<span className="text-sm font-medium">{t("workitems.tokenLabel")}</span>
					<Input
						aria-label={t("workitems.tokenLabel")}
						type="password"
						autoComplete="off"
						placeholder={
							current?.tokenConfigured ? t("workitems.tokenStored") : t("workitems.tokenPlaceholder")
						}
						value={token}
						onChange={(e) => setToken(e.target.value)}
					/>
					<span className="text-xs text-muted-foreground">
						{current?.tokenFromEnv ? t("workitems.tokenFromEnv") : t("workitems.tokenHint")}
					</span>
				</label>

				<div className="flex flex-wrap gap-2">
					<Button
						size="sm"
						disabled={save.isPending}
						onClick={() =>
							save.mutate({
								baseUrl: baseURLValue,
								workspace: workspaceValue,
								// Omitted when blank, so saving the form does not erase a
								// stored credential.
								...(token.trim() ? { apiToken: token.trim() } : {}),
							})
						}
					>
						{t("workitems.save")}
					</Button>
					<Button
						size="sm"
						variant="outline"
						disabled={test.isPending || !current?.tokenConfigured}
						onClick={() => test.mutate()}
					>
						{test.isPending ? t("workitems.testing") : t("workitems.test")}
					</Button>
				</div>

				{providerProjects.data && providerProjects.data.length > 0 ? (
					<label className="block space-y-1">
						<span className="text-sm font-medium">{t("workitems.projectLabel")}</span>
						<Select
							value={current?.externalProjectId ?? ""}
							onValueChange={(value) => save.mutate({ externalProjectId: value })}
						>
							<SelectTrigger aria-label={t("workitems.projectLabel")}>
								<SelectValue placeholder={t("workitems.projectPlaceholder")} />
							</SelectTrigger>
							<SelectContent>
								{providerProjects.data.map((p) => (
									<SelectItem key={p.id} value={p.id}>
										{p.identifier ? `${p.name} (${p.identifier})` : p.name}
									</SelectItem>
								))}
							</SelectContent>
						</Select>
					</label>
				) : null}

				<div className="space-y-2 rounded-md border border-border p-3">
					<Toggle
						label={t("workitems.enabledLabel")}
						hint={t("workitems.enabledHint")}
						checked={Boolean(current?.enabled)}
						disabled={!canEnable && !current?.enabled}
						onChange={(next) => save.mutate({ enabled: next })}
					/>
					<Toggle
						label={t("workitems.syncStatesLabel")}
						hint={t("workitems.syncStatesHint")}
						checked={Boolean(current?.syncStates)}
						disabled={!current?.enabled}
						onChange={(next) => save.mutate({ syncStates: next })}
					/>
					<Toggle
						label={t("workitems.syncCommentsLabel")}
						hint={t("workitems.syncCommentsHint")}
						checked={Boolean(current?.syncComments)}
						disabled={!current?.enabled}
						onChange={(next) => save.mutate({ syncComments: next })}
					/>
				</div>

				{tested ? <div className="text-sm text-success">{tested}</div> : null}
				{error ? (
					<div className="text-sm text-destructive" data-testid="workitems-error">
						{error}
					</div>
				) : null}

				{current?.enabled ? (
					<LinkList
						links={links.data ?? []}
						loading={links.isLoading}
						onUnlink={(id) => unlink.mutate(id)}
					/>
				) : null}
			</div>
		</SettingsSection>
	);
}

const STATUS_VARIANT: Record<string, BadgeVariant> = {
	connected: "success",
	degraded: "warning",
	off: "neutral",
};

function ConnectionStatus({ config }: { config: WorkItemsConfig | undefined }) {
	const { t } = useTranslation();
	if (!config) return null;

	let key: "off" | "connected" | "degraded" = "off";
	if (config.enabled) {
		key = config.degraded || !config.connected ? "degraded" : "connected";
	}
	return (
		<div className="flex flex-wrap items-center gap-2" data-testid="workitems-status">
			<Badge variant={STATUS_VARIANT[key]}>{t(`workitems.status.${key}` as const)}</Badge>
			{config.externalProjectName ? (
				<span className="text-sm text-muted-foreground">
					{config.externalProjectName}
					{config.externalProjectKey ? ` (${config.externalProjectKey})` : ""}
				</span>
			) : null}
			{config.lastCheckError ? (
				<span className="text-xs text-warning" data-testid="workitems-last-error">
					{config.lastCheckError}
				</span>
			) : null}
		</div>
	);
}

function Toggle({
	label,
	hint,
	checked,
	disabled,
	onChange,
}: {
	label: string;
	hint: string;
	checked: boolean;
	disabled?: boolean;
	onChange: (next: boolean) => void;
}) {
	return (
		<label className="flex items-start gap-2">
			<input
				type="checkbox"
				className="mt-0.5"
				checked={checked}
				disabled={disabled}
				aria-label={label}
				onChange={(e) => onChange(e.target.checked)}
			/>
			<span className="min-w-0">
				<span className="block text-sm">{label}</span>
				<span className="block text-xs text-muted-foreground">{hint}</span>
			</span>
		</label>
	);
}

function LinkList({
	links,
	loading,
	onUnlink,
}: {
	links: WorkItemLink[];
	loading: boolean;
	onUnlink: (id: string) => void;
}) {
	const { t } = useTranslation();
	if (loading) {
		return <div className="text-sm text-muted-foreground">{t("workitems.linksLoading")}</div>;
	}
	if (links.length === 0) {
		return (
			<div className="text-sm text-muted-foreground" data-testid="workitems-links-empty">
				{t("workitems.linksEmpty")}
			</div>
		);
	}
	return (
		<ul className="space-y-1.5" data-testid="workitems-links">
			{links.map((link) => (
				<li key={link.id} className="rounded-md border border-border p-2.5" data-testid="workitems-link">
					<div className="flex flex-wrap items-baseline gap-2">
						<span className="font-mono text-xs">{link.externalItemKey || link.externalItemId}</span>
						<span className="min-w-0 flex-1 text-sm">{link.title || t("workitems.untitled")}</span>
						{link.stateName || link.state ? (
							<Badge variant="outline">{link.stateName || link.state}</Badge>
						) : null}
						{/* A cached row says so, rather than passing itself off as current. */}
						{link.stale ? <Badge variant="warning">{t("workitems.stale")}</Badge> : null}
						{!link.syncEnabled ? <Badge variant="neutral">{t("workitems.muted")}</Badge> : null}
					</div>
					<div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
						<span>{t(`workitems.scope.${link.scope}` as const)}</span>
						{link.liveError ? <span className="text-warning">{link.liveError}</span> : null}
						{link.url ? (
							<a
								className="inline-flex items-center gap-1 underline-offset-2 hover:underline"
								href={link.url}
								target="_blank"
								rel="noreferrer"
							>
								<ExternalLink aria-hidden="true" className="size-3" />
								{t("workitems.open")}
							</a>
						) : null}
						<button
							type="button"
							className="inline-flex cursor-pointer items-center gap-1 underline-offset-2 hover:underline"
							onClick={() => onUnlink(link.id)}
						>
							<Unlink aria-hidden="true" className="size-3" />
							{t("workitems.unlink")}
						</button>
					</div>
				</li>
			))}
		</ul>
	);
}
