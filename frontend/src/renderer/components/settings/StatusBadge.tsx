import { useTranslation } from "react-i18next";
import { Badge } from "../ui/badge";

export type CapabilityState = "ready" | "unavailable" | "auth_required" | "unknown";

const CAPABILITY_LABELS: Record<CapabilityState, string> = {
	ready: "Ready",
	unavailable: "Unavailable",
	auth_required: "Authentication required",
	unknown: "Unknown",
};

const CAPABILITY_VARIANTS: Record<CapabilityState, "success" | "error" | "warning" | "neutral"> = {
	ready: "success",
	unavailable: "error",
	auth_required: "warning",
	unknown: "neutral",
};

/**
 * Renders a CapabilityState (from a real backend probe — see
 * service/environment) as a status pill. Never invents a state: an "unknown"
 * probe result renders as "Unknown", never as "Ready".
 */
export function CapabilityBadge({ state }: { state: CapabilityState | string }) {
	const known = (state as CapabilityState) in CAPABILITY_LABELS ? (state as CapabilityState) : "unknown";
	return <Badge variant={CAPABILITY_VARIANTS[known]}>{CAPABILITY_LABELS[known]}</Badge>;
}

const AUTH_LABELS: Record<string, string> = {
	authorized: "Authenticated",
	unauthorized: "Not authenticated",
	unknown: "Unknown",
};

const AUTH_VARIANTS: Record<string, "success" | "error" | "warning" | "neutral"> = {
	authorized: "success",
	unauthorized: "warning",
	unknown: "neutral",
};

/** Renders ports.AgentAuthStatus (authorized|unauthorized|unknown) as a pill. */
export function AuthStatusBadge({ status }: { status: string }) {
	const known = status in AUTH_LABELS ? status : "unknown";
	return <Badge variant={AUTH_VARIANTS[known]}>{AUTH_LABELS[known]}</Badge>;
}

const GITHUB_AUTH_LABELS: Record<string, string> = {
	authenticated: "Authenticated",
	unauthenticated: "Not authenticated",
	unknown: "Unknown",
};

const GITHUB_AUTH_VARIANTS: Record<string, "success" | "error" | "warning" | "neutral"> = {
	authenticated: "success",
	unauthenticated: "warning",
	unknown: "neutral",
};

/** Renders environment.GitHubAuthState (authenticated|unauthenticated|unknown). */
export function GitHubAuthBadge({ state }: { state: string }) {
	const known = state in GITHUB_AUTH_LABELS ? state : "unknown";
	return <Badge variant={GITHUB_AUTH_VARIANTS[known]}>{GITHUB_AUTH_LABELS[known]}</Badge>;
}

/** Renders a plain installed/not-installed pill, independent of auth state. */
export function InstalledBadge({ installed }: { installed: boolean }) {
	const { t } = useTranslation();
	return installed ? (
		<Badge variant="success">{t("settings.status.installed")}</Badge>
	) : (
		<Badge variant="error">{t("settings.status.notInstalled")}</Badge>
	);
}
