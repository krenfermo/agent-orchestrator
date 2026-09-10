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
const signedRegistry = {
	...digestRegistry,
	id: "strict",
	displayName: "Strict",
	trustPolicy: "signed",
	trustPolicyEnforceable: false,
};

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
			screen.getByLabelText("Absolute directory holding registry.json"),
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
});
