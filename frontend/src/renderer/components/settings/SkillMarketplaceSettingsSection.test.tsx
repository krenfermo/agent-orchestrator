import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiClient } from "../../lib/api-client";
import { SkillMarketplaceSettingsSection } from "./SkillMarketplaceSettingsSection";

const INSTALL_NOTICE = "This installs the Skill but does not enable it on any project.";
const INSTALLED_NOTICE = "Installed. Choose a project to enable it.";
const FRESHNESS_NOTICE =
	"A registry could not be reached, so some of what is listed is metadata AO cached earlier.";
const CACHED_AT = "2026-06-01T12:00:00Z";

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
	// reach gets the banner, in those words -- and it comes from the daemon's
	// `offline` field, not from the wording of an error string.
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
							metadataFreshness: "offline",
							metadataOffline: true,
						},
					],
					sources: [
						{
							registryId: "company-private",
							registryName: "Company Private",
							freshness: "offline",
							offline: true,
							fetchedAt: CACHED_AT,
							explanation: "AO could not reach this registry.",
						},
					],
					offline: true,
					freshnessNotice: FRESHNESS_NOTICE,
					installNotice: INSTALL_NOTICE,
				},
			};
		}) as never);
		renderSection();

		const banner = await screen.findByTestId("skill-marketplace-offline");
		expect(banner).toHaveTextContent("OFFLINE / STALE METADATA");
		expect(banner).toHaveTextContent(
			"What is listed for it is what AO last received, not what it offers now.",
		);
		// The daemon's own sentence, including the half that says the install
		// path is unaffected.
		expect(banner).toHaveTextContent(FRESHNESS_NOTICE);
		// The registry is still named, with its reason.
		expect(await screen.findByTestId("skill-registry-note")).toHaveTextContent(
			"could not be read: registry company-private could not be read: connection refused",
		);
	});

	// This is Check 19 on screen: a search whose rows came from cache must not
	// look like a search whose rows came from the registry.
	it("labels a cached row and never presents it as current", async () => {
		mockSearch([{ ...release, metadataFreshness: "offline", metadataOffline: true, metadataAsOf: CACHED_AT }], {
			sources: [
				{
					registryId: "ao-fixture",
					registryName: "AO Fixture",
					freshness: "offline",
					offline: true,
					fetchedAt: CACHED_AT,
					explanation: "AO could not reach this registry.",
				},
			],
			offline: true,
			freshnessNotice: FRESHNESS_NOTICE,
		});
		renderSection();

		expect(await screen.findByTestId("skill-release-freshness")).toHaveTextContent(
			"Cached, registry unreachable",
		);
		expect(await screen.findByTestId("skill-release-as-of")).toHaveTextContent(
			"This is not what the registry says now.",
		);
		// The row is still there. Offline labels; it does not hide.
		expect(screen.getByText("Security Audit")).toBeInTheDocument();
		// And freshness did not move trust: a cached row is not less trusted.
		expect(screen.getByText("Nothing checked yet")).toBeInTheDocument();
	});

	// A copy AO could not confirm AND that the cache no longer considers
	// current is the stronger of the two offline states, and reads as such.
	it("distinguishes a stale cached row from a merely unconfirmed one", async () => {
		mockSearch([{ ...release, metadataFreshness: "stale", metadataOffline: true, metadataAsOf: CACHED_AT }], {
			sources: [
				{
					registryId: "ao-fixture",
					freshness: "stale",
					offline: true,
					fetchedAt: CACHED_AT,
					explanation: "AO could not reach this registry.",
				},
			],
			offline: true,
			freshnessNotice: FRESHNESS_NOTICE,
		});
		renderSection();

		expect(await screen.findByTestId("skill-release-freshness")).toHaveTextContent(
			"Cached, out of date",
		);
	});

	// A line printed on every search is a line people stop reading, and then
	// stop seeing when it changes. A live search says nothing about freshness.
	it("shows no freshness banner or badge when everything came from the registry", async () => {
		mockSearch([{ ...release, metadataFreshness: "live", metadataOffline: false }], {
			sources: [
				{
					registryId: "ao-fixture",
					freshness: "live",
					offline: false,
					explanation: "The registry answered during this request.",
				},
			],
			offline: false,
		});
		renderSection();

		await screen.findByText("Security Audit");
		expect(screen.queryByTestId("skill-marketplace-offline")).not.toBeInTheDocument();
		expect(screen.queryByTestId("skill-release-freshness")).not.toBeInTheDocument();
		expect(screen.queryByTestId("skill-release-as-of")).not.toBeInTheDocument();
	});

	// An unreadable registry is not an unreachable one, and the two want
	// different fixes. Only the daemon's own field decides which banner shows.
	it("does not call an unreadable registry offline", async () => {
		mockSearch([], {
			notes: [
				{
					registryId: "company-private",
					reason: "registry.json is missing",
					metadataFreshness: "live",
					metadataOffline: false,
				},
			],
			sources: [
				{ registryId: "company-private", freshness: "live", offline: false, explanation: "" },
			],
			offline: false,
		});
		renderSection();

		const note = await screen.findByTestId("skill-registry-note");
		expect(note).toHaveTextContent("registry.json is missing");
		expect(note).not.toHaveTextContent("OFFLINE / STALE METADATA");
		expect(screen.queryByTestId("skill-marketplace-offline")).not.toBeInTheDocument();
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

// ---------------------------------------------------------------- external

const EXTERNAL_NOTICE =
	"Source: GitHub. Hosting on GitHub does not mean AO trusts the publisher.";
const OLD_COMMIT = "a".repeat(40);
const NEW_COMMIT = "b".repeat(40);

const externalRelease = {
	...release,
	registryId: "ext",
	registryName: "Acme skills",
	sourceProvider: "github",
	sourceOwner: "acme",
	sourceRepository: "skills",
	sourceTag: "v0.1.0",
	sourceCommit: NEW_COMMIT,
	sourceShortCommit: NEW_COMMIT.slice(0, 12),
	sourceVisibility: "public",
};

describe("SkillMarketplaceSettingsSection, external sources", () => {
	// The three facts a person needs before deciding: where it is, what tag it
	// was found under, and which commit is actually going to be installed.
	it("names the repository, the tag and the commit in full", async () => {
		mockSearch([externalRelease], { externalNotice: EXTERNAL_NOTICE });
		renderSection();

		const source = await screen.findByTestId("skill-release-source-security-audit");
		expect(source).toHaveTextContent("acme/skills");
		expect(source).toHaveTextContent("public");
		expect(source).toHaveTextContent("v0.1.0");
		// Short for recognition AND full for comparison: deciding two commits
		// are the same one needs every character.
		expect(source).toHaveTextContent(NEW_COMMIT.slice(0, 12));
		expect(source).toHaveTextContent(NEW_COMMIT);
	});

	// The sentence that must never be left implicit, and it is the daemon's.
	it("shows the hosting notice before an install from a forge", async () => {
		mockSearch([externalRelease], { externalNotice: EXTERNAL_NOTICE });
		renderSection();
		expect(await screen.findByTestId("skill-external-notice")).toHaveTextContent(
			EXTERNAL_NOTICE,
		);
	});

	// A local or private-registry release has no forge behind it, so the
	// notice would be a line printed on rows it does not describe.
	it("does not show the hosting notice for a release with no forge", async () => {
		mockSearch([release], { externalNotice: EXTERNAL_NOTICE });
		renderSection();
		await screen.findByTestId("skill-install-notice");
		expect(screen.queryByTestId("skill-external-notice")).toBeNull();
	});

	// The heart of phase 13 on screen: a tag that moved is stated with both
	// commits, the install is blocked, and taking the new commit is an
	// explicit decision this screen asks for in words.
	it("blocks a moved tag until somebody says they mean to take the new commit", async () => {
		const moved = {
			...externalRelease,
			tagMoved: true,
			tagMovedFromCommit: OLD_COMMIT,
			tagMovedExplanation: `tag v0.1.0 in acme/skills moved: AO recorded it at commit ${OLD_COMMIT} and it now points at ${NEW_COMMIT}.`,
		};
		mockSearch([moved], { externalNotice: EXTERNAL_NOTICE });
		const post = vi.spyOn(apiClient, "POST").mockResolvedValue({
			data: { nextStep: INSTALLED_NOTICE },
		} as never);
		renderSection();

		const warning = await screen.findByTestId("skill-release-tag-moved");
		expect(warning).toHaveTextContent(OLD_COMMIT);
		expect(warning).toHaveTextContent(NEW_COMMIT);

		const install = screen.getByRole("button", { name: /Install/ });
		expect(install).toBeDisabled();

		const acknowledge = screen
			.getByTestId("skill-release-acknowledge-moved")
			.querySelector("input") as HTMLInputElement;
		await userEvent.click(acknowledge);
		expect(install).toBeEnabled();
		await userEvent.click(install);

		await waitFor(() => expect(post).toHaveBeenCalled());
		const [, options] = post.mock.calls[0] as [string, { body: Record<string, unknown> }];
		// The acknowledgement travels; nothing else about the request changes,
		// because acknowledging the move skips no check.
		expect(options.body.acknowledgeMovedTag).toBe(true);
		expect(options.body.version).toBe("0.1.0");
		expect(options.body.allowOfflineFromCache).toBe(false);
	});

	// A revocation made HERE is not the registry's word and not a trust state.
	// It blocks the install and says why, in the daemon's sentence.
	it("blocks and explains a release whose source this installation withdrew", async () => {
		mockSearch(
			[
				{
					...externalRelease,
					externalRevoked: true,
					externalRevokedReason:
						"the repository acme/skills was withdrawn on this installation: under investigation",
				},
			],
			{ externalNotice: EXTERNAL_NOTICE },
		);
		renderSection();

		expect(await screen.findByTestId("skill-release-external-revoked")).toHaveTextContent(
			"under investigation",
		);
		expect(screen.getByRole("button", { name: /Install/ })).toBeDisabled();
	});
});
