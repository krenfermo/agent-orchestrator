import { create } from "zustand";
import { apiClient, apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";
import type { components } from "../../api/schema";

export type TenantView = components["schemas"]["TenantView"];

// P4-C: which organization the app is currently looking at.
//
// The renderer's job here is presentation, not authorization. The daemon
// already refuses to return anything from an organization this account cannot
// reach, so this store never decides what is ALLOWED -- only which of the
// organizations the caller genuinely belongs to is currently on screen. A
// second authorization implementation in React is one that can disagree with
// the real one.
//
// The design constraint that shaped everything below: an installation with one
// organization -- still the overwhelmingly common one -- must look exactly as
// it did before P4-C. No selector, no picker on project creation, no extra
// concept to learn. Everything multi-organization is conditional on there
// actually being more than one.

export type TenantStatus = "idle" | "loading" | "loaded" | "error";

// The chosen organization survives a restart, per installation, in the same
// browser profile. Deliberately NOT sent to the daemon: which organization a
// person is looking at is a view preference, and storing it server-side would
// make one person's sidebar follow them onto somebody else's screen.
const STORAGE_KEY = "ao.currentTenantId";

function readStoredTenantID(): string | null {
	try {
		return globalThis.localStorage?.getItem(STORAGE_KEY) ?? null;
	} catch {
		// A renderer with storage blocked still has to work; it just forgets
		// the choice between launches.
		return null;
	}
}

function writeStoredTenantID(id: string | null): void {
	try {
		if (id === null) globalThis.localStorage?.removeItem(STORAGE_KEY);
		else globalThis.localStorage?.setItem(STORAGE_KEY, id);
	} catch {
		// Ignored for the reason above.
	}
}

type TenantState = {
	tenants: TenantView[];
	// currentTenantId is null until load() resolves, and stays null on an
	// installation with a single organization -- see currentTenantId's note in
	// useCurrentTenantID.
	currentTenantId: string | null;
	status: TenantStatus;
	error: string | null;
	load: () => Promise<void>;
	setCurrentTenant: (id: string) => void;
	reset: () => void;
};

let inFlight: Promise<void> | null = null;

export const useTenantStore = create<TenantState>((set, get) => ({
	tenants: [],
	currentTenantId: readStoredTenantID(),
	status: "idle",
	error: null,

	// load fetches the organizations this account belongs to. De-duplicated
	// in-flight, matching auth-store's load().
	load: async () => {
		if (inFlight) return inFlight;
		if (!hasTrustedApiBaseUrl()) return;
		set({ status: "loading", error: null });
		inFlight = (async () => {
			try {
				const { data, error } = await apiClient.GET("/api/v1/tenants");
				if (error) throw new Error(apiErrorMessage(error));
				const tenants = data.tenants ?? [];
				// Fall back to storage when nothing is chosen in memory. The
				// store reads storage once at construction, which is too early
				// on a cold start where the renderer boots before storage is
				// readable; re-reading here means a restart lands back on the
				// organization the person was last looking at rather than on
				// whichever one happens to sort first.
				const stored = get().currentTenantId ?? readStoredTenantID();
				// A remembered organization the account no longer belongs to is
				// dropped rather than kept: it would filter every list down to
				// nothing and look like an empty installation.
				const current = tenants.some((t) => t.id === stored) ? stored : (tenants[0]?.id ?? null);
				if (current !== stored) writeStoredTenantID(current);
				set({ tenants, currentTenantId: current, status: "loaded", error: null });
			} catch (err) {
				// Fail visible, not silent: an empty organization list would be
				// indistinguishable from "you belong to nothing".
				set({ tenants: [], status: "error", error: apiErrorMessage(err) });
			} finally {
				inFlight = null;
			}
		})();
		return inFlight;
	},

	setCurrentTenant: (id: string) => {
		if (!get().tenants.some((t) => t.id === id)) return;
		writeStoredTenantID(id);
		set({ currentTenantId: id });
	},

	// reset is sign-out: it forgets the organizations AND the choice, in
	// memory and in storage. Leaving the choice behind would show the next
	// person to sign in on this machine the previous person's organization
	// name in the switcher before the list refreshed.
	reset: () => {
		inFlight = null;
		writeStoredTenantID(null);
		set({ tenants: [], currentTenantId: null, status: "idle", error: null });
	},
}));

/**
 * Whether this installation has more than one organization the caller can
 * reach. Every piece of multi-organization UI hangs off this, so a
 * single-organization installation renders exactly what it always did.
 */
export function useHasMultipleTenants(): boolean {
	return useTenantStore((s) => s.tenants.length > 1);
}

/**
 * The organization currently on screen, or null when there is nothing to
 * choose between. Null is the single-organization answer AND the
 * not-loaded-yet answer, which is correct for both: with one organization
 * there is nothing to filter by, and before the list resolves there is nothing
 * to filter with.
 */
export function useCurrentTenantID(): string | null {
	return useTenantStore((s) => (s.tenants.length > 1 ? s.currentTenantId : null));
}

/** The full record for the organization currently on screen. */
export function useCurrentTenant(): TenantView | null {
	return useTenantStore((s) => s.tenants.find((t) => t.id === s.currentTenantId) ?? null);
}
