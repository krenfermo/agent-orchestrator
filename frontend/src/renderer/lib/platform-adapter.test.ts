import { beforeEach, describe, expect, it, vi } from "vitest";

describe("platform adapter", () => {
	beforeEach(() => {
		vi.resetModules();
		vi.unstubAllGlobals();
	});

	it("selects web mode and uses same-origin without Electron", async () => {
		delete window.ao;
		vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
			ok: true,
			json: async () => ({ status: "ready", pid: 42 }),
		}));
		const { platformAdapter } = await import("./platform-adapter");
		const status = await platformAdapter.readDaemonStatus();
		expect(platformAdapter.mode).toBe("web");
		expect(platformAdapter.capabilities.selectServerDirectory).toBe(false);
		expect(status.state).toBe("ready");
		expect(platformAdapter.apiBaseUrl(status)).toBe("");
		expect(fetch).toHaveBeenCalledWith("/readyz", expect.any(Object));
	});

	it("selects the desktop adapter when preload is present", async () => {
		window.ao = {
			...window.ao!,
			daemon: {
				getStatus: vi.fn().mockResolvedValue({ state: "ready", port: 4123 }),
				start: vi.fn(), stop: vi.fn(), restart: vi.fn(), onStatus: vi.fn(() => () => undefined),
			},
		} as typeof window.ao;
		const { platformAdapter } = await import("./platform-adapter");
		const status = await platformAdapter.readDaemonStatus();
		expect(platformAdapter.mode).toBe("desktop");
		expect(platformAdapter.apiBaseUrl(status)).toBe("http://127.0.0.1:4123");
	});
});
