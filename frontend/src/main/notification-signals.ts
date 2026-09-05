/**
 * Notification signal policy for the main process.
 *
 * The renderer forwards every `notification_created` event to `notifications:show`,
 * so the type reaching main is always one defined by
 * `backend/internal/domain/notification.go`. These helpers keep the decision of
 * "toast vs. active attention signal" in one typed, testable place rather than as
 * hardcoded string literals scattered through the IPC handler.
 */

/** The notification types defined by `backend/internal/domain/notification.go`. */
export type NotificationType =
	| "needs_input"
	| "ready_to_merge"
	| "pr_merged"
	| "pr_closed_unmerged"
	| "task_completed"
	| "workflow_completed"
	| "task_needs_attention"
	| "workflow_needs_attention"
	| "task_failed"
	| "workflow_failed"
	| "human_question_required"
	| "repair_exhausted"
	| "integration_failed";

/**
 * Types that warrant an *active* attention signal (macOS dock bounce / Windows &
 * Linux taskbar flash). A merged or closed PR is worth a toast, but should not
 * demand attention as insistently as an agent blocked waiting on the user.
 *
 * Completions sit on the toast side of that line for the same reason. Finished
 * work is news, and the toast delivers it; nothing is waiting on the user, so
 * bouncing the dock would be an interruption with nothing to act on — and a
 * fleet of tasks finishing would make it a constant one.
 */
const ATTENTION_TYPES: ReadonlySet<string> = new Set<NotificationType>([
	"needs_input",
	"ready_to_merge",
	// A run that stopped on a decision, or ended without doing the work, is
	// waiting on the user in exactly the way a blocked agent is: nothing moves
	// again until they look. That is the whole test this set applies.
	"task_needs_attention",
	"workflow_needs_attention",
	"task_failed",
	"workflow_failed",
	// All three session-scoped facts pass the same test: AO has no autonomous
	// move left, so nothing moves again until the user looks. A pending
	// permission prompt is the most literal case of it.
	"human_question_required",
	"repair_exhausted",
	"integration_failed",
]);

/** Whether this notification type should bounce the dock / flash the taskbar. */
export function shouldSignalAttention(type: string | undefined): boolean {
	return type !== undefined && ATTENTION_TYPES.has(type);
}

/**
 * Whether to fire an OS toast. Deliberately independent of the type list: every
 * backend notification type gets a toast, so adding a new type in
 * `notification.go` can never silently drop its toast (the bug this replaced).
 */
export function shouldToast(notification: { title?: string }, isSupported: boolean): boolean {
	return Boolean(notification.title) && isSupported;
}
