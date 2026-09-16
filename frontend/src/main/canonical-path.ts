import { realpathSync } from "node:fs";
import path from "node:path";

function pathKey(value: string, platform: NodeJS.Platform): string {
	const resolved = path.resolve(value);
	return platform === "win32" ? resolved.toLowerCase() : resolved;
}

/**
 * The daemon's own sameDataDir (backend/internal/cli/daemon_discovery.go): the
 * same path once normalized, or once both resolve (symlinks, and on macOS the
 * on-disk case via realpath.native) to the same place. A path that cannot be
 * resolved only matches itself literally -- never by guessing.
 */
export function sameCanonicalPath(
	a: string,
	b: string,
	platform: NodeJS.Platform = process.platform,
	realpath: (p: string) => string = realpathSync.native,
): boolean {
	if (!a || !b) return false;
	if (pathKey(a, platform) === pathKey(b, platform)) return true;
	try {
		return pathKey(realpath(a), platform) === pathKey(realpath(b), platform);
	} catch {
		return false;
	}
}
