import { describe, expect, it } from "vitest";
import { readProcessState } from "./process-state";

const errno = (code: string) => Object.assign(new Error(code), { code });
const killOk = () => undefined;
const killThrows = (code: string) => () => {
	throw errno(code);
};
const ps =
	(stdout: string, error: (Error & { code?: number | string }) | null = null) =>
	(_file: string, _args: string[], _opts: { timeout: number }, cb: (e: typeof error, out: string) => void) =>
		cb(error, stdout);

describe("readProcessState", () => {
	it("reports gone when kill(pid, 0) says no such process", async () => {
		expect(await readProcessState(10, "darwin", killThrows("ESRCH"), ps(""))).toBe("gone");
	});

	it("reports a zombie as zombie, not alive (kill(pid, 0) succeeds on zombies)", async () => {
		expect(await readProcessState(10, "darwin", killOk, ps("Z\n"))).toBe("zombie");
		expect(await readProcessState(10, "linux", killOk, ps("Z+\n"))).toBe("zombie");
	});

	it("reports a running process as alive", async () => {
		expect(await readProcessState(10, "darwin", killOk, ps("Ss\n"))).toBe("alive");
		expect(await readProcessState(10, "darwin", killThrows("EPERM"), ps(""))).toBe("alive");
	});

	it("treats a pid that vanished between kill and ps as gone", async () => {
		expect(await readProcessState(10, "darwin", killOk, ps("", Object.assign(new Error("exit 1"), { code: 1 })))).toBe(
			"gone",
		);
	});

	it("is unknown -- never exited -- when ps cannot answer", async () => {
		expect(
			await readProcessState(10, "darwin", killOk, ps("", Object.assign(new Error("ENOENT"), { code: "ENOENT" }))),
		).toBe("unknown");
		expect(await readProcessState(10, "darwin", killThrows("EINVAL"), ps(""))).toBe("unknown");
		expect(await readProcessState(0, "darwin", killOk, ps("S"))).toBe("unknown");
	});

	it("does not shell out on Windows, where there are no zombies", async () => {
		expect(
			await readProcessState(10, "win32", killOk, () => {
				throw new Error("ps must not run");
			}),
		).toBe("alive");
	});
});
