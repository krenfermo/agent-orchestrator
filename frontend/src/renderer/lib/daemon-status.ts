import { setApiBaseUrl, setApiDaemonStatus } from "./api-client";
import { platformAdapter } from "./platform-adapter";

export type DaemonStatus = Awaited<ReturnType<typeof platformAdapter.readDaemonStatus>>;

export function applyDaemonStatus(nextStatus: DaemonStatus): void {
	setApiDaemonStatus(nextStatus);
	setApiBaseUrl(platformAdapter.apiBaseUrl(nextStatus));
}

export async function refreshDaemonStatus(): Promise<DaemonStatus> {
	const nextStatus = await readDaemonStatus();
	applyDaemonStatus(nextStatus);
	return nextStatus;
}

export function readDaemonStatus(): Promise<DaemonStatus> {
	return platformAdapter.readDaemonStatus();
}
