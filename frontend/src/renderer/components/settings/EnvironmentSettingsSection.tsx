import { RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import { useEnvironmentStatus } from "../../hooks/useEnvironmentStatus";
import { Button } from "../ui/button";
import { SettingsRow } from "./SettingsRow";
import { SettingsSection } from "./SettingsSection";
import { CapabilityBadge } from "./StatusBadge";

/**
 * Settings → Environment: the "is this box ready to run autonomous
 * workflows" summary (checkpoint 8H.5 §11). Every row's state comes from
 * EnvironmentStatus, itself built from real local probes — never invented.
 */
export function EnvironmentSettingsSection({ titleHidden }: { titleHidden?: boolean } = {}) {
	const { t } = useTranslation();
	const { status, isLoading, error, refetch } = useEnvironmentStatus();

	return (
		<SettingsSection title={t("settings.environment")} sectionId="environment" titleHidden={titleHidden} grouped>
			<p className="px-(--size-settings-row-padding) text-xs text-settings-muted">{t("settings.environment.hostLevelNote")}</p>
			<div className="settings-row-bar h-auto min-h-(--size-settings-row) flex-wrap gap-2">
				<span className="min-w-0 flex-1 text-sm leading-5 text-settings-label">
					{status
						? status.readiness.overall === "ready"
							? t("settings.environment.ready")
							: t("settings.environment.setupRequired")
						: isLoading
							? t("settings.common.checking")
							: "—"}
				</span>
				<Button type="button" variant="outline" size="sm" onClick={() => void refetch()} disabled={isLoading}>
					<RefreshCw className="size-icon-base" aria-hidden="true" />
					{t("settings.environment.refresh")}
				</Button>
			</div>

			{error && <p className="px-(--size-settings-row-padding) text-xs text-error">{error}</p>}

			{status && (
				<>
					<SettingsRow label={t("settings.environment.codex")}>
						<CapabilityBadge state={status.readiness.codex} />
					</SettingsRow>
					<SettingsRow label={t("settings.environment.claudeCode")}>
						<CapabilityBadge state={status.readiness.claude} />
					</SettingsRow>
					<SettingsRow label={t("settings.environment.github")}>
						<CapabilityBadge state={status.readiness.github} />
					</SettingsRow>
					<SettingsRow label={t("settings.projects")}>
						<span className="text-sm text-settings-muted">
							{t("settings.environment.projectsRegistered", { count: status.projects.count })}
						</span>
					</SettingsRow>
					<SettingsRow label={t("settings.environment.headless")}>
						<CapabilityBadge state={status.readiness.headless} />
					</SettingsRow>
				</>
			)}
		</SettingsSection>
	);
}
