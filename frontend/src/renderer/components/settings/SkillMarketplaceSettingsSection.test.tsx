import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiClient } from "../../lib/api-client";
import { SkillMarketplaceSettingsSection } from "./SkillMarketplaceSettingsSection";

const INSTALL_NOTICE = "This installs the Skill but does not enable it on any project.";
const INSTALLED_NOTICE = "Installed. Choose a project to enable it.";

const release = {
	registryId: "ao-fixture",
	registryName: "AO Fixture",
	skillId: "security-audit",
	name: "Security Audit",
	version: "0.1.0",
	publisher: "agent-orchestrator",
	description: "On-demand security review of one selected project.",
	riskLevel: "critical",
	manifestDigest: "aa".repeat(32),
	artifactDigest: "bb".repeat(32),
	requestedCapabilities: ["repo.read", "report.write"],
	executionModes: [
		{
			id: "static-code",
			name: "Static code review",
			description: "Read-only.",
			riskLevel: "medium",
			capabilities: ["repo.read"],
		},
	],
	aoMinVersion: "0.11.0",
	compatibility: "compatible",
	publishedAt: "2026-01-01T00:00:00Z",
	trust: "unverified",
	trustExplanation:
		"Nothing has been checked. AO has not fetched these bytes, so it has verified nothing about them.",
	installed: false,
	updateAvailable: false,
};

function renderSection() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<SkillMarketplaceSettingsSection />
		</QueryClientProvider>,
	);
}

// The panel makes two reads: the search, and the registry list the source
// filter is built from.
function mockSearch(releases: unknown[], extra: Record<string, unknown> = {}) {
	return vi.spyOn(apiClient, "GET").mockImplementation((async (path: string) => {
		if (path === "/api/v1/skills/registries") {
			return {
				data: {
					registries: [
						{
							id: "ao-fixture",
							displayName: "AO Fixture",
							type: "local",
							location: "/srv/ao/registry",
							enabled: true,
							trustPolicy: "digest",
							trustPolicyEnforceable: true,
							priority: 10,
							authType: "none",
							networkPolicySummary: "public addresses only",
							status: { lastProbeState: "" },
						},
					],
					trustModel: "",
				},
			};
		}
		return { data: { releases, notes: [], installNotice: INSTALL_NOTICE, ...extra } };
	}) as never);
}

afterEach(() => vi.restoreAllMocks());

