import { useTranslation } from "react-i18next";
import { SettingsRow } from "./SettingsRow";
import { SettingsSection } from "./SettingsSection";

// Checkpoint 8M §20: a small, read-only summary of SessionLifecyclePolicy's
// deterministic V1 defaults — no numeric knobs, per the checkpoint brief's
// explicit "no automatic token thresholds" instruction. No mutation surface
// yet, same rationale as RoutingPolicySettingsSection (8L): this only
// explains the current rules so "why did this session get compacted/
// replaced" has an answer without reading source.
export function SessionLifecyclePolicySettingsSection({ titleHidden }: { titleHidden?: boolean } = {}) {
	const { t } = useTranslation();
	return (
		<SettingsSection title={t("settings.sessionLifecyclePolicy.title")} sectionId="sessionLifecyclePolicy" titleHidden={titleHidden} grouped>
			<SettingsRow label={t("settings.sessionLifecyclePolicy.reuse")}>
				<span className="text-control text-settings-muted">{t("settings.sessionLifecyclePolicy.reuseValue")}</span>
			</SettingsRow>
			<SettingsRow label={t("settings.sessionLifecyclePolicy.newSession")}>
				<span className="text-control text-settings-muted">{t("settings.sessionLifecyclePolicy.newSessionValue")}</span>
			</SettingsRow>
			<SettingsRow label={t("settings.sessionLifecyclePolicy.compact")}>
				<span className="text-control text-settings-muted">{t("settings.sessionLifecyclePolicy.compactValue")}</span>
			</SettingsRow>
			<SettingsRow label={t("settings.sessionLifecyclePolicy.policyVersion")}>
				<span className="text-control text-settings-muted">v1</span>
			</SettingsRow>
			<p className="px-(--size-settings-row-padding) pb-(--size-settings-row-padding) text-xs leading-row text-settings-muted">
				{t("settings.sessionLifecyclePolicy.description")}
			</p>
		</SettingsSection>
	);
}
