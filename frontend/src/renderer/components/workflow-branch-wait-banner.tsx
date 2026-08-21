import { useTranslation } from "react-i18next";
import type { components } from "../../api/schema";
import { Badge } from "./ui/badge";

export type WorkflowRunView = components["schemas"]["WorkflowRunView"];

/**
 * WorkflowBranchWaitBanner surfaces Checkpoint 8P-E.11's waiting_for_branch
 * state: a direct-branch run that is ready to work but whose repository+branch
 * is currently owned by another workflow.
 *
 * Every value here comes straight off the backend's structured branchWait —
 * the branch and the owning workflow are facts recorded when the lock conflict
 * happened, not strings parsed out of a message. The banner renders only when
 * that state is genuinely present, so a run is never shown as "inactive" when
 * it is really queued, and never shown as queued when it is not.
 */
export function WorkflowBranchWaitBanner({ run }: { run: WorkflowRunView }) {
	const { t } = useTranslation();
	const wait = run.branchWait;
	if (!wait) return null;

	return (
		<div className="flex flex-col gap-2 rounded-lg border border-warning/40 bg-warning/10 p-3 text-sm text-warning">
			<div className="flex flex-wrap items-center gap-2">
				<Badge variant="warning">{t("shell.workflowsWaitingForBranch")}</Badge>
				<span className="font-mono font-medium">{wait.branch}</span>
			</div>
			{wait.heldByWorkflowRunId && (
				<p className="text-warning/90">
					{t("shell.workflowsBranchHeldBy", { workflowId: wait.heldByWorkflowRunId })}
				</p>
			)}
			{wait.repoPath && (
				<p className="truncate text-xs text-warning/80" title={wait.repoPath}>
					{wait.repoPath}
				</p>
			)}
			{/*
			 * Checkpoint 8P-E.13A: the holder's own state decides which note is
			 * true. A wait that clears by itself gets the reassuring one; a
			 * branch held by a workflow that has stopped for a human decision
			 * gets told so, because nothing about this run will change until
			 * that other workflow is continued or cancelled.
			 */}
			{wait.heldByReason && <p className="text-xs text-warning/90">{wait.heldByReason}</p>}
			<p className="text-xs text-warning/80">
				{wait.autoResume === false && wait.heldByReason
					? t("board.branchWaitBlocked")
					: t("shell.workflowsBranchWaitNote")}
			</p>
		</div>
	);
}
