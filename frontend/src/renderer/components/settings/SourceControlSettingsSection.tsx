import { useTranslation } from "react-i18next";
import { useEnvironmentStatus } from "../../hooks/useEnvironmentStatus";
import { Button } from "../ui/button";
import { SettingsRow } from "./SettingsRow";
import { SettingsSection } from "./SettingsSection";
import { GitHubAuthBadge, InstalledBadge } from "./StatusBadge";

/**
 * Settings → Source Control: the GitHub CLI capability card (checkpoint
 * 8H.5 §5). "Test connection" runs `gh auth status` only — no REST call, no
 * token is ever fetched or displayed.
 */
export function SourceControlSettingsSection({ titleHidden }: { titleHidden?: boolean } = {}) {
	const { t } = useTranslation();
	const { status, isLoading, error, testGitHub, testingGitHub } = useEnvironmentStatus();
	const github = status?.github;

	return (
		<SettingsSection title={t("settings.sourceControl")} sectionId="source-control" titleHidden={titleHidden} grouped>
			{error && <p className="px-(--size-settings-row-padding) text-xs text-error">{error}</p>}
			{isLoading && !github && (
				<p className="px-(--size-settings-row-padding) text-xs text-settings-muted">{t("settings.common.checking")}</p>
			)}
			{github && (
				<>
					<div className="settings-row-bar h-auto min-h-(--size-settings-row) flex-wrap gap-2">
						<span className="min-w-0 flex-1 text-sm font-medium leading-5 text-settings-label">
							{t("settings.environment.github")}
						</span>
						<InstalledBadge installed={github.installed} />
						<GitHubAuthBadge state={github.authState} />
						<Button type="button" variant="outline" size="sm" onClick={() => void testGitHub()} disabled={testingGitHub}>
							{testingGitHub ? t("settings.agents.testing") : t("settings.agents.testConnection")}
						</Button>
					</div>
					<SettingsRow label={t("settings.agents.binaryPath")}>
						<span className="truncate text-xs text-settings-muted" title={github.binaryPath}>
							{github.binaryPath || "—"}
						</span>
					</SettingsRow>
					<SettingsRow label={t("settings.agents.version")}>
						<span className="text-xs text-settings-muted">{github.version || "—"}</span>
					</SettingsRow>
					<SettingsRow label={t("settings.sourceControl.loggedInAs")}>
						<span className="text-xs text-settings-muted">{github.login || "—"}</span>
					</SettingsRow>
					<SettingsRow label={t("settings.sourceControl.host")}>
						<span className="text-xs text-settings-muted">{github.host || "—"}</span>
					</SettingsRow>
				</>
			)}
		</SettingsSection>
	);
}
