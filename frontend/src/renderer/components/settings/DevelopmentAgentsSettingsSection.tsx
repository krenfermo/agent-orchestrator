import { useState } from "react";
import { useTranslation } from "react-i18next";
import { apiErrorMessage } from "../../lib/api-client";
import { useCapacity, type CapacitySnapshot } from "../../hooks/useCapacity";
import { useProviderProfiles, type ProviderDescriptor, type ProviderProfile } from "../../hooks/useProviderProfiles";
import { Button } from "../ui/button";
import { Switch } from "../ui/switch";
import { SettingsRow } from "./SettingsRow";
import { SettingsSection } from "./SettingsSection";
import { Badge } from "../ui/badge";

/**
 * Settings → Agents & Models (Checkpoint 8P-B). Cards render from the
 * provider registry joined against the current user's own provider
 * profiles — never a hardcoded Claude/Codex pair. A provider with no real
 * adapter yet (e.g. MiniMax) renders honestly as unsupported instead of
 * being omitted or shown as if it worked.
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
	const [busy, setBusy] = useState(false);
	const [actionError, setActionError] = useState<string | undefined>();

	const run = async (fn: () => Promise<unknown>) => {
		setBusy(true);
		setActionError(undefined);
		try {
			await fn();
		} catch (err) {
			setActionError(apiErrorMessage(err));
		} finally {
			setBusy(false);
		}
	};

	const connected = profile?.authState === "authenticated";

	return (
		<div className="flex flex-col gap-1.5 border-t border-(--color-border-settings-dialog-header) pt-3 first:border-t-0 first:pt-0">
			<div className="settings-row-bar h-auto min-h-(--size-settings-row) flex-wrap gap-2">
				<span className="min-w-0 flex-1 text-sm font-medium leading-5 text-settings-label">{descriptor.displayName}</span>
				{!descriptor.available ? (
					<Badge variant="neutral">{t("settings.agents.unsupported", "Unsupported")}</Badge>
				) : (
					<>
						<Badge variant={connected ? "success" : "neutral"}>
							{connected
								? t("settings.agents.connected", "Connected")
								: t("settings.agents.notConnected", "Not connected")}
						</Badge>
						{profile && (
							<div className="flex items-center gap-1.5">
								<span className="text-xs text-settings-muted">{t("settings.agents.enabled", "Enabled")}</span>
								<Switch
									checked={profile.enabled}
									disabled={busy}
									onCheckedChange={(next) => void run(() => onSetEnabled(profile.id, next) ?? Promise.resolve())}
								/>
							</div>
						)}
						{profile ? (
							<>
								<Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => void run(() => onTest(profile.id))}>
									{t("settings.agents.testConnection")}
								</Button>
								<Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => void run(() => onDisconnect(profile.id))}>
									{t("settings.agents.disconnect", "Disconnect")}
								</Button>
							</>
						) : (
							<Button
								type="button"
								variant="outline"
								size="sm"
								disabled={busy}
								onClick={() => void run(async () => onConnect((await onCreate()).id))}
							>
								{t("settings.agents.connect", "Connect")}
							</Button>
						)}
					</>
				)}
			</div>
			{!descriptor.available && descriptor.unavailable && (
				<p className="px-(--size-settings-row-padding) text-xs text-settings-muted">{descriptor.unavailable}</p>
			)}
			{descriptor.available && (
				<>
					<SettingsRow label={t("settings.agents.authMethod", "Auth method")}>
						<span className="text-xs text-settings-muted">{profile?.authMethod ?? descriptor.authMethods[0] ?? "—"}</span>
					</SettingsRow>
					<SettingsRow label={t("settings.agents.model")}>
						<span className="text-xs text-settings-muted">{profile?.defaultModel || "—"}</span>
					</SettingsRow>
					<SettingsRow label={t("settings.agents.capabilities", "Capabilities")}>
						<span className="text-xs text-settings-muted">
							{descriptor.capabilities.length > 0 ? descriptor.capabilities.join(", ") : "—"}
						</span>
					</SettingsRow>
					<SettingsRow label={t("settings.agents.capacity")}>
						<span className="text-xs text-settings-muted">{capacity ? capacity.state : "unknown"}</span>
					</SettingsRow>
				</>
			)}
			{actionError && <p className="px-(--size-settings-row-padding) text-xs text-error">{actionError}</p>}
		</div>
	);
}
