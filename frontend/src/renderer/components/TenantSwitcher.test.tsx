import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it } from "vitest";

import { TooltipProvider } from "./ui/tooltip";
import { TenantSwitcher } from "./TenantSwitcher";
import { useTenantStore } from "../stores/tenant-store";

const acme = { id: "tnt-a", name: "Acme", slug: "acme", description: "", status: "active", role: "member" };
const umbrella = { id: "tnt-b", name: "Umbrella", slug: "umbrella", description: "", status: "active", role: "admin" };

function wrapper({ children }: { children: ReactNode }) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	// The shell mounts one TooltipProvider around everything; the switcher
	// lives inside it.
	return (
		<QueryClientProvider client={client}>
			<TooltipProvider>{children}</TooltipProvider>
		</QueryClientProvider>
	);
}

function seed(tenants: unknown[], current: string | null) {
	useTenantStore.setState({
		tenants: tenants as never,
		currentTenantId: current,
		status: "loaded",
		error: null,
	});
}

describe("TenantSwitcher", () => {
	beforeEach(() => {
		localStorage.clear();
		useTenantStore.getState().reset();
	});

	// The single-organization installation is the one that must not change, so
	// it is the first thing asserted: no control, no label, nothing new to
	// learn.
	it("renders nothing on a single-organization installation", () => {
		seed([acme], "tnt-a");
		const { container } = render(<TenantSwitcher tabIndex={0} />, { wrapper });
		expect(container).toBeEmptyDOMElement();
	});

	it("renders nothing before the organization list has loaded", () => {
		const { container } = render(<TenantSwitcher tabIndex={0} />, { wrapper });
		expect(container).toBeEmptyDOMElement();
	});

	it("names the current organization and switches to another", async () => {
		seed([acme, umbrella], "tnt-a");
		const user = userEvent.setup();
		render(<TenantSwitcher tabIndex={0} />, { wrapper });

		const trigger = screen.getByRole("button", { name: /switch organization/i });
		expect(trigger).toHaveTextContent("Acme");

		await user.click(trigger);
		await user.click(await screen.findByRole("menuitem", { name: /Umbrella/ }));

		expect(useTenantStore.getState().currentTenantId).toBe("tnt-b");
		expect(screen.getByRole("button", { name: /switch organization/i })).toHaveTextContent("Umbrella");
	});
})
