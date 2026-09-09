import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiClient } from "../../lib/api-client";
import { SkillImagesSettingsSection } from "./SkillImagesSettingsSection";

const active = {
	id: "skimg-1", tenantId: "default", projectId: "medusa", skillId: "security-audit",
	version: "1.2.0", modeId: "static-code", tool: "ao.static-scan/v1", reference: "alpine",
	digest: "sha256:aaaa", approvedBy: "ada", approvedAt: "2026-09-09T10:00:00Z",
	note: "diffed against upstream", active: true,
};
const revoked = {
	...active, id: "skimg-2", version: "1.1.0", digest: "sha256:bbbb",
	revokedAt: "2026-09-05T10:00:00Z", active: false, inactiveReason: "revoked",
};

function renderSection() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<SkillImagesSettingsSection />
		</QueryClientProvider>,
	);
}

function mockList(approvals: unknown[], extra: Record<string, unknown> = {}) {
	return vi.spyOn(apiClient, "GET").mockResolvedValue({
		data: { approvals, ...extra },
	} as never);
}

afterEach(() => vi.restoreAllMocks());

describe("SkillImagesSettingsSection", () => {
	// An empty trust root says what it MEANS. "No approvals" alone reads like a
	// setup step nobody got to.
	it("says nothing may execute when no image is approved", async () => {
		mockList([]);
		renderSection();
		expect(
			await screen.findByText("No image is approved, so no skill may execute."),
		).toBeInTheDocument();
	});

	// Revoked entries stay. They are the history of what this installation once
	// allowed; hiding them would answer "what did we approve" with "what is
	// approved right now".
	it("shows revoked approvals alongside active ones, with who decided", async () => {
		mockList([active, revoked], {
			trustModel: "An approval is not a publisher signature; AO verifies none.",
			revocationPolicy: "Revoking stops new executions immediately.",
		});
		renderSection();

		const list = await screen.findByTestId("skill-image-approvals");
		expect(list).toHaveTextContent("skimg-1");
		expect(list).toHaveTextContent("Active");
		expect(list).toHaveTextContent("skimg-2");
		expect(list).toHaveTextContent("Inactive (revoked)");
		expect(list).toHaveTextContent("sha256:aaaa");
		expect(list).toHaveTextContent("default/medusa · security-audit@1.2.0");
		// An approval nobody signed is one nobody can be asked about.
		expect(list).toHaveTextContent("Approved by ada");
		// The non-promises come from the daemon, so the screen cannot describe
		// the trust model more optimistically than the thing enforcing it.
		expect(screen.getByText(/not a publisher signature/)).toBeInTheDocument();
		expect(screen.getByText(/Revoking stops new executions/)).toBeInTheDocument();
	});

	// A revoked approval offers no Revoke button: the action would do nothing.
	it("offers Revoke only for an active approval", async () => {
		mockList([active, revoked]);
		renderSection();
		await screen.findByTestId("skill-image-approvals");
		expect(screen.getAllByRole("button", { name: "Revoke" })).toHaveLength(1);
	});

	it("revokes through the daemon and refreshes the list", async () => {
		mockList([active]);
		const del = vi.spyOn(apiClient, "DELETE").mockResolvedValue({ data: {} } as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Revoke" }));
		await waitFor(() =>
			expect(del).toHaveBeenCalledWith(
				"/api/v1/skills/images/{approvalId}",
				expect.objectContaining({ params: { path: { approvalId: "skimg-1" } } }),
			),
		);
	});

	// Every scope field is required: an approval missing one authorizes
	// somebody, somewhere, to run something.
	it("will not approve until the whole scope is filled in", async () => {
		mockList([]);
		renderSection();
		const approve = await screen.findByRole("button", { name: "Approve" });
		expect(approve).toBeDisabled();

		await userEvent.type(screen.getByLabelText("Digest"), "sha256:cccc");
		// One field is not a scope.
		expect(approve).toBeDisabled();
	});

	it("sends the confirmation with the approval, and surfaces a refusal", async () => {
		mockList([]);
		const post = vi.spyOn(apiClient, "POST").mockResolvedValue({
			error: { message: "sha256:cccc is not on this host, and AO does not pull to find out" },
		} as never);
		renderSection();
		await screen.findByTestId("skill-image-approve-form");

		for (const [label, value] of [
			["Tenant", "default"], ["Project", "medusa"], ["Skill", "security-audit"],
			["Version", "1.2.0"], ["Mode", "static-code"], ["Tool", "ao.static-scan/v1"],
			["Reference", "alpine"], ["Digest", "sha256:cccc"], ["What you checked", "looked at it"],
		] as const) {
			await userEvent.type(screen.getByLabelText(label), value);
		}
		await userEvent.click(screen.getByRole("button", { name: "Approve" }));

		await waitFor(() =>
			expect(post).toHaveBeenCalledWith(
				"/api/v1/skills/images",
				// confirm is set by the act of submitting, not by a checkbox
				// with a default the daemon would then have to trust.
				expect.objectContaining({ body: expect.objectContaining({ confirm: true }) }),
			),
		);
		// The daemon's refusal is shown verbatim rather than softened.
		expect(
			await screen.findByText(/is not on this host, and AO does not pull to find out/),
		).toBeInTheDocument();
	});
});
