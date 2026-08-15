import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DevelopmentAgentsSettingsSection } from "./DevelopmentAgentsSettingsSection";
import type { EnvironmentStatus } from "../../hooks/useEnvironmentStatus";

const { useEnvironmentStatusMock } = vi.hoisted(() => ({ useEnvironmentStatusMock: vi.fn() }));

vi.mock("../../hooks/useEnvironmentStatus", () => ({
	useEnvironmentStatus: useEnvironmentStatusMock,
}));

function baseStatus(overrides: Partial<EnvironmentStatus> = {}): EnvironmentStatus {
	return {
		codex: { id: "codex", installed: false, authState: "unknown", source: "unknown", lastCheckedAt: "2026-01-01T00:00:00Z" },
		claude: { id: "claude-code", installed: false, authState: "unknown", source: "unknown", lastCheckedAt: "2026-01-01T00:00:00Z" },
		github: { installed: false, authState: "unknown", lastCheckedAt: "2026-01-01T00:00:00Z" },
		projects: { count: 0 },
		readiness: { codex: "unavailable", claude: "unavailable", github: "unavailable", projects: "unavailable", headless: "ready", overall: "setup_required" },
		...overrides,
	} as EnvironmentStatus;
}

describe("DevelopmentAgentsSettingsSection", () => {
	it("shows Not installed when a binary was not resolved", () => {
		useEnvironmentStatusMock.mockReturnValue({ status: baseStatus(), isLoading: false, error: undefined, refetch: vi.fn() });
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getAllByText("Not installed")).toHaveLength(2);
	});

	it("shows Installed + Authenticated for a probed, authorized agent, with binary path and version", () => {
		useEnvironmentStatusMock.mockReturnValue({
			status: baseStatus({
				codex: {
					id: "codex",
					installed: true,
					binaryPath: "/usr/local/bin/codex",
					version: "codex-cli 1.2.3",
					authState: "authorized",
					source: "unknown",
					lastCheckedAt: "2026-01-01T00:00:00Z",
				},
			}),
			isLoading: false,
			error: undefined,
			refetch: vi.fn(),
		});
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getByText("Installed")).toBeInTheDocument();
		expect(screen.getByText("Authenticated")).toBeInTheDocument();
		expect(screen.getByText("/usr/local/bin/codex")).toBeInTheDocument();
		expect(screen.getByText("codex-cli 1.2.3")).toBeInTheDocument();
	});

	it("never renders unknown auth as authenticated", () => {
		useEnvironmentStatusMock.mockReturnValue({
			status: baseStatus({
				claude: { id: "claude-code", installed: true, binaryPath: "/usr/local/bin/claude", authState: "unknown", source: "unknown", lastCheckedAt: "2026-01-01T00:00:00Z" },
			}),
			isLoading: false,
			error: undefined,
			refetch: vi.fn(),
		});
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.queryByText("Authenticated")).not.toBeInTheDocument();
		expect(screen.getAllByText("Unknown").length).toBeGreaterThan(0);
	});

	it("never renders a token-shaped string anywhere in the DOM", () => {
		useEnvironmentStatusMock.mockReturnValue({
			status: baseStatus({
				codex: {
					id: "codex",
					installed: true,
					binaryPath: "/usr/local/bin/codex",
					version: "codex-cli 1.2.3",
					authState: "authorized",
					source: "unknown",
					lastCheckedAt: "2026-01-01T00:00:00Z",
				},
			}),
			isLoading: false,
			error: undefined,
			refetch: vi.fn(),
		});
		const { container } = render(<DevelopmentAgentsSettingsSection />);
		expect(container.textContent ?? "").not.toMatch(/sk-[A-Za-z0-9]{20,}|gh[oprsu]_[A-Za-z0-9]{20,}/);
	});
});
