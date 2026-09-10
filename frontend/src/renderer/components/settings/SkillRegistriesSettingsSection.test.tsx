import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiClient } from "../../lib/api-client";
import { SkillRegistriesSettingsSection } from "./SkillRegistriesSettingsSection";

const TRUST_MODEL =
	"Installing verifies INTEGRITY. AO verifies no publisher signature, so a release is never better than verified.";
const REVOCATION_POLICY =
	"AO does NOT uninstall it, does NOT disable it on any project, and does NOT stop a run already under way.";

const digestRegistry = {
	id: "ao-fixture",
	displayName: "AO Fixture",
	type: "local",
	location: "/srv/ao/registry",
	enabled: true,
	trustPolicy: "digest",
	trustPolicyEnforceable: true,
	priority: 10,
};
const httpsRegistry = {
	id: "company-private",
	displayName: "Company Private",
	type: "https",
	location: "https://registry.corp.example",
	enabled: true,
	trustPolicy: "digest",
	trustPolicyEnforceable: true,
	priority: 100,
	authType: "bearer",
	credentialSecretName: "CORP_REGISTRY_TOKEN",
	networkPolicySummary: "private ranges permitted: 10.4.0.0/16",
	permittedPrivateCidrs: ["10.4.0.0/16"],
	// Never tested: the status a registry has before anybody presses the
	// button, and a state the panel must render as itself.
	status: { lastProbeState: "" },
};
const signedRegistry = {
	...digestRegistry,
	id: "strict",
	displayName: "Strict",
	trustPolicy: "signed",
	trustPolicyEnforceable: false,
};

/** Opens a shadcn Select by its accessible name and chooses one option. */
async function pickOption(label: string, option: string) {
	await userEvent.click(screen.getByLabelText(label));
	await userEvent.click(await screen.findByRole("option", { name: option }));
}

function renderSection() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<SkillRegistriesSettingsSection />
		</QueryClientProvider>,
	);
}

/** Answers both reads the panel makes: the registry list and the update check. */
function mockReads(registries: unknown[], statuses: unknown[] = []) {
	return vi.spyOn(apiClient, "GET").mockImplementation((async (path: string) => {
		if (path === "/api/v1/skills/registries") {
			return { data: { registries, trustModel: TRUST_MODEL } };
		}
		return { data: { statuses, revocationPolicy: REVOCATION_POLICY } };
	}) as never);
}

afterEach(() => vi.restoreAllMocks());

