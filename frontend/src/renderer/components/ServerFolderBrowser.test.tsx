import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { BrowseResult } from "../hooks/useProjectRegistration";
import { ServerFolderBrowser } from "./ServerFolderBrowser";

const { browseMock } = vi.hoisted(() => ({ browseMock: vi.fn() }));

vi.mock("../hooks/useProjectRegistration", () => ({
	useProjectRegistration: () => ({ browse: browseMock }),
}));

describe("ServerFolderBrowser", () => {
	beforeEach(() => {
		browseMock.mockReset();
	});

	it("loads the top level on mount", async () => {
		browseMock.mockResolvedValue({ path: "/srv/repos", entries: [{ name: "medusa", path: "/srv/repos/medusa", isGitRepo: true }] } satisfies BrowseResult);
		render(<ServerFolderBrowser onUseFolder={vi.fn()} />);

		expect(await screen.findByText("medusa")).toBeInTheDocument();
		expect(browseMock).toHaveBeenCalledWith("");
	});

	it("navigates into a clicked subdirectory", async () => {
		browseMock.mockImplementation(async (path: string) => {
			if (path === "") {
				return { path: "/srv/repos", entries: [{ name: "medusa", path: "/srv/repos/medusa", isGitRepo: false }] } satisfies BrowseResult;
			}
			return { path, entries: [{ name: "backend", path: "/srv/repos/medusa/backend", isGitRepo: true }] } satisfies BrowseResult;
		});
		render(<ServerFolderBrowser onUseFolder={vi.fn()} />);

		await userEvent.click(await screen.findByText("medusa"));

		expect(await screen.findByText("backend")).toBeInTheDocument();
		expect(browseMock).toHaveBeenLastCalledWith("/srv/repos/medusa");
	});

	it("shows the current directory and calls onUseFolder with it", async () => {
		browseMock.mockResolvedValue({ path: "/srv/repos", entries: [] } satisfies BrowseResult);
		const onUseFolder = vi.fn();
		render(<ServerFolderBrowser onUseFolder={onUseFolder} />);

		expect(await screen.findByText("/srv/repos")).toBeInTheDocument();
		await userEvent.click(screen.getByRole("button", { name: "Use this folder" }));
		expect(onUseFolder).toHaveBeenCalledWith("/srv/repos");
	});

	it("goes back to the previous level without re-fetching what it doesn't need to", async () => {
		browseMock.mockImplementation(async (path: string) => {
			if (path === "" || path === "/srv/repos") {
				return { path: "/srv/repos", entries: [{ name: "medusa", path: "/srv/repos/medusa", isGitRepo: false }] } satisfies BrowseResult;
			}
			return { path, entries: [{ name: "backend", path: "/srv/repos/medusa/backend", isGitRepo: true }] } satisfies BrowseResult;
		});
		render(<ServerFolderBrowser onUseFolder={vi.fn()} />);

		await userEvent.click(await screen.findByText("medusa"));
		expect(await screen.findByText("backend")).toBeInTheDocument();

		await userEvent.click(screen.getByRole("button", { name: "Back" }));
		expect(await screen.findByText("medusa")).toBeInTheDocument();
		expect(browseMock).toHaveBeenLastCalledWith("/srv/repos");
	});

	it("shows the server error instead of silently failing", async () => {
		browseMock.mockRejectedValue(
			Object.assign(new Error("outside allowed roots"), { code: "PATH_OUTSIDE_ALLOWED_ROOTS" }),
		);
		render(<ServerFolderBrowser onUseFolder={vi.fn()} />);

		expect(await screen.findByText("That path is outside the allowed project roots.")).toBeInTheDocument();
	});
});
