import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Send } from "lucide-react";
import {
	useEmailNotificationSettings,
	type EmailNotificationSettingsInput,
	type EmailNotificationTLS,
} from "../../hooks/useEmailNotificationSettings";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { Switch } from "../ui/switch";
import { SettingsRow } from "./SettingsRow";
import { SettingsSection } from "./SettingsSection";

type Draft = EmailNotificationSettingsInput;

/**
 * Settings → Email notifications: the optional email that arrives when a task
 * or a workflow finishes, stops on something only the user can decide, or ends
 * without completing.
 *
 * The password field is write-only in both directions. The server never returns
 * the stored password, so this form starts empty even when one is configured
 * (`passwordSet` is what renders "a password is saved"), and it only sends the
 * field when the user actually typed something — sending an untouched empty box
 * would erase a working credential on the next unrelated save.
 *
 * Saving is explicit rather than per-field-on-change, unlike the other sections
 * here: half a set of SMTP credentials is not a state worth persisting on every
 * keystroke, and enabling with an incomplete form is a validation error the
 * user should see once, on a button they pressed.
 */
export function EmailNotificationsSettingsSection({ titleHidden }: { titleHidden?: boolean } = {}) {
	const { t } = useTranslation();
	const { settings, isLoading, error, save, isSaving, sendTest, isSendingTest } = useEmailNotificationSettings();

	const [draft, setDraft] = useState<Draft | null>(null);
	const [password, setPassword] = useState("");
	const [saveError, setSaveError] = useState<string | undefined>();
	const [status, setStatus] = useState<string | undefined>();

	useEffect(() => {
		if (!settings) return;
		setDraft({
			enabled: settings.enabled,
			recipient: settings.recipient,
			host: settings.host,
			port: settings.port,
			username: settings.username,
			tls: settings.tls,
			events: settings.events,
		});
	}, [settings]);

	if (isLoading || !draft) {
		return (
			<SettingsSection
				title={t("settings.emailNotifications.title")}
				sectionId="emailNotifications"
				titleHidden={titleHidden}
				grouped
			>
				<p className="px-(--size-settings-row-padding) py-(--size-settings-row-padding) text-xs text-settings-muted">
					{error ?? t("settings.common.checking")}
				</p>
			</SettingsSection>
		);
	}

	const update = (patch: Partial<Draft>) => {
		setDraft({ ...draft, ...patch });
		setStatus(undefined);
		setSaveError(undefined);
	};

	// The three event switches always travel together: the server treats an
	// absent selection as "unchanged", so sending a partial object would be
	// indistinguishable from sending nothing.
	const updateEvent = (patch: Partial<NonNullable<Draft["events"]>>) => {
		update({
			events: {
				completed: draft.events?.completed ?? false,
				needsAttention: draft.events?.needsAttention ?? false,
				failed: draft.events?.failed ?? false,
				...patch,
			},
		});
	};

	const persist = async () => {
		setSaveError(undefined);
		setStatus(undefined);
		try {
			// Omitted, not empty: the server reads an absent password as "keep
			// the stored one".
			await save(password === "" ? draft : { ...draft, password });
			setPassword("");
			setStatus(t("settings.emailNotifications.saved"));
		} catch (err) {
			setSaveError(err instanceof Error ? err.message : String(err));
		}
	};

	const runTest = async () => {
		setSaveError(undefined);
		setStatus(undefined);
		try {
			// Saved first, so the test exercises the settings the user is
			// looking at rather than whatever was stored before they edited.
			await save(password === "" ? draft : { ...draft, password });
			setPassword("");
			await sendTest();
			setStatus(t("settings.emailNotifications.testSent", { recipient: draft.recipient }));
		} catch (err) {
			setSaveError(err instanceof Error ? err.message : String(err));
		}
	};

	const busy = isSaving || isSendingTest;

	return (
		<SettingsSection
			title={t("settings.emailNotifications.title")}
			sectionId="emailNotifications"
			titleHidden={titleHidden}
			grouped
		>
			<p className="px-(--size-settings-row-padding) pt-(--size-settings-row-padding) text-xs leading-row text-settings-muted">
				{t("settings.emailNotifications.description")}
			</p>

			<SettingsRow label={t("settings.emailNotifications.enabled")}>
				<Switch
					aria-label={t("settings.emailNotifications.enabled")}
					checked={draft.enabled}
					disabled={busy}
					onCheckedChange={(next) => update({ enabled: next })}
				/>
			</SettingsRow>

			<SettingsRow label={t("settings.emailNotifications.recipient")}>
				<Input
					aria-label={t("settings.emailNotifications.recipient")}
					className="settings-field-control w-auto min-w-56"
					disabled={busy}
					onChange={(event) => update({ recipient: event.target.value })}
					placeholder="you@example.com"
					type="email"
					value={draft.recipient}
				/>
			</SettingsRow>

			<SettingsRow label={t("settings.emailNotifications.host")}>
				<Input
					aria-label={t("settings.emailNotifications.host")}
					className="settings-field-control w-auto min-w-56"
					disabled={busy}
					onChange={(event) => update({ host: event.target.value })}
					placeholder="smtp.gmail.com"
					value={draft.host}
				/>
			</SettingsRow>

			<SettingsRow label={t("settings.emailNotifications.port")}>
				<Input
					aria-label={t("settings.emailNotifications.port")}
					className="settings-field-control w-auto min-w-24"
					disabled={busy}
					inputMode="numeric"
					onChange={(event) => update({ port: Number(event.target.value) || 0 })}
					value={String(draft.port)}
				/>
			</SettingsRow>

			<SettingsRow label={t("settings.emailNotifications.username")}>
				<Input
					aria-label={t("settings.emailNotifications.username")}
					className="settings-field-control w-auto min-w-56"
					disabled={busy}
					onChange={(event) => update({ username: event.target.value })}
					placeholder="you@gmail.com"
					value={draft.username}
				/>
			</SettingsRow>

			<SettingsRow label={t("settings.emailNotifications.password")}>
				<Input
					aria-label={t("settings.emailNotifications.password")}
					className="settings-field-control w-auto min-w-56"
					disabled={busy}
					onChange={(event) => {
						setPassword(event.target.value);
						setStatus(undefined);
						setSaveError(undefined);
					}}
					// Empty even when one is stored: the server never sends it back,
					// so there is nothing to prefill and nothing to leak.
					placeholder={
						settings?.passwordSet
							? t("settings.emailNotifications.passwordStored")
							: t("settings.emailNotifications.passwordEmpty")
					}
					type="password"
					value={password}
				/>
			</SettingsRow>

			<SettingsRow label={t("settings.emailNotifications.events.completed")}>
				<Switch
					aria-label={t("settings.emailNotifications.events.completed")}
					checked={draft.events?.completed ?? false}
					disabled={busy}
					onCheckedChange={(next) => updateEvent({ completed: next })}
				/>
			</SettingsRow>

			<SettingsRow label={t("settings.emailNotifications.events.needsAttention")}>
				<Switch
					aria-label={t("settings.emailNotifications.events.needsAttention")}
					checked={draft.events?.needsAttention ?? false}
					disabled={busy}
					onCheckedChange={(next) => updateEvent({ needsAttention: next })}
				/>
			</SettingsRow>

			<SettingsRow label={t("settings.emailNotifications.events.failed")}>
				<Switch
					aria-label={t("settings.emailNotifications.events.failed")}
					checked={draft.events?.failed ?? false}
					disabled={busy}
					onCheckedChange={(next) => updateEvent({ failed: next })}
				/>
			</SettingsRow>

			<SettingsRow label={t("settings.emailNotifications.tls")}>
				<Select disabled={busy} onValueChange={(value) => update({ tls: value as EmailNotificationTLS })} value={draft.tls}>
					<SelectTrigger aria-label={t("settings.emailNotifications.tls")} className="settings-field-control w-auto min-w-48">
						<SelectValue />
					</SelectTrigger>
					<SelectContent align="end">
						<SelectItem value="starttls">{t("settings.emailNotifications.tls.starttls")}</SelectItem>
						<SelectItem value="implicit">{t("settings.emailNotifications.tls.implicit")}</SelectItem>
						<SelectItem value="none">{t("settings.emailNotifications.tls.none")}</SelectItem>
					</SelectContent>
				</Select>
			</SettingsRow>

			<div className="settings-row-bar h-auto min-h-(--size-settings-row) flex-wrap justify-end gap-2">
				<Button disabled={busy} onClick={() => void runTest()} size="sm" type="button" variant="outline">
					<Send className="size-icon-base" aria-hidden="true" />
					{t("settings.emailNotifications.sendTest")}
				</Button>
				<Button disabled={busy} onClick={() => void persist()} size="sm" type="button">
					{t("settings.emailNotifications.save")}
				</Button>
			</div>

			<p className="px-(--size-settings-row-padding) pb-(--size-settings-row-padding) text-xs leading-row text-settings-muted">
				{t("settings.emailNotifications.gmailHint")}
			</p>

			{status && (
				<p className="px-(--size-settings-row-padding) pb-(--size-settings-row-padding) text-xs text-success" role="status">
					{status}
				</p>
			)}
			{(saveError ?? error) && (
				<p className="px-(--size-settings-row-padding) pb-(--size-settings-row-padding) text-xs text-error" role="alert">
					{saveError ?? error}
				</p>
			)}
		</SettingsSection>
	);
}
