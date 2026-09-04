import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LoginScreen } from "./LoginScreen";
import { useAuthStore } from "../stores/auth-store";

afterEach(() => {
	useAuthStore.setState({
		user: null,
		status: "unauthenticated",
		error: null,
		providers: null,
		ssoPending: false,
		authMethod: null,
		issuer: null,
	});
});

// P4-A: an installation with no provider must look exactly as it did before —
// a password form and nothing else.
test("offers passwords only when the backend advertises no provider", async () => {
	const loadProviders = vi.fn().mockResolvedValue(undefined);
	useAuthStore.setState({ loadProviders, providers: { mode: "trusted_local", passwordEnabled: true } });

	render(<LoginScreen />);

	await waitFor(() => expect(loadProviders).toHaveBeenCalled());
	expect(screen.queryByRole("button", { name: /Sign in with/ })).toBeNull();
	expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
});

test("renders the provider's button and starts single sign-on on click", async () => {
	const loadProviders = vi.fn().mockResolvedValue(undefined);
	const startSso = vi.fn().mockResolvedValue(undefined);
	useAuthStore.setState({
		loadProviders,
		startSso,
		providers: {
			mode: "oidc",
			passwordEnabled: true,
			oidc: { displayName: "Okta", startPath: "/api/v1/auth/oidc/start" },
		},
	});

	render(<LoginScreen />);

	const button = await screen.findByRole("button", { name: "Sign in with Okta" });
	fireEvent.click(button);

	await waitFor(() => expect(startSso).toHaveBeenCalled());
});

test("shows a waiting hint and disables the button while the browser sign-in is in flight", async () => {
	useAuthStore.setState({
		loadProviders: vi.fn().mockResolvedValue(undefined),
		startSso: vi.fn(),
		ssoPending: true,
		providers: {
			mode: "oidc",
			passwordEnabled: true,
			oidc: { displayName: "Okta", startPath: "/api/v1/auth/oidc/start" },
		},
	});

	render(<LoginScreen />);

	const button = await screen.findByRole("button", { name: "Opening Okta…" });
	expect(button).toBeDisabled();
	expect(screen.getByText("Finish signing in in your browser, then return here.")).toBeInTheDocument();
});

// The renderer must never learn — and therefore never be able to leak — the
// issuer, the client id, or anything else about the provider configuration.
test("renders nothing about the provider beyond its label", async () => {
	useAuthStore.setState({
		loadProviders: vi.fn().mockResolvedValue(undefined),
		startSso: vi.fn(),
		providers: {
			mode: "oidc",
			passwordEnabled: true,
			oidc: { displayName: "Okta", startPath: "/api/v1/auth/oidc/start" },
		},
	});

	const { container } = render(<LoginScreen />);
	await screen.findByRole("button", { name: "Sign in with Okta" });

	expect(container.textContent).not.toMatch(/https?:\/\//);
	expect(container.textContent).not.toMatch(/client/i);
});
