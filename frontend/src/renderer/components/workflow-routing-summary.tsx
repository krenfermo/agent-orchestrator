import { useTranslation } from "react-i18next";
import type { components } from "../../api/schema";

export type RoutingDecisionView = components["schemas"]["ControllersRoutingDecisionView"];

const ROLE_LABEL_KEY: Record<string, string> = {
	planner: "shell.routingRole.planner",
	worker: "shell.routingRole.worker",
	fix_worker: "shell.routingRole.worker",
	reviewer: "shell.routingRole.reviewer",
	decision_resolver: "shell.routingRole.decisionResolver",
};
const ROLE_LABEL_DEFAULT: Record<string, string> = {
	planner: "Planner",
	worker: "Worker",
	fix_worker: "Worker",
	reviewer: "Reviewer",
	decision_resolver: "Decision Resolver",
};

// Checkpoint 8P-C.1 §18: user-friendly labels for the closed RoutingReason
// enum -- never expose the raw internal code in the UI when a translation
// exists here.
const REASON_LABEL_KEY: Record<string, string> = {
	user_preferred_provider: "shell.routingReason.userPreferredProvider",
	preferred_for_complexity: "shell.routingReason.userPreferredProvider",
	preferred_unavailable: "shell.routingReason.preferredUnavailable",
	fallback_selected: "shell.routingReason.fallbackSelected",
	provider_cooldown: "shell.routingReason.providerCooldown",
	provider_unavailable: "shell.routingReason.providerUnavailable",
	cross_provider_review: "shell.routingReason.crossProviderReview",
	review_independence_required: "shell.routingReason.reviewIndependenceRequired",
	same_provider_fallback_allowed: "shell.routingReason.sameProviderFallbackAllowed",
	waiting_for_capacity: "shell.routingReason.waitingForCapacity",
	planner_policy: "shell.routingReason.plannerPolicy",
	only_eligible_provider: "shell.routingReason.onlyEligibleProvider",
	provider_disabled: "shell.routingReason.providerDisabled",
	profile_not_connected: "shell.routingReason.profileNotConnected",
	capability_missing: "shell.routingReason.capabilityMissing",
	unsupported_provider: "shell.routingReason.unsupportedProvider",
};
const REASON_LABEL_DEFAULT: Record<string, string> = {
	user_preferred_provider: "Preferred by your policy",
	preferred_for_complexity: "Preferred by your policy",
	preferred_unavailable: "Preferred provider unavailable",
	fallback_selected: "Fallback provider selected",
	provider_cooldown: "Provider in cooldown",
	provider_unavailable: "Provider unavailable",
	cross_provider_review: "Independent review provider",
	review_independence_required: "Independent reviewer required",
	same_provider_fallback_allowed: "Same-provider fallback allowed",
	waiting_for_capacity: "Waiting for capacity",
	planner_policy: "Planner policy",
	only_eligible_provider: "Only eligible provider",
	provider_disabled: "Provider disabled",
	profile_not_connected: "Profile not connected",
	capability_missing: "Capability not supported",
	unsupported_provider: "Unsupported provider",
};

/**
 * Checkpoint 8P-C.1 §14/§16: a compact, read-only rendering of one step's
 * ALREADY-PERSISTED routing decision (see WorkflowStepView.routing) --
 * never recomputes anything. Friendly role/reason labels per §18; raw
 * harness/profile ids are never shown when a display name is known.
 */
export function WorkflowRoutingSummary({ routing }: { routing: RoutingDecisionView }) {
	const { t } = useTranslation();
	const roleLabel = t(ROLE_LABEL_KEY[routing.role] ?? "", ROLE_LABEL_DEFAULT[routing.role] ?? routing.role);
	const preferredLabel = routing.preferredProfile?.displayName || routing.preferredHarness;
	const selectedLabel = routing.selectedProfile?.displayName || routing.selectedHarness;
	const primaryReason = routing.reasonCodes?.[0];
	const reasonLabel = primaryReason ? t(REASON_LABEL_KEY[primaryReason] ?? "", REASON_LABEL_DEFAULT[primaryReason] ?? primaryReason) : undefined;

	return (
		<dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-2 gap-y-1 border-t border-border pt-2 text-xs text-muted-foreground">
			<dt>{roleLabel}</dt>
			<dd>
				{routing.waiting
					? t("shell.routingWaitingFor", { provider: preferredLabel ?? "—", defaultValue: `Waiting for ${preferredLabel ?? "—"}` })
					: (selectedLabel ?? "—")}
			</dd>
			{reasonLabel && (
				<>
					<dt>{t("shell.routingReasonLabel", "Reason")}</dt>
					<dd>{reasonLabel}</dd>
				</>
			)}
			{!routing.waiting && routing.fallbackUsed && preferredLabel && (
				<>
					<dt>{t("shell.routingFallbackLabel", "Fallback")}</dt>
					<dd>{t("shell.routingFallbackFrom", { from: preferredLabel, defaultValue: `${preferredLabel} unavailable → fallback selected` })}</dd>
				</>
			)}
		</dl>
	);
}
