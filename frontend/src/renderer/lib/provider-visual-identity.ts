/**
 * Centralized provider brand-identity mapping for Settings → Development
 * Agents (Checkpoint 8P-E.1 Phase 7). Presentational only: nothing here
 * feeds routing, capabilities, or auth decisions — it only decides how a
 * provider's card looks. Status (ready/error/etc.) is always conveyed by
 * icon + text as well as color, never by brand color alone.
 *
 * New providers get a restrained accent automatically via the neutral
 * fallback; add a case here only when a provider earns its own identity.
 */
export type ProviderVisualIdentity = {
	/** Tailwind text-color utility class, sourced from the --color-provider-*
	 * design tokens in styles/tokens.css (never a raw hex in a component). */
	accentTextClass: string;
	/** Tailwind border-color utility class, same token family. */
	accentBorderClass: string;
	/** Tailwind background-color utility class at low opacity, for a subtle
	 * card wash — never full-strength brand color as a background. */
	accentBgClass: string;
	/** Short organization label shown under the provider's display name. */
	organization: string;
};

const NEUTRAL_IDENTITY: ProviderVisualIdentity = {
	accentTextClass: "text-settings-muted",
	accentBorderClass: "border-(--color-border-settings-dialog-header)",
	accentBgClass: "bg-transparent",
	organization: "",
};

const IDENTITIES: Record<string, ProviderVisualIdentity> = {
	anthropic: {
		accentTextClass: "text-provider-anthropic",
		accentBorderClass: "border-provider-anthropic",
		accentBgClass: "bg-provider-anthropic/10",
		organization: "Anthropic",
	},
	openai: {
		accentTextClass: "text-provider-openai",
		accentBorderClass: "border-provider-openai",
		accentBgClass: "bg-provider-openai/10",
		organization: "OpenAI",
	},
};

/**
 * Returns the presentation metadata for a provider id (e.g. "anthropic",
 * "openai"). Unknown/future providers get a safe neutral identity rather
 * than a fabricated brand color.
 */
export function providerVisualIdentity(provider: string): ProviderVisualIdentity {
	return IDENTITIES[provider] ?? NEUTRAL_IDENTITY;
}
