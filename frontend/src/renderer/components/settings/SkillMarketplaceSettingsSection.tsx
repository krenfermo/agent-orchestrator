import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert, Loader2, Search } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { components } from "../../../api/schema";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { SettingsSection } from "./SettingsSection";

type Release = components["schemas"]["SkillReleaseView"];
type SearchResponse = components["schemas"]["SkillMarketplaceSearchResponse"];
type ReleaseDetail = components["schemas"]["SkillReleaseDetailResponse"];

/**
 * Settings → Skills → Marketplace. Search configured registries, read what a
 * release asks for, and install one exact version.
 *
 * Four things here are deliberate.
 *
 * **There is no Run button, and there is no route that would give it one.**
 * Installing moves a package exactly one step, from available to installed.
 * Reaching a project is a separate act under a different permission, and
 * reaching a container needs an approved image on top of that. The screen says
 * so before the install and again after it, using the daemon's own sentences.
 *
 * **The trust state comes from the daemon, with its explanation.** A search
 * fetched nothing, so every result reads "unverified" — and this component
 * cannot decide otherwise, because it has no rule for it. Nothing here ever
 * renders the word "trusted": AO verifies no publisher signature, and the
 * highest state it can reach is "verified", meaning the bytes matched the
 * digests the release named. The sentence that says exactly that is served, not
 * written here, so this screen cannot describe the trust model more
 * optimistically than the thing enforcing it.
 *
 * **A registry AO could not read is shown, not swallowed.** An empty result
 * list and an unreadable registry are different answers, and only one means
 * somebody should go and look.
 *
 * **A revoked release stays on screen, greyed and uninstallable.** Hiding it
 * would answer "where did that version go" with silence.
 */
