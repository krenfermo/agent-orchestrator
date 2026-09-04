import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiClient } from "../../lib/api-client";
import { appI18n } from "../../i18n";
import { useAuthStore, type Permission } from "../../stores/auth-store";
import { AccessSettingsSection } from "./AccessSettingsSection";

function renderSection(permissions: Permission[]) {
	useAuthStore.setState({
		permissions,
		user: { id: "owner", displayName: "Ada Lovelace", email: "owner@example.test", username: "owner", status: "active", role: "owner" },
		status: "authenticated",
	});
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<AccessSettingsSection />
		</QueryClientProvider>,
	);
}

const users = {
	users: [
		{
			id: "owner", displayName: "Ada Lovelace", email: "owner@example.test", username: "owner",
			status: "active", role: "owner", federated: false,
			createdAt: new Date().toISOString(), updatedAt: new Date().toISOString(),
		},
		{
			id: "member", displayName: "Grace Hopper", email: "member@example.test", username: "member",
			status: "active", role: "member", federated: false,
			createdAt: new Date().toISOString(), updatedAt: new Date().toISOString(),
		},
	],
};

const teams = { teams: [{ id: "t1", name: "Platform", slug: "platform", description: "", status: "active", createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() }] };

function mockGet() {
	return vi.spyOn(apiClient, "GET").mockImplementation((async (path: string) => {
		if (path === "/api/v1/users") return { data: users } as never;
		if (path === "/api/v1/teams") return { data: teams } as never;
		return { data: { members: [] } } as never;
	}) as never);
}

describe("AccessSettingsSection", () => {
	afterEach(async () => {
		vi.restoreAllMocks();
		useAuthStore.setState({ permissions: [] });
		await appI18n.changeLanguage("en");
	});

	it("lists accounts and teams when the backend reports the read capabilities", async () => {
		mockGet();
		renderSection(["users.read", "teams.read"]);

		expect(await screen.findByText("Grace Hopper")).toBeInTheDocument();
		expect(await screen.findByText("Platform")).toBeInTheDocument();
	});

	// The capability list is the backend's own answer. A person who can read
	// accounts but not change them sees the roster and none of the controls --
	// and the routes behind those controls refuse them regardless, which is what
	// the daemon's own tests assert.
	it("renders no management controls without the manage capability", async () => {
		mockGet();
		renderSection(["users.read", "teams.read"]);

		expect(await screen.findByText("Grace Hopper")).toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "Add user" })).not.toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "Disable" })).not.toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
	});

	it("offers management controls once the backend reports them", async () => {
		mockGet();
		renderSection(["users.read", "users.manage", "teams.read", "teams.manage"]);

		expect(await screen.findByRole("button", { name: "Add user" })).toBeInTheDocument();
		await waitFor(() => expect(screen.getByRole("button", { name: "Delete" })).toBeInTheDocument());
	});

	// Ownership moves by transfer, and the daemon refuses anything else. The UI
	// must not offer a control whose only outcome is a refusal.
	it("never offers to disable or re-role the owner", async () => {
		mockGet();
		renderSection(["users.read", "users.manage", "teams.read"]);

		expect(await screen.findByText("Ada Lovelace")).toBeInTheDocument();
		// One role picker, for the member -- not for the owner.
		await waitFor(() => expect(screen.getAllByLabelText("Role")).toHaveLength(1));
		// One disable button, for the member -- the owner and the caller
		// themselves are both excluded (the caller here IS the owner).
		expect(screen.getAllByRole("button", { name: "Disable" })).toHaveLength(1);
	});

	it("renders nothing at all without the read capabilities", () => {
		const get = mockGet();
		const { container } = renderSection([]);
		expect(container).toBeEmptyDOMElement();
		expect(get).not.toHaveBeenCalled();
	});
});
