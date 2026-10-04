import { useEffect, useRef } from "react";
import { useTranslation } from "react-i18next";
import { Loader2 } from "lucide-react";
import { useProviderSetup } from "../../hooks/useProviderSetup";
import { useShellMaybe } from "../../lib/shell-context";
import { useResolvedTheme } from "../../stores/ui-store";
import { TerminalPane } from "../TerminalPane";
import { Button } from "../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "../ui/dialog";

/**
 * Checkpoint 8P-E.8.4: the zero-terminal "Connect <Provider>" flow. Opening
 * this dialog starts a server-owned setup terminal (see
 * useProviderSetup/backend providersetup) running the provider CLI's own
 * login flow inside the profile owner's isolated runtime-home, embeds it via
 * the same TerminalPane/XtermTerminal stack a standalone shell terminal uses,
 * and polls Test Connection in the background so the dialog closes itself the
 * instant login succeeds -- the user never has to click Test Connection.
 */
export function ProviderSetupDialog({
	profileId,
	displayName,
	open,
	onOpenChange,
}: {
	profileId: string;
	displayName: string;
	open: boolean;
	onOpenChange: (open: boolean) => void;
}) {
	const { t } = useTranslation();
	const shell = useShellMaybe();
	const theme = useResolvedTheme();
	const { phase, handleId, instructions, error, start, stop } = useProviderSetup(profileId);

	useEffect(() => {
		if (open) void start();
		// start() is stable per profileId (see useProviderSetup); re-running it
		// on every render would open a fresh terminal each time.
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [open, profileId]);

	// The poll inside useProviderSetup drives phase back to "idle" on its own
	// once Test Connection reports authenticated -- that transition, and only
	// that one, means "close the dialog for us", as opposed to the initial
	// idle state before start() has run.
	const everWaitingRef = useRef(false);
	useEffect(() => {
		if (phase === "waiting") everWaitingRef.current = true;
		if (phase === "idle" && everWaitingRef.current) {
			everWaitingRef.current = false;
			onOpenChange(false);
		}
	}, [phase, onOpenChange]);

	const close = () => {
		onOpenChange(false);
		void stop();
	};

	return (
		<Dialog open={open} onOpenChange={(next) => (next ? undefined : close())}>
			<DialogContent className="flex h-[32rem] max-w-2xl flex-col">
				<DialogHeader>
					<DialogTitle>{t("settings.agents.setup.title", { name: displayName })}</DialogTitle>
					<DialogDescription>
						{phase === "starting" && t("settings.agents.setup.starting")}
						{phase === "waiting" && (instructions || t("settings.agents.setup.waiting"))}
						{phase === "timed_out" && t("settings.agents.setup.timedOut")}
						{phase === "error" && (error || t("settings.agents.setup.error"))}
					</DialogDescription>
				</DialogHeader>
				<div className="min-h-0 flex-1 overflow-hidden rounded-md border">
					{handleId ? (
						<TerminalPane
							daemonReady={shell?.daemonStatus.state === "ready"}
							fontSize={12}
							terminalTarget={{
								generation: handleId,
								kind: "shell",
								handleId,
								title: displayName,
							}}
							theme={theme}
						/>
					) : (
						<div className="grid h-full place-items-center text-muted-foreground">
							<Loader2 className="size-icon-md animate-spin" aria-hidden="true" />
						</div>
					)}
				</div>
				<div className="flex items-center justify-between gap-2">
					<span className="flex items-center gap-1.5 text-xs text-muted-foreground">
						{phase === "waiting" && (
							<>
								<Loader2 className="size-icon-sm animate-spin" aria-hidden="true" />
								{t("settings.agents.setup.waitingBadge")}
							</>
						)}
					</span>
					<div className="flex gap-2">
						{phase === "timed_out" && (
							<Button type="button" variant="outline" size="sm" onClick={() => void start()}>
								{t("settings.agents.tryAgain")}
							</Button>
						)}
						<Button type="button" variant="outline" size="sm" onClick={close}>
							{t("settings.agents.setup.cancel")}
						</Button>
					</div>
				</div>
			</DialogContent>
		</Dialog>
	);
}
