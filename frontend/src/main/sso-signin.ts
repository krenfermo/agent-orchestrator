import { randomBytes } from "node:crypto";
import { net, session, shell } from "electron";

/**
 * P4-A desktop single sign-on.
 *
 * The problem this solves: an identity provider must be visited in a real
 * browser (many refuse embedded webviews outright, and the ones that don't
 * still hide the address bar a person needs to check), but the AO session it
 * produces has to end up in the DESKTOP's cookie jar, not the system
 * browser's.
 *
 * The shape:
 *
 *   1. This process mints a HANDOFF SECRET and posts it to the daemon over
 *      loopback with the start request. The secret never leaves this machine
 *      and never travels to the identity provider.
 *   2. The daemon returns an authorization URL, which we open in the system
 *      browser. The person signs in there.
 *   3. The provider redirects to the daemon's loopback callback. The daemon
 *      verifies everything and records WHO signed in — but issues no session,
 *      and the page the browser lands on carries nothing.
 *   4. This process polls the claim endpoint with the handoff secret. When the
 *      login has completed, the daemon mints the session and returns it as a
 *      Set-Cookie on that loopback response.
 *
 * The last step is why `net.request` is used rather than `fetch`: bound to the
 * renderer's own session, it writes the cookie straight into that cookie jar.
 * The raw session token is therefore never a JavaScript value in this process
 * or in the renderer, and never appears in any URL, deep link, or query
 * parameter.
 */

/** How long to keep polling before giving up on an unfinished sign-in. */
const CLAIM_TIMEOUT_MS = 5 * 60 * 1000;
/** Gap between claim attempts. */
const CLAIM_INTERVAL_MS = 1500;

export type SsoSignInResult =
	| { status: "complete"; user: { id: string; displayName: string; email: string } }
	| { status: "cancelled" };

export class SsoSignInError extends Error {}

type Deps = {
	/** Daemon base URL, e.g. http://127.0.0.1:3001. */
	apiBaseUrl: string;
	/** Injected for tests; defaults to Electron's own. */
	openExternal?: (url: string) => Promise<unknown>;
	request?: typeof requestJSON;
	sleep?: (ms: number) => Promise<void>;
	now?: () => number;
};

type JSONResponse = { status: number; body: unknown };

/**
 * requestJSON issues a request through the RENDERER's session, so any
 * Set-Cookie on the response lands in the cookie jar the app's own requests
 * use. `useSessionCookies` is what makes that true for a request Electron's
 * net module issues outside a page context.
 */
async function requestJSON(url: string, init: { method: string; body?: string }): Promise<JSONResponse> {
	return await new Promise<JSONResponse>((resolve, reject) => {
		const request = net.request({
			url,
			method: init.method,
			session: session.defaultSession,
			useSessionCookies: true,
		});
		request.setHeader("Content-Type", "application/json");
		request.setHeader("Accept", "application/json");
		request.on("response", (response) => {
			const chunks: Buffer[] = [];
			response.on("data", (chunk) => chunks.push(Buffer.from(chunk)));
			response.on("end", () => {
				const raw = Buffer.concat(chunks).toString("utf8");
				let body: unknown = null;
				try {
					body = raw ? JSON.parse(raw) : null;
				} catch {
					body = null;
				}
				resolve({ status: response.statusCode, body });
			});
			response.on("error", reject);
		});
		request.on("error", reject);
		if (init.body) request.write(init.body);
		request.end();
	});
}

function apiErrorMessage(body: unknown, fallback: string): string {
	if (body && typeof body === "object" && "error" in body) {
		const error = (body as { error?: { message?: unknown } }).error;
		if (error && typeof error === "object" && typeof error.message === "string") return error.message;
	}
	return fallback;
}

const defaultSleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

/**
 * beginSsoSignIn runs the whole desktop flow and resolves once the session
 * cookie is installed, or throws with a message safe to show a person.
 */
export async function beginSsoSignIn(deps: Deps): Promise<SsoSignInResult> {
	const base = deps.apiBaseUrl.replace(/\/+$/, "");
	const request = deps.request ?? requestJSON;
	const openExternal = deps.openExternal ?? shell.openExternal;
	const sleep = deps.sleep ?? defaultSleep;
	const now = deps.now ?? Date.now;

	// 43 base64url characters, matching the length floor the daemon enforces.
	const handoffSecret = randomBytes(32).toString("base64url");

	const started = await request(`${base}/api/v1/auth/oidc/start`, {
		method: "POST",
		body: JSON.stringify({ clientKind: "desktop", handoffSecret }),
	});
	if (started.status !== 200) {
		throw new SsoSignInError(apiErrorMessage(started.body, "Single sign-on is not available on this installation."));
	}
	const { authorizationUrl, flowId } = (started.body ?? {}) as { authorizationUrl?: string; flowId?: string };
	if (!authorizationUrl || !flowId) {
		throw new SsoSignInError("The daemon did not return a sign-in request.");
	}

	await openExternal(authorizationUrl);

	const deadline = now() + CLAIM_TIMEOUT_MS;
	for (;;) {
		const claimed = await request(`${base}/api/v1/auth/oidc/claim`, {
			method: "POST",
			body: JSON.stringify({ flowId, handoffSecret }),
		});
		if (claimed.status === 200) {
			const body = (claimed.body ?? {}) as { status?: string; user?: SsoSignInResult extends { user: infer U } ? U : never };
			if (body.status === "complete" && body.user) {
				return { status: "complete", user: body.user };
			}
			// "pending": the person has not finished at the provider yet.
		} else {
			// Anything else is terminal: an expired flow, a revoked secret, a
			// refused identity. Polling past it would only hide the reason.
			throw new SsoSignInError(apiErrorMessage(claimed.body, "Single sign-on could not be completed."));
		}
		if (now() >= deadline) return { status: "cancelled" };
		await sleep(CLAIM_INTERVAL_MS);
	}
}
