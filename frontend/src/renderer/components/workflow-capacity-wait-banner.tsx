import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { components } from "../../api/schema";
import { isCapacityWaitReason } from "../lib/workflow-wake-reason";
import { formatDurationCompact } from "../lib/format-time";
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

function capacityReasonLabel(t: TFunction, reason: string): string {
	const key = `shell.workflowsCapacityWaitReason.${reason}`;
	const label = translate(t, key);
	return label === key ? reason : label;
}

function timestampLabel(value: string): string {
	const date = new Date(value);
	return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

/**
 * WorkflowCapacityWaitBanner surfaces a run's durable capacity wait: only
 * rendered when the backend actually has an open wake (run.nextWakeAt set) —
 * never a synthesized "waiting" guess from run.state alone, and never a
 * fabricated retry time.
 *
 * When the backend also ships its normalized `capacityWait` projection, this
 * renders that instead of the bare wake reason. The projection is the whole
 * point: "reviewer_capacity, attempt 214" says a role is blocked, whereas
 * "Claude — previous transient launch failure, observed 3h ago, AO is probing
 * it, next attempt 14:32" says what is actually happening. None of that is
 * computed here; every value below comes straight off the API, including the
 * health age, so the panel cannot disagree with the daemon's own clock.
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

	const wait = run.capacityWait;
	const nextAttempt = wait?.nextAttemptAt ?? run.nextWakeAt;
	const attempt = wait?.attempt ?? run.wakeAttemptCount;

	return (
		<div className="flex flex-col gap-2 rounded-lg border border-warning/40 bg-warning/10 p-3 text-sm text-warning">
			<div className="flex flex-wrap items-center gap-2">
				<Badge variant="warning">{t("shell.workflowsWaitingForCapacity")}</Badge>
				{run.waitReason && <span className="font-medium">{reasonLabel(t, run.waitReason)}</span>}
				{Boolean(attempt) && (
					<span className="text-warning/80">{t("shell.workflowsWakeAttempt", { count: attempt })}</span>
				)}
			</div>
			{wait && (
				<p className="text-warning/90">
					{capacityReasonLabel(t, wait.reason)}
					{wait.independenceRequired && <> · {t("shell.workflowsIndependenceRequired")}</>}
				</p>
			)}
			<p className="text-warning/90">{t("shell.workflowsNextWake", { when: timestampLabel(nextAttempt) })}</p>
			{wait?.knownResetAt && (
				<p className="text-warning/90">{t("shell.workflowsKnownReset", { when: timestampLabel(wait.knownResetAt) })}</p>
			)}
			{wait?.providers?.map((provider) => (
				<p className="text-xs text-warning/80" key={provider.profileId}>
					{provider.displayName || provider.harness || provider.profileId}
					{" · "}
					{provider.capacity}
					{provider.healthAgeSeconds
						? ` · ${t("shell.workflowsProviderHealthAge", { age: formatDurationCompact(provider.healthAgeSeconds) })}`
						: ""}
					{provider.healthReason ? ` · ${provider.healthReason}` : ""}
					{provider.probeEligible ? ` · ${t("shell.workflowsProviderProbing")}` : ""}
				</p>
			))}
		</div>
	);
}
