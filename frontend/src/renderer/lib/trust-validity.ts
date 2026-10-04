/**
 * trust-validity.ts — classifying a trust root or signing key for display.
 *
 * # Why this exists as one function
 *
 * The backend decides whether a signature verifies (`SigningKey.UsableAt` in
 * internal/skillregistry). This decides what a person is TOLD about a key, and
 * the two must agree. One pure function with its own tests is how that stays
 * true: a component that inlined `status === "revoked" ? red : green` would
 * lose the distinctions below the first time somebody touched it.
 *
 * # The five states, and the one that is easy to get wrong
 *
 * - `active` — usable now.
 * - `not-yet-valid` — validFrom is in the future.
 * - `expired` — validUntil has passed.
 * - `retired` — withdrawn from new signing, on schedule.
 * - `revoked` — compromised or repudiated.
 *
 * The one that is easy to get wrong is the difference between the middle three
 * and the last. A key that expired or retired keeps its history: signatures it
 * made INSIDE its window still verify, because closing a window on schedule is
 * housekeeping. A REVOKED key does not: it may have been in somebody else's
 * hands for an unknown period before anybody noticed, so "the signature
 * predates the revocation" establishes nothing.
 *
 * `stillVerifiesHistory` carries exactly that, so a screen can say "expired —
 * releases signed before then still verify" instead of implying everything the
 * key ever signed is now suspect.
 */

export type TrustValidityState = "active" | "not-yet-valid" | "expired" | "retired" | "revoked";

export type TrustValidity = {
	state: TrustValidityState;
	/**
	 * Whether signatures made while this key/root was valid still verify.
	 * False only for revoked, which repudiates history as well as the future.
	 */
	stillVerifiesHistory: boolean;
};

export type TrustValidityInput = {
	/** The stored status: active, retired or revoked. */
	status: string;
	validFrom?: string | null;
	validUntil?: string | null;
};

/**
 * classifyTrustValidity maps a stored row onto what to show.
 *
 * Order matters and mirrors the backend: revoked wins over every window
 * comparison, because a revoked key is refused whatever its dates say.
 */
export function classifyTrustValidity(input: TrustValidityInput, now: Date): TrustValidity {
	if (input.status === "revoked") {
		return { state: "revoked", stillVerifiesHistory: false };
	}
	const from = input.validFrom ? new Date(input.validFrom) : null;
	const until = input.validUntil ? new Date(input.validUntil) : null;

	if (from && !Number.isNaN(from.getTime()) && now < from) {
		return { state: "not-yet-valid", stillVerifiesHistory: true };
	}
	if (until && !Number.isNaN(until.getTime()) && now > until) {
		return { state: "expired", stillVerifiesHistory: true };
	}
	if (input.status === "retired") {
		return { state: "retired", stillVerifiesHistory: true };
	}
	return { state: "active", stillVerifiesHistory: true };
}

/**
 * Badge tone per state. It is a SECOND signal beside the label, never the only
 * one: a state communicated by colour alone is a state a colour-blind reader
 * cannot read, and every call site renders the label too.
 */
export function trustValidityTone(state: TrustValidityState): "success" | "warning" | "error" {
	switch (state) {
		case "active":
			return "success";
		case "revoked":
			return "error";
		default:
			return "warning";
	}
}
