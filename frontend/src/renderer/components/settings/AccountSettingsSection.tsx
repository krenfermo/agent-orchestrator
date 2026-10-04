import { KeyRound, Link2, LogOut, Mail, ShieldCheck } from "lucide-react";
import { useTranslation } from "react-i18next";
import { useAuthStore } from "../../stores/auth-store";
import { useUiStore } from "../../stores/ui-store";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { SettingsRow } from "./SettingsRow";
import { SettingsSection } from "./SettingsSection";

/**
 * Settings → Account (Checkpoint 8P-E.8). Shows the signed-in identity and
 * the sign-in methods surface. GitHub/Google/Apple rows are deliberately
 * disabled placeholders — see docs/auth-sso-design.md — this checkpoint
 * ships the design and schema for AO-identity OAuth, not working providers,
 * so nothing here should read as functional. Logout is the one live action.
 */
export function AccountSettingsSection({ titleHidden }: { titleHidden?: boolean } = {}) {
	const { t } = useTranslation();
	const user = useAuthStore((state) => state.user);
	const logout = useAuthStore((state) => state.logout);
	const closeSettings = useUiStore((state) => state.closeSettings);

	const handleLogout = async () => {
		await logout();
		closeSettings();
	};

	return (
		<SettingsSection title={t("settings.account")} sectionId="account" titleHidden={titleHidden} grouped>
			<SettingsRow icon={Mail} label={t("settings.account.signedInAs")}>
				<div className="flex items-center gap-2">
					<span className="truncate text-sm text-settings-label">{user?.displayName ?? user?.email ?? "—"}</span>
					{user?.role === "owner" ? (
						<Badge variant="neutral">{t("settings.account.roleOwner")}</Badge>
					) : user ? (
						<Badge variant="neutral">{t("settings.account.roleMember")}</Badge>
					) : null}
				</div>
			</SettingsRow>

			<SettingsRow icon={KeyRound} label={t("settings.account.signInMethods")}>
				<span className="text-sm text-settings-muted">{t("settings.account.password")}</span>
			</SettingsRow>
			<SettingsRow icon={Link2} label="GitHub">
				<span className="text-xs text-settings-muted">{t("settings.account.notYetAvailable")}</span>
			</SettingsRow>
			<SettingsRow icon={ShieldCheck} label="Google">
				<span className="text-xs text-settings-muted">{t("settings.account.notYetAvailable")}</span>
			</SettingsRow>
			<SettingsRow icon={ShieldCheck} label="Apple">
				<span className="text-xs text-settings-muted">{t("settings.account.notYetAvailable")}</span>
			</SettingsRow>

			<SettingsRow icon={LogOut} label={t("settings.account.logout")}>
				<Button variant="secondary" size="sm" onClick={() => void handleLogout()}>
					{t("settings.account.logoutAction")}
				</Button>
			</SettingsRow>
		</SettingsSection>
	);
}
