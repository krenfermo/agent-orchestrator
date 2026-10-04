import { beforeEach, describe, expect, it, vi } from "vitest";
import type { DaemonStatus } from "../../shared/daemon-status";

const apiGET = vi.fn();
const apiPOST = vi.fn();

vi.mock("@tanstack/react-router", async (importOriginal) => ({
	...(await importOriginal<typeof import("@tanstack/react-router")>()),
	createFileRoute: () => (options: unknown) => ({ options }),
}));

vi.mock("../lib/api-client", async () => {
	const actual = await vi.importActual<typeof import("../lib/api-client")>("../lib/api-client");
	return {
		...actual,
		apiClient: { GET: (...args: unknown[]) => apiGET(...args), POST: (...args: unknown[]) => apiPOST(...args) },
	};
});

let daemonReady = true;

vi.mock("../lib/daemon-status", async () => {
	const { setApiBaseUrl } = await vi.importActual<typeof import("../lib/api-client")>("../lib/api-client");
	return {
		refreshDaemonStatus: async (): Promise<DaemonStatus> => {
			setApiBaseUrl(daemonReady ? "http://127.0.0.1:3002" : null);
			return daemonReady ? { state: "ready", port: 3002 } : { state: "starting" };
		},
	};
});

import { setApiBaseUrl } from "../lib/api-client";
import { Route } from "../routes/_shell";
import { useAuthStore } from "../stores/auth-store";

type ShellLoader = (args: { context: { queryClient: { ensureQueryData: ReturnType<typeof vi.fn> } } }) => Promise<unknown>;

const ensureQueryData = vi.fn().mockResolvedValue([]);

function runLoader() {
	// createFileRoute is mocked above, so this is the plain options object the
	// mock returned — the real Route type does not describe it.
	const loader = (Route.options as unknown as { loader: ShellLoader }).loader;
	return loader({ context: { queryClient: { ensureQueryData } } });
}

beforeEach(() => {
	apiGET.mockReset();
	ensureQueryData.mockClear();
	daemonReady = true;
	setApiBaseUrl(null);
	useAuthStore.setState({
		user: null,
		status: "loading",
		error: null,
		setupRequired: null,
		providers: null,
		providersStatus: "idle",
		permissions: [],
	});
});

// The loader is the other half of the gate: rendering the sign-in screen is no
// use if the route already fetched the project list on the way to it.
describe("_shell loader", () => {
	it("does not prefetch the workspace list for a principal the daemon refuses", async () => {
		apiGET.mockImplementation(async (path: string) => {
			if (path === "/api/v1/auth/me") {
				return { error: { code: "NOT_AUTHENTICATED" }, response: { status: 401 } };
			}
			if (path === "/api/v1/auth/setup-status") return { data: { setupRequired: false } };
			if (path === "/api/v1/auth/providers") return { data: { mode: "oidc", passwordEnabled: true } };
			return { error: { code: "NOT_AUTHENTICATED" }, response: { status: 401 } };
		});

		await runLoader();

		expect(useAuthStore.getState().status).toBe("unauthenticated");
		expect(ensureQueryData).not.toHaveBeenCalled();
	});

	it("prefetches it once an identity resolves", async () => {
		apiGET.mockImplementation(async (path: string) => {
			if (path === "/api/v1/auth/me") {
				return { data: { status: "authenticated", user: { id: "u1" }, permissions: [] } };
			}
			if (path === "/api/v1/auth/setup-status") return { data: { setupRequired: false } };
			return { data: {} };
		});

		await runLoader();

		expect(ensureQueryData).toHaveBeenCalledTimes(1);
	});

	it("asks for nothing at all while the daemon is still starting", async () => {
		daemonReady = false;

		await runLoader();

		expect(apiGET).not.toHaveBeenCalled();
		expect(ensureQueryData).not.toHaveBeenCalled();
		expect(useAuthStore.getState().status).toBe("loading");
	});
});
