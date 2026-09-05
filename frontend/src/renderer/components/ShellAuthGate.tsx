import { type ReactNode, useEffect } from "react";
import { refreshDaemonStatus } from "../lib/daemon-status";
import { usesPreviewWorkspaceData } from "../lib/preview-mode";
import { authPermitsProtectedData, useAuthStore } from "../stores/auth-store";
import { DaemonStartupLoader } from "./DaemonStartupLoader";
import { LoginScreen } from "./LoginScreen";
import { SignupScreen } from "./SignupScreen";

// How long to wait before asking again while auth is still unresolved. The
// renderer boots seconds before the daemon binds its port, and nothing else
// tells the gate when that happened.
const AUTH_RETRY_MS = 1_000;

/**
 * ShellAuthGate stands between the router and the application shell, and it is
 * the only place that decides whether the shell may exist.
 *
 * It is a gate rather than a branch inside the shell because the shell's mount
 * IS the protected data load: the workspace query, the agent catalog, the event
 * stream and the terminal cache all start from its hooks. A check at the bottom
 * of that component runs after every one of them has already fired. So the
 * three states are separated here, above all of it:
 *
 *   unresolved ("loading")  — the daemon has not answered /auth/me yet. Not
 *                             knowing is not permission: show the startup
 *                             screen and keep asking.
 *   unauthenticated         — sign in (or, on a fresh install, create the first
 *                             account). No protected request is ever made.
 *   resolved and permitted  — mount the shell.
 */
export function ShellAuthGate({ children }: { children: ReactNode }) {
	const status = useAuthStore((state) => state.status);
	const setupRequired = useAuthStore((state) => state.setupRequired);
	const providersStatus = useAuthStore((state) => state.providersStatus);

	// Two things are worth retrying for: an identity we have not resolved, and —
	// once we know nobody is signed in — the provider list the sign-in screen
	// needs. Both are unanswerable until the daemon is up, and both are
	// permanent-looking if the single attempt lands before it is.
	const unresolved = status === "loading" || (status === "unauthenticated" && providersStatus === "idle");

	useEffect(() => {
		// Preview/e2e renders mock data with no daemon behind it; there is no
		// identity to resolve and nothing to ask.
		if (usesPreviewWorkspaceData || !unresolved) return;
		let cancelled = false;
		let timer: number | undefined;
		const attempt = async () => {
			// Pick up the daemon's port first: auth-store's calls are no-ops until
			// api-client has a URL to send them to.
			await refreshDaemonStatus().catch(() => undefined);
			if (cancelled) return;
			await useAuthStore.getState().refreshForDaemonReady();
			if (cancelled) return;
			timer = window.setTimeout(() => void attempt(), AUTH_RETRY_MS);
		};
		void attempt();
		return () => {
			cancelled = true;
			if (timer !== undefined) window.clearTimeout(timer);
		};
	}, [unresolved]);

	if (usesPreviewWorkspaceData) return <>{children}</>;

	if (status === "unauthenticated") {
		return setupRequired ? <SignupScreen /> : <LoginScreen />;
	}

	if (!authPermitsProtectedData(status)) {
		return (
			<div className="h-screen w-screen bg-background" data-testid="shell-auth-pending">
				<DaemonStartupLoader />
			</div>
		);
	}

	return <>{children}</>;
}
