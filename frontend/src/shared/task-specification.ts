/**
 * task-specification.ts — one definition of "how long a task specification may
 * be" for every surface in the renderer.
 *
 * A Task's specification and a workflow's objective are the same thing — what
 * the work IS — and the daemon bounds them with one constant
 * (domain.MaxWorkflowObjectiveBytes). The renderer used to know that number in
 * one place only, the workflow create form, while the new-task composer knew a
 * different, much smaller one by way of the daemon's old 4096-byte prompt cap.
 * That disagreement is the whole bug: a specification the workflow form
 * accepted came back from the task form as TASK_TOO_LONG.
 *
 * So the value lives here, imported by both, and is asserted against the
 * generated OpenAPI schema in the tests. The check it drives is a courtesy —
 * the browser tells you before you submit and the daemon is the authority that
 * refuses — but a UI that accepted more than the daemon does would produce a
 * refusal the author could not have predicted.
 */

/** Mirrors domain.MaxWorkflowObjectiveBytes: 128 KiB of UTF-8. */
export const MAX_TASK_SPECIFICATION_BYTES = 131072;

/**
 * Below this the counter stays hidden. A specification only becomes something
 * you have to budget once it is genuinely long; "12 / 131,072 bytes" next to a
 * one-line task is noise about a limit nobody is near.
 */
export const SPECIFICATION_COUNTER_FROM_BYTES = 2000;

/**
 * Bytes, not characters: the limit is in UTF-8 bytes and so is this. A limit
 * counted in characters means something different for a Spanish specification
 * than for an English one, and the resources it protects — the request body,
 * the column, the provider payload — are all counted in bytes.
 */
export function specificationByteLength(value: string): number {
	return new TextEncoder().encode(value.trim()).length;
}

/** How many bytes past the ceiling, for a message that says how much to cut. */
export function specificationOverageBytes(value: string): number {
	return Math.max(0, specificationByteLength(value) - MAX_TASK_SPECIFICATION_BYTES);
}

/**
 * The name the staged file carries in the composer's attachment list.
 *
 * It is a DISPLAY name only. The upload carries a mime type and bytes and no
 * name, so the daemon names the file itself under .ao/attachments and appends
 * that real, worktree-relative path to the prompt. Nothing shown to the agent
 * should quote this constant as a path.
 */
export const SPECIFICATION_ATTACHMENT_NAME = "task-specification.md";
