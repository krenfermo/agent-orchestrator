import { QueryClient } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";

describe("router history", () => {
	beforeEach(() => {
		vi.resetModules();
		window.history.replaceState(null, "", "/workflows");
	});

	it("uses browser paths in web mode", async () => {
		delete window.ao;
		const { createAppRouter } = await import("./router");
		const router = createAppRouter(new QueryClient());
		expect(router.history.location.pathname).toBe("/workflows");
	});
});
