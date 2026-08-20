import { ChevronLeft, Folder } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useProjectRegistration, type BrowseEntry } from "../hooks/useProjectRegistration";
import { describeProjectApiError } from "../lib/project-error-messages";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";

/**
 * ServerFolderBrowser is Checkpoint 8P-E.4's graphical replacement for
 * typing an absolute path in the web (non-Electron) Import Project/Workspace
 * flow. Electron has a native OS folder picker; a browser tab has no
 * filesystem access to the machine the AO *daemon* runs on (which may not
 * even be the machine the browser is running on -- see GET
 * /api/v1/projects/browse's own doc comment), so folder selection has to be
 * a server-side directory listing the user clicks through instead. This
 * component is the only thing that calls that endpoint; it never lets the
 * caller pick anything the server didn't just hand it as an entry's own
 * Path, so it can't be used to smuggle an arbitrary typed path past the
 * server-side allowedProjectRoots confinement.
 *
 * Navigating into a folder both descends the listing AND marks that folder
 * as the current selection (shown in the header with "Use this folder") --
 * the same model most graphical folder pickers use: browsing IS selecting
 * the folder you're currently looking inside of, no separate click target.
 */
export function ServerFolderBrowser({
	disabled,
	onUseFolder,
}: {
	disabled?: boolean;
	onUseFolder: (path: string) => void;
}) {
	const { t } = useTranslation();
	const { browse } = useProjectRegistration();
	const [currentPath, setCurrentPath] = useState<string | null>(null);
	const [entries, setEntries] = useState<BrowseEntry[]>([]);
	const [history, setHistory] = useState<string[]>([]);
	const [loading, setLoading] = useState(false);
	const [error, setError] = useState<{ message: string; code?: string } | undefined>();

	const load = async (path: string) => {
		setLoading(true);
		setError(undefined);
		try {
			const result = await browse(path);
			setCurrentPath(result.path);
			setEntries(result.entries);
		} catch (err) {
			setError(describeProjectApiError(err));
		} finally {
			setLoading(false);
		}
	};

	// Load the top level exactly once on mount.
	useEffect(() => {
		void load("");
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, []);

	const enter = (entry: BrowseEntry) => {
		if (currentPath !== null) setHistory((prev) => [...prev, currentPath]);
		void load(entry.path);
	};

	const goBack = () => {
		const prev = history[history.length - 1];
		if (prev === undefined) return;
		setHistory((h) => h.slice(0, -1));
		void load(prev);
	};

	// The virtual top level (multiple allowed roots, none entered yet) has no
	// folder of its own to select -- currentPath is "" and there is nothing
	// for "Use this folder" to confirm yet.
	const canUseCurrent = Boolean(currentPath) && !loading;

	return (
		<div className="flex flex-col gap-2 rounded-lg border border-dashed border-[var(--color-border-import-modal)] bg-[var(--color-bg-import-card)] p-4">
			<div className="flex items-center gap-2">
				{history.length > 0 && (
					<Button type="button" variant="outline" size="icon" disabled={disabled || loading} onClick={goBack} aria-label={t("createProject.browserBack")}>
						<ChevronLeft className="size-4" aria-hidden="true" />
					</Button>
				)}
				<div className="min-w-0 flex-1">
					<div className="text-[12px] font-semibold uppercase tracking-[0.08em] text-[var(--color-text-import-muted)]">
						{currentPath ? t("createProject.selected") : t("createProject.allowedLocations")}
					</div>
					{currentPath && (
						<div className="truncate font-mono text-[13px] text-[var(--color-text-import-title)]" title={currentPath}>
							{currentPath}
						</div>
					)}
				</div>
				{canUseCurrent && (
					<Button type="button" variant="primary" size="sm" disabled={disabled} onClick={() => onUseFolder(currentPath as string)}>
						{t("createProject.useThisFolder")}
					</Button>
				)}
			</div>

			{error && <p className="text-[12px] text-error">{error.message}</p>}

			<ul className="flex max-h-56 flex-col gap-0.5 overflow-y-auto rounded-md border border-[var(--color-border-import-modal)] bg-[var(--color-bg-settings-input)] p-1">
				{loading && <li className="px-2 py-1.5 text-[12px] text-[var(--color-text-import-muted)]">{t("createProject.browserLoading")}</li>}
				{!loading && !error && entries.length === 0 && (
					<li className="px-2 py-1.5 text-[12px] text-[var(--color-text-import-muted)]">{t("createProject.browserEmpty")}</li>
				)}
				{!loading &&
					entries.map((entry) => (
						<li key={entry.path}>
							<button
								type="button"
								disabled={disabled}
								onClick={() => enter(entry)}
								className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-[13px] text-[var(--color-text-import-title)] hover:bg-[var(--color-bg-import-card-hover)] disabled:pointer-events-none disabled:opacity-50"
							>
								<Folder className="size-4 shrink-0 text-[var(--color-text-import-muted)]" aria-hidden="true" />
								<span className="min-w-0 flex-1 truncate">{entry.name}</span>
								{entry.isGitRepo && <Badge variant="success">{t("createProject.browserGitBadge")}</Badge>}
							</button>
						</li>
					))}
			</ul>
		</div>
	);
}
