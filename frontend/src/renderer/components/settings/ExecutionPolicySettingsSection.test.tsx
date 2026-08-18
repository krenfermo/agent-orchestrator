import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ExecutionPolicySettingsSection } from "./ExecutionPolicySettingsSection";
import type { ExecutionPolicy } from "../../hooks/useExecutionPolicy";
import type { ProviderDescriptor, ProviderProfile } from "../../hooks/useProviderProfiles";

const { useExecutionPolicyMock, useProviderProfilesMock } = vi.hoisted(() => ({
	useExecutionPolicyMock: vi.fn(),
	useProviderProfilesMock: vi.fn(),
}));

vi.mock("../../hooks/useExecutionPolicy", () => ({
	useExecutionPolicy: useExecutionPolicyMock,
}));
vi.mock("../../hooks/useProviderProfiles", () => ({
	useProviderProfiles: useProviderProfilesMock,
}));

function descriptor(overrides: Partial<ProviderDescriptor> = {}): ProviderDescriptor {
	return {
		provider: "anthropic",
		harness: "claude-code",
		displayName: "Claude Code",
		capabilities: ["worker", "planner", "reviewer", "decision_resolver"],
		authMethods: ["cli_bootstrap"],
		models: ["sonnet"],
		available: true,
		...overrides,
	} as ProviderDescriptor;
}

function profile(overrides: Partial<ProviderProfile> = {}): ProviderProfile {
	return {
		id: "prof-claude",
		provider: "anthropic",
		harness: "claude-code",
		displayName: "Claude Code",
		enabled: true,
		authState: "authenticated",
		authMethod: "cli_bootstrap",
		capabilities: ["worker", "planner", "reviewer", "decision_resolver"],
		createdAt: "2026-01-01T00:00:00Z",
		updatedAt: "2026-01-01T00:00:00Z",
		...overrides,
	} as ProviderProfile;
}

function policy(overrides: Partial<ExecutionPolicy> = {}): ExecutionPolicy {
	return {
		autonomousMode: false,
		plannerPriority: ["prof-claude"],
		workerPriority: ["prof-claude", "prof-codex"],
		reviewerPriority: ["prof-codex", "prof-claude"],
		decisionResolverPriority: ["prof-codex", "prof-claude"],
		fallbackBehavior: "use_next_available",
		reviewIndependence: "require_different_provider",
		...overrides,
	} as ExecutionPolicy;
}

describe("ExecutionPolicySettingsSection", () => {
	const saveMock = vi.fn().mockResolvedValue(policy());

	beforeEach(() => {
		saveMock.mockClear();
		useExecutionPolicyMock.mockReturnValue({
			policy: policy(),
			isLoading: false,
			error: undefined,
			save: saveMock,
			isSaving: false,
		});
		useProviderProfilesMock.mockReturnValue({
			registry: [
				descriptor({ provider: "anthropic", harness: "claude-code", displayName: "Claude Code" }),
				descriptor({ provider: "openai", harness: "codex", displayName: "Codex" }),
				descriptor({ provider: "minimax", harness: "minimax", displayName: "MiniMax", available: false, capabilities: [] }),
			],
			profiles: [
				profile({ id: "prof-claude", provider: "anthropic", harness: "claude-code", displayName: "Claude Code" }),
				profile({ id: "prof-codex", provider: "openai", harness: "codex", displayName: "Codex" }),
			],
			isLoading: false,
			error: undefined,
		});
	});

	it("renders the current user's own profiles in priority order per role", () => {
		render(<ExecutionPolicySettingsSection />);
		const worker = screen.getByText("Worker priority").closest("div") as HTMLElement;
		expect(worker).toBeTruthy();
		expect(screen.getAllByText("Claude Code").length).toBeGreaterThan(0);
		expect(screen.getAllByText("Codex").length).toBeGreaterThan(0);
	});

	it("never renders an unsupported provider (MiniMax) as a selectable priority entry", () => {
		render(<ExecutionPolicySettingsSection />);
		expect(screen.queryByText("MiniMax")).not.toBeInTheDocument();
	});

	it("shows a status badge for a disabled/unconnected profile without removing it from the list", () => {
		useProviderProfilesMock.mockReturnValue({
			registry: [descriptor()],
			profiles: [profile({ id: "prof-claude", enabled: true, authState: "unauthenticated" })],
			isLoading: false,
			error: undefined,
		});
		useExecutionPolicyMock.mockReturnValue({
			policy: policy({ workerPriority: ["prof-claude"], plannerPriority: ["prof-claude"], reviewerPriority: [], decisionResolverPriority: [] }),
			isLoading: false,
			error: undefined,
			save: saveMock,
			isSaving: false,
		});
		render(<ExecutionPolicySettingsSection />);
		expect(screen.getAllByText("Not connected").length).toBeGreaterThan(0);
		expect(screen.getAllByText("Claude Code").length).toBeGreaterThan(0);
	});

	it("saves the fallback behavior selection", async () => {
		render(<ExecutionPolicySettingsSection />);
		const user = userEvent.setup();
		await user.click(screen.getByRole("combobox", { name: "Fallback" }));
		await user.click(await screen.findByRole("option", { name: "Wait for preferred" }));
		expect(saveMock).toHaveBeenCalledWith(expect.objectContaining({ fallbackBehavior: "wait_for_preferred" }));
	});

	it("saves the review independence selection", async () => {
		render(<ExecutionPolicySettingsSection />);
		const user = userEvent.setup();
		await user.click(screen.getByRole("combobox", { name: "Review independence" }));
		await user.click(await screen.findByRole("option", { name: "Allow same-provider fallback" }));
		expect(saveMock).toHaveBeenCalledWith(expect.objectContaining({ reviewIndependence: "allow_same_provider_fallback" }));
	});

	it("saves the autonomy toggle", () => {
		render(<ExecutionPolicySettingsSection />);
		const toggle = screen.getAllByRole("switch")[0];
		fireEvent.click(toggle);
		expect(saveMock).toHaveBeenCalledWith(expect.objectContaining({ autonomousMode: true }));
	});
});
