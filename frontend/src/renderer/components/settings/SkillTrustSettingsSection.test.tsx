import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiClient } from "../../lib/api-client";
import { SkillTrustSettingsSection } from "./SkillTrustSettingsSection";

const enterpriseRoot = {
	trustRootId: "corp-root",
	tier: "enterprise",
	displayName: "Corp Publishing",
	publisher: "corp",
	status: "active",
	validFrom: "2026-01-01T00:00:00Z",
	builtIn: false,
	keys: [
		{
			keyId: "corp-root-key",
			trustRootId: "corp-root",
			publisher: "corp",
			isRootKey: true,
			algorithm: "ed25519",
			publicKey: "cHVibGlj",
			fingerprint: "aaaabbbbccccddddeeeeffff0000111122223333444455556666777788889999",
			fingerprintShort: "aaaa bbbb cccc dddd",
			origin: "administrative",
			status: "active",
			validFrom: "2026-01-01T00:00:00Z",
		},
		{
			keyId: "corp-signing-key",
			trustRootId: "corp-root",
			publisher: "corp",
			isRootKey: false,
			algorithm: "ed25519",
			publicKey: "c2lnbmluZw==",
			fingerprint: "9999888877776666555544443333222211110000ffffeeeeddddccccbbbbaaaa",
			fingerprintShort: "9999 8888 7777 6666",
			origin: "certificate",
			status: "active",
			validFrom: "2026-01-01T00:00:00Z",
			rotatedFromKeyId: "corp-signing-key-0",
		},
	],
};

const builtInRoot = {
	...enterpriseRoot,
	trustRootId: "ao-official",
	tier: "official",
	displayName: "AO Official",
	publisher: "ao",
	builtIn: true,
	keys: [{ ...enterpriseRoot.keys[1], keyId: "ao-key", trustRootId: "ao-official", origin: "built-in" }],
};

function renderSection() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<SkillTrustSettingsSection />
		</QueryClientProvider>,
	);
}

function mockTrust(body: Record<string, unknown>) {
	return vi.spyOn(apiClient, "GET").mockResolvedValue({
		data: { roots: [], revocations: [], trustModel: "", officialRootAvailable: false, ...body },
	} as never);
}

afterEach(() => vi.restoreAllMocks());

describe("SkillTrustSettingsSection", () => {
	// An empty trust store says what it MEANS. "No roots" alone reads like a
	// setup step nobody got to; "nothing can reach trusted" is the state.
	it("says nothing can reach trusted when no root is configured", async () => {
		mockTrust({});
		renderSection();
		expect(
			await screen.findByText(
				"No trust roots are configured, so no release can reach “trusted” on this installation.",
			),
		).toBeInTheDocument();
	});

	// The trust model comes from the daemon. A screen that wrote its own
	// version of this sentence would eventually write a nicer one.
	it("renders the daemon's trust model rather than its own wording", async () => {
		mockTrust({
			roots: [enterpriseRoot],
			trustModel: "Trusted says who signed, not what they signed off on.",
		});
		renderSection();
		expect(
			await screen.findByText("Trusted says who signed, not what they signed off on."),
		).toBeInTheDocument();
	});

	// The absence of an official root is stated, not left as a greyed-out
	// control a person has to guess about.
	it("explains why the official policy does not work on this build", async () => {
		mockTrust({
			roots: [enterpriseRoot],
			officialRootAvailable: false,
			officialRootNote: "This build carries no AO Official trust root.",
		});
		renderSection();
		expect(await screen.findByTestId("skill-trust-official-note")).toHaveTextContent(
			"This build carries no AO Official trust root.",
		);
	});

	it("shows a key's fingerprint in full as well as grouped", async () => {
		mockTrust({ roots: [enterpriseRoot] });
		renderSection();
		const roots = await screen.findByTestId("skill-trust-roots");
		// Grouped, for recognising a key you already know.
		expect(roots).toHaveTextContent("aaaa bbbb cccc dddd");
		// And in full, because deciding two keys are the same one needs every
		// character.
		expect(roots).toHaveTextContent(
			"aaaabbbbccccddddeeeeffff0000111122223333444455556666777788889999",
		);
	});

	// "A root certified it" and "somebody here said so" are different
	// assurances, and a screen that rendered them identically would be
	// overstating one of them.
	it("distinguishes a certified key from an administratively added one", async () => {
		mockTrust({ roots: [enterpriseRoot] });
		renderSection();
		const roots = await screen.findByTestId("skill-trust-roots");
		expect(roots).toHaveTextContent("Certified by root");
		expect(roots).toHaveTextContent("Added by an administrator");
	});

	it("names the key a rotation replaced", async () => {
		mockTrust({ roots: [enterpriseRoot] });
		renderSection();
		expect(await screen.findByText("Rotated from corp-signing-key-0")).toBeInTheDocument();
	});

	// The revoke affordance on a built-in root is ABSENT, not disabled: it is
	// not a thing this host may do, and a greyed-out button invites somebody to
	// go looking for the way round it.
	it("offers no way to revoke a built-in root or its keys", async () => {
		mockTrust({ roots: [builtInRoot], officialRootAvailable: true });
		renderSection();
		await screen.findByTestId("skill-trust-builtin");
		expect(screen.queryByText("Revoke root")).not.toBeInTheDocument();
		expect(screen.queryByText("Revoke key")).not.toBeInTheDocument();
	});

	it("offers revocation for an enterprise root and its keys", async () => {
		mockTrust({ roots: [enterpriseRoot] });
		renderSection();
		expect(await screen.findByText("Revoke root")).toBeInTheDocument();
		expect(screen.getAllByText("Revoke key")).toHaveLength(2);
	});

	// The screen must never invite a private key. The label says public, the
	// note says why, and there is no generate affordance anywhere.
	it("asks only for a public key and says never to paste a private one", async () => {
		mockTrust({ roots: [enterpriseRoot] });
		renderSection();
		expect(await screen.findByText("Public key (base64)")).toBeInTheDocument();
		expect(
			screen.getByText(
				"The PUBLIC key only: 32 bytes, base64. Never paste a private key — AO verifies signatures and never makes them.",
			),
		).toBeInTheDocument();
		expect(screen.queryByText(/generate/i)).not.toBeInTheDocument();
	});

	// A daemon refusal is shown verbatim rather than softened. The refusals in
	// this area are the control that actually gets acted on.
	it("shows the daemon's refusal when a key is rejected", async () => {
		mockTrust({ roots: [enterpriseRoot] });
		vi.spyOn(apiClient, "PUT").mockResolvedValue({
			error: { message: "an ed25519 public key is 32 bytes and this one is 64" },
		} as never);
		renderSection();

		await screen.findByText("Add a signing key");
		const user = userEvent.setup();
		await user.type(screen.getByLabelText("Key id"), "pasted-wrong-half");
		await user.type(screen.getByLabelText("Trust root id"), "corp-root");
		await user.type(screen.getByLabelText("Public key (base64)"), "AAAA");
		await user.click(screen.getByRole("button", { name: "Add key" }));

		await waitFor(() =>
			expect(
				screen.getByText("an ed25519 public key is 32 bytes and this one is 64"),
			).toBeInTheDocument(),
		);
	});
});
