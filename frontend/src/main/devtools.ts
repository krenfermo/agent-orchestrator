/**
 * Safe DevTools toggling.
 *
 * Electron's `toggleDevTools` menu role cannot be used by this app. AO's window
 * is a `BaseWindow` hosting `WebContentsView`s, and the role's implementation is
 *
 *   webContentsMethod: (wc) => {
 *     const win = wc.getOwnerBrowserWindow();
 *     if (win) win.webContents.toggleDevTools();
 *   }
 *
 * `BaseWindow` has no `webContents`, so `win.webContents` is `undefined` and the
 * role throws `TypeError: Cannot read properties of undefined (reading
 * 'toggleDevTools')` straight out of `MenuItem.click`, which crashes the main
 * process. (Electron guards its sibling `reload`/`forceReload` roles with
 * `window instanceof BrowserWindow`; `toggleDevTools` was never updated.)
 *
 * So AO resolves the target itself. Every target is re-resolved and re-checked
 * at the moment it is used, never captured before an `await`: a window can close
 * or a panel be destroyed while the panel toggle is still in flight.
 */

export type DevToolsTarget = {
	isDestroyed: () => boolean;
	toggleDevTools: () => void;
};

export type DevToolsDeps = {
	/**
	 * Toggles DevTools on the last-focused Browser panel, resolving to a truthy
	 * state when it acted. Returns `undefined` when there is no browser host at
	 * all (no window yet, or one already torn down).
	 */
	togglePanelDevTools: () => Promise<unknown> | undefined;
	/** The app shell's WebContents, or null/undefined when no window is up. */
	getShellContents: () => DevToolsTarget | null | undefined;
	log?: (message: string, detail?: unknown) => void;
};

/**
 * live returns the target only if it is still usable. A WebContents that has
 * been torn down can throw from `isDestroyed()` itself, so even the check is
 * guarded — the whole point of this module is that no path reaches an
 * unhandled main-process exception.
 */
function live(target: DevToolsTarget | null | undefined): DevToolsTarget | null {
	if (!target) return null;
	try {
		return target.isDestroyed() ? null : target;
	} catch {
		return null;
	}
}

/**
 * toggleDevToolsSafely opens DevTools against a proven-live WebContents:
 * the last-focused Browser panel if there is one, else the app shell, else
 * nothing at all (logged). It never rejects.
 */
export async function toggleDevToolsSafely(deps: DevToolsDeps): Promise<void> {
	const log = deps.log ?? (() => undefined);
	try {
		if (await deps.togglePanelDevTools()) return;
	} catch (error) {
		log("AO: browser panel DevTools toggle failed; falling back to the shell", error);
	}
	// Re-resolve rather than reuse anything read before the await above: the
	// window may have closed while the panel toggle was pending.
	let shell: DevToolsTarget | null = null;
	try {
		shell = live(deps.getShellContents());
	} catch (error) {
		log("AO: could not resolve the shell WebContents for DevTools", error);
	}
	if (!shell) {
		log("AO: DevTools requested with no live WebContents to open it against");
		return;
	}
	try {
		shell.toggleDevTools();
	} catch (error) {
		log("AO: DevTools toggle failed", error);
	}
}
