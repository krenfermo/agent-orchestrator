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

	it("shows Setup required (not Ready) for a registry entry with no profile yet", () => {
		useProviderProfilesMock.mockReturnValue(baseHook({ registry: [descriptor()], profiles: [] }));
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getByText("Setup required")).toBeInTheDocument();
		expect(screen.getByText("Connect")).toBeInTheDocument();
	});

	it("shows AO execution Ready only when enabled AND authenticated", () => {
		useProviderProfilesMock.mockReturnValue(
			baseHook({ registry: [descriptor()], profiles: [profile({ authState: "authenticated", enabled: true })] }),
		);
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getByText("Ready")).toBeInTheDocument();
		expect(screen.getAllByText("Connected").length).toBeGreaterThan(0);
	});

	it("shows Setup required, not Ready, for an enabled-but-unauthenticated profile (never fakes Ready from binary presence alone)", () => {
		useProviderProfilesMock.mockReturnValue(
			baseHook({ registry: [descriptor()], profiles: [profile({ authState: "unauthenticated", enabled: true })] }),
		);
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getByText("Setup required")).toBeInTheDocument();
		expect(screen.queryByText("Ready")).not.toBeInTheDocument();
	});

	it("shows Connected, disabled for workflows for a connected-but-disabled profile", () => {
		useProviderProfilesMock.mockReturnValue(
			baseHook({ registry: [descriptor()], profiles: [profile({ authState: "authenticated", enabled: false })] }),
		);
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getByText("Disabled")).toBeInTheDocument();
		expect(screen.getByText("Connected, disabled for workflows.")).toBeInTheDocument();
	});

	it("renders Not installed and Unavailable execution for a not_installed auth state, never Ready", () => {
		useProviderProfilesMock.mockReturnValue(
			baseHook({ registry: [descriptor()], profiles: [profile({ authState: "not_installed", enabled: true })] }),
		);
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getByText("Not installed")).toBeInTheDocument();
		expect(screen.getByText("Unavailable")).toBeInTheDocument();
	});

	it("targets the correct profile id when testing a connection", () => {
		const testMock = vi.fn().mockResolvedValue({ ok: true, message: "Claude Code is authenticated and ready for AO workflows." });
		useProviderProfilesMock.mockReturnValue(
			baseHook({ registry: [descriptor()], profiles: [profile({ id: "prof-xyz" })], test: testMock }),
		);
		render(<DevelopmentAgentsSettingsSection />);
		fireEvent.click(screen.getByText("Test connection"));
		expect(testMock).toHaveBeenCalledWith("prof-xyz");
	});

	it("shows a persistent success message after a successful Test Connection (not silently dropped)", async () => {
		const testMock = vi.fn().mockResolvedValue({ ok: true, message: "Claude Code is authenticated and ready for AO workflows." });
		useProviderProfilesMock.mockReturnValue(baseHook({ registry: [descriptor()], profiles: [profile()], test: testMock }));
		render(<DevelopmentAgentsSettingsSection />);
		fireEvent.click(screen.getByText("Test connection"));
		expect(await screen.findByText("Connection successful")).toBeInTheDocument();
		expect(screen.getByText("Claude Code is authenticated and ready for AO workflows.")).toBeInTheDocument();
	});

	it("shows an actionable auth-failure message after a failed Test Connection (not silently dropped)", async () => {
		const testMock = vi.fn().mockResolvedValue({
			ok: false,
			message: "Claude Code is installed, but this AO user is not authenticated. Run the provider's own login inside a session for this profile, then test again.",
		});
		useProviderProfilesMock.mockReturnValue(baseHook({ registry: [descriptor()], profiles: [profile()], test: testMock }));
		render(<DevelopmentAgentsSettingsSection />);
		fireEvent.click(screen.getByText("Test connection"));
		expect(await screen.findByText("Authentication required")).toBeInTheDocument();
		expect(
			screen.getByText(
				"Claude Code is installed, but this AO user is not authenticated. Run the provider's own login inside a session for this profile, then test again.",
			),
		).toBeInTheDocument();
	});

	it("never renders raw internal enum strings (cli_bootstrap, planner, worker, ...) to the user", () => {
		useProviderProfilesMock.mockReturnValue(
			baseHook({
				registry: [descriptor({ capabilities: ["planner", "worker", "decision_resolver"], authMethods: ["cli_bootstrap"] })],
				profiles: [profile({ capabilities: ["planner", "worker", "decision_resolver"], authMethod: "cli_bootstrap" })],
			}),
		);
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.queryByText("cli_bootstrap")).not.toBeInTheDocument();
		expect(screen.queryByText("decision_resolver")).not.toBeInTheDocument();
		expect(screen.getByText("CLI account")).toBeInTheDocument();
		expect(screen.getByText("Decision resolver", { exact: false })).toBeInTheDocument();
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

	it("calls disconnect with the profile id", () => {
		const disconnectMock = vi.fn().mockResolvedValue(profile());
		useProviderProfilesMock.mockReturnValue(baseHook({ registry: [descriptor()], profiles: [profile({ id: "prof-1" })], disconnect: disconnectMock }));
		render(<DevelopmentAgentsSettingsSection />);
		fireEvent.click(screen.getByText("Disconnect"));
		expect(disconnectMock).toHaveBeenCalledWith("prof-1");
	});

	it("calls setEnabled when the Enabled switch is toggled", () => {
		const setEnabledMock = vi.fn().mockResolvedValue(profile());
		useProviderProfilesMock.mockReturnValue(
			baseHook({ registry: [descriptor()], profiles: [profile({ id: "prof-1", enabled: true })], setEnabled: setEnabledMock }),
		);
		render(<DevelopmentAgentsSettingsSection />);
		fireEvent.click(screen.getByRole("switch"));
		expect(setEnabledMock).toHaveBeenCalledWith(expect.objectContaining({ id: "prof-1", enabled: false }));
	});

	it("shows a friendly capacity label instead of a raw state string", () => {
		useCapacityMock.mockReturnValue({
			capacity: [{ harness: "claude-code", state: "limited", certainty: "actual", detectedAt: null, resetAt: null }],
			isLoading: false,
			error: undefined,
		});
		useProviderProfilesMock.mockReturnValue(baseHook({ registry: [descriptor()], profiles: [profile({ authState: "authenticated" })] }));
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getByText("Temporarily limited")).toBeInTheDocument();
		expect(screen.queryByText("limited")).not.toBeInTheDocument();
	});

	it("shows the descriptor's first model as the default when the profile has none set, never a bare em-dash", () => {
		useProviderProfilesMock.mockReturnValue(baseHook({ registry: [descriptor({ models: ["sonnet", "opus"] })], profiles: [profile({ defaultModel: "" })] }));
		render(<DevelopmentAgentsSettingsSection />);
		expect(screen.getByText("sonnet")).toBeInTheDocument();
	});
});
