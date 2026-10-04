import type { components } from "../../api/schema";

/**
 * workflow-presentation.ts — P3-A's renderer side of the human status model.
 *
 * Every value the UI shows about a run's status comes from the daemon's
 * `presentation` projection. This module holds the ONE thing the renderer is
 * allowed to own about it: the mapping from the backend's stable codes to
 * translation keys and visual tone. It contains no Git logic, no lifecycle
 * derivation and no fallbacks that invent a status the daemon did not report —
 * the frontend renders backend semantics.
 *
 * The rule for every lookup below is the same: a code the renderer has no copy
 * for falls back to something that still names the real thing (the code itself,
 * or the daemon's own English sentence), never to a blank or a guess. A
 * vocabulary the daemon grows before the renderer does must degrade to "less
 * polished", never to "wrong" or "empty".
 */

export type WorkflowPresentation = components["schemas"]["WorkflowPresentationView"];
export type PresentationStage = WorkflowPresentation["stage"];
export type PresentationAction = components["schemas"]["WorkflowPresentationAction"];
export type PresentationActionId = PresentationAction["id"];
export type PresentationProgressStage = components["schemas"]["WorkflowPresentationStage"];
export type PresentationPlacement = components["schemas"]["WorkflowPresentationPlacement"];
export type PresentationEvent = components["schemas"]["WorkflowPresentationEvent"];

/** The stages in which AO is genuinely executing something right now. */
const ACTIVE_STAGES: ReadonlySet<string> = new Set([
	"planning",
	"working",
	"reviewing",
	"correcting",
	"verifying",
	"integrating",
]);

export function isActiveStage(stage: string | undefined): boolean {
	return ACTIVE_STAGES.has(stage ?? "");
}

/**
 * Semantic tone per stage, reusing the status tokens the design system already
 * defines. No new colors, and — §29 — tone is never the only signal: every
 * stage also carries a label, and every progress entry also carries a state.
 */
export type StageTone = "working" | "review" | "attention" | "failed" | "done" | "idle";

export function stageTone(stage: string): StageTone {
	switch (stage) {
		case "reviewing":
			return "review";
		case "planning":
		case "working":
		case "correcting":
		case "verifying":
		case "integrating":
			return "working";
		case "needs_attention":
			return "attention";
		case "failed":
			return "failed";
		case "completed":
			return "done";
		default:
			return "idle";
	}
}

/** Translation key for a stage's short label ("Trabajando", "Revisando"). */
export function stageLabelKey(stage: string): string {
	return `wf.stage.${stage}`;
}

/**
 * Translation key for the sentence a person reads first.
 *
 * The daemon's summaryCode is either a canonical stop/wait reason or the stage
 * itself. Both live under one namespace so the lookup is a single miss-tolerant
 * call, and `wf.summary.unknown` is the honest last resort: it says AO stopped
 * and points at the technical detail rather than pretending to explain.
 */
export function summaryKey(code: string): string {
	return `wf.summary.${code}`;
}

/** Translation key for an offered action's button label. */
export function actionLabelKey(id: string): string {
	return `wf.action.${id}`;
}

/** Translation key for why an offered action is unavailable. */
export function disabledReasonKey(code: string): string {
	return `wf.disabled.${code}`;
}

/** Translation key for one timeline entry's line. */
export function timelineKey(kind: string): string {
	return `wf.timeline.${kind}`;
}

/** Translation key for a placement's human name ("Branch actual"). */
export function placementLabelKey(type: string): string {
	return `wf.placement.${type}`;
}

/**
 * The one-line answer to "¿qué hago?" (§16).
 *
 * It is derived from two backend flags and nothing else, in the order that
 * makes AO's own activity outrank a stop it is already handling: a repair in
 * flight is not the user's turn however the run row reads.
 */
export function guidanceKey(presentation: Pick<WorkflowPresentation, "requiresHuman" | "automaticActionActive" | "stage">): string {
	if (presentation.automaticActionActive && !presentation.requiresHuman) return "wf.guidance.automatic";
	if (presentation.requiresHuman) return "wf.guidance.human";
	if (presentation.stage === "waiting") return "wf.guidance.waiting";
	if (presentation.stage === "completed") return "wf.guidance.completed";
	if (isActiveStage(presentation.stage)) return "wf.guidance.none";
	return "wf.guidance.none";
}

/**
 * The actions a person may press, in a stable order.
 *
 * Primary first, then the rest as the daemon listed them. Disabled entries are
 * kept: "why is this greyed out" is answerable and "where did the button go" is
 * not, which is the whole reason the daemon sends a reason with them.
 */
export function orderedActions(actions: readonly PresentationAction[] | undefined): PresentationAction[] {
	if (!actions) return [];
	return [...actions].sort((a, b) => Number(Boolean(b.primary)) - Number(Boolean(a.primary)));
}
