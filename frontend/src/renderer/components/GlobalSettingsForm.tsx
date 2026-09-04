import { useState } from "react";
import { useTranslation } from "react-i18next";
import { AccessSettingsSection } from "./settings/AccessSettingsSection";
import { AccountSettingsSection } from "./settings/AccountSettingsSection";
import { DevelopmentAgentsSettingsSection } from "./settings/DevelopmentAgentsSettingsSection";
import { EfficiencyPolicySettingsSection } from "./settings/EfficiencyPolicySettingsSection";
import { EmailNotificationsSettingsSection } from "./settings/EmailNotificationsSettingsSection";
import { EnvironmentSettingsSection } from "./settings/EnvironmentSettingsSection";
import { ExecutionPolicySettingsSection } from "./settings/ExecutionPolicySettingsSection";
import { GeneralSettingsSection } from "./settings/GeneralSettingsSection";
import { ProjectsSettingsSection } from "./settings/ProjectsSettingsSection";
import { ReportProblemDialog } from "./settings/ReportProblemDialog";
import { SessionLifecyclePolicySettingsSection } from "./settings/SessionLifecyclePolicySettingsSection";
import { SettingsLinkRow } from "./settings/SettingsRow";
import { SettingsSection } from "./settings/SettingsSection";
import { SourceControlSettingsSection } from "./settings/SourceControlSettingsSection";
import { UpdatesSection } from "./settings/UpdatesSection";

export type GlobalSettingsSection =
	| "general"
	| "environment"
	| "agents"
	| "sourceControl"
	| "projects"
	| "updates"
	| "account"
	| "access"
	| "help"
	| "all";

export function GlobalSettingsForm({
	section = "all",
	onOpenKeyboardShortcuts,
	onOpenConnectMobile,
}: {
	section?: GlobalSettingsSection;
	onOpenKeyboardShortcuts?: () => void;
	onOpenConnectMobile?: () => void;
}) {
	const { t } = useTranslation();
	const [reportProblemOpen, setReportProblemOpen] = useState(false);
	// One section per page means the dialog header already names it, so the
	// page's leading heading would just repeat that title. Only "all" (no
	// single-page header) shows every section's own heading.
	const leadingTitleHidden = section !== "all";

	return (
		<>
			<div
				aria-label={t("settings.title")}
				className="flex w-full flex-col gap-(--size-settings-section-gap)"
				data-testid="settings-page"
			>
				{(section === "all" || section === "general") && (
					<>
						<GeneralSettingsSection
							onConnectMobile={() => onOpenConnectMobile?.()}
							titleHidden={leadingTitleHidden}
						/>
						<SettingsSection title={t("settings.preferences")} grouped>
							<SettingsLinkRow
								label={t("settings.keyboardShortcuts")}
								onClick={() => onOpenKeyboardShortcuts?.()}
							/>
						</SettingsSection>
						<EmailNotificationsSettingsSection />
						<EfficiencyPolicySettingsSection />
						<ExecutionPolicySettingsSection />
						<SessionLifecyclePolicySettingsSection />
					</>
				)}
				{(section === "all" || section === "environment") && (
					<EnvironmentSettingsSection titleHidden={leadingTitleHidden} />
				)}
				{(section === "all" || section === "agents") && (
					<DevelopmentAgentsSettingsSection titleHidden={leadingTitleHidden} />
				)}
				{(section === "all" || section === "sourceControl") && (
					<SourceControlSettingsSection titleHidden={leadingTitleHidden} />
				)}
				{(section === "all" || section === "projects") && (
					<ProjectsSettingsSection titleHidden={leadingTitleHidden} />
				)}
				{(section === "all" || section === "updates") && <UpdatesSection titleHidden={leadingTitleHidden} />}
				{(section === "all" || section === "account") && (
					<AccountSettingsSection titleHidden={leadingTitleHidden} />
				)}
				{(section === "all" || section === "access") && (
					<AccessSettingsSection titleHidden={leadingTitleHidden} />
				)}
				{(section === "all" || section === "help") && (
					<SettingsSection title={t("settings.getHelp")} titleHidden={leadingTitleHidden} grouped>
						<SettingsLinkRow label={t("settings.reportProblem")} onClick={() => setReportProblemOpen(true)} />
					</SettingsSection>
				)}
			</div>
			<ReportProblemDialog open={reportProblemOpen} onOpenChange={setReportProblemOpen} />
		</>
	);
}
