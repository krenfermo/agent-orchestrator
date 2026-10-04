import type { BaseWindow, WebContents, WebContentsView } from "electron";

type MainWindowHost = Pick<BaseWindow, "contentView" | "isDestroyed">;

export type WebContentsViewConstructor = new (options: {
	webPreferences: Electron.WebPreferences;
}) => WebContentsView;

export type WindowComposition = {
	shellView: WebContentsView;
	shellWebContents: WebContents;
	setOverlayOpen: (open: boolean) => void;
	resize: () => void;
	dispose: () => void;
};

/**
 * Owns the explicit shell surface used by the native compositor.
 *
 * The native path uses a BaseWindow so there is no implicit BrowserWindow
 * renderer competing with this explicit shell surface. The main process can
 * then move the shell above/below the live browser page without a hidden blank
 * renderer covering it.
 */
export function createWindowComposition(options: {
	mainWindow: MainWindowHost;
	WebContentsView: WebContentsViewConstructor;
	preload: string;
}): WindowComposition {
	const shellView = new options.WebContentsView({
		webPreferences: {
			preload: options.preload,
			contextIsolation: true,
			nodeIntegration: false,
			sandbox: true,
			transparent: true,
		},
	});
	shellView.setBackgroundColor("#00000000");
	options.mainWindow.contentView.addChildView(shellView, 0);

	// Captured once, at construction, while the native objects are certainly
	// alive. Reading `shellView.webContents` again during teardown can throw
	// the very "Object has been destroyed" this module has to survive, so the
	// reference is taken here and reused rather than re-read later.
	const shellWebContents = shellView.webContents;

	/**
	 * Whether the window this composition draws into is gone.
	 *
	 * Every access to `mainWindow.contentView` -- the property read itself,
	 * before any method call -- throws `TypeError: Object has been destroyed`
	 * once Electron has destroyed the BaseWindow. So the check has to happen
	 * before touching it, and cannot be replaced by a try/catch around the use:
	 * a catch would also swallow real failures.
	 *
	 * `isDestroyed()` is safe to call on a destroyed Electron object by design;
	 * that is the whole reason it exists. The optional call keeps this working
	 * for a host object that does not implement it, which is treated as alive.
	 */
	const windowGone = (): boolean => options.mainWindow.isDestroyed?.() === true;

	let overlayOpen = false;
	const resize = (): void => {
		if (windowGone()) return;
		const bounds = options.mainWindow.contentView.getBounds();
		shellView.setBounds({ x: 0, y: 0, width: bounds.width, height: bounds.height });
		shellView.setVisible(true);
	};
	options.mainWindow.contentView.on("bounds-changed", resize);

	const setOverlayOpen = (open: boolean): void => {
		if (overlayOpen === open) return;
		overlayOpen = open;
		// A late overlay toggle -- an IPC message already in flight when the
		// window went away -- must not throw. Recording the state and doing
		// nothing is correct: there is no surface left to reorder.
		if (windowGone()) return;
		if (open) {
			// Re-adding an existing child raises it above all page/DevTools views.
			options.mainWindow.contentView.addChildView(shellView);
		} else {
			// Index zero leaves every live native surface above the transparent shell.
			options.mainWindow.contentView.addChildView(shellView, 0);
		}
	};

	resize();

	let disposed = false;

	return {
		shellView,
		shellWebContents,
		setOverlayOpen,
		resize,
		/**
		 * Release this composition's own surface.
		 *
		 * Two properties are load-bearing, and both were missing:
		 *
		 * **Idempotent.** Dispose is reached from more than one direction --
		 * the window's own "closed" handler and an explicit teardown -- so a
		 * second call has to be a no-op rather than a second close of an
		 * already-closed WebContents.
		 *
		 * **Safe after the window is gone.** This runs from
		 * `mainWindow.on("closed")`, which fires AFTER Electron destroyed the
		 * BaseWindow, and it runs a tick later still because the caller waits
		 * on the asynchronous browser-runtime teardown first. Reaching for
		 * `contentView` at that point threw `Object has been destroyed`, and
		 * because the throw happened inside a `.finally()` on a voided promise
		 * chain it surfaced as an unhandled rejection -- one statement into
		 * dispose, so neither the listener nor the WebContents was ever
		 * released.
		 *
		 * The window half is therefore skipped when the window is gone (its
		 * hierarchy went with it, so there is nothing to remove), and the
		 * WebContents half runs either way, because that object is this
		 * module's own and is exactly what the old code never reached.
		 *
		 * Nothing here is wrapped in a blanket catch. An error that is not the
		 * destroyed-window case is a real failure and should reach the caller.
		 */
		dispose: () => {
			if (disposed) return;
			disposed = true;

			if (!windowGone()) {
				options.mainWindow.contentView.removeListener("bounds-changed", resize);
				options.mainWindow.contentView.removeChildView(shellView);
			}

			if (!shellWebContents.isDestroyed()) shellWebContents.close();
		},
	};
}
