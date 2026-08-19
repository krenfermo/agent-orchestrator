import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { AlertTriangle, CheckCircle2, Loader2, XCircle } from "lucide-react";
import { apiErrorMessage } from "../../lib/api-client";
import { useCapacity, type CapacitySnapshot } from "../../hooks/useCapacity";
import { useProviderProfiles, type ProviderDescriptor, type ProviderProfile } from "../../hooks/useProviderProfiles";
import { deriveProviderStatus, type ExecutionState } from "../../lib/provider-status";
import { providerVisualIdentity } from "../../lib/provider-visual-identity";
import { cn } from "../../lib/utils";
import { Button } from "../ui/button";
import { Switch } from "../ui/switch";
import { SettingsRow } from "./SettingsRow";
import { SettingsSection } from "./SettingsSection";
import { Badge, type BadgeVariant } from "../ui/badge";

/**
 * Settings → Agents & Models (Checkpoint 8P-B, revised 8P-E.1). Cards render
 * from the provider registry joined against the current user's own provider
 * profiles — never a hardcoded Claude/Codex pair. A provider with no real
 * adapter yet (e.g. MiniMax) renders honestly as unsupported instead of
 * being omitted or shown as if it worked.
 *
 * Checkpoint 8P-E.1: a single Connected/Not-connected badge previously
 * conflated "CLI not installed", "not authenticated", and "disabled" into
 * one ambiguous signal, and Test Connection's result was silently dropped.
 * Every card now shows three distinct axes (Installed / Account / AO
 * execution — see lib/provider-status.ts) and keeps the last Test/Connect
 * result visible until the next action.
 */
export function DevelopmentAgentsSettingsSection({ titleHidden }: { titleHidden?: boolean } = {}) {
	const { t } = useTranslation();
	const { registry, profiles, isLoading, error, createProfile, connect, disconnect, test, setEnabled } = useProviderProfiles();
	const { capacity } = useCapacity();
	const capacityFor = (harness: string) => capacity?.find((c) => c.harness === harness);

	return (
		<SettingsSection title={t("settings.agents")} sectionId="agents" titleHidden={titleHidden} grouped>
			{error && <p className="px-(--size-settings-row-padding) text-xs text-error">{error}</p>}
			{isLoading && !registry && (
				<p className="px-(--size-settings-row-padding) text-xs text-settings-muted">{t("settings.common.checking")}</p>
			)}
			{registry?.map((descriptor) => {
				const profile = profiles?.find((p) => p.provider === descriptor.provider && p.harness === descriptor.harness);
				return (
					<ProviderCard
						key={`${descriptor.provider}:${descriptor.harness}`}
						descriptor={descriptor}
						profile={profile}
						capacity={capacityFor(descriptor.harness)}
						onCreate={() => createProfile({ provider: descriptor.provider, harness: descriptor.harness, displayName: descriptor.displayName })}
						onConnect={(id) => connect(id)}
						onDisconnect={(id) => disconnect(id)}
						onTest={(id) => test(id)}
						onSetEnabled={(id, enabled) => (profile ? setEnabled({ id, profile, enabled }) : undefined)}
					/>
				);
			})}
		</SettingsSection>
	);
}

const EXECUTION_VARIANT: Record<ExecutionState, BadgeVariant> = {
	ready: "success",
	disabled: "neutral",
	setup_required: "warning",
	unavailable: "error",
	capacity_limited: "warning",
	testing: "neutral",
};

type LastResult = { ok: boolean; message: string; at: Date } | undefined;