export function SkillMarketplaceSettingsSection() {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [query, setQuery] = useState("");
	// The text actually searched, committed on submit. Typing must not fire a
	// request per keystroke at a registry.
	const [submitted, setSubmitted] = useState("");
	const [includeRevoked, setIncludeRevoked] = useState(false);
	const [openRelease, setOpenRelease] = useState<{ registryId: string; skillId: string } | null>(null);
	const [error, setError] = useState<string | null>(null);
	const [installed, setInstalled] = useState<string | null>(null);

	const searchKey = ["skills", "marketplace", submitted, includeRevoked] as const;
	const search = useQuery<SearchResponse>({
		queryKey: searchKey,
		queryFn: async () => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/skills/marketplace", {
				credentials: "include",
				params: {
					query: {
						...(submitted ? { q: submitted } : {}),
						...(includeRevoked ? { includeRevoked: true } : {}),
					},
				},
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
	});

	const detail = useQuery<ReleaseDetail>({
		queryKey: ["skills", "marketplace", "release", openRelease?.registryId, openRelease?.skillId],
		enabled: openRelease !== null,
		queryFn: async () => {
			if (!openRelease) throw new Error("no release selected");
			const { data, error: apiError } = await apiClient.GET(
				"/api/v1/skills/marketplace/{registryId}/{skillId}",
				{
					credentials: "include",
					params: { path: { registryId: openRelease.registryId, skillId: openRelease.skillId } },
				},
			);
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
	});

	const install = useMutation({
		mutationFn: async (release: Release) => {
			const { data, error: apiError } = await apiClient.POST("/api/v1/skills/marketplace/install", {
				credentials: "include",
				// The exact version, always. There is no "latest" install target,
				// and this component never picks one.
				body: {
					registryId: release.registryId,
					skillId: release.skillId,
					version: release.version,
					asUpdate: release.updateAvailable,
				},
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
		onSuccess: (data) => {
			setError(null);
			// The daemon's own sentence, not one written here.
			setInstalled(data.nextStep);
			void queryClient.invalidateQueries({ queryKey: ["skills"] });
		},
		onError: (err: Error) => {
			setInstalled(null);
			setError(err.message);
		},
	});

	const releases = search.data?.releases ?? [];
	const notes = search.data?.notes ?? [];

	return (
		<SettingsSection title={t("settings.skillMarketplace.title")} data-testid="skill-marketplace-settings">
			<p className="text-caption text-settings-muted">{t("settings.skillMarketplace.intro")}</p>

			<form
				className="flex items-center gap-2"
				onSubmit={(e) => {
					e.preventDefault();
					setSubmitted(query.trim());
				}}
			>
				<Input
					value={query}
					placeholder={t("settings.skillMarketplace.searchPlaceholder")}
					aria-label={t("settings.skillMarketplace.searchLabel")}
					onChange={(e) => setQuery(e.target.value)}
				/>
				<Button type="submit" variant="secondary" size="sm" disabled={search.isFetching}>
					{search.isFetching ? (
						<Loader2 className="size-3 animate-spin" aria-hidden="true" />
					) : (
						<Search className="size-3" aria-hidden="true" />
					)}
					{t("settings.skillMarketplace.searchAction")}
				</Button>
			</form>
			<label className="flex items-center gap-2 text-caption text-settings-muted">
				<input
					type="checkbox"
					checked={includeRevoked}
					onChange={(e) => setIncludeRevoked(e.target.checked)}
				/>
				{t("settings.skillMarketplace.includeRevoked")}
			</label>

			{error ? (
				<p className="flex items-start gap-2 text-caption text-error" role="alert">
					<CircleAlert className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
					{error}
				</p>
			) : null}
			{installed ? (
				<p className="text-caption text-settings-muted" data-testid="skill-marketplace-installed">
					{installed}
				</p>
			) : null}

			{search.isError ? (
				<p className="text-caption text-error">{(search.error as Error).message}</p>
			) : null}

			{/* An unreadable registry is NAMED. "This registry has nothing" is a
			    materially more comfortable fact than the truth. */}
			{notes.map((note) => (
				<p className="text-caption text-warning" key={note.registryId} data-testid="skill-registry-note">
					{t("settings.skillMarketplace.registryUnreadable", {
						registry: note.registryId,
						reason: note.reason,
					})}
				</p>
			))}

			{!search.isLoading && releases.length === 0 ? (
				<p className="text-caption text-settings-muted">
					{t("settings.skillMarketplace.empty")}
				</p>
			) : null}

			<ul className="flex flex-col gap-2" data-testid="skill-marketplace-results">
				{releases.map((release) => (
					<li
						className="flex flex-col gap-1 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] p-3"
						key={`${release.registryId}/${release.skillId}/${release.version}`}
					>
						<div className="flex flex-wrap items-center gap-2">
							<span className="text-caption font-medium">{release.name}</span>
							<span className="text-caption text-settings-muted">
								{release.skillId}@{release.version}
							</span>
							{/* Installed / Not installed, always stated. */}
							<Badge variant={release.installed ? "success" : "outline"}>
								{release.installed
									? t("settings.skillMarketplace.installed")
									: t("settings.skillMarketplace.notInstalled")}
							</Badge>
							{release.updateAvailable ? (
								<Badge variant="accent">
									{t("settings.skillMarketplace.updateAvailable", {
										version: release.installedVersion,
									})}
								</Badge>
							) : null}
							{release.revoked ? (
								<Badge variant="error">{t("settings.skillMarketplace.revoked")}</Badge>
							) : null}
							{release.deprecated ? (
								<Badge variant="warning">{t("settings.skillMarketplace.deprecated")}</Badge>
							) : null}
						</div>
						<p className="text-caption text-settings-muted">{release.description}</p>
						<p className="text-caption text-settings-muted">
							{t("settings.skillMarketplace.origin", {
								publisher: release.publisher,
								registry: release.registryName || release.registryId,
							})}
						</p>
						<p className="text-caption text-settings-muted">
							{t("settings.skillMarketplace.capabilities", {
								list: release.requestedCapabilities.join(", "),
							})}
						</p>
						<div className="flex flex-wrap items-center gap-2 text-caption text-settings-muted">
							<Badge variant="outline">
								{t(`settings.skillMarketplace.trust.${release.trust}`)}
							</Badge>
							<Badge variant={release.compatibility === "compatible" ? "outline" : "warning"}>
								{t(`settings.skillMarketplace.compatibility.${release.compatibility}`)}
							</Badge>
							<span>{t(`settings.project.skills.risk.${release.riskLevel}`)}</span>
						</div>
						{/* Served by the daemon, so this screen cannot describe the
						    trust model more optimistically than the thing enforcing it. */}
						<p className="text-caption text-settings-muted">{release.trustExplanation}</p>
						{release.revoked ? (
							<p className="text-caption text-error">
								{t("settings.skillMarketplace.revokedReason", {
									reason: release.revocationReason,
								})}
							</p>
						) : null}

						<div className="flex flex-wrap items-center gap-2">
							<Button
								variant="secondary"
								size="sm"
								onClick={() =>
									setOpenRelease(
										openRelease?.skillId === release.skillId &&
											openRelease?.registryId === release.registryId
											? null
											: { registryId: release.registryId, skillId: release.skillId },
									)
								}
							>
								{t("settings.skillMarketplace.viewVersions")}
							</Button>
							<Button
								variant="primary"
								size="sm"
								// A revoked release can never be installed, and the
								// daemon refuses it too; disabling here is convenience,
								// not the control.
								disabled={release.revoked || release.installed || install.isPending}
								onClick={() => install.mutate(release)}
							>
								{install.isPending
									? t("settings.skillMarketplace.installing")
									: release.updateAvailable
										? t("settings.skillMarketplace.updateAction", { version: release.version })
										: t("settings.skillMarketplace.installAction", { version: release.version })}
							</Button>
						</div>
						{/* Shown BEFORE the install, every time. */}
						{!release.installed && !release.revoked ? (
							<p className="text-caption text-settings-muted" data-testid="skill-install-notice">
								{search.data?.installNotice}
							</p>
						) : null}

						{openRelease?.skillId === release.skillId &&
						openRelease?.registryId === release.registryId ? (
							<div className="flex flex-col gap-1 border-t border-[var(--color-border-settings-input)] pt-2">
								{detail.isLoading ? (
									<p className="text-caption text-settings-muted">
										{t("settings.skillMarketplace.loadingVersions")}
									</p>
								) : null}
								{detail.isError ? (
									<p className="text-caption text-error">{(detail.error as Error).message}</p>
								) : null}
								<p className="break-all text-caption text-settings-muted">
									{t("settings.skillMarketplace.digests", {
										manifest: release.manifestDigest,
										artifact: release.artifactDigest,
									})}
								</p>
								{/* A declared signature is a CLAIM. A line reading
								    "signature: cosign" alone would look like a check. */}
								{release.signatureFormat ? (
									<p className="text-caption text-warning">
										{t("settings.skillMarketplace.signatureClaimed", {
											format: release.signatureFormat,
										})}
									</p>
								) : null}
								{release.executionModes.map((mode) => (
									<p className="text-caption text-settings-muted" key={mode.id}>
										{t("settings.skillMarketplace.mode", {
											id: mode.id,
											risk: mode.riskLevel,
											list: mode.capabilities.join(", "),
										})}
									</p>
								))}
								<ul className="flex flex-col gap-1" data-testid="skill-release-versions">
									{(detail.data?.versions ?? []).map((version) => (
										<li className="flex items-center gap-2 text-caption" key={version.version}>
											<span>{version.version}</span>
											{version.installed ? (
												<Badge variant="success">
													{t("settings.skillMarketplace.installed")}
												</Badge>
											) : null}
											{/* A withdrawn version stays listed: the version
											    list is where somebody finds out it is gone. */}
											{version.revoked ? (
												<Badge variant="error">
													{t("settings.skillMarketplace.revoked")}
												</Badge>
											) : null}
											{version.deprecated ? (
												<Badge variant="warning">
													{t("settings.skillMarketplace.deprecated")}
												</Badge>
											) : null}
										</li>
									))}
								</ul>
							</div>
						) : null}
					</li>
				))}
			</ul>
		</SettingsSection>
	);
}
