import { render, screen, waitFor } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { DaemonStatus } from "../../shared/daemon-status";

const apiGET = vi.fn();
const apiPOST = vi.fn();

vi.mock("../lib/api-client", async () => {
	const actual = await vi.importActual<typeof import("../lib/api-client")>("../lib/api-client");
	return {
		...actual,
		apiClient: { GET: (...args: unknown[]) => apiGET(...args), POST: (...args: unknown[]) => apiPOST(...args) },
	};
});

// The daemon binds its port some time after the renderer boots, and until it
// does api-client has no URL to send anything to. `daemonReady` is that fact,
// and refreshDaemonStatus is how the gate observes it — exactly as in the app.
let daemonReady = false;

vi.mock("../lib/daemon-status", async () => {
	const { setApiBaseUrl } = await vi.importActual<typeof import("../lib/api-client")>("../lib/api-client");
	return {
		refreshDaemonStatus: async (): Promise<DaemonStatus> => {
			setApiBaseUrl(daemonReady ? "http://127.0.0.1:3002" : null);
			return daemonReady ? { state: "ready", port: 3002 } : { state: "starting" };
		},
	};
});

import { apiClient, setApiBaseUrl } from "../lib/api-client";
import { useAuthStore } from "../stores/auth-store";
import { ShellAuthGate } from "./ShellAuthGate";

const GOOGLE_PROVIDERS = {
	mode: "oidc",
	passwordEnabled: true,
	oidc: { displayName: "Google", startPath: "/api/v1/auth/oidc/start" },
};

const UNAUTHORIZED = { error: { code: "NOT_AUTHENTICATED", message: "authentication required" }, response: { status: 401 } };

// Stands in for the shell. The shell's mount IS the protected data load, so a
// probe that fetches on mount is the honest stand-in: if this ever renders while
// signed out, /projects and /sessions were requested while signed out.
function ProtectedProbe() {
	useEffect(() => {
		void apiClient.GET("/api/v1/projects");
		void apiClient.GET("/api/v1/sessions");
	}, []);
	return <div data-testid="app-shell">shell</div>;
}

function renderGate() {
	return render(
		<ShellAuthGate>
			<ProtectedProbe />
		</ShellAuthGate>,
	);
}

function requestedPaths(): string[] {
	return apiGET.mock.calls.map((call) => String(call[0]));
}

function signedOutDaemon() {
	apiGET.mockImplementation(async (path: string) => {
		if (path === "/api/v1/auth/me") return UNAUTHORIZED;
		if (path === "/api/v1/auth/setup-status") return { data: { setupRequired: false } };
		if (path === "/api/v1/auth/providers") return { data: GOOGLE_PROVIDERS };
		return UNAUTHORIZED;
	});
}

beforeEach(() => {
	apiGET.mockReset();
	apiPOST.mockReset();
	daemonReady = false;
	setApiBaseUrl(null);
	useAuthStore.setState({
		user: null,
		status: "loading",
		error: null,
		setupRequired: null,
		authMethod: null,
		issuer: null,
		providers: null,
		providersStatus: "idle",
		ssoError: null,
		permissions: [],
		ssoPending: false,
	});
});

afterEach(() => {
	setApiBaseUrl(null);
});

describe("ShellAuthGate", () => {
	it("renders the sign-in screen, never the shell, when the daemon refuses the current principal", async () => {
		daemonReady = true;
		signedOutDaemon();

		renderGate();

		expect(await screen.findByRole("button", { name: "Sign in" })).toBeInTheDocument();
		expect(screen.queryByTestId("app-shell")).toBeNull();
	});

	it("asks the daemon which providers it offers once it knows nobody is signed in", async () => {
		daemonReady = true;
		signedOutDaemon();

		renderGate();

		await waitFor(() => expect(requestedPaths()).toContain("/api/v1/auth/providers"));
	});

	it("offers the provider's button and the password form when the daemon advertises both", async () => {
		daemonReady = true;
		signedOutDaemon();

		renderGate();

		expect(await screen.findByRole("button", { name: "Sign in with Google" })).toBeInTheDocument();
		expect(screen.getByLabelText("Username or email")).toBeInTheDocument();
		expect(screen.getByLabelText("Password")).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
	});

	it("makes no protected request at all while nobody is signed in", async () => {
		daemonReady = true;
		signedOutDaemon();

		renderGate();

		await screen.findByRole("button", { name: "Sign in" });
		// Give the shell every chance to mount and poll if it were going to.
		await new Promise((resolve) => setTimeout(resolve, 1_200));

		expect(requestedPaths()).not.toContain("/api/v1/projects");
		expect(requestedPaths()).not.toContain("/api/v1/sessions");
		expect(screen.queryByTestId("app-shell")).toBeNull();
	});

	it("mounts the shell, and lets it load protected data, once an identity resolves", async () => {
		daemonReady = true;
		apiGET.mockImplementation(async (path: string) => {
			if (path === "/api/v1/auth/me") {
				return {
					data: {
						status: "authenticated",
						user: { id: "u1", email: "qa@example.com", displayName: "QA", role: "owner" },
						authMethod: "oidc",
						issuer: "https://accounts.google.com",
						permissions: ["project.create"],
					},
				};
			}
			if (path === "/api/v1/auth/setup-status") return { data: { setupRequired: false } };
			return { data: { projects: [], sessions: [] } };
		});

		renderGate();

		expect(await screen.findByTestId("app-shell")).toBeInTheDocument();
		await waitFor(() => expect(requestedPaths()).toContain("/api/v1/projects"));
		expect(requestedPaths()).toContain("/api/v1/sessions");
	});

	it("holds the startup screen — not the shell — until a daemon that starts late answers", async () => {
		signedOutDaemon();

		renderGate();

		expect(await screen.findByTestId("shell-auth-pending")).toBeInTheDocument();
		expect(screen.queryByTestId("app-shell")).toBeNull();
		expect(requestedPaths()).toEqual([]);

		daemonReady = true;

		expect(await screen.findByRole("button", { name: "Sign in with Google" }, { timeout: 5_000 })).toBeInTheDocument();
		expect(screen.queryByTestId("app-shell")).toBeNull();
	});

	it("keeps the sign-in screen correct across a daemon restart while signed out", async () => {
		daemonReady = true;
		signedOutDaemon();

		renderGate();
		await screen.findByRole("button", { name: "Sign in with Google" });

		// The daemon goes away and comes back — on a new port, as a restart may.
		daemonReady = false;
		setApiBaseUrl(null);
		await new Promise((resolve) => setTimeout(resolve, 50));
		daemonReady = true;
		setApiBaseUrl("http://127.0.0.1:3003");
		await useAuthStore.getState().refreshForDaemonReady();

		expect(await screen.findByRole("button", { name: "Sign in with Google" })).toBeInTheDocument();
		expect(screen.queryByTestId("app-shell")).toBeNull();
		expect(requestedPaths()).not.toContain("/api/v1/projects");
	});

	it("offers account creation instead of sign-in on an installation with no accounts yet", async () => {
		daemonReady = true;
		apiGET.mockImplementation(async (path: string) => {
			if (path === "/api/v1/auth/me") return UNAUTHORIZED;
			if (path === "/api/v1/auth/setup-status") return { data: { setupRequired: true } };
			if (path === "/api/v1/auth/providers") return { data: GOOGLE_PROVIDERS };
			return UNAUTHORIZED;
		});

		renderGate();

		expect(await screen.findByRole("button", { name: "Create account" })).toBeInTheDocument();
		expect(screen.queryByTestId("app-shell")).toBeNull();
	});
});
