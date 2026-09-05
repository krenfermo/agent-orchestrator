import { describe, expect, it, vi } from "vitest";
import { toggleDevToolsSafely, type DevToolsTarget } from "./devtools";

function target(overrides: Partial<DevToolsTarget> = {}): DevToolsTarget {
	return { isDestroyed: () => false, toggleDevTools: vi.fn(), ...overrides };
}

/**
 * The contract under test is narrow and absolute: whatever the state of the
 * window, the panel or the shell, this never rejects and never throws — an
 * unhandled throw here is what crashed the Electron main process.
 */
describe("toggleDevToolsSafely", () => {
	it("uses the last-focused browser panel when it handled the toggle", async () => {
		const shell = target();
		await toggleDevToolsSafely({
			togglePanelDevTools: () => Promise.resolve({ open: true }),
			getShellContents: () => shell,
		});
		expect(shell.toggleDevTools).not.toHaveBeenCalled();
	});

	it("falls back to the shell when there is no focused panel", async () => {
		const shell = target();
		await toggleDevToolsSafely({
			togglePanelDevTools: () => Promise.resolve(null),
			getShellContents: () => shell,
		});
		expect(shell.toggleDevTools).toHaveBeenCalledTimes(1);
	});

	it("falls back to the shell when the panel toggle resolves false", async () => {
		const shell = target();
		await toggleDevToolsSafely({
			togglePanelDevTools: () => Promise.resolve(false),
			getShellContents: () => shell,
		});
		expect(shell.toggleDevTools).toHaveBeenCalledTimes(1);
	});

	it("falls back to the shell when the panel toggle rejects", async () => {
		const shell = target();
		const log = vi.fn();
		await toggleDevToolsSafely({
			togglePanelDevTools: () => Promise.reject(new Error("panel gone")),
			getShellContents: () => shell,
			log,
		});
		expect(shell.toggleDevTools).toHaveBeenCalledTimes(1);
		expect(log).toHaveBeenCalled();
	});

	it("falls back to the shell when there is no browser host at all", async () => {
		const shell = target();
		await toggleDevToolsSafely({
			togglePanelDevTools: () => undefined,
			getShellContents: () => shell,
		});
		expect(shell.toggleDevTools).toHaveBeenCalledTimes(1);
	});

	it("no-ops and logs when no window exists yet", async () => {
		const log = vi.fn();
		await expect(
			toggleDevToolsSafely({ togglePanelDevTools: () => undefined, getShellContents: () => null, log }),
		).resolves.toBeUndefined();
		expect(log).toHaveBeenCalledWith("AO: DevTools requested with no live WebContents to open it against");
	});

	it("no-ops when the shell WebContents is destroyed", async () => {
		const shell = target({ isDestroyed: () => true });
		const log = vi.fn();
		await toggleDevToolsSafely({ togglePanelDevTools: () => undefined, getShellContents: () => shell, log });
		expect(shell.toggleDevTools).not.toHaveBeenCalled();
		expect(log).toHaveBeenCalled();
	});

	it("no-ops when isDestroyed itself throws on a torn-down WebContents", async () => {
		const shell = target({
			isDestroyed: () => {
				throw new Error("Object has been destroyed");
			},
		});
		await expect(
			toggleDevToolsSafely({ togglePanelDevTools: () => undefined, getShellContents: () => shell }),
		).resolves.toBeUndefined();
		expect(shell.toggleDevTools).not.toHaveBeenCalled();
	});

	// The TOCTOU case: the shell was alive when the action started, and the
	// window closed while the panel toggle was still in flight. Re-resolving
	// after the await is what catches it.
	it("re-resolves the shell after the async panel toggle rather than reusing a stale one", async () => {
		const shell = target();
		let windowClosed = false;
		await toggleDevToolsSafely({
			togglePanelDevTools: async () => {
				windowClosed = true;
				return null;
			},
			getShellContents: () => (windowClosed ? null : shell),
		});
		expect(shell.toggleDevTools).not.toHaveBeenCalled();
	});

	it("swallows a throw from toggleDevTools on a WebContents destroyed mid-call", async () => {
		const shell = target({
			toggleDevTools: () => {
				throw new Error("Object has been destroyed");
			},
		});
		const log = vi.fn();
		await expect(
			toggleDevToolsSafely({ togglePanelDevTools: () => undefined, getShellContents: () => shell, log }),
		).resolves.toBeUndefined();
		expect(log).toHaveBeenCalledWith("AO: DevTools toggle failed", expect.any(Error));
	});

	it("never rejects when every lookup throws at once", async () => {
		const log = vi.fn();
		await expect(
			toggleDevToolsSafely({
				togglePanelDevTools: () => {
					throw new Error("host exploded");
				},
				getShellContents: () => {
					throw new Error("no composition");
				},
				log,
			}),
		).resolves.toBeUndefined();
		expect(log).toHaveBeenCalled();
	});

	// The menu accelerator can fire before the first window is built and after
	// the last one is closed; both land on the same "nothing live" path.
	it("no-ops when the menu item fires with no window in either direction", async () => {
		for (const getShellContents of [() => null, () => undefined]) {
			await expect(
				toggleDevToolsSafely({ togglePanelDevTools: () => undefined, getShellContents }),
			).resolves.toBeUndefined();
		}
	});
});