describe("SkillMarketplaceSettingsSection", () => {
	it("shows a release with its publisher, capabilities and installed state", async () => {
		mockSearch([release]);
		renderSection();

		// The list element is always mounted, so wait on the row's own text
		// rather than on the container.
		await screen.findByText("Security Audit");
		const results = screen.getByTestId("skill-marketplace-results");
		expect(results).toHaveTextContent("security-audit@0.1.0");
		expect(results).toHaveTextContent("Published by agent-orchestrator in AO Fixture");
		expect(results).toHaveTextContent("Asks for: repo.read, report.write");
		expect(results).toHaveTextContent("Not installed");
	});

	// A search fetched nothing, so it must not look like it verified anything.
	// The word "trusted" must never appear: AO verifies no publisher signature.
	it("reports a search hit as unverified and never as trusted", async () => {
		mockSearch([release]);
		renderSection();

		await screen.findByText("Nothing checked yet");
		const results = screen.getByTestId("skill-marketplace-results");
		expect(results).not.toHaveTextContent(/trusted/i);
		// The explanation is the daemon's, not one this screen wrote.
		expect(results).toHaveTextContent("AO has not fetched these bytes");
	});

	// The notice must be on screen BEFORE the install, every time.
	it("says installing does not enable anything, before the install", async () => {
		mockSearch([release]);
		renderSection();

		expect(await screen.findByTestId("skill-install-notice")).toHaveTextContent(INSTALL_NOTICE);
	});

	it("installs one exact version and then says to choose a project", async () => {
		mockSearch([release]);
		const post = vi.spyOn(apiClient, "POST").mockResolvedValue({
			data: {
				install: { id: "security-audit", version: "0.1.0" },
				origin: { trust: "verified" },
				updated: false,
				nextStep: INSTALLED_NOTICE,
			},
		} as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Install 0.1.0" }));

		await waitFor(() => expect(post).toHaveBeenCalled());
		const [, options] = post.mock.calls[0] as [string, { body: Record<string, unknown> }];
		// The exact version, never "latest".
		expect(options.body).toMatchObject({
			registryId: "ao-fixture",
			skillId: "security-audit",
			version: "0.1.0",
		});
		expect(await screen.findByTestId("skill-marketplace-installed")).toHaveTextContent(
			INSTALLED_NOTICE,
		);
	});

	// Hiding a withdrawn release would answer "where did that version go" with
	// silence. It stays, marked, and cannot be installed.
	it("shows a revoked release with its reason and no install button", async () => {
		mockSearch([
			{
				...release,
				revoked: true,
				revocationReason: "signing key compromised",
				trust: "revoked",
				trustExplanation: "The registry withdrew this release.",
			},
		]);
		renderSection();

		await screen.findByText("Withdrawn by the registry: signing key compromised");
		expect(screen.getByRole("button", { name: "Install 0.1.0" })).toBeDisabled();
		expect(screen.queryByTestId("skill-install-notice")).not.toBeInTheDocument();
	});

	// An empty result and an unreadable registry are different answers, and
	// only one of them means somebody should go and look.
	it("names a registry it could not read instead of showing an empty list", async () => {
		mockSearch([], {
			notes: [{ registryId: "company-private", reason: "registry.json is missing" }],
		});
		renderSection();

		expect(await screen.findByTestId("skill-registry-note")).toHaveTextContent(
			"Registry company-private could not be read: registry.json is missing",
		);
	});

	// A declared signature is a CLAIM. Rendering it without saying so would
	// make it read as a check somebody performed.
	it("marks a declared signature as unverified when the details are opened", async () => {
		vi.spyOn(apiClient, "GET").mockImplementation((async (path: string) => {
			if (path === "/api/v1/skills/marketplace") {
				return {
					data: {
						releases: [{ ...release, signatureFormat: "cosign", keyId: "kid-1" }],
						notes: [],
						installNotice: INSTALL_NOTICE,
					},
				};
			}
			return { data: { release, versions: [release], installNotice: INSTALL_NOTICE } };
		}) as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: "Versions and details" }));
		expect(
			await screen.findByText(
				"This release claims a cosign signature. AO verifies no signature, so nothing checked it.",
			),
		).toBeInTheDocument();
	});

	// There is no Run button on this screen, and no route that would give it
	// one. Installing moves a package exactly one step.
	it("offers no way to run anything", async () => {
		mockSearch([{ ...release, installed: true }]);
		renderSection();

		await screen.findByText("Security Audit");
		for (const name of [/^run$/i, /dry run/i, /execute/i, /enable/i]) {
			expect(screen.queryByRole("button", { name })).not.toBeInTheDocument();
		}
	});

	// ------------------------------------------------ phase 11: connectivity

	// Cached data must never be presented as current. A registry AO could not
	// reach gets the banner, in those words.
	it("says OFFLINE / STALE METADATA for a registry it could not reach", async () => {
		vi.spyOn(apiClient, "GET").mockImplementation((async (path: string) => {
			if (path === "/api/v1/skills/registries") {
				return { data: { registries: [], trustModel: "" } };
			}
			return {
				data: {
					releases: [release],
					notes: [
						{
							registryId: "company-private",
							reason: "registry company-private could not be read: connection refused",
						},
					],
					installNotice: INSTALL_NOTICE,
				},
			};
		}) as never);
		renderSection();

		const note = await screen.findByTestId("skill-registry-note");
		expect(note).toHaveTextContent("OFFLINE / STALE METADATA");
		expect(note).toHaveTextContent(
			"What is listed for it is what AO last received, not what it offers now.",
		);
	});

	// With more than one registry configured, "where did this come from" is the
	// first question a reader has.
	it("names the source registry on every result", async () => {
		mockSearch([release]);
		renderSection();

		expect(await screen.findByTestId("skill-release-source")).toHaveTextContent(
			"Registry: AO Fixture",
		);
	});

	// A filter narrows what is SHOWN. An unverified release is never hidden by
	// default: hiding it would answer "what else is out there" with a curated
	// lie.
	it("shows unverified releases by default and hides them only when asked", async () => {
		mockSearch([release]);
		renderSection();

		await screen.findByText("Security Audit");
		await userEvent.click(screen.getByLabelText("Verified only"));
		await waitFor(() =>
			expect(screen.queryByText("Security Audit")).not.toBeInTheDocument(),
		);
	});

	// The source filter narrows the QUERY, because the daemon can answer it.
	it("asks the daemon for one registry when a source is chosen", async () => {
		const get = mockSearch([release]);
		renderSection();

		await screen.findByText("Security Audit");
		get.mockClear();
		await userEvent.selectOptions(screen.getByLabelText("Registry"), "ao-fixture");

		await waitFor(() => {
			const calls = get.mock.calls as unknown as [
				string,
				{ params: { query: Record<string, unknown> } },
			][];
			const searchCall = calls.find(([path]) => path === "/api/v1/skills/marketplace");
			expect(searchCall?.[1].params.query).toMatchObject({ registryId: "ao-fixture" });
		});
	});

	// Where the bytes came from is part of what an install reports: "verified"
	// says nothing about whether AO went to the network for them.
	it("says when an install was served from the verified artifact cache", async () => {
		mockSearch([release]);
		vi.spyOn(apiClient, "POST").mockResolvedValue({
			data: { install: {}, origin: {}, updated: false, fromCache: true, nextStep: INSTALLED_NOTICE },
		} as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: /Install 0\.1\.0/ }));

		await waitFor(() =>
			expect(screen.getByTestId("skill-marketplace-installed")).toHaveTextContent(
				"Installed from AO's verified artifact cache",
			),
		);
	});

	// The ordinary install never quietly falls back to cached bytes when a
	// registry cannot be reached. That is a separate, explicit act.
	it("never asks for an offline install on its own", async () => {
		mockSearch([release]);
		const post = vi.spyOn(apiClient, "POST").mockResolvedValue({
			data: { install: {}, origin: {}, updated: false, nextStep: INSTALLED_NOTICE },
		} as never);
		renderSection();

		await userEvent.click(await screen.findByRole("button", { name: /Install 0\.1\.0/ }));

		await waitFor(() => expect(post).toHaveBeenCalled());
		const [, options] = post.mock.calls[0] as [string, { body: Record<string, unknown> }];
		expect(options.body).toMatchObject({ allowOfflineFromCache: false });
	});
});
