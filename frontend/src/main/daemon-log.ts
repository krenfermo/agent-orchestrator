import { closeSync, openSync, readSync, statSync } from "node:fs";

export const MAX_DAEMON_LOG_READBACK_BYTES = 64 * 1024;

/**
 * What a keep-alive daemon appended to its log file since `offset` (the size
 * the file had when that daemon was spawned), capped to the last `maxBytes`.
 * A file that shrank (truncated or rotated) yields nothing rather than another
 * process's older lines.
 */
export function readDaemonLogSince(
	logPath: string,
	offset: number,
	maxBytes: number = MAX_DAEMON_LOG_READBACK_BYTES,
): string {
	let fd: number | undefined;
	try {
		const size = statSync(logPath).size;
		if (size <= offset) return "";
		const start = Math.max(offset, size - maxBytes);
		const buffer = Buffer.alloc(size - start);
		fd = openSync(logPath, "r");
		const read = readSync(fd, buffer, 0, buffer.length, start);
		return buffer.subarray(0, read).toString("utf8");
	} catch {
		return "";
	} finally {
		if (fd !== undefined) {
			try {
				closeSync(fd);
			} catch {
				// best-effort
			}
		}
	}
}
