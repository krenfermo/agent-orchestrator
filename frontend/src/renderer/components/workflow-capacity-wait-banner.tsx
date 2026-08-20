import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { components } from "../../api/schema";
import { isCapacityWaitReason } from "../lib/workflow-wake-reason";
import { Badge } from "./ui/badge";

export type WorkflowRunView = components["schemas"]["WorkflowRunView"];

// Same rationale as workflow-questions-section.tsx's own `translate` helper:
// waitReason is one of wake.Reason's small, finite, backend-defined set
// (already enumerated in en.json), not a value the generated TFunction key
// union can express.
function translate(t: TFunction, key: string): string {
	const untypedT = t as unknown as (key: string) => string;
	return untypedT(key);
}

function reasonLabel(t: TFunction, reason: string): string {
	const key = `shell.workflowsWaitingForCapacityReason.${reason}`;
	const label = translate(t, key);
	return label === key ? reason : label;
}

/**
 * WorkflowCapacityWaitBanner surfaces Checkpoint 8N.1's durable wake state
 * for a run: only rendered when the backend actually has an open wake
 * (run.nextWakeAt set) — never a synthesized "waiting" guess from run.state
 * alone, and never a fabricated retry time. next automatic retry, which
 * role is blocked, and the attempt count are all real values straight off
 * WorkflowRunDetailView; nothing here is computed client-side.
 */
export function WorkflowCapacityWaitBanner({ run }: { run: WorkflowRunView }) {
	const { t } = useTranslation();
	// Checkpoint 8P-E.3: run.nextWakeAt is set for ANY pending durable wake,
	// including the routine autonomous_progress heartbeat that re-polls a
	// healthy, still-progressing autonomous run. Rendering "Waiting for
	// capacity" for that case is misleading -- a real run showed this banner
	// while Claude's own health record said available/dispatch-succeeded.
	// Only render when the wake is actually capacity/rate-limit-shaped.
	if (!run.nextWakeAt || !isCapacityWaitReason(run.waitReason)) return null;

	const nextWakeDate = new Date(run.nextWakeAt);
	const nextWakeLabel = Number.isNaN(nextWakeDate.getTime()) ? run.nextWakeAt : nextWakeDate.toLocaleString();

	return (
		<div className="flex flex-col gap-2 rounded-lg border border-warning/40 bg-warning/10 p-3 text-sm text-warning">
			<div className="flex flex-wrap items-center gap-2">
				<Badge variant="warning">{t("shell.workflowsWaitingForCapacity")}</Badge>
				{run.waitReason && <span className="font-medium">{reasonLabel(t, run.waitReason)}</span>}
				{Boolean(run.wakeAttemptCount) && (
					<span className="text-warning/80">{t("shell.workflowsWakeAttempt", { count: run.wakeAttemptCount })}</span>
				)}
			</div>
			<p className="text-warning/90">{t("shell.workflowsNextWake", { when: nextWakeLabel })}</p>
		</div>
	);
}
