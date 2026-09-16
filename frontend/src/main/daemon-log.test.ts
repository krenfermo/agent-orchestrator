// @vitest-environment node
import { appendFileSync, mkdtempSync, rmSync, truncateSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { readDaemonLogSince } from "./daemon-log";

const dirs: string[] = [];
function logFile(contents: string): string {
	const dir = mkdtempSync(path.join(os.tmpdir(), "ao-daemon-log-"));
	dirs.push(dir);
	const file = path.join(dir, "daemon.log");
	writeFileSync(file, contents);
	return file;
}

afterEach(() => {
	for (const dir of dirs.splice(0)) rmSync(dir, { recursive: true, force: true });
});

describe("readDaemonLogSince", () => {
	it("returns only what this daemon appended, never older runs' lines", () => {
		const file = logFile("another AO daemon holds data dir /d; refusing to start\n");
		const offset = Buffer.byteLength("another AO daemon holds data dir /d; refusing to start\n");
		appendFileSync(file, "panic: boom\n");
		expect(readDaemonLogSince(file, offset)).toBe("panic: boom\n");
	});

	it("keeps only the tail when the daemon wrote more than the cap", () => {
		const file = logFile("");
		appendFileSync(file, "a".repeat(100) + "TAIL");
		expect(readDaemonLogSince(file, 0, 4)).toBe("TAIL");
	});

	it("returns nothing when the file shrank (truncated or rotated) or is missing", () => {
		const file = logFile("x".repeat(50));
		truncateSync(file, 10);
		expect(readDaemonLogSince(file, 50)).toBe("");
		expect(readDaemonLogSince(path.join(os.tmpdir(), "does-not-exist-ao.log"), 0)).toBe("");
	});
});
