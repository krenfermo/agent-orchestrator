import { useTranslation } from "react-i18next";
import { SettingsRow } from "./SettingsRow";
import { SettingsSection } from "./SettingsSection";

// Checkpoint 8L: a small, read-only summary of ExecutionRouter's
// deterministic V1 defaults every workflow run is seeded with. No mutation
// surface yet — persistent settings infrastructure for a writable routing
// policy does not exist (see checkpoint brief §21: "read-only default
// policy is acceptable in 8L, but architecture must support configuration
// later"). This section only explains the current defaults so "why did this
// task route to Codex" has an answer without reading source.
export function RoutingPolicySettingsSection({ titleHidden }: { titleHidden?: boolean } = {}) {
	const { t } = useTranslation();
	return (
		<SettingsSection title={t("settings.routingPolicy.title")} sectionId="routingPolicy" titleHidden={titleHidden} grouped>
			<SettingsRow label={t("settings.routingPolicy.trivialWorker")}>
				<span className="text-control text-settings-muted">{t("settings.routingPolicy.harness.codex")}</span>
			</SettingsRow>
			<SettingsRow label={t("settings.routingPolicy.normalWorker")}>
				<span className="text-control text-settings-muted">{t("settings.routingPolicy.harness.claudeCode")}</span>
			</SettingsRow>
			<SettingsRow label={t("settings.routingPolicy.highRiskWorker")}>
				<span className="text-control text-settings-muted">{t("settings.routingPolicy.harness.claudeCode")}</span>
			</SettingsRow>
			<SettingsRow label={t("settings.routingPolicy.reviewer")}>
				<span className="text-control text-settings-muted">{t("settings.routingPolicy.reviewerValue")}</span>
			</SettingsRow>
			<SettingsRow label={t("settings.routingPolicy.planner")}>
				<span className="text-control text-settings-muted">{t("settings.routingPolicy.harness.claudeCode")}</span>
			</SettingsRow>
			<SettingsRow label={t("settings.routingPolicy.policyVersion")}>
				<span className="text-control text-settings-muted">v1</span>
			</SettingsRow>
			<p className="px-(--size-settings-row-padding) pb-(--size-settings-row-padding) text-xs leading-row text-settings-muted">
				{t("settings.routingPolicy.description")}
			</p>
		</SettingsSection>
	);
}
