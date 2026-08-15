import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { EnvironmentSettingsSection } from "./EnvironmentSettingsSection";

const { useEnvironmentStatusMock } = vi.hoisted(() => ({ useEnvironmentStatusMock: vi.fn() }));

vi.mock("../../hooks/useEnvironmentStatus", () => ({
	useEnvironmentStatus: useEnvironmentStatusMock,
}));

function statusWith(overall: "ready" | "setup_required", projectCount: number) {
	return {
		codex: { id: "codex", installed: true, authState: "authorized", source: "unknown", lastCheckedAt: "" },
		claude: { id: "claude-code", installed: false, authState: "unknown", source: "unknown", lastCheckedAt: "" },
		github: { installed: true, authState: "authenticated", lastCheckedAt: "" },
		projects: { count: projectCount },
		readiness: {
			codex: "ready",
			claude: "unavailable",
			github: "ready",
			projects: projectCount > 0 ? "ready" : "unavailable",
			headless: "ready",
			overall,
		},
	};
}

describe("EnvironmentSettingsSection", () => {
	it("shows READY FOR AUTONOMOUS WORKFLOWS when overall readiness is ready", () => {
		useEnvironmentStatusMock.mockReturnValue({ status: statusWith("ready", 2), isLoading: false, error: undefined, refetch: vi.fn() });
		render(<EnvironmentSettingsSection />);
		expect(screen.getByText("Ready for autonomous workflows")).toBeInTheDocument();
		expect(screen.getByText("2 registered")).toBeInTheDocument();
	});

	it("shows SETUP REQUIRED when no projects are registered", () => {
		useEnvironmentStatusMock.mockReturnValue({ status: statusWith("setup_required", 0), isLoading: false, error: undefined, refetch: vi.fn() });
		render(<EnvironmentSettingsSection />);
		expect(screen.getByText("Setup required")).toBeInTheDocument();
	});

	it("surfaces a probe error without crashing", () => {
		useEnvironmentStatusMock.mockReturnValue({ status: undefined, isLoading: false, error: "Request failed", refetch: vi.fn() });
		render(<EnvironmentSettingsSection />);
		expect(screen.getByText("Request failed")).toBeInTheDocument();
	});
});