function ProviderCard({
	descriptor,
	profile,
	capacity,
	onCreate,
	onConnect,
	onDisconnect,
	onTest,
	onSetEnabled,
}: {
	descriptor: ProviderDescriptor;
	profile?: ProviderProfile;
	capacity?: CapacitySnapshot;
	onCreate: () => Promise<ProviderProfile>;
	onConnect: (id: string) => Promise<ProviderProfile>;
	onDisconnect: (id: string) => Promise<ProviderProfile>;
	onTest: (id: string) => Promise<{ ok: boolean; message: string }>;
	onSetEnabled: (id: string, enabled: boolean) => Promise<ProviderProfile> | undefined;
}) {
	const { t } = useTranslation();
	const [busyAction, setBusyAction] = useState<"connect" | "test" | "disconnect" | "enable" | undefined>();
	const [actionError, setActionError] = useState<string | undefined>();
	const [lastResult, setLastResult] = useState<LastResult>(undefined);
	const busy = busyAction !== undefined;

	const identity = providerVisualIdentity(descriptor.provider);
	const status = deriveProviderStatus(descriptor, profile, capacity, busyAction === "connect" || busyAction === "test");

	const runAction = async (action: "connect" | "test" | "disconnect" | "enable", fn: () => Promise<unknown>) => {
		setBusyAction(action);
		setActionError(undefined);
		try {
			const result = await fn();
			if (action === "test" && result && typeof result === "object" && "ok" in result) {
				const r = result as { ok: boolean; message: string };
				setLastResult({ ok: r.ok, message: r.message, at: new Date() });
			} else if (action !== "enable") {
				setLastResult(undefined);
			}
		} catch (err) {
			setActionError(apiErrorMessage(err));
			setLastResult(undefined);
		} finally {
			setBusyAction(undefined);
		}
	};

	const capabilityLabel = (capability: string) => t(`settings.agents.capability.${capability}`, capability);
	const authMethodLabel = (method: string) => t(`settings.agents.authMethod.${method}`, method);

	const effectiveModel = profile?.defaultModel || descriptor.models?.[0];
	const modelDisplay = effectiveModel ?? t("settings.agents.modelSelectedAtRuntime");

	const capacityDisplay = (() => {
		if (!capacity) return t("settings.agents.capacityUnknown");
		switch (capacity.state) {
			case "available":
				return t("settings.agents.capacityAvailable");
			case "limited":
				return t("settings.agents.capacityLimited");
			case "cooldown":
				return capacity.resetAt
					? t("settings.agents.capacityCooldown", { time: new Date(capacity.resetAt).toLocaleString() })
					: t("settings.agents.capacityLimited");
			case "unavailable":
				return t("settings.agents.capacityUnavailable");
			default:
				return t("settings.agents.capacityUnknown");
		}
	})();

	return (
		<div
			className={cn(
				"flex flex-col gap-1.5 rounded-md border-t border-(--color-border-settings-dialog-header) pt-3 first:border-t-0 first:pt-0",
				descriptor.available && "border-l-2 pl-2",
				descriptor.available && identity.accentBorderClass,
			)}
		>
			<div className="settings-row-bar h-auto min-h-(--size-settings-row) flex-wrap gap-2">
				<span className={cn("min-w-0 flex-1 text-sm font-medium leading-5", identity.accentTextClass || "text-settings-label")}>
					{descriptor.displayName}
				</span>
				{!descriptor.available ? (
					<Badge variant="neutral">{t("settings.agents.unsupported")}</Badge>
				) : (
					<Badge variant={EXECUTION_VARIANT[status.execution]}>{executionLabel(t, status.execution)}</Badge>
				)}
				{descriptor.available && profile && (
					<div className="flex items-center gap-1.5">
						<span className="text-xs text-settings-muted">{t("settings.agents.enabled")}</span>
						<Switch
							checked={profile.enabled}
							disabled={busy}
							onCheckedChange={(next) => void runAction("enable", () => onSetEnabled(profile.id, next) ?? Promise.resolve())}
						/>
					</div>
				)}
				{descriptor.available &&
					(profile ? (
						<>
							<Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => void runAction("test", () => onTest(profile.id))}>
								{busyAction === "test" ? (
									<>
										<Loader2 className="size-icon-sm animate-spin" aria-hidden="true" />
										{t("settings.agents.testing")}
									</>
								) : (
									t("settings.agents.testConnection")
								)}
							</Button>
							<Button
								type="button"
								variant="outline"
								size="sm"
								disabled={busy}
								onClick={() => void runAction("disconnect", () => onDisconnect(profile.id))}
							>
								{t("settings.agents.disconnect")}
							</Button>
						</>
					) : (
						<Button
							type="button"
							variant="outline"
							size="sm"
							disabled={busy}
							onClick={() => void runAction("connect", async () => onConnect((await onCreate()).id))}
						>
							{busyAction === "connect" ? (
								<>
									<Loader2 className="size-icon-sm animate-spin" aria-hidden="true" />
									{t("settings.agents.testing")}
								</>
							) : (
								t("settings.agents.connect")
							)}
						</Button>
					))}
			</div>

			{!descriptor.available && descriptor.unavailable && (
				<p className="px-(--size-settings-row-padding) text-xs text-settings-muted">{descriptor.unavailable}</p>
			)}

			{descriptor.available && (
				<>
					{identity.organization && (
						<p className="px-(--size-settings-row-padding) text-xs text-settings-muted">{identity.organization}</p>
					)}

					<SettingsRow label={t("settings.agents.installed")}>
						<span className="flex items-center gap-1.5 text-xs text-settings-muted">
							<StateGlyph ok={status.installed === "installed"} unknown={status.installed === "unknown"} />
							{status.installed === "unknown" ? "—" : status.installed === "installed" ? t("settings.agents.installed") : t("settings.agents.notInstalled")}
						</span>
					</SettingsRow>
					<SettingsRow label={t("settings.agents.account")}>
						<span className="flex items-center gap-1.5 text-xs text-settings-muted">
							<StateGlyph ok={status.account === "connected"} unknown={status.account === "unknown"} />
							{accountLabel(t, status.account)}
						</span>
					</SettingsRow>
					{profile && !profile.enabled && status.account === "connected" && (
						<p className="px-(--size-settings-row-padding) text-xs text-settings-muted">{t("settings.agents.connectedDisabledHint")}</p>
					)}
					{profile && profile.enabled && status.account !== "connected" && status.installed === "installed" && (
						<p className="px-(--size-settings-row-padding) text-xs text-warning">{t("settings.agents.setupRequiredHint")}</p>
					)}

					<SettingsRow label={t("settings.agents.authMethod")}>
						<span className="text-xs text-settings-muted">{authMethodLabel(profile?.authMethod ?? descriptor.authMethods[0] ?? "")}</span>
					</SettingsRow>
					<SettingsRow label={effectiveModel ? t("settings.agents.defaultModel") : t("settings.agents.model")}>
						<span className="text-xs text-settings-muted">{modelDisplay}</span>
					</SettingsRow>
					<SettingsRow label={t("settings.agents.capabilities")}>
						<span className="text-xs text-settings-muted">
							{descriptor.capabilities.length > 0 ? descriptor.capabilities.map(capabilityLabel).join(", ") : "—"}
						</span>
					</SettingsRow>
					<SettingsRow label={t("settings.agents.capacity")}>
						<span className="text-xs text-settings-muted">{capacityDisplay}</span>
					</SettingsRow>
				</>
			)}

			{lastResult && (
				<div
					className={cn(
						"mx-(--size-settings-row-padding) flex items-start gap-1.5 rounded-md border p-2 text-xs",
						lastResult.ok ? "border-success/40 text-success" : "border-warning/40 text-warning",
					)}
				>
					{lastResult.ok ? (
						<CheckCircle2 className="mt-0.5 size-icon-sm shrink-0" aria-hidden="true" />
					) : (
						<AlertTriangle className="mt-0.5 size-icon-sm shrink-0" aria-hidden="true" />
					)}
					<div className="flex flex-col gap-0.5">
						<span className="font-medium">{lastResult.ok ? t("settings.agents.testSuccess") : t("settings.agents.authRequired")}</span>
						<span>{lastResult.message}</span>
						<span className="text-settings-muted">{t("settings.agents.lastChecked", { time: lastResult.at.toLocaleTimeString() })}</span>
					</div>
				</div>
			)}

			{actionError && (
				<p className="mx-(--size-settings-row-padding) flex items-center gap-1.5 text-xs text-error">
					<XCircle className="size-icon-sm shrink-0" aria-hidden="true" />
					{actionError}
				</p>
			)}
		</div>
	);
}

function StateGlyph({ ok, unknown }: { ok: boolean; unknown: boolean }) {
	if (unknown) return <span className="text-settings-muted">—</span>;
	return ok ? (
		<CheckCircle2 className="size-icon-sm text-success" aria-hidden="true" />
	) : (
		<XCircle className="size-icon-sm text-settings-muted" aria-hidden="true" />
	);
}

function executionLabel(t: TFunction, state: ExecutionState): string {
	switch (state) {
		case "ready":
			return t("settings.agents.executionReady");
		case "disabled":
			return t("settings.agents.executionDisabled");
		case "setup_required":
			return t("settings.agents.executionSetupRequired");
		case "unavailable":
			return t("settings.agents.executionUnavailable");
		case "capacity_limited":
			return t("settings.agents.executionCapacityLimited");
		case "testing":
			return t("settings.agents.testing");
	}
}

function accountLabel(t: TFunction, state: "connected" | "not_connected" | "auth_error" | "unknown"): string {
	switch (state) {
		case "connected":
			return t("settings.agents.accountConnected");
		case "not_connected":
			return t("settings.agents.accountNotConnected");
		case "auth_error":
			return t("settings.agents.accountAuthError");
		case "unknown":
			return t("settings.agents.accountUnknown");
	}
}
