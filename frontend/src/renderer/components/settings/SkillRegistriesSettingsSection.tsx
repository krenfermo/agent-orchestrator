import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { components } from "../../../api/schema";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { Badge } from "../ui/badge";
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

type Registry = components["schemas"]["SkillRegistryView"];
type UpdateCheck = components["schemas"]["SkillUpdateCheckResponse"];
type Probe = components["schemas"]["SkillRegistryProbeView"];
type RevocationSync = components["schemas"]["SkillRegistryRevocationSyncView"];

// "official" is last because it is the narrowest, and it is present even
// though no build ships an AO Official root yet: a policy an administrator can
// choose and AO then refuses is honest, and a policy hidden until the day it
// works is one nobody knows to plan for.
const TRUST_POLICIES = ["digest", "pinned_publisher", "signed", "official"] as const;
// The external vocabulary is SEPARATE, and the two never mix. "digest" on a
// company mirror means "our registry, and AO hashed the bytes"; the same word
// on a stranger's repository would read identically and mean materially less.
// The daemon refuses the mixture in either direction; this list is what stops
// the form offering it in the first place.
const EXTERNAL_TRUST_POLICIES = [
	"external_integrity",
	"external_signed",
	"external_org_allowlist",
	"external_deny",
] as const;
const REGISTRY_TYPES = ["local", "https", "github"] as const;
const AUTH_TYPES = ["none", "bearer", "api_key_header"] as const;

type TrustPolicyOption = (typeof TRUST_POLICIES)[number] | (typeof EXTERNAL_TRUST_POLICIES)[number];

/**
 * Settings → Skills → Registries. Which outside sources this installation may
 * install packages from, what AO last observed about each, and what they have
 * withdrawn.
 *
 * AO ships with nothing configured and no default endpoint, so the empty state
 * is the real one and says what it means: nothing can be installed from a
 * registry until somebody adds one.
 *
 * Five things here are rendered from the daemon rather than decided in React.
 *
 * **A trust policy this build cannot satisfy is marked as such** — one
 * requiring a verified signature, which AO does not verify — because the
 * strictest-looking setting must not read as if it were working.
 *
 * **A connection verdict is one of six, and CONNECTED is the hardest to
 * reach.** A socket opening, TLS verifying and a 200 arriving are each
 * necessary and none is sufficient; the daemon decides, and this screen renders
 * the word it was given plus the daemon's own explaining sentence. There is no
 * rule here that could turn any other state green.
 *
 * **"Never tested" is a real state**, shown as itself rather than as a failure.
 * A red badge for something nobody has tested teaches people that red means
 * nothing.
 *
 * **The credential is a NAME.** The field takes the name of a sealed secret,
 * the row shows a name, and no value is ever on this wire — not omitted from
 * the response, simply never loaded.
 *
 * **The private-range exception is shown.** It is the one setting that widens
 * what AO may connect to, and an exception nobody can see is one nobody
 * reviews.
 *
 * The revocation policy's exact non-promises come from the daemon too: AO marks
 * a withdrawn release and blocks new installs of it, and does not uninstall it,
 * disable it anywhere, or stop a run already under way.
 */
