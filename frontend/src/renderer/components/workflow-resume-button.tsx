import { Link } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useContinueWorkflowRun } from "../hooks/useWorkflowRun";
import { apiErrorMessage } from "../lib/api-client";

/**
 * WorkflowResumeButton is the "Reanudar" control for a run stopped on something
 * a person has now resolved.
 *
 * Two things about it are load-bearing:
 *
 *  1. It is rendered only when the backend says `canContinue`. Whether a stop is
 *     recoverable is provider/lifecycle policy, and the one place that policy is
 *     allowed to live is the backend (workflow.canContinueRun). A second copy of
 *     the rule here would drift from it the first time a reason is added.
 *
 *  2. When the stop is a MIRROR of a child run's — an objective reporting
 *     "child_needs_attention" — the POST goes to that child, not to the
 *     objective. Continuing the parent does nothing for a task that is the thing
 *     actually stopped, and a link is offered alongside so the user can go read
 *     it first. `attentionWorkflowId` is empty for a stop the run owns itself,
 *     in which case this acts on the run in front of the user, as expected.
 *
 * Duplicate submits are impossible by construction: the button is disabled while
 * either mutation is pending, and the handler additionally refuses to fire a
 * second call while one is in flight (a keyboard repeat can outrun a re-render).
 */
export function WorkflowResumeButton({
	attentionWorkflowId,
	continueError,
	continueRun,
	continuing,
}: {
	attentionWorkflowId?: string;
	continueError?: string;
	continueRun: () => Promise<unknown>;
	continuing: boolean;
}) {
	const { t } = useTranslation();
	const child = useContinueWorkflowRun(attentionWorkflowId);
	const targetsChild = Boolean(attentionWorkflowId);
	const pending = targetsChild ? child.isPending : continuing;
	const error = targetsChild ? (child.error ? apiErrorMessage(child.error) : undefined) : continueError;

	const submit = () => {
		if (pending) return;
		if (targetsChild) {
			// mutate, not mutateAsync: the failure is already rendered from the
			// mutation's own error state, and an unawaited mutateAsync would also
			// reject into an unhandled rejection.
			child.mutate();
			return;
		}
		continueRun().catch(() => {
			// Surfaced through `continueError` by the owning hook.
		});
	};

	return (
		<div>
			<button
				className="rounded border border-primary bg-primary px-3 py-1.5 text-sm text-primary-foreground disabled:opacity-50"
				disabled={pending}
				onClick={submit}
				type="button"
			>
				{pending ? t("shell.workflowsResuming") : t("shell.workflowsResume")}
			</button>
			{targetsChild && attentionWorkflowId && (
				<Link
					className="ml-2 text-sm underline"
					params={{ workflowId: attentionWorkflowId }}
					to="/workflows/$workflowId"
				>
					{t("shell.workflowsOpenBlockedTask")}
				</Link>
			)}
			{error && <p className="mt-1 text-sm text-destructive">{error}</p>}
		</div>
	);
}
