import type { AppShortcutId, ShortcutCategory } from "../../shared/shortcuts";
import type { components } from "../../api/schema";
import type { MessageKey } from "./messages";

/** Exhaustive mappings keep dynamic domain identifiers inside the typed catalog. */
export const shortcutLabelKeys: Record<AppShortcutId, MessageKey> = {
	"new-session": "shortcut.new-session",
	"new-shell-terminal": "shortcut.new-shell-terminal",
	"close-shell-terminal": "shortcut.close-shell-terminal",
	"keyboard-shortcuts": "shortcut.keyboard-shortcuts",
	"command-palette": "shortcut.command-palette",
	"open-settings": "shortcut.open-settings",
	"toggle-sidebar": "shortcut.toggle-sidebar",
	"open-project": "shortcut.open-project",
	"previous-session": "shortcut.previous-session",
	"next-session": "shortcut.next-session",
	"previous-tab": "shortcut.previous-tab",
	"next-tab": "shortcut.next-tab",
	"toggle-inspector": "shortcut.toggle-inspector",
	"focus-terminal": "shortcut.focus-terminal",
	"toggle-browser-devtools": "titlebar.devtools",
};

export const shortcutCategoryLabelKeys: Record<ShortcutCategory, MessageKey> = {
	General: "shortcut.category.general",
	Navigation: "shortcut.category.navigation",
	Session: "shortcut.category.session",
};

export type AgentSwitchErrorCode = NonNullable<components["schemas"]["AgentSwitch"]["errorCode"]>;

/** Exhaustive known-code mapping with a separate runtime fallback for newer daemons. */
export const agentSwitchErrorLabelKeys: Record<AgentSwitchErrorCode, MessageKey> = {
	daemon_restart_pre_stop: "switchAgent.error.daemonRestartPreStop",
	daemon_restart_post_stop: "switchAgent.error.daemonRestartPostStop",
	daemon_restart_unrecoverable_target: "switchAgent.error.daemonRestartUnrecoverableTarget",
	daemon_restart_before_delivery: "switchAgent.error.daemonRestartBeforeDelivery",
	delivery_unconfirmed: "switchAgent.error.deliveryUnconfirmed",
	source_session_terminated: "switchAgent.error.sourceSessionTerminated",
	source_stop_unconfirmed: "switchAgent.error.sourceStopUnconfirmed",
	target_binary_missing: "switchAgent.error.targetBinaryMissing",
	target_agent_unauthorized: "switchAgent.error.targetAgentUnauthorized",
	request_cancelled: "switchAgent.error.requestCancelled",
	source_blocked: "switchAgent.error.sourceBlocked",
	failed_pre_stop: "switchAgent.error.failedPreStop",
	failed_post_stop: "switchAgent.error.failedPostStop",
	target_ready_failed: "switchAgent.error.targetReadyFailed",
	delivery_failed: "switchAgent.error.deliveryFailed",
	switch_failed: "switchAgent.error.switchFailed",
	target_start_unconfirmed: "switchAgent.error.targetStartUnconfirmed",
};

/**
 * Workflow attention copy, keyed by the backend's stable attention reason.
 *
 * The daemon composes its own English sentence for a stop (workflow.attention's
 * HumanAction, and the checkpoint's next action) and the UI used to print that
 * string verbatim — so a Spanish install read "The worker reported its turn
 * finished and left no change in its workspace." The reason CODE beside it is
 * stable, typed vocabulary; the sentence is not. So the code selects the copy
 * and the locale decides its language.
 *
 * Only the codes a person is actually asked to act on are mapped. Anything
 * absent falls back to the daemon's own sentence, which is why an older UI
 * against a newer daemon loses nothing: it shows the English the daemon sent
 * rather than a missing-key placeholder. The reason code itself is NEVER
 * translated — it stays beside the copy for diagnosis.
 */
export const attentionActionLabelKeys: Record<string, MessageKey> = {
	worker_turn_produced_nothing: "attention.action.workerTurnProducedNothing",
	worker_workspace_unreadable: "attention.action.workerWorkspaceUnreadable",
	worker_dispatch_ambiguous: "attention.action.workerDispatchAmbiguous",
	worker_credential_unadoptable: "attention.action.workerCredentialUnadoptable",
	ambiguous_worker_state: "attention.action.ambiguousWorkerState",
	worker_terminated_unexpectedly: "attention.action.workerTerminatedUnexpectedly",
};

/** The observation sentence for the same stops, shown as the run's next action. */
export const attentionNextActionLabelKeys: Record<string, MessageKey> = {
	worker_turn_produced_nothing: "attention.next.workerTurnProducedNothing",
	worker_workspace_unreadable: "attention.next.workerWorkspaceUnreadable",
	worker_dispatch_ambiguous: "attention.next.workerDispatchAmbiguous",
	worker_credential_unadoptable: "attention.next.workerCredentialUnadoptable",
	ambiguous_worker_state: "attention.next.ambiguousWorkerState",
	worker_terminated_unexpectedly: "attention.next.workerTerminatedUnexpectedly",
};

/**
 * The translated sentence for a stop, or undefined when the daemon's own string
 * is the only thing available. Callers render `key ? t(key) : backendSentence`.
 */
export function attentionActionMessageKey(reason: string | undefined): MessageKey | undefined {
	return reason ? attentionActionLabelKeys[reason] : undefined;
}

export function attentionNextActionMessageKey(reason: string | undefined): MessageKey | undefined {
	return reason ? attentionNextActionLabelKeys[reason] : undefined;
}
