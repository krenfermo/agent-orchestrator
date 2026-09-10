import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiClient } from "../../lib/api-client";
import { SkillInstalledSettingsSection } from "./SkillInstalledSettingsSection";

const base = {
	id: "security-audit",
	version: "1.0.0",
	name: "Security Audit",
	description: "Audits a repository.",
	riskLevel: "medium",
	originType: "local",
	publisher: "corp",
	digest: "a".repeat(64),
	capabilities: ["repo.read"],
	modes: [],
	requiresIsolatedRunner: false,
	approval: "per_activation",
	installedAt: "2026-09-10T10:00:00Z",
};

const trustedOrigin = {
	skillId: "security-audit",
	version: "1.0.0",
	registryId: "corp-signed",
	registryName: "Corp Signed",
	registryType: "https",
	publisher: "corp",
	manifestDigest: "b".repeat(64),
	artifactDigest: "c".repeat(64),
	trust: "trusted",
	trustExplanation: "AO verified the bytes AND a signature. It does not say the code is safe.",
	trustPolicy: "signed",
	compatibility: "compatible",
	installedAt: "2026-09-10T10:00:00Z",
	revocationStateObserved: "none-known: nothing was listed as withdrawn",
	provenance: {
		verified: true,
		scheme: "ao-sig-ed25519/v1",
		algorithm: "ed25519",
		keyId: "corp-signing-key",
		keyFingerprint: "d".repeat(64),
		keyFingerprintShort: "dddd dddd dddd dddd",
		keyOrigin: "certificate",
		trustRootId: "corp-root",
		trustRootName: "Corp Publishing",
		trustRootTier: "enterprise",
		publisher: "corp",
		signedAt: "2026-09-09T10:00:00Z",
		verifiedAt: "2026-09-10T10:00:00Z",
	},
};

const verifiedOrigin = {
	...trustedOrigin,
	registryId: "corp-plain",
	registryName: "Corp Plain",
	trust: "verified",
	trustPolicy: "digest",
	trustExplanation:
		"AO fetched the bytes and computed both digests itself. That is integrity, not provenance.",
	provenance: undefined,
};

function renderSection() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<SkillInstalledSettingsSection />
		</QueryClientProvider>,
	);
}

function mockSkills(skills: unknown[]) {
	return vi.spyOn(apiClient, "GET").mockResolvedValue({ data: { skills } } as never);
}

afterEach(() => vi.restoreAllMocks());

describe("SkillInstalledSettingsSection", () => {
	it("says so when nothing is installed", async () => {
		mockSkills([]);
		renderSection();
		expect(await screen.findByText("No Skills are installed.")).toBeInTheDocument();
	});

	// The gap this screen exists to close: a TRUSTED install must be
	// distinguishable from a VERIFIED one, with the chain that earned it.
	it("shows a TRUSTED install with its full provenance chain", async () => {
		mockSkills([{ ...base, origin: trustedOrigin }]);
		renderSection();

		// Await the chain first: the list element exists while the query is
		// still in flight, so grabbing it up front asserts against an empty ul.
		const chain = await screen.findByTestId("skill-installed-provenance");
		const list = screen.getByTestId("skill-installed-list");
		expect(list).toHaveTextContent("Signature verified");
		expect(chain).toHaveTextContent("corp-signing-key");
		expect(chain).toHaveTextContent("Certified by root");
		expect(chain).toHaveTextContent("Enterprise");
		expect(chain).toHaveTextContent("Corp Publishing");
		// Short for recognition AND full for comparison.
		expect(chain).toHaveTextContent("dddd dddd dddd dddd");
		expect(chain).toHaveTextContent("d".repeat(64));
		expect(list).toHaveTextContent("c".repeat(64)); // artifact digest
		expect(list).toHaveTextContent("b".repeat(64)); // manifest digest
		expect(list).toHaveTextContent("Corp Signed");
	});

	// VERIFIED must not borrow trusted's chrome: no provenance object means AO
	// checked no signature, and the screen says that rather than showing an
	// empty chain.
	it("shows a VERIFIED install as integrity-only with no chain", async () => {
		mockSkills([{ ...base, origin: verifiedOrigin }]);
		renderSection();

		expect(
			await screen.findByText(
				"No signature was checked: this release carries none and its registry does not require one.",
			),
		).toBeInTheDocument();
		const list = screen.getByTestId("skill-installed-list");
		expect(list).toHaveTextContent("Integrity verified");
		expect(list).toHaveTextContent("integrity, not provenance");
		expect(screen.queryByTestId("skill-installed-provenance")).not.toBeInTheDocument();
	});

	// "AO checked and refused" is not "AO never checked".
	it("distinguishes a refused signature from an unsigned release", async () => {
		mockSkills([
			{
				...base,
				origin: {
					...trustedOrigin,
					trust: "verified",
					provenance: {
						verified: false,
						keyId: "corp-signing-key",
						refusalCode: "SIGNATURE_INVALID",
						refusal: "the release was altered after signing",
					},
				},
			},
		]);
		renderSection();
		const refused = await screen.findByTestId("skill-installed-signature-refused");
		expect(refused).toHaveTextContent("Signature refused");
		expect(refused).toHaveTextContent("SIGNATURE_INVALID");
		expect(refused).toHaveTextContent("altered after signing");
	});

	it("marks a revoked install and never implies AO removed it", async () => {
		mockSkills([
			{
				...base,
				origin: {
					...trustedOrigin,
					trust: "revoked",
					revoked: true,
					revocationReason: "withdrawn by the publisher",
				},
			},
		]);
		renderSection();
		const revoked = await screen.findByTestId("skill-installed-revoked");
		expect(revoked).toHaveTextContent("withdrawn by the publisher");
		expect(revoked).toHaveTextContent("removed nothing");
	});

	// A hand-vetted local install has no provenance, and that is a different
	// fact from a record saying nothing was verified.
	it("says an install has no registry origin rather than showing an empty one", async () => {
		mockSkills([{ ...base, origin: undefined }]);
		renderSection();
		expect(await screen.findByText("No registry origin")).toBeInTheDocument();
		const list = screen.getByTestId("skill-installed-list");
		expect(list).toHaveTextContent("AO holds no provenance record");
	});

	// The rule the whole family follows.
	it("never claims a trusted package is safe, secure or vulnerability-free", async () => {
		mockSkills([{ ...base, origin: trustedOrigin }]);
		const { container } = renderSection();
		await screen.findByTestId("skill-installed-provenance");
		const text = container.textContent ?? "";
		expect(text).not.toMatch(/vulnerability[- ]free/i);
		// "safe" may appear only inside the daemon's own negation.
		for (const sentence of text.split(/(?<=[.!?])\s+/)) {
			if (/\bsafe\b|\bsecure\b/i.test(sentence)) {
				expect(sentence).toMatch(/does not|never|not mean/i);
			}
		}
	});
});
