import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiClient } from "../../lib/api-client";
import { SkillExternalSettingsSection } from "./SkillExternalSettingsSection";

const movedTag = {
	registryId: "ext",
	owner: "acme",
	repository: "skills",
	tag: "v1.2.3",
	commit: "b".repeat(40),
	previousCommit: "a".repeat(40),
	movedAt: "2026-09-10T10:00:00Z",
	firstSeenAt: "2026-09-01T10:00:00Z",
	explanation:
		"tag v1.2.3 in acme/skills moved: AO recorded it at commit " +
		"a".repeat(40) +
		" and it now points at " +
		"b".repeat(40) +
		". Nothing already installed was changed.",
};

const revocation = {
	subject: "external_repository",
	subjectId: "acme/skills",
	reason: "an advisory somebody here read",
	revokedAt: "2026-09-10T11:00:00Z",
	revokedBy: "admin",
};

const tagPolicy = "A tag is a name somebody can re-point; the commit is the release.";
const revocationPolicy = "Withdrawing blocks NEW installs. It does not uninstall anything.";

function renderSection() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<SkillExternalSettingsSection />
		</QueryClientProvider>,
	);
}

function mockSurface(tags: unknown[], revocations: unknown[]) {
	return vi.spyOn(apiClient, "GET").mockImplementation((async (path: string) => {
		if (path === "/api/v1/skills/external/tags") {
			return { data: { tags, policy: tagPolicy } };
		}
		return { data: { revocations, policy: revocationPolicy } };
	}) as never);
}

afterEach(() => vi.restoreAllMocks());

describe("SkillExternalSettingsSection", () => {
	// "No tag has moved" is a real and good state. Rendering it as an empty
	// list would leave a reader unsure whether AO is watching at all.
	it("says plainly that nothing has moved and nothing is withdrawn", async () => {
		mockSurface([], []);
		renderSection();
		expect(await screen.findByText("No tag AO has recorded has moved.")).toBeInTheDocument();
		expect(screen.getByText("Nothing is withdrawn.")).toBeInTheDocument();
	});

	// The whole point of the ledger: both commits, in full, because deciding
	// two commits are different needs every character.
	it("shows a moved tag with both commits in full", async () => {
		mockSurface([movedTag], []);
		renderSection();

		// Await the row itself: the section exists while the query is still in
		// flight, so grabbing it up front asserts against an empty list.
		await screen.findByText("v1.2.3");
		const rows = screen.getByTestId("skill-external-moved-tags");
		expect(rows).toHaveTextContent("acme/skills");
		expect(rows).toHaveTextContent("v1.2.3");
		expect(rows).toHaveTextContent("Moved");
		expect(rows).toHaveTextContent("a".repeat(40));
		expect(rows).toHaveTextContent("b".repeat(40));
		// And the daemon's sentence about what AO does, not one written here.
		expect(await screen.findByTestId("skill-external-tag-policy")).toHaveTextContent(tagPolicy);
	});

	// The non-promises matter as much as the promise, and they come from the
	// daemon so this screen cannot describe a revocation as more than it is.
	it("shows a withdrawal with the daemon's policy sentence", async () => {
		mockSurface([], [revocation]);
		renderSection();

		// The reason, not the subject label: the form's own select trigger
		// already reads "One repository" before any query resolves, so waiting
		// on that would assert against an empty list.
		await screen.findByText("an advisory somebody here read");
		const list = screen.getByTestId("skill-external-revocations");
		expect(list).toHaveTextContent("One repository");
		expect(list).toHaveTextContent("acme/skills");
		expect(list).toHaveTextContent("an advisory somebody here read");
		expect(list).toHaveTextContent("admin");
		expect(screen.getByTestId("skill-external-revocation-policy")).toHaveTextContent(
			revocationPolicy,
		);
		// A forge publishes no feed, and the screen says so rather than
		// implying AO was told.
		expect(list).toHaveTextContent("A forge publishes no revocation feed");
	});

	// A withdrawal that does not say why is indistinguishable from a mistake,
	// so the form will not submit one.
	it("will not submit a withdrawal with no reason", async () => {
		mockSurface([], []);
		renderSection();
		await screen.findByTestId("skill-external-revoke-form");
		expect(screen.getByRole("button", { name: "Withdraw" })).toBeDisabled();
	});
});
