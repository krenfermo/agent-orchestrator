import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ProviderSetupDialog } from "./ProviderSetupDialog";

const { useProviderSetupMock } = vi.hoisted(() => ({ useProviderSetupMock: vi.fn() }));

vi.mock("../../hooks/useProviderSetup", () => ({
	useProviderSetup: useProviderSetupMock,
}));

vi.mock("../../lib/shell-context", () => ({
	useShellMaybe: () => ({ daemonStatus: { state: "ready" } }),
}));

vi.mock("../../stores/ui-store", () => ({
	useResolvedTheme: () => "dark",
}));

// The embedded terminal itself is exercised by TerminalPane's own tests; here
// we only need to know the dialog asked it to attach to the right handle.
vi.mock("../TerminalPane", () => ({
	TerminalPane: ({ terminalTarget }: { terminalTarget: { handleId: string } }) => (
		<div data-testid="terminal-pane">attached:{terminalTarget.handleId}</div>
	),
}));

function baseState(overrides: Partial<ReturnType<typeof useProviderSetupMock>> = {}) {
	return {
		phase: "idle",
		handleId: undefined,
		instructions: undefined,
		error: undefined,
		start: vi.fn(),
		stop: vi.fn(),
		...overrides,
	};
}

describe("ProviderSetupDialog", () => {
	beforeEach(() => {
		useProviderSetupMock.mockReset();
	});

	it("starts a setup session when opened", () => {
		const start = vi.fn();
		useProviderSetupMock.mockReturnValue(baseState({ start }));
		render(<ProviderSetupDialog profileId="prof-1" displayName="Claude Code" open onOpenChange={vi.fn()} />);
		expect(start).toHaveBeenCalled();
	});

	it("shows a spinner before a handle exists, then attaches the terminal to the returned handle", () => {
		useProviderSetupMock.mockReturnValue(baseState({ phase: "starting" }));
		const { rerender } = render(<ProviderSetupDialog profileId="prof-1" displayName="Claude Code" open onOpenChange={vi.fn()} />);
		expect(screen.queryByTestId("terminal-pane")).not.toBeInTheDocument();

		useProviderSetupMock.mockReturnValue(baseState({ phase: "waiting", handleId: "h-1", instructions: "Run /login." }));
		rerender(<ProviderSetupDialog profileId="prof-1" displayName="Claude Code" open onOpenChange={vi.fn()} />);
		expect(screen.getByTestId("terminal-pane")).toHaveTextContent("attached:h-1");
		expect(screen.getByText("Run /login.")).toBeInTheDocument();
	});

	it("shows a retry action once the wait times out", () => {
		useProviderSetupMock.mockReturnValue(baseState({ phase: "timed_out" }));
		render(<ProviderSetupDialog profileId="prof-1" displayName="Claude Code" open onOpenChange={vi.fn()} />);
		expect(screen.getByText("Try again")).toBeInTheDocument();
	});

	it("Cancel stops the session and closes the dialog", () => {
		const stop = vi.fn();
		const onOpenChange = vi.fn();
		useProviderSetupMock.mockReturnValue(baseState({ phase: "waiting", handleId: "h-1", stop }));
		render(<ProviderSetupDialog profileId="prof-1" displayName="Claude Code" open onOpenChange={onOpenChange} />);
		fireEvent.click(screen.getByText("Cancel"));
		expect(stop).toHaveBeenCalled();
		expect(onOpenChange).toHaveBeenCalledWith(false);
	});
});
