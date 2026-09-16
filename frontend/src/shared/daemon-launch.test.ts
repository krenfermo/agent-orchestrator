import { describe, expect, it } from "vitest";
import { bundledDaemonIdentityError, configuredDaemonIdentityError, resolveDaemonLaunch } from "./daemon-launch";

describe("resolveDaemonLaunch", () => {
	it("uses AO_DAEMON_COMMAND when configured", () => {
		expect(
			resolveDaemonLaunch({ AO_DAEMON_COMMAND: "/tmp/ao daemon" }, false, "/resources", "/app", "/home/user", "darwin"),
		).toEqual({
			command: "/tmp/ao daemon",
			args: [],
			cwd: "/app",
			shell: true,
			source: "configured",
		});
	});

	it("runs the backend daemon from source in dev without an explicit command", () => {
		expect(resolveDaemonLaunch({}, false, "/resources", "/repo/frontend", "/home/user", "darwin")).toEqual({
			command: "go",
			args: ["run", "./cmd/ao", "daemon"],
			cwd: "/repo/frontend/../backend",
			shell: false,
			source: "dev",
		});
	});

	it("uses the bundled daemon binary for packaged macOS/Linux builds", () => {
		expect(
			resolveDaemonLaunch(
				{},
				true,
				"/Applications/Agent Orchestrator.app/Contents/Resources",
				"/app",
				"/Users/alice",
				"darwin",
			),
		).toEqual({
			command: "/Applications/Agent Orchestrator.app/Contents/Resources/daemon/ao",
			args: ["daemon"],
			cwd: "/Users/alice/.ao",
			shell: false,
			source: "bundled",
		});
	});

	it("uses the bundled daemon exe for packaged Windows builds", () => {
		expect(
			resolveDaemonLaunch(
				{},
				true,
				"C:\\Program Files\\AO\\resources",
				"C:\\Program Files\\AO\\resources\\app.asar",
				"C:\\Users\\alice",
				"win32",
			),
		).toEqual({
			command: "C:\\Program Files\\AO\\resources/daemon/ao.exe",
			args: ["daemon"],
			cwd: "C:\\Users\\alice/.ao",
			shell: false,
			source: "bundled",
		});
	});
});

describe("bundledDaemonIdentityError", () => {
	const samePath = (a: string, b: string): boolean => a === b;
	const appImage = "/home/user/Apps/agent-orchestrator.AppImage";
	// The bundled command under AppImage: a random FUSE mount, different per launch.
	const launchCommand = "/tmp/.mount_agent-mDQfUL/resources/daemon/ao";

	it("accepts the same install across two AppImage mounts (relaunch-to-update)", () => {
		const probe = {
			executablePath: "/tmp/.mount_agent-1Qs4N6/resources/daemon/ao",
			appImagePath: appImage,
		};
		expect(bundledDaemonIdentityError(probe, launchCommand, appImage, samePath)).toBeNull();
	});

	it("rejects a daemon from a different AppImage install", () => {
		const other = "/home/user/Apps/agent-orchestrator-nightly.AppImage";
		const probe = { executablePath: "/tmp/.mount_agent-1Qs4N6/resources/daemon/ao", appImagePath: other };
		expect(bundledDaemonIdentityError(probe, launchCommand, appImage, samePath)).toBe(
			`Another AO daemon is already running from ${other}; expected ${appImage}. Stop the other daemon before using this app.`,
		);
	});

	it("fails closed under AppImage when the daemon does not report its install identity", () => {
		const probe = { executablePath: "/tmp/.mount_agent-1Qs4N6/resources/daemon/ao" };
		expect(bundledDaemonIdentityError(probe, launchCommand, appImage, samePath)).toBe(
			"An older AO daemon is already running, but it does not report its install identity. Stop it and restart this app.",
		);
	});

	it("compares executable paths outside AppImage", () => {
		const command = "/opt/Agent Orchestrator/resources/daemon/ao";
		expect(bundledDaemonIdentityError({ executablePath: command }, command, undefined, samePath)).toBeNull();
		expect(bundledDaemonIdentityError({ executablePath: "/other/ao" }, command, undefined, samePath)).toBe(
			`Another AO daemon is already running from /other/ao; expected ${command}. Stop the other daemon before using this app.`,
		);
	});

	it("fails closed outside AppImage when the daemon does not report its binary path", () => {
		expect(bundledDaemonIdentityError({}, "/opt/ao/resources/daemon/ao", undefined, samePath)).toBe(
			"An older AO daemon is already running, but it does not report its binary path. Stop it and restart this app.",
		);
	});
});

describe("configuredDaemonIdentityError (AO_DAEMON_COMMAND is not an ownership exception)", () => {
	const same = (a: string, b: string) => a === b;

	it("8. a daemon of another binary than the configured command is not ours", () => {
		expect(configuredDaemonIdentityError({ executablePath: "/frozen/ao" }, "/frozen/ao daemon", same)).toBeNull();
		expect(configuredDaemonIdentityError({ executablePath: "/frozen/ao" }, "'/frozen/ao' daemon", same)).toBeNull();
		expect(
			configuredDaemonIdentityError({ executablePath: "/other/ao" }, "/frozen/ao daemon", same),
		).toContain("/other/ao");
	});

	it("fails closed when the command or the daemon cannot prove a binary", () => {
		expect(configuredDaemonIdentityError({ executablePath: "/frozen/ao" }, "ao daemon", same)).not.toBeNull();
		expect(configuredDaemonIdentityError({}, "/frozen/ao daemon", same)).not.toBeNull();
		expect(configuredDaemonIdentityError({ executablePath: "/frozen/ao" }, "'unterminated daemon", same)).not.toBeNull();
	});
});

describe("configuredDaemonIdentityError on Windows paths", () => {
	it("accepts an absolute Windows binary", () => {
		expect(
			configuredDaemonIdentityError({ executablePath: "C:\\ao\\ao.exe" }, '"C:\\ao\\ao.exe" daemon', (a, b) => a === b),
		).toBeNull();
	});
});

describe("configuredDaemonIdentityError command parsing", () => {
	const same = (a: string, b: string) => a === b;
	it("accepts an unquoted Windows path and rejects text glued to a closing quote", () => {
		expect(configuredDaemonIdentityError({ executablePath: "C:\\ao\\ao.exe" }, "C:\\ao\\ao.exe daemon", same)).toBeNull();
		expect(configuredDaemonIdentityError({ executablePath: "/opt/ao" }, '"/opt/ao"x daemon', same)).not.toBeNull();
	});
	it("refuses wrappers it cannot prove (env prefix, bare names)", () => {
		expect(configuredDaemonIdentityError({ executablePath: "/bin/ao" }, "env X=1 /bin/ao daemon", same)).not.toBeNull();
		expect(configuredDaemonIdentityError({ executablePath: "/bin/ao" }, "ao daemon", same)).not.toBeNull();
	});
});
