import type { MenuItemConstructorOptions } from "electron";

/**
 * The View-menu actions AO performs itself.
 *
 * Every one of these is a plain callback rather than an Electron role on
 * purpose. AO's window is a `BaseWindow` hosting `WebContentsView`s, and
 * Electron's window-oriented View roles assume a `BrowserWindow`:
 *
 *   - `reload` / `forceReload` are guarded with `window instanceof
 *     BrowserWindow` upstream, so on a BaseWindow they silently do nothing —
 *     Cmd+R/Ctrl+R was dead.
 *   - `toggleDevTools` is NOT guarded. It runs
 *     `wc.getOwnerBrowserWindow().webContents.toggleDevTools()`, and
 *     `BaseWindow.webContents` is `undefined`, so it throws
 *     `TypeError: Cannot read properties of undefined (reading 'toggleDevTools')`
 *     out of `MenuItem.click` and crashes the main process.
 *
 * There is deliberately no role-based fallback for these: a fallback that
 * crashes the app is worse than no menu item at all. See main/devtools.ts.
 */
export type AppMenuActions = {
	toggleDevTools: () => void;
	reload: () => void;
	forceReload: () => void;
};

/**
 * Windows keeps its own bespoke shape: the native menu bar is hidden
 * (autoHideMenuBar) and the visible menu is painted by WindowTitlebar, so this
 * template exists mainly to keep the accelerators alive.
 */
export function buildWindowsAppMenuTemplate(actions: AppMenuActions): MenuItemConstructorOptions[] {
	return [
		{
			label: "Edit",
			submenu: [
				{ role: "undo" },
				{ role: "redo" },
				{ type: "separator" },
				{ role: "cut" },
				{ role: "copy" },
				{ role: "paste" },
				{ role: "selectAll" },
			],
		},
		{
			label: "View",
			submenu: [
				{ label: "Reload", accelerator: "Ctrl+R", click: actions.reload },
				{ label: "Toggle DevTools", accelerator: "Ctrl+Shift+I", click: actions.toggleDevTools },
				{ type: "separator" },
				{ role: "resetZoom" },
				{ accelerator: "Ctrl+=", role: "zoomIn" },
				{ accelerator: "Ctrl+Plus", acceleratorWorksWhenHidden: true, role: "zoomIn", visible: false },
				{ accelerator: "Ctrl+-", role: "zoomOut" },
				{ type: "separator" },
				{ role: "togglefullscreen" },
			],
		},
		{
			label: "Window",
			submenu: [{ role: "minimize" }, { role: "close" }],
		},
	];
}

/**
 * macOS and Linux mirror the structure of Electron's own default menu — the one
 * they used to get implicitly — so nothing a person relies on (the app menu,
 * Quit, the Edit commands, the Window menu) disappears. Only the View submenu
 * differs, and only because its role-based items do not work on a BaseWindow.
 */
function buildNativeAppMenuTemplate(isMac: boolean, actions: AppMenuActions): MenuItemConstructorOptions[] {
	return [
		...(isMac ? [{ role: "appMenu" } as MenuItemConstructorOptions] : []),
		{ role: "fileMenu" },
		{ role: "editMenu" },
		{
			label: "View",
			submenu: [
				{ label: "Reload", accelerator: "CmdOrCtrl+R", click: actions.reload },
				{ label: "Force Reload", accelerator: "Shift+CmdOrCtrl+R", click: actions.forceReload },
				{
					label: "Toggle Developer Tools",
					accelerator: isMac ? "Alt+Command+I" : "Ctrl+Shift+I",
					click: actions.toggleDevTools,
				},
				{ type: "separator" },
				{ role: "resetZoom" },
				{ role: "zoomIn" },
				{ role: "zoomOut" },
				{ type: "separator" },
				{ role: "togglefullscreen" },
			],
		},
		{ role: "windowMenu" },
	];
}

/**
 * buildAppMenuTemplate is the single entry point. An application menu is
 * installed on EVERY platform, not just Windows: leaving macOS and Linux on
 * Electron's default menu is what exposed the crashing `toggleDevTools` role.
 */
export function buildAppMenuTemplate(
	platform: NodeJS.Platform,
	actions: AppMenuActions,
): MenuItemConstructorOptions[] {
	if (platform === "win32") return buildWindowsAppMenuTemplate(actions);
	return buildNativeAppMenuTemplate(platform === "darwin", actions);
}
