import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { ProjectsSettingsSection } from "./ProjectsSettingsSection";

const { useProjectsListMock, useProjectRegistrationMock } = vi.hoisted(() => ({
	useProjectsListMock: vi.fn(),
	useProjectRegistrationMock: vi.fn(),
}));

vi.mock("../../hooks/useProjectsList", () => ({
	useProjectsList: useProjectsListMock,
}));

vi.mock("../../hooks/useProjectRegistration", () => ({
	useProjectRegistration: useProjectRegistrationMock,
}));

function defaultRegistration(overrides: Partial<ReturnType<typeof useProjectRegistrationMock>> = {}) {
	return {
		browse: vi.fn().mockResolvedValue({ path: "", entries: [] }),
		register: vi.fn().mockResolvedValue({}),
		registering: false,
		registerError: undefined,
		resetRegisterError: vi.fn(),
		clone: vi.fn().mockResolvedValue({}),
		cloning: false,
		cloneError: undefined,
		resetCloneError: vi.fn(),
		...overrides,
	};
}

describe("ProjectsSettingsSection", () => {
	it("lists registered projects with path/repo/branch/kind/validity", () => {
		useProjectsListMock.mockReturnValue({
			projects: [
				{ id: "ao", name: "Agent Orchestrator", path: "/repos/ao", repo: "https://github.com/acme/ao", defaultBranch: "main", kind: "single_repo", valid: true, sessionPrefix: "ao" },
			],
			isLoading: false,
			error: undefined,
		});
		useProjectRegistrationMock.mockReturnValue(defaultRegistration());
		render(<ProjectsSettingsSection />);

		expect(screen.getByText("Agent Orchestrator")).toBeInTheDocument();
		expect(screen.getByText("/repos/ao")).toBeInTheDocument();
		expect(screen.getByText("main")).toBeInTheDocument();
		expect(screen.getByText("Valid")).toBeInTheDocument();
	});

	it("shows a missing-repo badge for a registered but invalid path", () => {
		useProjectsListMock.mockReturnValue({
			projects: [{ id: "gone", name: "Gone", path: "/repos/gone", kind: "single_repo", valid: false, sessionPrefix: "gone" }],
			isLoading: false,
			error: undefined,
		});
		useProjectRegistrationMock.mockReturnValue(defaultRegistration());
		render(<ProjectsSettingsSection />);
		expect(screen.getByText("Missing")).toBeInTheDocument();
	});

	it("registers a repo under an allowed root using a relative path", async () => {
		const register = vi.fn().mockResolvedValue({});
		useProjectsListMock.mockReturnValue({ projects: [], isLoading: false, error: undefined });
		useProjectRegistrationMock.mockReturnValue(defaultRegistration({ register }));
		render(<ProjectsSettingsSection />);

		await userEvent.type(screen.getByPlaceholderText(/allowed project root/i), "sub/repo");
		await userEvent.click(screen.getByRole("button", { name: /^register$/i }));

		expect(register).toHaveBeenCalledWith({ path: "sub/repo" });
	});

	it("shows a clear error message and keeps the technical code out of the headline", async () => {
		useProjectsListMock.mockReturnValue({ projects: [], isLoading: false, error: undefined });
		useProjectRegistrationMock.mockReturnValue(
			defaultRegistration({
				registerError: { message: "That path is outside the allowed project roots.", code: "PATH_OUTSIDE_ALLOWED_ROOTS" },
			}),
		);
		render(<ProjectsSettingsSection />);
		expect(screen.getByText("That path is outside the allowed project roots.")).toBeInTheDocument();
		expect(screen.queryByText("PATH_OUTSIDE_ALLOWED_ROOTS")).not.toBeInTheDocument();
	});

	it("clones a GitHub repo into an allowed root", async () => {
		const clone = vi.fn().mockResolvedValue({});
		useProjectsListMock.mockReturnValue({ projects: [], isLoading: false, error: undefined });
		useProjectRegistrationMock.mockReturnValue(defaultRegistration({ clone }));
		render(<ProjectsSettingsSection />);

		await userEvent.type(screen.getByPlaceholderText(/owner\/repo/i), "octocat/hello-world");
		await userEvent.click(screen.getByRole("button", { name: /^clone$/i }));

		expect(clone).toHaveBeenCalledWith({ repo: "octocat/hello-world", destinationName: undefined });
	});
});
