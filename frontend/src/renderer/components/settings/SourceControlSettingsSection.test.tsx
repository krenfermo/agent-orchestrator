import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { SourceControlSettingsSection } from "./SourceControlSettingsSection";

const { useEnvironmentStatusMock } = vi.hoisted(() => ({ useEnvironmentStatusMock: vi.fn() }));

vi.mock("../../hooks/useEnvironmentStatus", () => ({
	useEnvironmentStatus: useEnvironmentStatusMock,
}));

describe("SourceControlSettingsSection", () => {
	it("shows Not installed when gh is missing", () => {
		useEnvironmentStatusMock.mockReturnValue({
			status: {
				codex: { id: "codex", installed: false, authState: "unknown", source: "unknown", lastCheckedAt: "" },
				claude: { id: "claude-code", installed: false, authState: "unknown", source: "unknown", lastCheckedAt: "" },
				github: { installed: false, authState: "unknown", lastCheckedAt: "" },
				projects: { count: 0 },
				readiness: { codex: "unavailable", claude: "unavailable", github: "unavailable", projects: "unavailable", headless: "ready", overall: "setup_required" },
			},
			isLoading: false,
			error: undefined,
			testGitHub: vi.fn(),
			testingGitHub: false,
		});
		render(<SourceControlSettingsSection />);
		expect(screen.getByText("Not installed")).toBeInTheDocument();
	});

	it("shows authenticated login/host and never a token", () => {
		useEnvironmentStatusMock.mockReturnValue({
			status: {
				codex: { id: "codex", installed: false, authState: "unknown", source: "unknown", lastCheckedAt: "" },
				claude: { id: "claude-code", installed: false, authState: "unknown", source: "unknown", lastCheckedAt: "" },
				github: {
					installed: true,
					binaryPath: "/usr/local/bin/gh",
					version: "gh version 2.40.0",
					authState: "authenticated",
					login: "octocat",
					host: "github.com",
					lastCheckedAt: "",
				},
				projects: { count: 1 },
				readiness: { codex: "unavailable", claude: "unavailable", github: "ready", projects: "ready", headless: "ready", overall: "setup_required" },
			},
			isLoading: false,
			error: undefined,
			testGitHub: vi.fn(),
			testingGitHub: false,
		});
		const { container } = render(<SourceControlSettingsSection />);
		expect(screen.getByText("octocat")).toBeInTheDocument();
		expect(screen.getByText("github.com")).toBeInTheDocument();
		expect(screen.getByText("Authenticated")).toBeInTheDocument();
		expect(container.textContent ?? "").not.toMatch(/gh[oprsu]_[A-Za-z0-9]{20,}/);
	});

	it("distinguishes unauthenticated from authenticated", () => {
		useEnvironmentStatusMock.mockReturnValue({
			status: {
				codex: { id: "codex", installed: false, authState: "unknown", source: "unknown", lastCheckedAt: "" },
				claude: { id: "claude-code", installed: false, authState: "unknown", source: "unknown", lastCheckedAt: "" },
				github: { installed: true, binaryPath: "/usr/local/bin/gh", authState: "unauthenticated", lastCheckedAt: "" },
				projects: { count: 0 },
				readiness: { codex: "unavailable", claude: "unavailable", github: "auth_required", projects: "unavailable", headless: "ready", overall: "setup_required" },
			},
			isLoading: false,
			error: undefined,
			testGitHub: vi.fn(),
			testingGitHub: false,
		});
		render(<SourceControlSettingsSection />);
		expect(screen.getByText("Not authenticated")).toBeInTheDocument();
		expect(screen.queryByText("Authenticated")).not.toBeInTheDocument();
	});
});
