import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert, Loader2, ShieldAlert, ShieldCheck } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import type { components } from "../../../api/schema";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "../ui/select";
import { Switch } from "../ui/switch";
import { SettingsSection } from "./SettingsSection";

type ProjectSkills = components["schemas"]["ProjectSkillsResponse"];
type SkillCatalog = components["schemas"]["SkillListResponse"];
type SkillInstall = components["schemas"]["SkillInstallView"];
type SkillCapability = components["schemas"]["SkillCapabilityView"];
type SkillActivation = components["schemas"]["SkillActivationView"];
type SkillDryRun = components["schemas"]["SkillDryRunResponse"];

/**
 * Project settings → Skills. Which skills are enabled on THIS project, with
 * what capabilities, and what a run of each mode would need.
 *
 * Three things about this screen are deliberate:
 *
 * The capability list, its risk ratings and the permission each one is gated on
 * all come from the daemon's response, never from a table in React. A manifest
 * may not describe its own capability as cheaper than it is, and neither may
 * this component — if the capability policy changes in Go, this screen follows
 * without an edit.
 *
 * The controls render from `permissions` in the response — the caller's own
 * effective permissions on this project — rather than from a role name this
 * component would have to interpret. A viewer sees the state read-only.
 *
 * The dry run is the only "run"-shaped control here, and it runs nothing. AO
 * has no isolated runner, so a mode needing containment reports blocked with
 * the reason. The panel shows that refusal plainly instead of hiding the mode:
 * "this cannot run yet, and here is exactly what is missing" is the answer
 * somebody came to this screen for.
 */
