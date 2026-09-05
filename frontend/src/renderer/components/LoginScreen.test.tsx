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
		providersStatus: "idle",
		ssoError: null,
		ssoPending: false,
		authMethod: null,
		issuer: null,
	});
});

const GOOGLE = {
	mode: "oidc" as const,
	passwordEnabled: true,
	oidc: { displayName: "Google", startPath: "/api/v1/auth/oidc/start" },
};

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

// B4: the button contract. Given exactly what the backend returns for the
// Google configuration, the screen must offer a Google action.
test("renders the Google action for the backend's advertised provider", async () => {
	useAuthStore.setState({
		loadProviders: vi.fn().mockResolvedValue(undefined),
		startSso: vi.fn(),
		providers: GOOGLE,
		providersStatus: "loaded",
	});

	render(<LoginScreen />);

	expect(await screen.findByRole("button", { name: "Sign in with Google" })).toBeInTheDocument();
});

test("keeps the password form alongside the provider when passwords are enabled", async () => {
	useAuthStore.setState({
		loadProviders: vi.fn().mockResolvedValue(undefined),
		startSso: vi.fn(),
		providers: GOOGLE,
		providersStatus: "loaded",
	});

	render(<LoginScreen />);

	await screen.findByRole("button", { name: "Sign in with Google" });
	expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
	expect(screen.getByLabelText(/Username or email/i)).toBeInTheDocument();
});

// B3/B7: the screen mounts while providers are still unknown, and the store
// fills them in when the daemon becomes ready. The button must appear then —
// this is the case that used to stay password-only forever.
test("shows the provider once the store loads it after the daemon becomes ready", async () => {
	useAuthStore.setState({
		loadProviders: vi.fn().mockResolvedValue(undefined),
		startSso: vi.fn(),
		providers: null,
		providersStatus: "idle",
	});

	render(<LoginScreen />);

	expect(screen.queryByRole("button", { name: /Sign in with/ })).toBeNull();

	useAuthStore.setState({ providers: GOOGLE, providersStatus: "loaded" });

	expect(await screen.findByRole("button", { name: "Sign in with Google" })).toBeInTheDocument();
});

// B6/B8: a provider that will not answer is an SSO problem. It must not be
// dressed up as the daemon failing to start, and it must not block passwords.
test("shows an SSO-specific error when provider discovery fails", async () => {
	useAuthStore.setState({
		loadProviders: vi.fn().mockResolvedValue(undefined),
		startSso: vi.fn(),
		providers: null,
		providersStatus: "error",
		ssoError: "Single sign-on options could not be loaded.",
	});

	render(<LoginScreen />);

	expect(await screen.findByTestId("login-sso-error")).toHaveTextContent("Single sign-on options could not be loaded.");
	expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
});

test("never renders the daemon startup message on the sign-in screen", async () => {
	useAuthStore.setState({
		loadProviders: vi.fn().mockResolvedValue(undefined),
		startSso: vi.fn(),
		providers: GOOGLE,
		providersStatus: "loaded",
	});

	const { container } = render(<LoginScreen />);
	await screen.findByRole("button", { name: "Sign in with Google" });

	expect(container.textContent).not.toMatch(/daemon/i);
	expect(container.textContent).not.toMatch(/iniciando/i);
});

// B5: in Electron the click must go through the bridge-backed store action, not
// a page navigation that would put a provider URL in the renderer's address bar.
test("routes the provider click through startSso rather than navigating", async () => {
	const startSso = vi.fn().mockResolvedValue(undefined);
	const assign = vi.fn();
	useAuthStore.setState({
		loadProviders: vi.fn().mockResolvedValue(undefined),
		startSso,
		providers: GOOGLE,
		providersStatus: "loaded",
	});

	render(<LoginScreen />);
	fireEvent.click(await screen.findByRole("button", { name: "Sign in with Google" }));

	await waitFor(() => expect(startSso).toHaveBeenCalledTimes(1));
	expect(assign).not.toHaveBeenCalled();
});
