// @vitest-environment node
import { mkdirSync, mkdtempSync, rmSync, symlinkSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { sameCanonicalPath } from "./canonical-path";

const dirs: string[] = [];
afterEach(() => {
	for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
});
function tmp(): string {
	const d = mkdtempSync(path.join(os.tmpdir(), "ao-canon-"));
	dirs.push(d);
	return d;
}

describe("sameCanonicalPath (P9 data dir identity)", () => {
	it("matches the same directory through a symlink and with a trailing slash", () => {
		const root = tmp();
		const real = path.join(root, "data");
		mkdirSync(real);
		symlinkSync(real, path.join(root, "link"));
		expect(sameCanonicalPath(path.join(root, "link"), real)).toBe(true);
		expect(sameCanonicalPath(`${real}/`, real)).toBe(true);
		expect(sameCanonicalPath(path.join(root, "data", "..", "data"), real)).toBe(true);
	});

	it("never matches different directories, empty values, or an unresolvable path to another", () => {
		const root = tmp();
		mkdirSync(path.join(root, "x"));
		mkdirSync(path.join(root, "y"));
		expect(sameCanonicalPath(path.join(root, "x"), path.join(root, "y"))).toBe(false);
		expect(sameCanonicalPath("", path.join(root, "x"))).toBe(false);
		expect(sameCanonicalPath(path.join(root, "missing"), path.join(root, "x"))).toBe(false);
	});

	it("compares case-insensitively only on Windows", () => {
		const fail = () => {
			throw new Error("no realpath");
		};
		expect(sameCanonicalPath("C:\\AO\\Data", "c:\\ao\\data", "win32", fail)).toBe(true);
		expect(sameCanonicalPath("/Users/x/AO", "/Users/x/ao", "linux", fail)).toBe(false);
	});
});