export function ProjectSkillsSettingsSection({ projectId }: { projectId: string }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const skillsKey = ["skills", "project", projectId] as const;
	const [error, setError] = useState<string | null>(null);
	const [selectedSkill, setSelectedSkill] = useState<string>("");
	const [grant, setGrant] = useState<Record<string, boolean>>({});
	const [dryRunMode, setDryRunMode] = useState<Record<string, string>>({});
	const [dryRun, setDryRun] = useState<SkillDryRun | null>(null);

	const skills = useQuery({
		queryKey: skillsKey,
		queryFn: async (): Promise<ProjectSkills> => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/projects/{id}/skills", {
				credentials: "include",
				params: { path: { id: projectId } },
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
	});

	// The capability policy is installation-wide and gated on settings.read. A
	// project administrator may not hold it, so a refused read simply leaves
	// the grant editor showing capability names without their risk copy rather
	// than breaking the screen.
	const catalog = useQuery({
		queryKey: ["skills", "catalog"] as const,
		queryFn: async (): Promise<SkillCatalog> => {
			const { data, error: apiError } = await apiClient.GET("/api/v1/skills", {
				credentials: "include",
			});
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
		enabled: skills.isSuccess,
		retry: false,
	});

	const permissions = skills.data?.permissions ?? [];
	const canManage = permissions.includes("project.manage");
	const installed = skills.data?.installed ?? [];
	const activations = skills.data?.activations ?? [];
	const capabilityPolicy = useMemo(() => {
		const byName = new Map<string, SkillCapability>();
		for (const cap of catalog.data?.capabilities ?? []) byName.set(cap.name, cap);
		return byName;
	}, [catalog.data]);

	const invalidate = () => {
		void queryClient.invalidateQueries({ queryKey: skillsKey });
	};

	const enable = useMutation({
		mutationFn: async ({ skill, version }: { skill: string; version: string }) => {
			const capabilities = Object.entries(grant)
				.filter(([, on]) => on)
				.map(([name]) => name);
			const { error: apiError } = await apiClient.PUT("/api/v1/projects/{id}/skills/{skillId}", {
				credentials: "include",
				params: { path: { id: projectId, skillId: skill } },
				body: { version, capabilities },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setSelectedSkill("");
			setGrant({});
			setError(null);
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const disable = useMutation({
		mutationFn: async (skill: string) => {
			const { error: apiError } = await apiClient.DELETE("/api/v1/projects/{id}/skills/{skillId}", {
				credentials: "include",
				params: { path: { id: projectId, skillId: skill } },
			});
			if (apiError) throw new Error(apiErrorMessage(apiError));
		},
		onSuccess: () => {
			setDryRun(null);
			setError(null);
			invalidate();
		},
		onError: (err: Error) => setError(err.message),
	});

	const check = useMutation({
		mutationFn: async ({ skill, modeId }: { skill: string; modeId: string }): Promise<SkillDryRun> => {
			const { data, error: apiError } = await apiClient.POST(
				"/api/v1/projects/{id}/skills/{skillId}/dry-run",
				{
					credentials: "include",
					params: { path: { id: projectId, skillId: skill } },
					body: { modeId, inputs: { mode: modeId } },
				},
			);
			if (apiError || !data) throw new Error(apiErrorMessage(apiError));
			return data;
		},
		onSuccess: (data) => {
			setDryRun(data);
			setError(null);
		},
		onError: (err: Error) => {
			setDryRun(null);
			setError(err.message);
		},
	});

	const riskBadge = (risk: string) => {
		switch (risk) {
			case "critical":
			case "high":
				return "error" as const;
			case "medium":
				return "warning" as const;
			default:
				return "neutral" as const;
		}
	};

	const riskLabel = (risk: string) => {
		switch (risk) {
			case "critical":
				return t("settings.project.skills.risk.critical");
			case "high":
				return t("settings.project.skills.risk.high");
			case "medium":
				return t("settings.project.skills.risk.medium");
			default:
				return t("settings.project.skills.risk.low");
		}
	};

	const verdictLabel = (verdict: string) => {
		switch (verdict) {
			case "executable":
				return t("settings.project.skills.verdict.executable");
			case "requires_approval":
				return t("settings.project.skills.verdict.requiresApproval");
			default:
				return t("settings.project.skills.verdict.blocked");
		}
	};

	const notActivated = installed.filter(
		(skill) => !activations.some((row) => row.skillId === skill.id),
	);
	const pending: SkillInstall | undefined = notActivated.find((skill) => skill.id === selectedSkill);

	const activationRow = (row: SkillActivation) => {
		const pkg = installed.find((skill) => skill.id === row.skillId);
		const modes = pkg?.modes ?? [];
		const mode = dryRunMode[row.skillId] ?? modes[0]?.id ?? "";
		return (
			<li
				key={row.skillId}
				className="flex flex-col gap-2 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] bg-[var(--color-bg-settings-input)] p-3"
				data-testid="project-skill-row"
			>
				<div className="flex items-center gap-3">
					<ShieldCheck className="size-4 shrink-0 text-settings-muted" aria-hidden="true" />
					<span className="min-w-0 flex-1 truncate text-sm text-settings-label">
						{row.skillName || row.skillId}
						<span className="ml-2 text-caption text-settings-muted">{row.version}</span>
					</span>
					<Badge variant={row.enabled ? "success" : "neutral"}>
						{row.enabled
							? t("settings.project.skills.state.enabled")
							: t("settings.project.skills.state.disabled")}
					</Badge>
					{canManage && row.enabled ? (
						<Button
							variant="secondary"
							size="sm"
							disabled={disable.isPending}
							onClick={() => disable.mutate(row.skillId)}
						>
							{t("settings.project.skills.disable")}
						</Button>
					) : null}
				</div>

				<p className="text-caption text-settings-muted">
					{row.grantedCapabilities.length > 0
						? t("settings.project.skills.granted", { list: row.grantedCapabilities.join(", ") })
						: t("settings.project.skills.grantedNone")}
				</p>
				{row.approvedBy ? (
					<p className="text-caption text-settings-muted">
						{t("settings.project.skills.approvedBy", { actor: row.approvedBy })}
					</p>
				) : null}
				{row.unavailable ? (
					<p className="flex items-start gap-2 text-caption text-error">
						<CircleAlert className="mt-0.5 size-3 shrink-0" aria-hidden="true" />
						{t("settings.project.skills.unavailable", { reason: row.unavailable })}
					</p>
				) : null}

				{row.enabled && modes.length > 0 ? (
					<div className="flex items-center gap-2">
						<Select value={mode} onValueChange={(next) => setDryRunMode({ ...dryRunMode, [row.skillId]: next })}>
							<SelectTrigger className="w-56" aria-label={t("settings.project.skills.mode")}>
								<SelectValue placeholder={t("settings.project.skills.mode")} />
							</SelectTrigger>
							<SelectContent>
								{modes.map((m) => (
									<SelectItem key={m.id} value={m.id}>
										{m.name || m.id}
									</SelectItem>
								))}
							</SelectContent>
						</Select>
						<Button
							variant="secondary"
							size="sm"
							disabled={check.isPending || !mode}
							onClick={() => check.mutate({ skill: row.skillId, modeId: mode })}
						>
							{t("settings.project.skills.check")}
						</Button>
					</div>
				) : null}

				{dryRun && dryRun.skillId === row.skillId ? dryRunPanel(dryRun) : null}
			</li>
		);
	};

	const dryRunPanel = (result: SkillDryRun) => (
		<div
			className="flex flex-col gap-2 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] p-3"
			data-testid="project-skill-dry-run"
		>
			<div className="flex items-center gap-2">
				<Badge variant={result.verdict === "executable" ? "success" : "warning"}>
					{verdictLabel(result.verdict)}
				</Badge>
				<span className="text-caption text-settings-muted">
					{result.modeName || result.modeId}
				</span>
				<Badge variant={riskBadge(result.modeRisk)}>{riskLabel(result.modeRisk)}</Badge>
			</div>
			{/* A dry run changes nothing. Saying so on the panel keeps the
			    control from reading like a "run" button. */}
			<p className="text-caption text-settings-muted">{t("settings.project.skills.dryRunNote")}</p>
			<ul className="flex flex-col gap-1">
				{result.decisions.map((decision) => (
					<li key={decision.capability} className="flex items-start gap-2 text-caption">
						{decision.satisfied ? (
							<ShieldCheck className="mt-0.5 size-3 shrink-0 text-success" aria-hidden="true" />
						) : (
							<ShieldAlert className="mt-0.5 size-3 shrink-0 text-error" aria-hidden="true" />
						)}
						<span className="min-w-0 flex-1">
							<span className="text-settings-label">{decision.capability}</span>
							<span className="ml-2 text-settings-muted">
								{decision.satisfied ? decision.description : decision.detail}
							</span>
						</span>
					</li>
				))}
			</ul>
			{result.missingPermissions.length > 0 ? (
				<p className="text-caption text-error">
					{t("settings.project.skills.missingPermissions", {
						list: result.missingPermissions.join(", "),
					})}
				</p>
			) : null}
			{result.runner.needsIsolation || result.runner.needsEgressControl ? (
				<p className="text-caption text-settings-muted">
					{t("settings.project.skills.runnerMissing")}
				</p>
			) : null}
		</div>
	);

	return (
		<SettingsSection title={t("settings.project.skills")} sectionId="project-skills">
			<p className="px-3 text-caption text-settings-muted">
				{t("settings.project.skills.description")}
			</p>

			{skills.isLoading ? (
				<div className="flex items-center gap-2 px-3 py-2 text-caption text-settings-muted">
					<Loader2 className="size-3 animate-spin" aria-hidden="true" />
					{t("settings.project.skills.loading")}
				</div>
			) : skills.error ? (
				<p className="px-3 text-caption text-error">{(skills.error as Error).message}</p>
			) : (
				<ul className="flex flex-col gap-1.5">
					{activations.length === 0 ? (
						<p className="px-3 text-caption text-settings-muted">
							{installed.length === 0
								? t("settings.project.skills.noneInstalled")
								: t("settings.project.skills.noneActivated")}
						</p>
					) : (
						activations.map(activationRow)
					)}
				</ul>
			)}

			{error ? <p className="px-3 text-caption text-error">{error}</p> : null}

			{canManage && notActivated.length > 0 ? (
				<div className="flex flex-col gap-2 rounded-(--radius-settings-dialog-lg) border border-[var(--color-border-settings-input)] bg-[var(--color-bg-settings-input)] p-3">
					<p className="text-caption text-settings-muted">
						{t("settings.project.skills.activateHint")}
					</p>
					<Select
						value={selectedSkill}
						onValueChange={(next) => {
							setSelectedSkill(next);
							setGrant({});
						}}
					>
						<SelectTrigger aria-label={t("settings.project.skills.activate")}>
							<SelectValue placeholder={t("settings.project.skills.activate")} />
						</SelectTrigger>
						<SelectContent>
							{notActivated.map((skill) => (
								<SelectItem key={skill.id} value={skill.id}>
									{skill.name} · {skill.version}
								</SelectItem>
							))}
						</SelectContent>
					</Select>

					{pending ? (
						<>
							<p className="text-caption text-settings-muted">{pending.description}</p>
							<ul className="flex flex-col gap-1.5" data-testid="project-skill-grant">
								{pending.capabilities.map((name) => {
									const policy = capabilityPolicy.get(name);
									const grantable = !policy || permissions.includes(policy.requiredPermission);
									return (
										<li key={name} className="flex items-start gap-3">
											<Switch
												checked={grant[name] === true}
												disabled={!grantable}
												onCheckedChange={(on) => setGrant({ ...grant, [name]: on })}
												aria-label={name}
											/>
											<span className="min-w-0 flex-1">
												<span className="flex items-center gap-2">
													<span className="text-sm text-settings-label">{name}</span>
													{policy ? (
														<Badge variant={riskBadge(policy.risk)}>{riskLabel(policy.risk)}</Badge>
													) : null}
												</span>
												{policy ? (
													<span className="block text-caption text-settings-muted">
														{policy.description}
													</span>
												) : null}
												{/* Two different reasons a capability may be
												    unavailable, and the difference matters: one is
												    about the person, the other about the platform. */}
												{policy && !grantable ? (
													<span className="block text-caption text-error">
														{t("settings.project.skills.cannotGrant", {
															permission: policy.requiredPermission,
														})}
													</span>
												) : null}
												{policy?.requiresIsolation ? (
													<span className="block text-caption text-settings-muted">
														{t("settings.project.skills.needsRunner")}
													</span>
												) : null}
											</span>
										</li>
									);
								})}
							</ul>
							<Button
								variant="primary"
								size="sm"
								disabled={enable.isPending || !Object.values(grant).some(Boolean)}
								onClick={() => enable.mutate({ skill: pending.id, version: pending.version })}
							>
								{t("settings.project.skills.enable")}
							</Button>
							<p className="text-caption text-settings-muted">
								{t("settings.project.skills.enableNote")}
							</p>
						</>
					) : null}
				</div>
			) : null}
		</SettingsSection>
	);
}
