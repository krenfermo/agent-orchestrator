import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiClient } from "../../lib/api-client";
import { appI18n } from "../../i18n";
import { ProjectAccessSettingsSection } from "./ProjectAccessSettingsSection";

function renderSection() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<ProjectAccessSettingsSection projectId="proj-one" />
		</QueryClientProvider>,
	);
}

function mockAccess(permissions: string[]) {
	return vi.spyOn(apiClient, "GET").mockImplementation((async (path: string) => {
		if (path === "/api/v1/projects/{id}/access") {
			return {
				data: {
					projectId: "proj-one",
					ownerUserId: "owner",
					permissions,
					grants: [
						{
							subjectKind: "team", subjectId: "t1", role: "member",
							createdAt: new Date().toISOString(), updatedAt: new Date().toISOString(),
						},
					],
				},
			} as never;
		}
		if (path === "/api/v1/users") {
			return {
				data: {
					users: [{
						id: "owner", displayName: "Ada Lovelace", email: "a@example.test", username: "ada",
						status: "active", role: "owner", federated: false,
						createdAt: new Date().toISOString(), updatedAt: new Date().toISOString(),
					}],
				},
			} as never;
		}
		return {
			data: {
				teams: [{
					id: "t1", name: "Platform", slug: "platform", description: "", status: "active",
					createdAt: new Date().toISOString(), updatedAt: new Date().toISOString(),
				}],
			},
		} as never;
	}) as never);
}

describe("ProjectAccessSettingsSection", () => {
	afterEach(async () => {
		vi.restoreAllMocks();
		await appI18n.changeLanguage("en");
	});

	// The controls render from the caller's own effective permissions ON THIS
	// PROJECT, which the daemon computes and returns with the access list. A
	// project administrator therefore manages their project without holding any
	// installation-wide authority.
	it("offers grant and revoke when the response reports project.access.manage", async () => {
		mockAccess(["project.read", "project.access.read", "project.access.manage"]);
		renderSection();

		expect(await screen.findByText("Platform")).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "Grant access" })).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "Revoke" })).toBeInTheDocument();
	});

	it("renders read-only when the response reports only project.access.read", async () => {
		mockAccess(["project.read", "project.access.read"]);
		renderSection();

		expect(await screen.findByText("Platform")).toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "Grant access" })).not.toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "Revoke" })).not.toBeInTheDocument();
		expect(
			screen.getByText("You can see this project's access list but not change it."),
		).toBeInTheDocument();
	});

	// The project's owner row is an implicit administrator grant that predates
	// P4-B and cannot be revoked from here. Omitting it would make the list
	// silently incomplete.
	it("shows the project owner as an unrevokable entry", async () => {
		mockAccess(["project.read", "project.access.read", "project.access.manage"]);
		renderSection();

		// The name also appears inside the "grant access" subject picker, so the
		// assertion is that the owner is listed at all, not that it is unique.
		expect((await screen.findAllByText("Ada Lovelace")).length).toBeGreaterThan(0);
		expect(screen.getByText("Project owner")).toBeInTheDocument();
		expect(screen.getAllByRole("button", { name: "Revoke" })).toHaveLength(1);
	});
});
