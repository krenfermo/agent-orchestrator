import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useProjectsList } from "../../hooks/useProjectsList";
import { useProjectRegistration, type BrowseEntry } from "../../hooks/useProjectRegistration";
import { describeProjectApiError } from "../../lib/project-error-messages";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { SettingsSection } from "./SettingsSection";

/**
 * Settings → Projects: registered-project table plus the web "register
 * existing repository" and "clone from GitHub" flows (checkpoint 8H.5 §6-8).
 * Both flows write through the server-side allowedProjectRoots confinement —
 * this component never sends an absolute filesystem path the user typed
 * freely, only a path relative to a root (optionally picked from Browse).
 */
export function ProjectsSettingsSection({ titleHidden }: { titleHidden?: boolean } = {}) {
	const { t } = useTranslation();
	const { projects, isLoading, error } = useProjectsList();
	const {
		browse,
		register,
		registering,
		registerError,
		resetRegisterError,
		clone,
		cloning,
		cloneError,
		resetCloneError,
	} = useProjectRegistration();

	const [registerPath, setRegisterPath] = useState("");
	const [browseEntries, setBrowseEntries] = useState<BrowseEntry[] | null>(null);
	const [browseError, setBrowseError] = useState<{ message: string; code?: string } | undefined>();
	const [browsing, setBrowsing] = useState(false);

	const [cloneRepo, setCloneRepo] = useState("");
	const [cloneDest, setCloneDest] = useState("");

	const runBrowse = async () => {
		setBrowsing(true);
		setBrowseError(undefined);
		try {
			const result = await browse("");
			setBrowseEntries(result.entries);
		} catch (err) {
			setBrowseError(describeProjectApiError(err));
		} finally {
			setBrowsing(false);
		}
	};

	const onRegisterSubmit = (event: React.FormEvent) => {
		event.preventDefault();
		if (!registerPath.trim()) return;
		resetRegisterError();
		void register({ path: registerPath.trim() }).then(() => {
			setRegisterPath("");
			setBrowseEntries(null);
		});
	};

	const onCloneSubmit = (event: React.FormEvent) => {
		event.preventDefault();
		if (!cloneRepo.trim()) return;
		resetCloneError();
		void clone({ repo: cloneRepo.trim(), destinationName: cloneDest.trim() || undefined }).then(() => {
			setCloneRepo("");
			setCloneDest("");
		});
	};

	return (
		<SettingsSection title={t("settings.projects")} sectionId="projects" titleHidden={titleHidden} grouped>
			{error && <p className="px-(--size-settings-row-padding) text-xs text-error">{error}</p>}
			{isLoading && (
				<p className="px-(--size-settings-row-padding) text-xs text-settings-muted">{t("settings.common.loading")}</p>
			)}
			{!isLoading && projects.length === 0 && (
				<p className="px-(--size-settings-row-padding) text-xs text-settings-muted">{t("settings.projects.noneRegistered")}</p>
			)}
			{projects.length > 0 && (
				<div className="overflow-x-auto px-(--size-settings-row-padding) pb-2">
					<table className="w-full min-w-[560px] text-left text-xs">
						<thead>
							<tr className="text-settings-muted">
								<th className="py-1 pr-3 font-medium">{t("settings.projects.tableName")}</th>
								<th className="py-1 pr-3 font-medium">{t("settings.projects.tablePath")}</th>
								<th className="py-1 pr-3 font-medium">{t("settings.projects.tableOrigin")}</th>
								<th className="py-1 pr-3 font-medium">{t("settings.projects.tableBranch")}</th>
								<th className="py-1 pr-3 font-medium">{t("settings.projects.tableKind")}</th>
								<th className="py-1 font-medium">{t("settings.projects.tableStatus")}</th>
							</tr>
						</thead>
						<tbody>
							{projects.map((p) => (
								<tr key={p.id} className="border-t border-(--color-border-settings-dialog-header)">
									<td className="py-1.5 pr-3">
										<div className="text-settings-label">{p.name}</div>
										<div className="text-2xs text-settings-muted">{p.id}</div>
									</td>
									<td className="max-w-[220px] truncate py-1.5 pr-3 text-settings-muted" title={p.path}>
										{p.path}
									</td>
									<td className="max-w-[180px] truncate py-1.5 pr-3 text-settings-muted" title={p.repo}>
										{p.repo || "—"}
									</td>
									<td className="py-1.5 pr-3 text-settings-muted">{p.defaultBranch || "—"}</td>
									<td className="py-1.5 pr-3 text-settings-muted">{p.kind}</td>
									<td className="py-1.5">
										{p.valid ? (
											<Badge variant="success">{t("settings.projects.statusValid")}</Badge>
										) : (
											<Badge variant="error">{t("settings.projects.statusMissing")}</Badge>
										)}
									</td>
								</tr>
							))}
						</tbody>
					</table>
				</div>
			)}

			<form
				onSubmit={onRegisterSubmit}
				className="flex flex-col gap-2 border-t border-(--color-border-settings-dialog-header) px-(--size-settings-row-padding) py-3"
			>
				<span className="text-sm font-medium text-settings-label">{t("settings.projects.registerTitle")}</span>
				<div className="flex gap-2">
					<input
						className="min-w-0 flex-1 rounded-md border border-(--color-border-settings-input) bg-[var(--color-bg-settings-input)] px-2 py-1.5 text-sm"
						value={registerPath}
						onChange={(event) => setRegisterPath(event.target.value)}
						placeholder={t("settings.projects.registerPathPlaceholder")}
					/>
					<Button type="button" variant="outline" size="sm" onClick={() => void runBrowse()} disabled={browsing}>
						{browsing ? t("settings.projects.browsing") : t("settings.projects.browse")}
					</Button>
				</div>
				{browseError && <p className="text-xs text-error">{browseError.message}</p>}
				{browseEntries && (
					<ul className="flex max-h-40 flex-col gap-1 overflow-y-auto rounded-md border border-(--color-border-settings-input) p-2">
						{browseEntries.length === 0 && (
							<li className="text-xs text-settings-muted">{t("settings.projects.noSubdirectories")}</li>
						)}
						{browseEntries.map((entry) => (
							<li key={entry.path}>
								<button
									type="button"
									className="flex w-full items-center justify-between gap-2 rounded px-2 py-1 text-left text-xs hover:bg-settings-menu-selected"
									onClick={() => setRegisterPath(entry.name)}
								>
									<span className="truncate text-settings-label">{entry.name}</span>
									{entry.isGitRepo && <Badge variant="success">{t("settings.projects.gitBadge")}</Badge>}
								</button>
							</li>
						))}
					</ul>
				)}
				<Button type="submit" variant="primary" size="sm" disabled={registering || !registerPath.trim()} className="self-start">
					{registering ? t("settings.projects.registering") : t("settings.projects.register")}
				</Button>
				{registerError && <p className="text-xs text-error">{registerError.message}</p>}
			</form>

			<form
				onSubmit={onCloneSubmit}
				className="flex flex-col gap-2 border-t border-(--color-border-settings-dialog-header) px-(--size-settings-row-padding) py-3"
			>
				<span className="text-sm font-medium text-settings-label">{t("settings.projects.cloneTitle")}</span>
				<input
					className="rounded-md border border-(--color-border-settings-input) bg-[var(--color-bg-settings-input)] px-2 py-1.5 text-sm"
					value={cloneRepo}
					onChange={(event) => setCloneRepo(event.target.value)}
					placeholder={t("settings.projects.cloneRepoPlaceholder")}
				/>
				<input
					className="rounded-md border border-(--color-border-settings-input) bg-[var(--color-bg-settings-input)] px-2 py-1.5 text-sm"
					value={cloneDest}
					onChange={(event) => setCloneDest(event.target.value)}
					placeholder={t("settings.projects.cloneDestPlaceholder")}
				/>
				<Button type="submit" variant="primary" size="sm" disabled={cloning || !cloneRepo.trim()} className="self-start">
					{cloning ? t("settings.projects.cloning") : t("settings.projects.clone")}
				</Button>
				{cloneError && <p className="text-xs text-error">{cloneError.message}</p>}
			</form>
		</SettingsSection>
	);
}
