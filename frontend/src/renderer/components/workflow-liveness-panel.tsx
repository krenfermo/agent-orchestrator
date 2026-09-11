import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { components } from "../../api/schema";

/**
 * workflow-liveness-panel.tsx — the two clocks, side by side.
 *
 * THE BUG THIS ENDS. A run's card said "last activity 12 minutes ago" while its
 * worker was making six model calls a minute. Nothing was lying: the number
 * shown was the workflow's own last durable act, and a work step in progress
 * writes no checkpoints, so it stands still for the whole of a twenty-minute
 * turn by design. The session's own `activity_last_at` was no better — it is a
 * TRANSITION clock, frozen at the instant the worker went active.
 *
 * So this panel renders the third clock, and it renders BOTH:
 *
 *   Last signal:     3s ago      <- is it alive
 *   Last transition: 20m ago     <- how long has it been doing this
 *
 * THE RULE THIS FILE ENFORCES: no surface may say "silent", "stuck" or
 * "inactive" from the transition clock. Only `silentForSeconds` — derived by
 * the daemon from `lastSignalAt`, so every surface agrees — may make that
 * claim, and it only makes it past a threshold generous enough that a worker
 * legitimately mid-test-suite never trips it.
 */

type LivenessResponse = components["schemas"]["ControllersWorkerLivenessResponse"];

/**
 * quietThresholdSeconds is when this panel starts saying a worker has gone
 * quiet. Deliberately generous: an agent running a test suite or a long build
 * is silent for minutes at a time and is perfectly healthy, and warning about
 * it is exactly the misreport this panel exists to end. A genuinely dead worker
 * is the recovery path's business — that path is untouched and is not gated on
 * this number.
 */
const quietThresholdSeconds = 600;

function agoText(t: TFunction, seconds: number | null | undefined): string {
	if (seconds === null || seconds === undefined) return t("shell.liveness.unknown");
	if (seconds < 60) return t("shell.liveness.secondsAgo", { value: Math.max(0, Math.round(seconds)) });
	if (seconds < 3600) return t("shell.liveness.minutesAgo", { value: Math.round(seconds / 60) });
	return t("shell.liveness.hoursAgo", { value: (seconds / 3600).toFixed(1) });
}

/** secondsSince is how long ago an ISO instant was, or null when unparseable. */
function secondsSince(iso: string | undefined, now: number): number | null {
	if (!iso) return null;
	const at = Date.parse(iso);
	if (Number.isNaN(at)) return null;
	return (now - at) / 1000;
}

/**
 * WorkflowLivenessPanel renders the running agent's clocks, or nothing.
 *
 * Nothing, when there is no running agent: a completed run has no liveness, and
 * rendering "heard from 0s ago" under it would be the same class of misreport
 * inverted.
 */
export function WorkflowLivenessPanel({ liveness }: { liveness: LivenessResponse | undefined }) {
	const { t } = useTranslation();
	if (!liveness || !liveness.observed) return null;

	const now = Date.now();
	// Prefer the daemon's own figure so every surface agrees; fall back to the
	// timestamp only if the daemon could not compute one.
	const silent = liveness.silentForSeconds ?? secondsSince(liveness.lastSignalAt, now);
	const sinceTransition = secondsSince(liveness.lastTransitionAt, now);
	const quiet = silent !== null && silent >= quietThresholdSeconds;

	return (
		<div className="rounded-lg border border-border p-3 text-xs" data-testid="worker-liveness">
			<div className="flex items-baseline justify-between">
				<h3 className="font-medium">{t("shell.liveness.title")}</h3>
				<span
					className={quiet ? "text-warning" : "text-status-working"}
					data-testid="worker-liveness-headline"
				>
					{quiet ? t("shell.liveness.quiet") : t("shell.liveness.working")}
				</span>
			</div>
			<dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 text-muted-foreground">
				{/* The liveness clock FIRST, because it is the one that answers
				    the question a person opened this panel with. */}
				<dt>{t("shell.liveness.lastSignal")}</dt>
				<dd data-testid="worker-last-signal">{agoText(t, silent)}</dd>
				<dt>{t("shell.liveness.lastTransition")}</dt>
				<dd data-testid="worker-last-transition">{agoText(t, sinceTransition)}</dd>
			</dl>
			{/* Said in words, because the whole failure was somebody reading one
			    clock as the other. */}
			<p className="mt-1 text-muted-foreground">{t("shell.liveness.explainer")}</p>
		</div>
	);
}
