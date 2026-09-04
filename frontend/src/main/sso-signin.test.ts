import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("electron", () => ({
	net: { request: vi.fn() },
	session: { defaultSession: {} },
	shell: { openExternal: vi.fn() },
}));

import { beginSsoSignIn, SsoSignInError } from "./sso-signin";

const BASE = "http://127.0.0.1:3001";

type Call = { url: string; method: string; body?: string };

function harness(responses: Array<{ status: number; body: unknown }>) {
	const calls: Call[] = [];
	const opened: string[] = [];
	let index = 0;
	return {
		calls,
		opened,
		deps: {
			apiBaseUrl: BASE,
			openExternal: async (url: string) => {
				opened.push(url);
			},
			request: async (url: string, init: { method: string; body?: string }) => {
				calls.push({ url, method: init.method, body: init.body });
				const response = responses[Math.min(index, responses.length - 1)];
				index += 1;
				return response;
			},
			sleep: async () => undefined,
			now: () => 0,
		},
	};
}

describe("beginSsoSignIn", () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it("opens the system browser and polls until the session is claimed", async () => {
		const h = harness([
			{ status: 200, body: { authorizationUrl: "https://idp.example/authorize", flowId: "flow-1" } },
			{ status: 200, body: { status: "pending" } },
			{ status: 200, body: { status: "complete", user: { id: "u1", displayName: "Ada", email: "ada@example.com" } } },
		]);

		const result = await beginSsoSignIn(h.deps);

		expect(result).toEqual({ status: "complete", user: { id: "u1", displayName: "Ada", email: "ada@example.com" } });
		expect(h.opened).toEqual(["https://idp.example/authorize"]);
		expect(h.calls[0].url).toBe(`${BASE}/api/v1/auth/oidc/start`);
		expect(h.calls[1].url).toBe(`${BASE}/api/v1/auth/oidc/claim`);
	});

	it("mints a fresh handoff secret that never travels to the provider", async () => {
		const h = harness([
			{ status: 200, body: { authorizationUrl: "https://idp.example/authorize?state=abc", flowId: "flow-1" } },
			{ status: 200, body: { status: "complete", user: { id: "u1", displayName: "Ada", email: "a@example.com" } } },
		]);

		await beginSsoSignIn(h.deps);

		const startBody = JSON.parse(h.calls[0].body ?? "{}") as { clientKind: string; handoffSecret: string };
		expect(startBody.clientKind).toBe("desktop");
		expect(startBody.handoffSecret.length).toBeGreaterThanOrEqual(32);
		// The secret is the whole point of the handoff: it must not appear in
		// anything the browser — and therefore the provider — ever sees.
		expect(h.opened[0]).not.toContain(startBody.handoffSecret);

		const claimBody = JSON.parse(h.calls[1].body ?? "{}") as { handoffSecret: string };
		expect(claimBody.handoffSecret).toBe(startBody.handoffSecret);
	});

	it("stops on a terminal claim failure instead of polling past the reason", async () => {
		const h = harness([
			{ status: 200, body: { authorizationUrl: "https://idp.example/authorize", flowId: "flow-1" } },
			{ status: 401, body: { error: { message: "this sign-in expired; start again" } } },
		]);

		await expect(beginSsoSignIn(h.deps)).rejects.toThrow(SsoSignInError);
		expect(h.calls).toHaveLength(2);
	});

	it("reports the daemon's own message when SSO is not configured", async () => {
		const h = harness([{ status: 409, body: { error: { message: "single sign-on is not configured on this installation" } } }]);

		await expect(beginSsoSignIn(h.deps)).rejects.toThrow(/not configured/);
		expect(h.opened).toHaveLength(0);
	});

	it("gives up as cancelled once the sign-in window elapses", async () => {
		let clock = 0;
		const h = harness([
			{ status: 200, body: { authorizationUrl: "https://idp.example/authorize", flowId: "flow-1" } },
			{ status: 200, body: { status: "pending" } },
		]);
		const result = await beginSsoSignIn({
			...h.deps,
			now: () => {
				clock += 10 * 60 * 1000;
				return clock;
			},
		});
		expect(result).toEqual({ status: "cancelled" });
	});
});
