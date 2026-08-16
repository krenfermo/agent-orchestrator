import { useTranslation } from "react-i18next";
import { SettingsRow } from "./SettingsRow";
import { SettingsSection } from "./SettingsSection";

// Checkpoint 8I: a small, read-only summary of the deterministic
// ReviewPolicy/VerifyScopePolicy defaults every workflow run is seeded
// with. No mutation surface — the policy is versioned and evaluated
// server-side per task; this section only explains the current defaults so
// "why did review get skipped" has an answer without reading source.
export function EfficiencyPolicySettingsSection({ titleHidden }: { titleHidden?: boolean } = {}) {
	const { t } = useTranslation();
	return (
		<SettingsSection title={t("settings.efficiencyPolicy.title")} sectionId="efficiencyPolicy" titleHidden={titleHidden} grouped>
			<SettingsRow label={t("settings.efficiencyPolicy.reviewPolicyVersion")}>
				<span className="text-control text-settings-muted">v1</span>
			</SettingsRow>
			<p className="px-(--size-settings-row-padding) pb-(--size-settings-row-padding) text-xs leading-row text-settings-muted">
				{t("settings.efficiencyPolicy.description")}
			</p>
		</SettingsSection>
	);
}
