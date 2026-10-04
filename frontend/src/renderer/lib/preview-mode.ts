/**
 * preview-mode.ts — two different questions that used to be one flag.
 *
 * `usesPreviewWorkspaceData` meant "we are the browser build", and it was ALSO
 * used to decide whether to serve invented workspaces. Those are not the same
 * question, and conflating them had a real cost: running `dev:web` against a
 * live daemon showed demo projects that do not exist in it, beside API calls
 * that were succeeding against the real thing. A preview that quietly shows
 * fixture data is the one thing a preview must never do, because everything
 * about it looks like a verification.
 *
 * So:
 *
 * - `isBrowserPreviewMode` is the BUILD question: no Electron shell, therefore
 *   no preload bridge, no native browser, no updater. The seams that exist
 *   because Electron is absent keep using it.
 * - `usesDemoWorkspaceData()` is the DATA question, and it is opt-in: an
 *   explicit demo flag, or the e2e harness having injected its own seam.
 *   Absent both, the browser build reads the real daemon and reports the real
 *   connection state when it cannot.
 */

/** True for the browser build: no Electron APIs, no preload bridge. */
export const isBrowserPreviewMode = import.meta.env.VITE_NO_ELECTRON === "1";

/**
 * Deprecated alias kept for the seams that genuinely mean "no Electron here".
 * Do not use it to decide whether to show invented data.
 */
export const usesPreviewWorkspaceData = isBrowserPreviewMode;

/**
 * usesDemoWorkspaceData reports whether this session should render invented
 * workspaces instead of the daemon's.
 *
 * It is a FUNCTION, not a constant, because the e2e harness injects its seam
 * with an init script that runs before the app but after this module could
 * have been evaluated; a constant would capture the answer too early.
 *
 * Two ways in, both explicit:
 *
 *   VITE_AO_DEMO_DATA=1     somebody asked for the demo dataset
 *   window.__aoFakeAgent    the Playwright harness is driving a fake timeline
 *
 * Being in the browser build is deliberately NOT one of them.
 */
export function usesDemoWorkspaceData(): boolean {
	if (import.meta.env.VITE_AO_DEMO_DATA === "1") return true;
	if (typeof window === "undefined") return false;
	return Boolean((window as unknown as { __aoFakeAgent?: unknown }).__aoFakeAgent);
}
