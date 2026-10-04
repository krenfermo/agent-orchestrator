import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useWorkflowPendingChanges } from "../hooks/useWorkflowPendingChanges";
import { Textarea } from "./ui/textarea";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogTitle,
	settingsDialogBodyClass,
	settingsDialogContentClass,
	settingsDialogFooterClass,
	settingsDialogHeaderClass,
} from "./ui/dialog";
import { Button } from "./ui/button";

/**
 * WorkflowCommitDialog — P3-A §17's "Hacer commit y continuar".
 *
 * The order on screen is the order the daemon performs, and it is the whole
 * point of the dialog: SHOW the files, PROPOSE a message the user edits, then
 * commit, then let the daemon re-probe and resume. Nothing is stashed, nothing
 * is committed under a message nobody read, and the run is not reported as
 * resumed unless the daemon says it resumed it.
 *
 * The file list is not decoration. A person about to put their uncommitted work
 * into a commit is entitled to see exactly what is going in before they agree
 * to it, and a flow that skipped this step would be the silent commit this
 * checkpoint exists to replace.
 */
export function WorkflowCommitDialog({
	workflowId,
	open,
	onOpenChange,
}: {
	workflowId: string;
	open: boolean;
	onOpenChange: (open: boolean) => void;
}) {
	const { t } = useTranslation();
	const { pending, isLoading, error, commit, committing, commitError, commitOutcome, reset } =
		useWorkflowPendingChanges(workflowId, open);
	const [message, setMessage] = useState("");
	const [touched, setTouched] = useState(false);

	// The daemon's proposal is a starting point, and it stops being one the
	// moment the user types: re-seeding over their edit would discard it every
	// time the query refetched.
	useEffect(() => {
		if (!touched && pending?.proposedMessage) setMessage(pending.proposedMessage);
	}, [pending?.proposedMessage, touched]);

	useEffect(() => {
		if (!open) {
			setTouched(false);
			setMessage("");
			reset();
		}
	}, [open, reset]);

	// The daemon resumed the run: the dialog's job is done and the page behind
	// it now shows a run that is moving again.
	useEffect(() => {
		if (commitOutcome?.resumed) onOpenChange(false);
	}, [commitOutcome?.resumed, onOpenChange]);

	const changes = pending?.changes ?? [];
	const canCommit = Boolean(pending?.available) && message.trim() !== "" && !committing;

	return (
		<Dialog open={open} onOpenChange={onOpenChange}>
			<DialogContent className={settingsDialogContentClass}>
				<div className={settingsDialogHeaderClass}>
					<DialogTitle>{t("wf.commit.title")}</DialogTitle>
					<DialogDescription>{t("wf.commit.description")}</DialogDescription>
				</div>
				<div className={settingsDialogBodyClass}>
					{isLoading ? <p className="text-sm text-muted-foreground">{t("wf.commit.loading")}</p> : null}
					{error ? <p className="text-sm text-destructive">{error}</p> : null}
					{pending && !pending.available ? (
						<p className="text-sm text-destructive" data-testid="workflow-commit-unavailable">
							{t("wf.commit.unavailable")}
							{pending.unavailable ? <span className="block text-xs">{pending.unavailable}</span> : null}
						</p>
					) : null}
					{pending?.available && changes.length === 0 ? (
						<p className="text-sm text-muted-foreground">{t("wf.commit.noChanges")}</p>
					) : null}
					{changes.length > 0 ? (
						<section className="flex flex-col gap-1">
							<h3 className="text-sm font-medium">{t("wf.commit.filesLabel")}</h3>
							<ul className="max-h-48 overflow-y-auto rounded border border-border p-2" data-testid="workflow-commit-files">
								{changes.map((change) => (
									<li className="flex gap-2 font-mono text-xs" key={change.path}>
										<span className="shrink-0 text-muted-foreground">{change.status}</span>
										<span className="truncate">{change.path}</span>
									</li>
								))}
							</ul>
						</section>
					) : null}
					<label className="flex flex-col gap-1 text-sm">
						{t("wf.commit.messageLabel")}
						<Textarea
							data-testid="workflow-commit-message"
							onChange={(event) => {
								setTouched(true);
								setMessage(event.target.value);
							}}
							rows={3}
							value={message}
						/>
					</label>
					{message.trim() === "" ? (
						<p className="text-xs text-muted-foreground">{t("wf.commit.messageRequired")}</p>
					) : null}
					{commitError ? <p className="text-sm text-destructive">{commitError}</p> : null}
					{/*
					 * A commit that happened without a resume is reported as exactly
					 * that. Saying "done" over it would send the user away believing
					 * the run is moving when the daemon deliberately left it stopped.
					 */}
					{commitOutcome && !commitOutcome.resumed ? (
						<p className="text-sm text-warning" data-testid="workflow-commit-not-resumed">
							{t("wf.commit.notResumed")}
							{commitOutcome.detail ? <span className="block text-xs">{commitOutcome.detail}</span> : null}
						</p>
					) : null}
				</div>
				<div className={settingsDialogFooterClass}>
					<Button onClick={() => onOpenChange(false)} type="button" variant="outline">
						{t("wf.action.cancel")}
					</Button>
					<Button
						data-testid="workflow-commit-confirm"
						disabled={!canCommit}
						onClick={() => void commit(message.trim()).catch(() => undefined)}
						type="button"
					>
						{committing ? t("wf.commit.busy") : t("wf.commit.confirm")}
					</Button>
				</div>
			</DialogContent>
		</Dialog>
	);
}