export function SkillRegistriesSettingsSection() {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [error, setError] = useState<string | null>(null);
	// Keyed by registry id, and explicitly possibly-absent: most registries
	// have not been tested in this session, and a map that claimed otherwise
	// would let `probe.state` typecheck where there is no probe.
	const [probes, setProbes] = useState<Record<string, Probe | undefined>>({});
	const [syncs, setSyncs] = useState<Record<string, RevocationSync | undefined>>({});
	const [form, setForm] = useState({
		id: "",
		displayName: "",
		type: "local" as (typeof REGISTRY_TYPES)[number],
		location: "",
		trustPolicy: "digest" as TrustPolicyOption,
		pinnedPublisher: "",
		priority: "100",
		authType: "none" as (typeof AUTH_TYPES)[number],
		credentialSecretName: "",
		apiKeyHeader: "",
		privateCidrs: "",
		owner: "",
		repository: "",
		allowedOwners: "",
	});

	const key = ["skills", "registries"] as const;
	const registries = useQuery({
		queryKey: key,
		queryFn: async () => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/skills/registries", {
				credentials: "include",
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
	});

	// AO polls nothing on its own. This runs when the panel is open, which is a
	// request the person made by opening it.
	const updates = useQuery<UpdateCheck>({
		queryKey: ["skills", "updates"],
		queryFn: async () => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/skills/updates", {
				credentials: "include",
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
	});

	const invalidate = () => {
		void queryClient.invalidateQueries({ queryKey: key });
		void queryClient.invalidateQueries({ queryKey: ["skills", "updates"] });
	};

	const isHttps = form.type === "https";
	// An external registry is a different kind of thing, and the form says so
	// rather than reusing the private-registry controls: it takes an owner and
	// a repository instead of a credential header, and only the external trust
	// policies.
	const isExternal = form.type === "github";
	const policies: readonly TrustPolicyOption[] = isExternal
		? EXTERNAL_TRUST_POLICIES
		: TRUST_POLICIES;
	const save = useMutation({
		mutationFn: async () => {
			const { error: apiError } = await apiClient.PUT("/api/v1/skills/registries/{registryId}", {
				credentials: "include",
				params: { path: { registryId: form.id.trim() } },
				body: {
					displayName: form.displayName.trim(),
					type: form.type,
					location: form.location.trim(),
					enabled: true,
					trustPolicy: form.trustPolicy,
					// The publisher pin is required by pinned_publisher and
					// OPTIONAL on an external registry, where it is the "this
					// repository must keep publishing as acme" control.
					...((form.trustPolicy === "pinned_publisher" || isExternal) &&
					form.pinnedPublisher.trim()
						? { pinnedPublisher: form.pinnedPublisher.trim() }
						: {}),
					...(isExternal
						? {
								owner: form.owner.trim(),
								...(form.repository.trim() ? { repository: form.repository.trim() } : {}),
								...(form.trustPolicy === "external_org_allowlist"
									? {
											allowedOwners: form.allowedOwners
												.split(",")
												.map((entry) => entry.trim())
												.filter(Boolean),
										}
									: {}),
							}
						: {}),
					priority: Number.parseInt(form.priority, 10) || 100,
					// A local directory authenticates to nothing and opens no
					// socket, so neither field is sent for one: the daemon
					// refuses both, and sending them would be asking for a
					// refusal this form could have avoided.
					...(isHttps && form.authType !== "none"
						? {
								authType: form.authType,
								credentialSecretName: form.credentialSecretName.trim(),
								...(form.authType === "api_key_header" && form.apiKeyHeader.trim()
									? { apiKeyHeader: form.apiKeyHeader.trim() }
									: {}),
							}
						: {}),
					...(isHttps && form.privateCidrs.trim()
						? {
								permittedPrivateCidrs: form.privateCidrs
									.split(",")
									.map((entry) => entry.trim())
									.filter(Boolean),
							}
						: {}),
				},
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			setForm({
				id: "",
				displayName: "",
				type: "local",
				location: "",
				trustPolicy: "digest",
				pinnedPublisher: "",
				priority: "100",
				authType: "none",
				credentialSecretName: "",
				apiKeyHeader: "",
				privateCidrs: "",
				owner: "",
				repository: "",
				allowedOwners: "",
			});
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const remove = useMutation({
		mutationFn: async (id: string) => {
			const { error: apiError } = await apiClient.DELETE("/api/v1/skills/registries/{registryId}", {
				credentials: "include",
				params: { path: { registryId: id } },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	// A connection test reads one small metadata endpoint. It downloads
	// nothing, installs nothing and enables nothing — and the daemon says so in
	// the sentence this screen renders under the verdict.
	const test = useMutation({
		mutationFn: async (id: string) => {
			const { data, error: apiError } = await apiClient.POST(
				"/api/v1/skills/registries/{registryId}/test",
				{ credentials: "include", params: { path: { registryId: id } } },
			);
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
		onSuccess: (data) => {
			setError(null);
			setProbes((current) => ({ ...current, [data.registryId]: data }));
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const syncRevocations = useMutation({
		mutationFn: async (id: string) => {
			const { data, error: apiError } = await apiClient.POST(
				"/api/v1/skills/registries/{registryId}/revocations/sync",
				{ credentials: "include", params: { path: { registryId: id } } },
			);
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
		onSuccess: (data) => {
			setError(null);
			setSyncs((current) => ({ ...current, [data.registryId]: data }));
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const rows = registries.data?.registries ?? [];
	const statuses = updates.data?.statuses ?? [];
	const revoked = statuses.filter((s) => s.revokedNow);
	const complete =
		form.id.trim() !== "" &&
		form.displayName.trim() !== "" &&
		form.location.trim() !== "" &&
		(form.trustPolicy !== "pinned_publisher" || form.pinnedPublisher.trim() !== "") &&
		(!isHttps || form.authType === "none" || form.credentialSecretName.trim() !== "") &&
		(!isExternal || form.owner.trim() !== "") &&
		// An empty allowlist under the allowlist policy would refuse
		// everything and read as configured. The daemon says so too; the form
		// simply does not let it be submitted.
		(form.trustPolicy !== "external_org_allowlist" || form.allowedOwners.trim() !== "");

	return (
		<SettingsSection title={t("settings.skillRegistries.title")} data-testid="skill-registries-settings">
			<p className="text-caption text-settings-muted">{t("settings.skillRegistries.intro")}</p>

			{error ? (
				<p className="flex items-start gap-2 text-caption text-error" role="alert">
					<CircleAlert className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
					{error}
				</p>
			) : null}

			{rows.length === 0 ? (
				// The empty state says what it MEANS, not just that a list is empty.
				<p className="text-caption text-settings-muted">{t("settings.skillRegistries.empty")}</p>
			) : (
				<ul className="flex flex-col gap-2" data-testid="skill-registries">
					{rows.map((reg: Registry) => {
						const probe = probes[reg.id];
						const sync = syncs[reg.id];
						// The verdict shown is the one just returned, falling back
						// to what the daemon recorded. Neither is computed here.
						const probeState = probe?.state ?? reg.status?.lastProbeState ?? "";
						const probeDetail = probe?.detail ?? reg.status?.lastProbeDetail ?? "";
						return (
							<li
								className="flex flex-col gap-1 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] p-3"
								key={reg.id}
							>
								<div className="flex flex-wrap items-center gap-2">
									<span className="text-caption font-medium">{reg.displayName}</span>
									<span className="text-caption text-settings-muted">{reg.id}</span>
									<Badge variant={reg.enabled ? "success" : "warning"}>
										{reg.enabled
											? t("settings.skillRegistries.enabled")
											: t("settings.skillRegistries.disabled")}
									</Badge>
									{/* "Never tested" is a state, not a failure. */}
									<Badge
										variant={
											probeState === ""
												? "outline"
												: probeState === "CONNECTED"
													? "success"
													: "error"
										}
										data-testid={`skill-registry-probe-${reg.id}`}
									>
										{probeState === ""
											? t("settings.skillRegistries.neverTested")
											: t(`settings.skillRegistries.probe.${probeState}`)}
									</Badge>
									{reg.external ? (
										<Badge variant="warning" data-testid={`skill-registry-external-${reg.id}`}>
											{t("settings.skillRegistries.externalBadge")}
										</Badge>
									) : null}
									{reg.tenantId ? (
										<Badge variant="outline">
											{t("settings.skillRegistries.tenant", { tenant: reg.tenantId })}
										</Badge>
									) : null}
								</div>
								<p className="break-all text-caption text-settings-muted">
									{t("settings.skillRegistries.location", {
										type: reg.type,
										location: reg.location,
									})}
								</p>
								{/* Which account and repository AO reads, always named. With
								    an external registry "where does this come from" is the
								    first question, and the answer is not the location. */}
								{reg.external && reg.owner ? (
									<p className="break-all text-caption text-settings-muted">
										{t("settings.skillRegistries.scope", {
											scope: reg.repository ? `${reg.owner}/${reg.repository}` : reg.owner,
										})}
									</p>
								) : null}
								{/* An allowlist nobody can see is one nobody reviews. */}
								{reg.allowedOwners && reg.allowedOwners.length > 0 ? (
									<p className="break-all text-caption text-settings-muted">
										{t("settings.skillRegistries.allowlistRow", {
											owners: reg.allowedOwners.join(", "),
										})}
									</p>
								) : null}
								{/* The sentence that must never be left implicit. */}
								{reg.external ? (
									<p className="text-caption text-warning">
										{t("settings.skillRegistries.externalNote")}
									</p>
								) : null}
								<p className="text-caption text-settings-muted">
									{t(`settings.skillRegistries.policy.${reg.trustPolicy}`, {
										publisher: reg.pinnedPublisher ?? "",
									})}
								</p>
								{/* A policy that cannot be satisfied must not read as if
								    it were working: it installs nothing at all. The
								    daemon decides this -- it is the thing that knows
								    whether a matching trust root exists. */}
								{!reg.trustPolicyEnforceable ? (
									<p className="text-caption text-warning">
										{t("settings.skillRegistries.policyUnenforceable")}
									</p>
								) : null}
								{/* The secret NAME, whenever the row carries one. Never a
								    value: none is on this wire at all. */}
								{reg.credentialSecretName ? (
									<p className="text-caption text-settings-muted">
										{t("settings.skillRegistries.credential", {
											name: reg.credentialSecretName,
										})}
									</p>
								) : null}
								{/* And HOW it is presented, which is a separate fact: a
								    registry can name a credential and be configured not
								    to send it, and that is a misconfiguration worth
								    seeing rather than inferring. */}
								{reg.authType && reg.authType !== "none" ? (
									<p className="text-caption text-settings-muted">
										{t("settings.skillRegistries.auth", {
											type: reg.authType,
											name: reg.credentialSecretName ?? "",
										})}
									</p>
								) : null}
								{/* An exception nobody can see is one nobody reviews. */}
								{reg.type === "https" && reg.networkPolicySummary ? (
									<p className="text-caption text-settings-muted">
										{t("settings.skillRegistries.network", {
											summary: reg.networkPolicySummary,
										})}
									</p>
								) : null}
								{probeDetail ? (
									<p
										className={
											probeState === "CONNECTED"
												? "text-caption text-settings-muted"
												: "text-caption text-warning"
										}
										data-testid={`skill-registry-probe-detail-${reg.id}`}
									>
										{probeDetail}
									</p>
								) : null}
								{reg.status?.lastSyncAt ? (
									<p className="text-caption text-settings-muted">
										{t("settings.skillRegistries.lastSync", {
											at: new Date(reg.status.lastSyncAt).toLocaleString(),
										})}
									</p>
								) : null}
								{reg.status?.lastRevocationSyncAt ? (
									<p className="text-caption text-settings-muted">
										{t("settings.skillRegistries.lastRevocationSync", {
											at: new Date(reg.status.lastRevocationSyncAt).toLocaleString(),
										})}
									</p>
								) : null}

								{sync ? (
									<div className="flex flex-col gap-1" data-testid={`skill-revocations-${reg.id}`}>
										<p className="text-caption font-medium">
											{t("settings.skillRegistries.revocationsTitle")}
										</p>
										{sync.unreachable ? (
											// "AO could not ask" and "nothing is revoked"
											// are different answers.
											<p className="text-caption text-warning">
												{t("settings.skillRegistries.syncUnreachable", {
													reason: sync.unreachable,
												})}
											</p>
										) : null}
										{sync.revocations.length === 0 && !sync.unreachable ? (
											<p className="text-caption text-settings-muted">
												{t("settings.skillRegistries.revocationsNone")}
											</p>
										) : null}
										{sync.revocations.map((rev) => (
											<p
												className={
													rev.installed
														? "text-caption text-error"
														: "text-caption text-settings-muted"
												}
												key={`${rev.skillId}@${rev.version}`}
											>
												{t("settings.skillRegistries.revocationRow", {
													skill: rev.skillId,
													version: rev.version,
													reason: rev.reason,
												})}
												{rev.installed
													? ` — ${t("settings.skillRegistries.revocationInstalled")}`
													: ""}
											</p>
										))}
										{sync.revocations.some((rev) => rev.installed) && sync.policy ? (
											// The non-promises matter as much as the promise.
											<p className="text-caption text-warning">{sync.policy}</p>
										) : null}
									</div>
								) : null}

								<div className="flex flex-wrap items-center gap-2">
									<Button
										variant="secondary"
										size="sm"
										disabled={test.isPending}
										onClick={() => test.mutate(reg.id)}
										data-testid={`skill-registry-test-${reg.id}`}
									>
										{test.isPending
											? t("settings.skillRegistries.testing")
											: t("settings.skillRegistries.testAction")}
									</Button>
									{reg.type === "https" ? (
										<Button
											variant="secondary"
											size="sm"
											disabled={syncRevocations.isPending}
											onClick={() => syncRevocations.mutate(reg.id)}
											data-testid={`skill-registry-sync-${reg.id}`}
										>
											{syncRevocations.isPending
												? t("settings.skillRegistries.syncing")
												: t("settings.skillRegistries.syncRevocationsAction")}
										</Button>
									) : null}
									<Button
										variant="secondary"
										size="sm"
										disabled={remove.isPending}
										onClick={() => remove.mutate(reg.id)}
									>
										{t("settings.skillRegistries.removeAction")}
									</Button>
								</div>
							</li>
						);
					})}
				</ul>
			)}

			{registries.data?.trustModel ? (
				<p className="text-caption text-settings-muted" data-testid="skill-registry-trust-model">
					{registries.data.trustModel}
				</p>
			) : null}

			{/* What the registries say about what is already installed. */}
			{statuses.length > 0 ? (
				<div className="flex flex-col gap-1" data-testid="skill-update-statuses">
					<p className="text-caption font-medium">{t("settings.skillRegistries.installedTitle")}</p>
					{statuses.map((status) => (
						<p className="text-caption text-settings-muted" key={`${status.skillId}@${status.version}`}>
							{status.skillId}@{status.version} —{" "}
							{status.revokedNow
								? t("settings.skillRegistries.statusRevoked", {
										reason: status.revocationReason ?? "",
									})
								: status.unreachable
									? status.unreachable
									: status.updateAvailable
										? t("settings.skillRegistries.statusUpdate", {
												version: status.latestVersion ?? "",
											})
										: t("settings.skillRegistries.statusCurrent")}
						</p>
					))}
					{/* The non-promises matter as much as the promise. */}
					{revoked.length > 0 && updates.data?.revocationPolicy ? (
						<p className="text-caption text-warning" data-testid="skill-revocation-policy">
							{updates.data.revocationPolicy}
						</p>
					) : null}
				</div>
			) : null}

			<div className="flex flex-col gap-2" data-testid="skill-registry-add-form">
				<p className="text-caption font-medium">{t("settings.skillRegistries.addTitle")}</p>
				<p className="text-caption text-settings-muted">{t("settings.skillRegistries.addNote")}</p>
				<div className="grid grid-cols-2 gap-2">
					<label className="flex flex-col gap-1 text-caption">
						{t("settings.skillRegistries.id")}
						<Input
							value={form.id}
							placeholder="company-private"
							onChange={(e) => setForm({ ...form, id: e.target.value })}
						/>
					</label>
					<label className="flex flex-col gap-1 text-caption">
						{t("settings.skillRegistries.name")}
						<Input
							value={form.displayName}
							onChange={(e) => setForm({ ...form, displayName: e.target.value })}
						/>
					</label>
				</div>
				<div className="grid grid-cols-2 gap-2">
					<label className="flex flex-col gap-1 text-caption">
						{t("settings.skillRegistries.type")}
						<Select
							value={form.type}
							onValueChange={(value) =>
								setForm({ ...form, type: value as (typeof REGISTRY_TYPES)[number] })
							}
						>
							<SelectTrigger aria-label={t("settings.skillRegistries.type")}>
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								{REGISTRY_TYPES.map((option) => (
									<SelectItem key={option} value={option}>
										{t(`settings.skillRegistries.typeOption.${option}`)}
									</SelectItem>
								))}
							</SelectContent>
						</Select>
					</label>
					<label className="flex flex-col gap-1 text-caption">
						{t("settings.skillRegistries.priority")}
						<Input
							value={form.priority}
							onChange={(e) => setForm({ ...form, priority: e.target.value })}
						/>
					</label>
				</div>
				<label className="flex flex-col gap-1 text-caption">
					{t("settings.skillRegistries.locationLabel")}
					<Input
						value={form.location}
						placeholder={
							isExternal
								? "https://api.github.com"
								: isHttps
									? "https://registry.corp.example"
									: "/srv/ao/registry"
						}
						onChange={(e) => setForm({ ...form, location: e.target.value })}
					/>
				</label>
				{/* Outside the label on purpose: a label's text IS the field's
				    accessible name, and guidance folded into one renames the
				    field for anybody reading it with a screen reader. */}
				<p className="text-caption text-settings-muted">
					{isExternal
						? t("settings.skillRegistries.locationHelpGithub")
						: isHttps
							? t("settings.skillRegistries.locationHelpHttps")
							: t("settings.skillRegistries.locationHelpLocal")}
				</p>

				{/* An external registry reads an account, not a credential header.
				    Owner is required; an empty repository means every repository
				    the owner has that publishes AO releases, scanned under a hard
				    request budget rather than without bound. */}
				{isExternal ? (
					<>
						<div className="grid grid-cols-2 gap-2">
							<label className="flex flex-col gap-1 text-caption">
								{t("settings.skillRegistries.owner")}
								<Input
									value={form.owner}
									placeholder="acme"
									onChange={(e) => setForm({ ...form, owner: e.target.value })}
								/>
							</label>
							<label className="flex flex-col gap-1 text-caption">
								{t("settings.skillRegistries.repository")}
								<Input
									value={form.repository}
									placeholder="skills"
									onChange={(e) => setForm({ ...form, repository: e.target.value })}
								/>
							</label>
						</div>
						<p className="text-caption text-settings-muted">
							{t("settings.skillRegistries.repositoryHelp")}
						</p>
						<label className="flex flex-col gap-1 text-caption">
							{t("settings.skillRegistries.pinnedPublisher")}
							<Input
								value={form.pinnedPublisher}
								onChange={(e) => setForm({ ...form, pinnedPublisher: e.target.value })}
							/>
						</label>
						<p className="text-caption text-settings-muted">
							{t("settings.skillRegistries.externalPublisherHelp")}
						</p>
					</>
				) : null}

				{/* A local directory authenticates to nothing and opens no socket,
				    so neither control is offered for one. */}
				{isHttps ? (
					<>
						<div className="grid grid-cols-2 gap-2">
							<label className="flex flex-col gap-1 text-caption">
								{t("settings.skillRegistries.authType")}
								<Select
									value={form.authType}
									onValueChange={(value) =>
										setForm({ ...form, authType: value as (typeof AUTH_TYPES)[number] })
									}
								>
									<SelectTrigger aria-label={t("settings.skillRegistries.authType")}>
										<SelectValue />
									</SelectTrigger>
									<SelectContent>
										{AUTH_TYPES.map((option) => (
											<SelectItem key={option} value={option}>
												{t(`settings.skillRegistries.authOption.${option}`)}
											</SelectItem>
										))}
									</SelectContent>
								</Select>
							</label>
							{form.authType === "api_key_header" ? (
								<div className="flex flex-col gap-1">
									<label className="flex flex-col gap-1 text-caption">
										{t("settings.skillRegistries.apiKeyHeader")}
										<Input
											value={form.apiKeyHeader}
											placeholder="X-API-Key"
											onChange={(e) => setForm({ ...form, apiKeyHeader: e.target.value })}
										/>
									</label>
									<p className="text-caption text-settings-muted">
										{t("settings.skillRegistries.apiKeyHeaderHelp")}
									</p>
								</div>
							) : null}
						</div>
						{form.authType !== "none" ? (
							<div className="flex flex-col gap-1">
								<label className="flex flex-col gap-1 text-caption">
									{t("settings.skillRegistries.credentialSecret")}
									<Input
										value={form.credentialSecretName}
										placeholder="CORP_REGISTRY_TOKEN"
										onChange={(e) =>
											setForm({ ...form, credentialSecretName: e.target.value })
										}
									/>
								</label>
								{/* The field takes a NAME. There is no field anywhere
								    on this screen that takes a credential value. */}
								<p className="text-caption text-settings-muted">
									{t("settings.skillRegistries.credentialSecretHelp")}
								</p>
							</div>
						) : null}
						<label className="flex flex-col gap-1 text-caption">
							{t("settings.skillRegistries.privateCidrs")}
							<Input
								value={form.privateCidrs}
								placeholder="10.4.0.0/16"
								onChange={(e) => setForm({ ...form, privateCidrs: e.target.value })}
							/>
						</label>
						<p className="text-caption text-settings-muted">
							{t("settings.skillRegistries.privateCidrsHelp")}
						</p>
					</>
				) : null}

				<div className="grid grid-cols-2 gap-2">
					<label className="flex flex-col gap-1 text-caption">
						{t("settings.skillRegistries.trustPolicy")}
						<Select
							value={form.trustPolicy}
							onValueChange={(value) =>
								setForm({ ...form, trustPolicy: value as TrustPolicyOption })
							}
						>
							<SelectTrigger aria-label={t("settings.skillRegistries.trustPolicy")}>
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								{policies.map((policy) => (
									<SelectItem key={policy} value={policy}>
										{t(`settings.skillRegistries.policyOption.${policy}`)}
									</SelectItem>
								))}
							</SelectContent>
						</Select>
					</label>
					{form.trustPolicy === "pinned_publisher" ? (
						<label className="flex flex-col gap-1 text-caption">
							{t("settings.skillRegistries.pinnedPublisher")}
							<Input
								value={form.pinnedPublisher}
								onChange={(e) => setForm({ ...form, pinnedPublisher: e.target.value })}
							/>
						</label>
					) : null}
				</div>
				{/* "signed" works today against a trust root configured in
				    Settings -> Skills -> Trust; "official" does not, because this
				    build carries no AO Official root. Two different warnings,
				    because they send a person to two different places. */}
				{form.trustPolicy === "signed" ? (
					<p className="text-caption text-settings-muted">
						{t("settings.skillRegistries.policyNeedsTrustRoot")}
					</p>
				) : null}
				{form.trustPolicy === "official" ? (
					<p className="text-caption text-warning">
						{t("settings.skillRegistries.policyOfficialUnavailable")}
					</p>
				) : null}
				{/* The allowlist itself, offered only by the policy that enforces
				    it: a list nothing checks is a control an administrator
				    believes they have. Plain owner names only -- a wildcard here
				    is how an organization allowlist becomes an everything
				    allowlist, and the daemon refuses one outright. */}
				{form.trustPolicy === "external_org_allowlist" ? (
					<>
						<label className="flex flex-col gap-1 text-caption">
							{t("settings.skillRegistries.allowedOwners")}
							<Input
								value={form.allowedOwners}
								placeholder="acme, globex"
								onChange={(e) => setForm({ ...form, allowedOwners: e.target.value })}
							/>
						</label>
						<p className="text-caption text-settings-muted">
							{t("settings.skillRegistries.allowedOwnersHelp")}
						</p>
					</>
				) : null}
				{/* Every external policy gets the same sentence, because the
				    dangerous reading is the same for all four. */}
				{isExternal ? (
					<p className="text-caption text-warning" data-testid="skill-registry-external-help">
						{t("settings.skillRegistries.policyExternalNote")}
					</p>
				) : null}
				<Button
					variant="primary"
					size="sm"
					className="self-start"
					disabled={!complete || save.isPending}
					onClick={() => save.mutate()}
				>
					{save.isPending
						? t("settings.skillRegistries.adding")
						: t("settings.skillRegistries.addAction")}
				</Button>
			</div>
		</SettingsSection>
	);
}
