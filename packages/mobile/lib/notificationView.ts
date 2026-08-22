// Presentation rules for the notification history list. Pure — no React Native
// or Expo imports — so the wording and the routing decision are unit-testable,
// the same split as pushStatus.ts / push.ts.
import type { Theme } from "./theme";

export type NotificationVisual = {
	icon: "message-circle" | "git-merge" | "git-pull-request" | "x-circle" | "bell";
	color: string;
	label: string;
};

/** Icon, colour and short label for one notification type. */
export function notificationVisual(t: Theme, type: string): NotificationVisual {
	switch (type) {
		case "needs_input":
			return { icon: "message-circle", color: t.amber, label: "Needs input" };
		case "ready_to_merge":
			return { icon: "git-merge", color: t.green, label: "Ready to merge" };
		case "pr_merged":
			return { icon: "git-merge", color: t.blue, label: "Merged" };
		case "pr_closed_unmerged":
			return { icon: "x-circle", color: t.red, label: "Closed" };
		// The run-level families. `task_completed`/`workflow_completed` were
		// falling through to the default and rendering their raw type string as
		// the label; naming them here alongside the two new ones keeps one switch
		// that is right rather than one that is half right.
		case "task_completed":
			return { icon: "bell", color: t.green, label: "Task finished" };
		case "workflow_completed":
			return { icon: "bell", color: t.green, label: "Workflow finished" };
		case "task_needs_attention":
			return { icon: "bell", color: t.amber, label: "Task needs attention" };
		case "workflow_needs_attention":
			return { icon: "bell", color: t.amber, label: "Workflow needs attention" };
		case "task_failed":
			return { icon: "x-circle", color: t.red, label: "Task failed" };
		case "workflow_failed":
			return { icon: "x-circle", color: t.red, label: "Workflow failed" };
		default:
			return { icon: "bell", color: t.textTertiary, label: type || "Notification" };
	}
}

/**
 * Where tapping a notification should land. Mirrors the routing PushManager
 * already applies to a notification tap, so opening an item from history and
 * opening it from the tray agree — the rule lives here rather than being written
 * twice.
 */
export function notificationTarget(n: { type: string; sessionId?: string }): string {
	return n.type === "needs_input" && n.sessionId ? `/session/${n.sessionId}` : "/prs";
}

/** Compact "3m" / "4h" / "2d" stamp. Returns "" for an unparseable timestamp. */
export function relativeTime(iso: string, now: number = Date.now()): string {
	const then = Date.parse(iso);
	if (Number.isNaN(then)) return "";
	const secs = Math.max(0, Math.round((now - then) / 1000));
	if (secs < 60) return "now";
	const mins = Math.floor(secs / 60);
	if (mins < 60) return `${mins}m`;
	const hours = Math.floor(mins / 60);
	if (hours < 24) return `${hours}h`;
	const days = Math.floor(hours / 24);
	if (days < 7) return `${days}d`;
	return `${Math.floor(days / 7)}w`;
}