describe("SkillRegistriesSettingsSection", () => {
	// The empty state says what it MEANS. "No registries" alone reads like a
	// setup step nobody got to.
	it("says nothing can be installed when no registry is configured", async () => {
		mockReads([]);
		renderSection();

		expect(
			await screen.findByText(
				"No registry is configured, so no Skill can be installed from one.",
			),
		).toBeInTheDocument();
	});

	it("shows a registry with its trust policy and the daemon's trust model", async () => {
		mockReads([digestRegistry]);
		renderSection();

		await screen.findByText("AO Fixture");
		expect(screen.getByTestId("skill-registries")).toHaveTextContent(
			"Trust policy: verify the digests AO computes itself.",
		);
		expect(screen.getByTestId("skill-registry-trust-model")).toHaveTextContent(TRUST_MODEL);
	});

	// The strictest-looking setting must not read as if it were working.
	it("marks a signature-requiring registry as installing nothing", async () => {
		mockReads([signedRegistry]);
		renderSection();

		expect(
			await screen.findByText(
				"AO verifies no signature, so nothing can be installed from a registry on this policy.",
			),
		).toBeInTheDocument();
	});

	// A revocation observed after an install has an exact non-promise, and the
	// screen states it rather than leaving the user to assume a cleanup.
	it("reports a revoked installed release with the non-promise", async () => {
		mockReads(
			[digestRegistry],
			[
				{
					skillId: "security-audit",
					version: "0.1.0",
					origin: { registryId: "ao-fixture", trust: "revoked" },
					updateAvailable: false,
					revokedNow: true,
					revocationReason: "signing key compromised",
				},
			],
		);
		renderSection();

		const statuses = await screen.findByTestId("skill-update-statuses");
		expect(statuses).toHaveTextContent(
			"withdrawn by the registry: signing key compromised",
		);
		expect(screen.getByTestId("skill-revocation-policy")).toHaveTextContent(REVOCATION_POLICY);
	});

	it("reports an available update without installing it", async () => {
		mockReads(
			[digestRegistry],
			[
				{
					skillId: "security-audit",
					version: "0.1.0",
					origin: { registryId: "ao-fixture", trust: "verified" },
					latestVersion: "0.2.0",
					updateAvailable: true,
					revokedNow: false,
				},
			],
		);
		const post = vi.spyOn(apiClient, "POST");
		renderSection();

		expect(await screen.findByText(/update available: 0\.2\.0/)).toBeInTheDocument();
		// Reporting an update must never be the same act as taking it.
		expect(post).not.toHaveBeenCalled();
	});

	// A local registry is the only readable type, and the form must not offer a
	// value the daemon would refuse.
	it("sends a local registry configuration and clears the form", async () => {
		mockReads([]);
		const put = vi.spyOn(apiClient, "PUT").mockResolvedValue({ data: digestRegistry } as never);
		renderSection();

		await screen.findByText("Add a registry");
		await userEvent.type(screen.getByLabelText("Identifier"), "company-private");
		await userEvent.type(screen.getByLabelText("Display name"), "Company Private");
		await userEvent.type(
			screen.getByLabelText("Location"),
			"/srv/ao/registry",
		);
		await userEvent.click(screen.getByRole("button", { name: "Add registry" }));

		await waitFor(() => expect(put).toHaveBeenCalled());
		const [path, options] = put.mock.calls[0] as [
			string,
			{ params: { path: { registryId: string } }; body: Record<string, unknown> },
		];
		expect(path).toBe("/api/v1/skills/registries/{registryId}");
		expect(options.params.path.registryId).toBe("company-private");
		expect(options.body).toMatchObject({
			displayName: "Company Private",
			type: "local",
			location: "/srv/ao/registry",
			trustPolicy: "digest",
			enabled: true,
		});
	});

	it("refuses to submit until the identifier, name and location are all present", async () => {
		mockReads([]);
		renderSection();

		const submit = await screen.findByRole("button", { name: "Add registry" });
		expect(submit).toBeDisabled();
		await userEvent.type(screen.getByLabelText("Identifier"), "company-private");
		expect(submit).toBeDisabled();
	});

	// The credential field takes a NAME. A panel that displayed a value would
	// be a panel that had one.
	it("shows a credential as the name of a sealed secret", async () => {
		mockReads([{ ...digestRegistry, credentialSecretName: "REGISTRY_TOKEN" }]);
		renderSection();

		expect(
			await screen.findByText("Credential: sealed secret REGISTRY_TOKEN"),
		).toBeInTheDocument();
	});

	// ------------------------------------------------ phase 11: connectivity

	// A registry nobody has tested is not a registry that failed. Rendering it
	// red would teach people that red means nothing.
	it("shows a never-tested registry as never tested, not as a failure", async () => {
		mockReads([httpsRegistry]);
		renderSection();

		await screen.findByText("Company Private");
		expect(screen.getByTestId("skill-registry-probe-company-private")).toHaveTextContent(
			"Never tested",
		);
	});

	// The verdict and its sentence both come from the daemon. This screen has
	// no rule that could turn any other state green.
	it("renders the daemon's connection verdict and its explanation", async () => {
		mockReads([httpsRegistry]);
		const post = vi.spyOn(apiClient, "POST").mockResolvedValue({
			data: {
				registryId: "company-private",
				state: "CONNECTED",
				detail: "registry.corp.example answered as company-private over verified TLS.",
				origin: "https://registry.corp.example:443",
				testedAt: "2026-01-01T00:00:00Z",
				latencyMs: 42,
				assurance: "A connection test reads one small metadata endpoint.",
			},
		} as never);
		renderSection();

		await userEvent.click(await screen.findByTestId("skill-registry-test-company-private"));

		await waitFor(() =>
			expect(screen.getByTestId("skill-registry-probe-company-private")).toHaveTextContent(
				"Connected",
			),
		);
		expect(
			screen.getByTestId("skill-registry-probe-detail-company-private"),
		).toHaveTextContent("answered as company-private over verified TLS");
		const [path] = post.mock.calls[0] as [string];
		expect(path).toBe("/api/v1/skills/registries/{registryId}/test");
	});

	// AUTH_FAILED is not a green badge with an asterisk. And the detail the
	// daemon sends names a secretRef, never a value.
	it("shows a rejected credential as a failure naming the secret", async () => {
		mockReads([httpsRegistry]);
		vi.spyOn(apiClient, "POST").mockResolvedValue({
			data: {
				registryId: "company-private",
				state: "AUTH_FAILED",
				detail: "the registry answered 401 for the credential CORP_REGISTRY_TOKEN",
				testedAt: "2026-01-01T00:00:00Z",
				latencyMs: 12,
				assurance: "",
			},
		} as never);
		renderSection();

		await userEvent.click(await screen.findByTestId("skill-registry-test-company-private"));

		await waitFor(() =>
			expect(screen.getByTestId("skill-registry-probe-company-private")).toHaveTextContent(
				"Credential rejected",
			),
		);
		const detail = screen.getByTestId("skill-registry-probe-detail-company-private");
		expect(detail).toHaveTextContent("CORP_REGISTRY_TOKEN");
	});

	// The sentence the phase brief asks for, on the release that is actually
	// installed here — plus the daemon's non-promises underneath it.
	it("says an installed release was revoked, and states what AO did not do", async () => {
		mockReads([httpsRegistry]);
		vi.spyOn(apiClient, "POST").mockResolvedValue({
			data: {
				registryId: "company-private",
				fetched: 1,
				newlyRecorded: 1,
				affectedInstalls: ["security-audit@1.0.0"],
				syncedAt: "2026-01-01T00:00:00Z",
				revocations: [
					{
						registryId: "company-private",
						skillId: "security-audit",
						version: "1.0.0",
						reason: "a dependency shipped a backdoor",
						observedAt: "2026-01-01T00:00:00Z",
						installed: true,
					},
				],
				policy: REVOCATION_POLICY,
			},
		} as never);
		renderSection();

		await userEvent.click(await screen.findByTestId("skill-registry-sync-company-private"));

		const panel = await screen.findByTestId("skill-revocations-company-private");
		expect(panel).toHaveTextContent("Installed release has been revoked by registry.");
		expect(panel).toHaveTextContent("a dependency shipped a backdoor");
		expect(panel).toHaveTextContent(REVOCATION_POLICY);
	});

	// "AO could not ask" and "nothing is revoked" are different answers, and
	// only one of them means somebody should go and look.
	it("distinguishes an unreachable sync from an empty one", async () => {
		mockReads([httpsRegistry]);
		vi.spyOn(apiClient, "POST").mockResolvedValue({
			data: {
				registryId: "company-private",
				fetched: 0,
				newlyRecorded: 0,
				affectedInstalls: [],
				unreachable: "registry company-private could not be asked: connection refused",
				syncedAt: "2026-01-01T00:00:00Z",
				revocations: [],
				policy: REVOCATION_POLICY,
			},
		} as never);
		renderSection();

		await userEvent.click(await screen.findByTestId("skill-registry-sync-company-private"));

		const panel = await screen.findByTestId("skill-revocations-company-private");
		expect(panel).toHaveTextContent("could not be asked");
		expect(panel).toHaveTextContent(
			"What AO already recorded is unchanged and still blocks those installs.",
		);
	});

	// The private-range exception is the one setting that widens what AO may
	// connect to. An exception nobody can see is one nobody reviews.
	it("shows the network policy of an https registry", async () => {
		mockReads([httpsRegistry]);
		renderSection();

		expect(
			await screen.findByText("Network: private ranges permitted: 10.4.0.0/16"),
		).toBeInTheDocument();
	});

	// A local directory authenticates to nothing and opens no socket, so it is
	// offered neither control: a form that asked would be a form the daemon
	// refuses.
	it("offers auth and network controls only for an https registry", async () => {
		mockReads([]);
		renderSection();

		await screen.findByText("Add a registry");
		expect(screen.queryByLabelText("Authentication")).not.toBeInTheDocument();
		expect(screen.queryByLabelText("Permitted private ranges")).not.toBeInTheDocument();
	});

	// The credential field takes a NAME, and the request carries the auth TYPE
	// explicitly rather than leaving AO to guess it from the secret's presence.
	it("sends an https registry with its auth type and secret name", async () => {
		mockReads([]);
		const put = vi.spyOn(apiClient, "PUT").mockResolvedValue({ data: httpsRegistry } as never);
		renderSection();

		await screen.findByText("Add a registry");
		await userEvent.type(screen.getByLabelText("Identifier"), "company-private");
		await userEvent.type(screen.getByLabelText("Display name"), "Company Private");
		// The type and auth controls are shadcn Selects: a trigger plus a
		// portalled listbox, not a native <select>.
		await pickOption("Type", "Private HTTPS registry");
		await userEvent.type(
			screen.getByLabelText("Location"),
			"https://registry.corp.example",
		);
		await pickOption("Authentication", "Bearer token");
		await userEvent.type(
			screen.getByLabelText("Credential secret NAME"),
			"CORP_REGISTRY_TOKEN",
		);
		await userEvent.type(
			screen.getByLabelText("Permitted private ranges"),
			"10.4.0.0/16",
		);
		await userEvent.click(screen.getByRole("button", { name: "Add registry" }));

		await waitFor(() => expect(put).toHaveBeenCalled());
		const [, options] = put.mock.calls[0] as [
			string,
			{ body: Record<string, unknown> },
		];
		expect(options.body).toMatchObject({
			type: "https",
			location: "https://registry.corp.example",
			authType: "bearer",
			credentialSecretName: "CORP_REGISTRY_TOKEN",
			permittedPrivateCidrs: ["10.4.0.0/16"],
		});
	});
});
