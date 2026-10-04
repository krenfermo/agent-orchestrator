import { formatTimeCompact as formatPortableTimeCompact } from "@aoagents/product-ui";
import { appI18n, type MessageKey } from "../i18n";

export function formatTimeCompact(isoDate: string | null | undefined): string {
	return formatPortableTimeCompact(isoDate, {
		translate: (key, values) => appI18n.t(key as MessageKey, values),
	});
}

/**
 * How long a run has been going, as a compact unit string ("42s", "7m",
 * "2h 05m", "3d 04h").
 *
 * Deliberately unit-only rather than a translated sentence: it sits next to a
 * translated label in a definition list, and h/m/s read the same in every
 * locale the app ships. Returns undefined — never "0s" — when the start time is
 * missing or unparseable, so an unknown stays unknown instead of becoming a
 * number nobody observed.
 */
export function formatElapsedCompact(startedAt: string | null | undefined, now: number = Date.now()): string | undefined {
	if (!startedAt) return undefined;
	const started = new Date(startedAt).getTime();
	if (!Number.isFinite(started)) return undefined;
	return formatDurationCompact(Math.floor((now - started) / 1000));
}

/**
 * The same compact unit string for a duration the backend already measured, in
 * seconds. Used for provider-health age, which the daemon computes against its
 * own clock so the renderer never has to guess whether the two agree.
 */
export function formatDurationCompact(totalSeconds: number): string {
	const seconds = Math.max(0, Math.floor(totalSeconds));
	if (seconds < 60) return `${seconds}s`;
	const minutes = Math.floor(seconds / 60);
	if (minutes < 60) return `${minutes}m`;
	const hours = Math.floor(minutes / 60);
	if (hours < 24) return `${hours}h ${String(minutes % 60).padStart(2, "0")}m`;
	return `${Math.floor(hours / 24)}d ${String(hours % 24).padStart(2, "0")}h`;
}
