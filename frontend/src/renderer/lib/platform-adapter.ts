import type { DaemonStatus } from "../../shared/daemon-status";
import { aoBridge } from "./bridge";

export type PlatformMode = "desktop" | "web";

export type PlatformAdapter = {
	mode: PlatformMode;
	capabilities: {
		selectServerDirectory: boolean;
		nativeBrowser: boolean;
		updater: boolean;
	};
	readDaemonStatus(): Promise<DaemonStatus>;
	apiBaseUrl(status: DaemonStatus): string | null;
};

function browserPort(): number {
	if (window.location.port) return Number(window.location.port);
	return window.location.protocol === "https:" ? 443 : 80;
}

const desktopAdapter: PlatformAdapter = {
	mode: "desktop",
	capabilities: { selectServerDirectory: true, nativeBrowser: true, updater: true },
	readDaemonStatus: () => aoBridge.daemon.getStatus(),
	apiBaseUrl: (status) =>
		status.state === "ready" && status.port ? `http://127.0.0.1:${status.port}` : null,
};

const webAdapter: PlatformAdapter = {
	mode: "web",
	capabilities: { selectServerDirectory: false, nativeBrowser: false, updater: false },
	async readDaemonStatus() {
		try {
			const response = await fetch("/readyz", { headers: { Accept: "application/json" } });
			if (!response.ok) throw new Error(`ready probe returned ${response.status}`);
			const probe = (await response.json()) as { pid?: number; status?: string };
			if (probe.status !== "ready") throw new Error("daemon is not ready");
			return { state: "ready", port: browserPort(), pid: probe.pid };
		} catch (error) {
			return {
				state: "error",
				code: "daemon_unreachable",
				message: error instanceof Error ? error.message : "AO server is unavailable",
			};
		}
	},
	apiBaseUrl: (status) => (status.state === "ready" ? "" : null),
};

export const platformAdapter: PlatformAdapter = window.ao ? desktopAdapter : webAdapter;

export function isDesktopMode(): boolean {
	return platformAdapter.mode === "desktop";
}
