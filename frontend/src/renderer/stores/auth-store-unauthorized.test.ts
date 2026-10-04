import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { apiClient, setApiBaseUrl } from "../lib/api-client";
import { useAuthStore } from "./auth-store";

// Deliberately unmocked api-client: the point of this file is the wiring
// between a real 401 on the wire and the store's state. The store registers its
// listener on import, so nothing here installs one.
function signedIn() {
	useAuthStore.setState({
		user: { id: "u1", email: "qa@example.com", displayName: "QA", role: "owner" } as never,
		status: "authenticated",
		authMethod: "oidc",
		issuer: "https://accounts.google.com",
		permissions: ["project.create"],
		error: null,
	});
}

beforeEach(() => {
	setApiBaseUrl("http://127.0.0.1:3002");
	signedIn();
});

afterEach(() => {
	vi.restoreAllMocks();
	setApiBaseUrl(null);
});

describe("a protected 401 reaching the store", () => {
	it("signs the session out, so the application stops believing it is signed in", async () => {
		vi.spyOn(globalThis, "fetch").mockResolvedValue(
			new Response(JSON.stringify({ code: "NOT_AUTHENTICATED" }), {
				status: 401,
				headers: { "Content-Type": "application/json" },
			}),
		);

		await apiClient.GET("/api/v1/projects");

		const state = useAuthStore.getState();
		expect(state.status).toBe("unauthenticated");
		expect(state.user).toBeNull();
		expect(state.permissions).toEqual([]);
		expect(state.authMethod).toBeNull();
	});

	it("leaves an authenticated session alone when the daemon answers normally", async () => {
		vi.spyOn(globalThis, "fetch").mockResolvedValue(
			new Response(JSON.stringify({ projects: [] }), {
				status: 200,
				headers: { "Content-Type": "application/json" },
			}),
		);

		await apiClient.GET("/api/v1/projects");

		expect(useAuthStore.getState().status).toBe("authenticated");
	});

	it("does not re-signal an already signed-out session", async () => {
		// A fresh Response per call: a single one has its body read by the first.
		vi.spyOn(globalThis, "fetch").mockImplementation(
			async () =>
				new Response(JSON.stringify({ code: "NOT_AUTHENTICATED" }), {
					status: 401,
					headers: { "Content-Type": "application/json" },
				}),
		);
		const listener = vi.fn();
		const unsubscribe = useAuthStore.subscribe(listener);

		await apiClient.GET("/api/v1/projects");
		const afterFirst = listener.mock.calls.length;
		await apiClient.GET("/api/v1/sessions");

		expect(listener.mock.calls.length).toBe(afterFirst);
		unsubscribe();
	});
});
