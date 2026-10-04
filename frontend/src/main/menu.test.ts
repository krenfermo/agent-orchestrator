import { describe, expect, it, vi } from "vitest";
import { buildAppMenuTemplate, buildWindowsAppMenuTemplate, type AppMenuActions } from "./menu";

type MenuItem = ReturnType<typeof buildWindowsAppMenuTemplate>[number];
type SubmenuItem = NonNullable<Extract<MenuItem["submenu"], readonly unknown[]>>[number];

function actions(): AppMenuActions {
	return { toggleDevTools: vi.fn(), reload: vi.fn(), forceReload: vi.fn() };
}

function submenuOf(template: MenuItem[], label: string): readonly SubmenuItem[] {
	const menu = template.find((item) => item.label === label);
	if (!menu || !Array.isArray(menu.submenu)) throw new Error(`${label} menu not found`);
	return menu.submenu;
}

function viewSubmenu(platform: NodeJS.Platform = "win32"): readonly SubmenuItem[] {
	return submenuOf(buildAppMenuTemplate(platform, actions()), "View");
}

const PLATFORMS: NodeJS.Platform[] = ["darwin", "win32", "linux"];

describe("buildWindowsAppMenuTemplate", () => {
	it("registers both plus key forms for zoom in", () => {
		const zoomInItems = viewSubmenu().filter((item) => item.role === "zoomIn");

		expect(zoomInItems).toEqual(
			expect.arrayContaining([
				expect.objectContaining({ accelerator: "Ctrl+=", role: "zoomIn" }),
				expect.objectContaining({ accelerator: "Ctrl+Plus", role: "zoomIn", visible: false }),
			]),
		);
	});

	it("keeps the direct minus accelerator for zoom out", () => {
		expect(viewSubmenu()).toContainEqual(expect.objectContaining({ accelerator: "Ctrl+-", role: "zoomOut" }));
	});
});

describe("buildAppMenuTemplate", () => {
	// The regression this whole module exists for: Electron's toggleDevTools role
	// dereferences `window.webContents`, which is undefined on the BaseWindow AO
	// uses, and crashes the main process out of MenuItem.click.
	it.each(PLATFORMS)("never emits a role-based DevTools item on %s", (platform) => {
		const items = viewSubmenu(platform);
		expect(items.some((item) => item.role === "toggleDevTools")).toBe(false);
		expect(items.some((item) => item.label?.toString().includes("Developer Tools") || item.label === "Toggle DevTools")).toBe(
			true,
		);
	});

	// reload/forceReload are guarded upstream with `instanceof BrowserWindow`, so
	// as roles they do not crash — they silently do nothing on a BaseWindow.
	it.each(PLATFORMS)("never emits role-based reload items on %s", (platform) => {
		const items = viewSubmenu(platform);
		expect(items.some((item) => item.role === "reload" || item.role === "forceReload")).toBe(false);
	});

	it.each(PLATFORMS)("routes DevTools to the injected callback on %s", (platform) => {
		const menuActions = actions();
		const items = submenuOf(buildAppMenuTemplate(platform, menuActions), "View");
		const devtools = items.find((item) => item.accelerator === (platform === "darwin" ? "Alt+Command+I" : "Ctrl+Shift+I"));
		expect(devtools).toBeDefined();
		(devtools?.click as () => void)();
		expect(menuActions.toggleDevTools).toHaveBeenCalledTimes(1);
	});

	it.each(PLATFORMS)("routes reload to the injected callback on %s", (platform) => {
		const menuActions = actions();
		const reload = submenuOf(buildAppMenuTemplate(platform, menuActions), "View").find(
			(item) => item.label === "Reload",
		);
		(reload?.click as () => void)();
		expect(menuActions.reload).toHaveBeenCalledTimes(1);
		expect(menuActions.forceReload).not.toHaveBeenCalled();
	});

	it("keeps the macOS app, file, edit and window menus Electron's default supplied", () => {
		const template = buildAppMenuTemplate("darwin", actions());
		expect(template.map((item) => item.role)).toEqual(["appMenu", "fileMenu", "editMenu", undefined, "windowMenu"]);
	});

	it("gives Linux the same structure without the macOS app menu", () => {
		const template = buildAppMenuTemplate("linux", actions());
		expect(template.map((item) => item.role)).toEqual(["fileMenu", "editMenu", undefined, "windowMenu"]);
	});

	it("keeps the bespoke Windows shape", () => {
		const template = buildAppMenuTemplate("win32", actions());
		expect(template.map((item) => item.label)).toEqual(["Edit", "View", "Window"]);
	});

	it("has no way to build a template without DevTools actions", () => {
		// Type-level guarantee, asserted at runtime too: there is no optional
		// argument left that would fall back to the crashing native role.
		expect(buildWindowsAppMenuTemplate.length).toBe(1);
	});
});
