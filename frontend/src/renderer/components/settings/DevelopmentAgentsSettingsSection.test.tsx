import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { DevelopmentAgentsSettingsSection } from "./DevelopmentAgentsSettingsSection";
import type { ProviderDescriptor, ProviderProfile } from "../../hooks/useProviderProfiles";

const { useProviderProfilesMock, useCapacityMock } = vi.hoisted(() => ({
	useProviderProfilesMock: vi.fn(),
	useCapacityMock: vi.fn(),
}));

vi.mock("../../hooks/useProviderProfiles", () => ({
	useProviderProfiles: useProviderProfilesMock,
}));

vi.mock("../../hooks/useCapacity", () => ({
	useCapacity: useCapacityMock,
}));

function descriptor(overrides: Partial<ProviderDescriptor> = {}): ProviderDescriptor {
	return {
		provider: "anthropic",
		harness: "claude-code",
		displayName: "Claude Code",
		capabilities: ["worker", "planner"],
		authMethods: ["cli_bootstrap"],
		models: ["sonnet", "opus"],
		available: true,
		...overrides,
	} as ProviderDescriptor;
}

function profile(overrides: Partial<ProviderProfile> = {}): ProviderProfile {
	return {
		id: "prof-1",
		provider: "anthropic",
		harness: "claude-code",
		displayName: "Claude Code",
		enabled: true,
		authState: "unknown",
		authMethod: "cli_bootstrap",
		capabilities: ["worker", "planner"],
		createdAt: "2026-01-01T00:00:00Z",
		updatedAt: "2026-01-01T00:00:00Z",
		...overrides,
	} as ProviderProfile;
}

function baseHook(overrides: Partial<ReturnType<typeof useProviderProfilesMock>> = {}) {
	return {
		registry: [descriptor()],
		profiles: [],
		isLoading: false,
		error: undefined,
		createProfile: vi.fn(),
		connect: vi.fn(),
		disconnect: vi.fn(),
		test: vi.fn(),
		setEnabled: vi.fn(),
		...overrides,
	};
}

describe("DevelopmentAgentsSettingsSection", () => {
	beforeEach(() => {
		useCapacityMock.mockReturnValue({ capacity: undefined, isLoading: false, error: undefined });
	});

	it("renders a card per registry entry, not a hardcoded pair", () => {
		useProviderProfilesMock.mockReturnValue(
			baseHook({
				registry: [
					descriptor({ provider: "anthropic", harness: "claude-code", displayName: "Claude Code" }),
					descriptor({ provider: "openai", harness: "codex", displayName: "Codex" }),
					descriptor({ provider: "minimax", harness: "minimax", displayName: "MiniMax", available: false, unavailable: "no adapter yet", authMethods: ["unsupported"], capabilities: [] }),
				],
			}),
		);
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getByText("Claude Code")).toBeInTheDocument();
		expect(screen.getByText("Codex")).toBeInTheDocument();
		expect(screen.getByText("MiniMax")).toBeInTheDocument();
	});

	it("shows an unsupported provider honestly, without a Connect action", () => {
		useProviderProfilesMock.mockReturnValue(
			baseHook({
				registry: [descriptor({ provider: "minimax", harness: "minimax", displayName: "MiniMax", available: false, unavailable: "no MiniMax adapter is implemented in this codebase yet", authMethods: ["unsupported"], capabilities: [] })],
			}),
		);
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getByText("Unsupported")).toBeInTheDocument();
		expect(screen.getByText("no MiniMax adapter is implemented in this codebase yet")).toBeInTheDocument();
		expect(screen.queryByText("Connect")).not.toBeInTheDocument();
	});

	it("shows Not connected for a registry entry with no profile yet", () => {
		useProviderProfilesMock.mockReturnValue(baseHook({ registry: [descriptor()], profiles: [] }));
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getByText("Not connected")).toBeInTheDocument();
		expect(screen.getByText("Connect")).toBeInTheDocument();
	});

	it("shows Connected for a profile with authState authenticated", () => {
		useProviderProfilesMock.mockReturnValue(baseHook({ registry: [descriptor()], profiles: [profile({ authState: "authenticated" })] }));
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getByText("Connected")).toBeInTheDocument();
	});

	it("targets the correct profile id when testing a connection", () => {
		const testMock = vi.fn().mockResolvedValue({ ok: true, message: "authenticated" });
		useProviderProfilesMock.mockReturnValue(
			baseHook({ registry: [descriptor()], profiles: [profile({ id: "prof-xyz" })], test: testMock }),
		);
		render(<DevelopmentAgentsSettingsSection />);
		fireEvent.click(screen.getByText("Test connection"));
		expect(testMock).toHaveBeenCalledWith("prof-xyz");
	});

	it("creates then connects a profile for a provider with none yet", async () => {
		const createMock = vi.fn().mockResolvedValue(profile({ id: "prof-new" }));
		const connectMock = vi.fn().mockResolvedValue(profile({ id: "prof-new" }));
		useProviderProfilesMock.mockReturnValue(
			baseHook({ registry: [descriptor()], profiles: [], createProfile: createMock, connect: connectMock }),
		);
		render(<DevelopmentAgentsSettingsSection />);
		fireEvent.click(screen.getByText("Connect"));
		await vi.waitFor(() => expect(connectMock).toHaveBeenCalledWith("prof-new"));
	});
});
