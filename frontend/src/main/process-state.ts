import { execFile } from "node:child_process";
import type { ProcessState } from "../shared/daemon-replace";

type ExecFile = (
	file: string,
	args: string[],
	options: { timeout: number },
	callback: (error: (Error & { code?: number | string }) | null, stdout: string) => void,
) => unknown;

type Kill = (pid: number, signal: 0) => void;

/**
 * Whether `pid` still exists, and whether it is only a zombie.
 *
 * kill(pid, 0) succeeds for a zombie: a process that has exited but whose
 * parent has not reaped it yet. A zombie has already closed every descriptor,
 * so the kernel has released any flock it held (the daemon's data-dir lock).
 * Anything that cannot be proven is "unknown" and the caller must fail closed.
 */
export async function readProcessState(
	pid: number,
	platform: NodeJS.Platform = process.platform,
	kill: Kill = (p, s) => process.kill(p, s),
	run: ExecFile = execFile as unknown as ExecFile,
): Promise<ProcessState> {
	if (!Number.isInteger(pid) || pid <= 0) return "unknown";
	try {
		kill(pid, 0);
	} catch (error) {
		const code = (error as NodeJS.ErrnoException).code;
		if (code === "ESRCH") return "gone";
		// EPERM: a process exists under another uid -- never ours, but it exists.
		return code === "EPERM" ? "alive" : "unknown";
	}
	if (platform === "win32") return "alive";
	return new Promise<ProcessState>((resolve) => {
		run("ps", ["-o", "stat=", "-p", String(pid)], { timeout: 2_000 }, (error, stdout) => {
			const stat = String(stdout ?? "").trim();
			if (error) {
				// ps exits 1 with no output when the pid vanished between the two checks.
				resolve(error.code === 1 && stat === "" ? "gone" : "unknown");
				return;
			}
			if (stat === "") resolve("unknown");
			else resolve(stat.startsWith("Z") ? "zombie" : "alive");
		});
	});
}
