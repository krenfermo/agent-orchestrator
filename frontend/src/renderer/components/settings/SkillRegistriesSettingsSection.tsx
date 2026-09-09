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

const TRUST_POLICIES = ["digest", "pinned_publisher", "signed"] as const;

/**
 * Settings → Skills → Registries. Which outside sources this installation may
 * install packages from, and what each installed package's provenance says.
 *
 * AO ships with nothing configured and no default endpoint, so the empty state
 * is the real one and says what it means: nothing can be installed from a
 * registry until somebody adds one.
 *
 * Two things are rendered from the daemon rather than decided here. A trust
 * policy this build cannot satisfy — one requiring a verified signature, which
 * AO does not verify — is marked as such, because the strictest-looking setting
 * must not read as if it were working. And the revocation policy's exact
 * non-promises come with the update check: AO marks a withdrawn release and
 * blocks new installs of it, and does not uninstall it, disable it anywhere, or
 * stop a run already under way.
 *
 * The credential field takes the NAME of a sealed secret and never a value.
 */
export function SkillRegistriesSettingsSection() {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [error, setError] = useState<string | null>(null);
	const [form, setForm] = useState({
		id: "",
		displayName: "",
		location: "",
		trustPolicy: "digest" as (typeof TRUST_POLICIES)[number],
		pinnedPublisher: "",
		priority: "100",
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

	const save = useMutation({
		mutationFn: async () => {
			const { error: apiError } = await apiClient.PUT("/api/v1/skills/registries/{registryId}", {
				credentials: "include",
				params: { path: { registryId: form.id.trim() } },
				body: {
					displayName: form.displayName.trim(),
					// A local directory is the only type this build can read. The
					// daemon refuses the others rather than storing a registry it
					// would answer every search about with silence.
					type: "local",
					location: form.location.trim(),
					enabled: true,
					trustPolicy: form.trustPolicy,
					...(form.trustPolicy === "pinned_publisher"
						? { pinnedPublisher: form.pinnedPublisher.trim() }
						: {}),
					priority: Number.parseInt(form.priority, 10) || 100,
				},
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setError(null);
			setForm({
				id: "",
				displayName: "",
				location: "",
				trustPolicy: "digest",
				pinnedPublisher: "",
				priority: "100",
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

	const rows = registries.data?.registries ?? [];
	const statuses = updates.data?.statuses ?? [];
	const revoked = statuses.filter((s) => s.revokedNow);
	const complete =
		form.id.trim() !== "" &&
		form.displayName.trim() !== "" &&
		form.location.trim() !== "" &&
		(form.trustPolicy !== "pinned_publisher" || form.pinnedPublisher.trim() !== "");

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
					{rows.map((reg: Registry) => (
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
								{reg.tenantId ? (
									<Badge variant="outline">
										{t("settings.skillRegistries.tenant", { tenant: reg.tenantId })}
									</Badge>
								) : null}
							</div>
							<p className="break-all text-caption text-settings-muted">
								{t("settings.skillRegistries.location", { type: reg.type, location: reg.location })}
							</p>
							<p className="text-caption text-settings-muted">
								{t(`settings.skillRegistries.policy.${reg.trustPolicy}`, {
									publisher: reg.pinnedPublisher ?? "",
								})}
							</p>
							{/* The strictest-looking setting must not read as if it
							    were working: it installs nothing at all. */}
							{!reg.trustPolicyEnforceable ? (
								<p className="text-caption text-warning">
									{t("settings.skillRegistries.policyUnenforceable")}
								</p>
							) : null}
							{reg.credentialSecretName ? (
								<p className="text-caption text-settings-muted">
									{t("settings.skillRegistries.credential", {
										name: reg.credentialSecretName,
									})}
								</p>
							) : null}
							<Button
								variant="secondary"
								size="sm"
								className="self-start"
								disabled={remove.isPending}
								onClick={() => remove.mutate(reg.id)}
							>
								{t("settings.skillRegistries.removeAction")}
							</Button>
						</li>
					))}
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
				<label className="flex flex-col gap-1 text-caption">
					{t("settings.skillRegistries.locationLabel")}
					<Input
						value={form.location}
						placeholder="/srv/ao/registry"
						onChange={(e) => setForm({ ...form, location: e.target.value })}
					/>
				</label>
				<div className="grid grid-cols-2 gap-2">
					<label className="flex flex-col gap-1 text-caption">
						{t("settings.skillRegistries.trustPolicy")}
						<Select
							value={form.trustPolicy}
							onValueChange={(value) =>
								setForm({ ...form, trustPolicy: value as (typeof TRUST_POLICIES)[number] })
							}
						>
							<SelectTrigger aria-label={t("settings.skillRegistries.trustPolicy")}>
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								{TRUST_POLICIES.map((policy) => (
									<SelectItem key={policy} value={policy}>
										{t(`settings.skillRegistries.policyOption.${policy}`)}
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
				{form.trustPolicy === "pinned_publisher" ? (
					<label className="flex flex-col gap-1 text-caption">
						{t("settings.skillRegistries.pinnedPublisher")}
						<Input
							value={form.pinnedPublisher}
							onChange={(e) => setForm({ ...form, pinnedPublisher: e.target.value })}
						/>
					</label>
				) : null}
				{form.trustPolicy === "signed" ? (
					<p className="text-caption text-warning">
						{t("settings.skillRegistries.policyUnenforceable")}
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
