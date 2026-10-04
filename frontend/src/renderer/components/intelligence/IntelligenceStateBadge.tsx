import { useTranslation } from "react-i18next";
import { Badge, type BadgeVariant } from "../ui/badge";
import type { MessageKey } from "../../i18n/messages";
import type { IntelligenceState } from "../../hooks/useProjectIntelligence";

// The one place the derived lifecycle becomes a colour.
//
// Stale is a WARNING, not a neutral note and not an error: the graph is intact
// and still useful, and what is missing is only the proof that it still
// describes the checkout. Colouring it green would be the lie the whole
// subsystem is built to avoid; colouring it red would send people to rebuild
// something that is merely a commit behind.
const LABEL_KEYS: Record<IntelligenceState, MessageKey> = {
	pending: "intelligence.state.pending",
	indexing: "intelligence.state.indexing",
	ready: "intelligence.state.ready",
	stale: "intelligence.state.stale",
	failed: "intelligence.state.failed",
};

const VARIANTS: Record<IntelligenceState, BadgeVariant> = {
	pending: "neutral",
	indexing: "accent",
	ready: "success",
	stale: "warning",
	failed: "error",
};

export function IntelligenceStateBadge({ state }: { state: string }) {
	const { t } = useTranslation();
	const key = (state in VARIANTS ? state : "pending") as IntelligenceState;
	return (
		<Badge variant={VARIANTS[key]} data-testid={`intelligence-state-${key}`}>
			{t(LABEL_KEYS[key])}
		</Badge>
	);
}
