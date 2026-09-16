export type DaemonLaunchSpec = {
	command: string;
	args: string[];
	cwd: string;
	shell: boolean;
	source: "configured" | "bundled" | "dev";
};

function joinPath(...segments: string[]): string {
	return segments.map((segment) => segment.replace(/[/\\]+$/, "")).join("/");
}

export function bundledDaemonBinaryName(platform: NodeJS.Platform): string {
	return platform === "win32" ? "ao.exe" : "ao";
}

export function resolveDaemonLaunch(
	env: Record<string, string | undefined>,
	isPackaged: boolean,
	resourcesPath: string,
	appPath: string,
	homeDir: string,
	platform: NodeJS.Platform,
): DaemonLaunchSpec | null {
	const configuredCommand = env.AO_DAEMON_COMMAND?.trim();
	if (configuredCommand) {
		return {
			command: configuredCommand,
			args: [],
			cwd: appPath,
			shell: true,
			source: "configured",
		};
	}

	if (!isPackaged) {
		return {
			command: "go",
			args: ["run", "./cmd/ao", "daemon"],
			cwd: joinPath(appPath, "..", "backend"),
			shell: false,
			source: "dev",
		};
	}

	return {
		command: joinPath(resourcesPath, "daemon", bundledDaemonBinaryName(platform)),
		args: ["daemon"],
		cwd: joinPath(homeDir, ".ao"),
		shell: false,
		source: "bundled",
	};
}

export type BundledDaemonProbe = {
	executablePath?: string;
	appImagePath?: string;
};

/**
 * Identity check for a bundled daemon. Under AppImage the executable path is a
 * random /tmp/.mount_* path regenerated on every launch, so identity is the
 * stable outer .AppImage file path the daemon reports (AO_APPIMAGE, echoed as
 * appImagePath) — compared against this process's own APPIMAGE. Outside
 * AppImage the packaged executable path is stable and compared directly.
 *
 * Returns an error message, or null when the probed daemon belongs to this
 * install. A probe that cannot prove its identity (missing field) fails closed.
 */
export function bundledDaemonIdentityError(
	probe: BundledDaemonProbe,
	expectedCommand: string,
	appImagePath: string | undefined,
	samePath: (a: string, b: string) => boolean,
): string | null {
	if (appImagePath) {
		if (!probe.appImagePath) {
			return "An older AO daemon is already running, but it does not report its install identity. Stop it and restart this app.";
		}
		if (!samePath(probe.appImagePath, appImagePath)) {
			return `Another AO daemon is already running from ${probe.appImagePath}; expected ${appImagePath}. Stop the other daemon before using this app.`;
		}
		return null;
	}
	if (!probe.executablePath) {
		return "An older AO daemon is already running, but it does not report its binary path. Stop it and restart this app.";
	}
	if (!samePath(probe.executablePath, expectedCommand)) {
		return `Another AO daemon is already running from ${probe.executablePath}; expected ${expectedCommand}. Stop the other daemon before using this app.`;
	}
	return null;
}

/** The first word of a shell command (single/double quotes honoured), or null when unparseable. */
function commandExecutable(command: string): string | null {
	const text = command.trim();
	if (!text) return null;
	const quote = text[0];
	if (quote === "'" || quote === '"') {
		const end = text.indexOf(quote, 1);
		return end > 1 ? text.slice(1, end) : null;
	}
	const word = text.split(/\s+/)[0];
	return /['"\\$`]/.test(word) ? null : word;
}

/**
 * Identity check for a daemon started through AO_DAEMON_COMMAND. The command is
 * configuration, not proof of ownership: a running daemon is this launch's only
 * when it is the executable the command names (an absolute path, compared with
 * symlinks resolved by `samePath`). Anything that cannot be proven -- a bare
 * command name resolved through PATH, an unparseable command, a daemon that does
 * not report its binary -- fails closed.
 */
export function configuredDaemonIdentityError(
	probe: BundledDaemonProbe,
	configuredCommand: string,
	samePath: (a: string, b: string) => boolean,
): string | null {
	const executable = commandExecutable(configuredCommand);
	if (!executable || !/^(\/|[A-Za-z]:[\\/]|\\\\)/.test(executable)) {
		return "AO_DAEMON_COMMAND does not name an absolute daemon binary, so AO cannot prove a running daemon is the configured one. Stop it yourself, then restart the app.";
	}
	if (!probe.executablePath) {
		return "An AO daemon is already running, but it does not report its binary path. Stop it and restart this app.";
	}
	if (!samePath(probe.executablePath, executable)) {
		return `Another AO daemon is already running from ${probe.executablePath}; AO_DAEMON_COMMAND names ${executable}. Stop the other daemon before using this app.`;
	}
	return null;
}
