import { useState } from "react";
import { useTranslation } from "react-i18next";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogHeader,
	DialogTitle,
} from "./ui/dialog";
import type { IncidentActionView, IncidentDiagnosisView, IncidentView } from "../hooks/useWorkflowIncident";
import { useWorkflowIncident } from "../hooks/useWorkflowIncident";
import { apiErrorMessage } from "../lib/api-client";

/**
 * WorkflowIncidentDialog is the "¿Qué hago?" modal for a run stopped on
 * something a person has to deal with.
 *
 * It answers six questions in a fixed order, because that is the order someone
 * staring at a stopped run actually asks them: what happened, what is frozen,
 * why AO stopped, what evidence it has, what the risk is, and what to do.
 *
 * It renders decisions; it does not make them. Whether an action needs an
 * approval, whether it is executable at all, what it will do and how risky it
 * is are fields on the response — the backend's authorization policy is the
 * single source, and re-deriving any of it here would eventually disagree with
 * the executor. A control that looks safe and is not is a worse bug than one
 * that errors.
 *
 * The approval checkbox is deliberately not a second confirm dialog. The
 * consequence is spelled out next to the box, in AO's own words rather than the
 * proposing agent's, and ticking it is what turns the button on.
 */
export function WorkflowIncidentDialog({
	workflowId,
	open,
	onOpenChange,
}: {
	workflowId: string;
	open: boolean;
	onOpenChange: (open: boolean) => void;
}) {
	const { t } = useTranslation();
	const { query, diagnose, execute } = useWorkflowIncident(workflowId, open);
	const [approved, setApproved] = useState(false);

	const incident = query.data;
	const error =
		(query.error && apiErrorMessage(query.error)) ||
		(diagnose.error && apiErrorMessage(diagnose.error)) ||
		(execute.error && apiErrorMessage(execute.error)) ||
		undefined;

	return (
		<Dialog onOpenChange={onOpenChange} open={open}>
			<DialogContent className="max-h-[85vh] max-w-2xl overflow-y-auto">
				<DialogHeader>
					<DialogTitle>{t("incident.title")}</DialogTitle>
					<DialogDescription>
						{incident?.stopReason ? t("incident.stoppedBecause", { reason: incident.stopReason }) : t("incident.loading")}
					</DialogDescription>
				</DialogHeader>

				{error && <p className="text-sm text-destructive">{error}</p>}

				{incident?.stale && (
					<p className="rounded border border-amber-500/40 bg-amber-500/10 p-2 text-sm">
						{t("incident.stale")}
					</p>
				)}

				{incident && !incident.diagnosis && (
					<UndiagnosedBody
						incident={incident}
						onDiagnose={() => diagnose.mutate()}
						pending={diagnose.isPending || incident.state === "diagnosing"}
					/>
				)}

				{incident?.diagnosis && (
					<DiagnosedBody
						approved={approved}
						incident={incident}
						onApprovedChange={setApproved}
						onExecute={(approve) => execute.mutate({ incidentId: incident.id, approve })}
						pending={execute.isPending}
					/>
				)}
			</DialogContent>
		</Dialog>
	);
}

/** UndiagnosedBody is what AO can say before anyone spends an agent on it. */
function UndiagnosedBody({
	incident,
	onDiagnose,
	pending,
}: {
	incident: IncidentView;
	onDiagnose: () => void;
	pending: boolean;
}) {
	const { t } = useTranslation();
	return (
		<div className="space-y-4 text-sm">
			<Section title={t("incident.whatHappened")}>{incident.stopDetail || incident.stopReason}</Section>

			{incident.contextPack && (
				<Section title={t("incident.evidence")}>
					<ul className="list-disc pl-5">
						{incident.contextPack.sections?.map((s: string) => (
							<li key={s}>{s}</li>
						))}
					</ul>
					{/* The budget is shown, not hidden: an operator deciding whether to
					    spend an agent deserves to know what it will be given, and a
					    diagnosis that says "insufficient evidence" is only meaningful
					    if you can see what was withheld. */}
					<p className="mt-2 text-muted-foreground">
						{t("incident.packBudget", {
							bytes: incident.contextPack.bytes,
							tokens: incident.contextPack.estimatedTokens,
							max: incident.contextPack.maxBytes,
						})}
					</p>
					{incident.contextPack.droppedSections?.length ? (
						<p className="text-muted-foreground">
							{t("incident.packDropped", { sections: incident.contextPack.droppedSections.join(", ") })}
						</p>
					) : null}
				</Section>
			)}

			<div className="flex items-center gap-3">
				<button
					className="rounded border border-primary bg-primary px-3 py-1.5 text-sm text-primary-foreground disabled:opacity-50"
					disabled={pending || !incident.canDiagnose}
					onClick={onDiagnose}
					type="button"
				>
					{pending ? t("incident.investigating") : t("incident.investigate")}
				</button>
				<span className="text-muted-foreground">
					{t("incident.diagnosisBudget", { used: incident.diagnosesUsed, max: incident.maxDiagnoses })}
				</span>
			</div>
		</div>
	);
}

/** DiagnosedBody renders the classification, the evidence and the one action. */
function DiagnosedBody({
	incident,
	approved,
	onApprovedChange,
	onExecute,
	pending,
}: {
	incident: IncidentView;
	approved: boolean;
	onApprovedChange: (next: boolean) => void;
	onExecute: (approve: boolean) => void;
	pending: boolean;
}) {
	const { t } = useTranslation();
	const d = incident.diagnosis;
	if (!d) return null;
	const action = d.proposedAction;

	return (
		<div className="space-y-4 text-sm">
			<p className="font-medium">{d.summary}</p>
			<ClassBadge classification={d.classification} />

			{d.whatHappened && <Section title={t("incident.whatHappened")}>{d.whatHappened}</Section>}
			{d.whatIsStuck && <Section title={t("incident.whatIsStuck")}>{d.whatIsStuck}</Section>}
			{d.whyAOStopped && <Section title={t("incident.whyStopped")}>{d.whyAOStopped}</Section>}

			{d.evidence?.length ? (
				<Section title={t("incident.evidence")}>
					<ul className="list-disc pl-5">
						{d.evidence.map((e: string) => (
							<li key={e}>{e}</li>
						))}
					</ul>
				</Section>
			) : null}

			{/* Missing evidence is rendered as prominently as evidence. A refusal
			    that names its gap is the useful half of an unsafe/insufficient
			    verdict, and burying it would turn a considered "I cannot tell"
			    into what looks like a failure. */}
			{d.missingEvidence?.length ? (
				<Section title={t("incident.missingEvidence")}>
					<ul className="list-disc pl-5">
						{d.missingEvidence.map((m: string) => (
							<li key={m}>{m}</li>
						))}
					</ul>
				</Section>
			) : null}

			{d.risk && <Section title={t("incident.risk")}>{d.risk}</Section>}

			{d.options?.length ? (
				<Section title={t("incident.options")}>
					<ul className="space-y-2">
						{d.options.map((o: NonNullable<IncidentDiagnosisView["options"]>[number]) => (
							<li key={o.id}>
								<span className="font-medium">{o.label}</span> — {o.detail}
								{o.consequence && <span className="block text-muted-foreground">{o.consequence}</span>}
							</li>
						))}
					</ul>
				</Section>
			) : null}

			{action && (
				<ActionPanel
					action={action}
					approved={approved}
					disabled={!incident.canExecute || incident.stale}
					onApprovedChange={onApprovedChange}
					onExecute={onExecute}
					pending={pending}
				/>
			)}
		</div>
	);
}

/**
 * ActionPanel is the only place in the modal that can cause anything to happen.
 *
 * `describe` is AO's statement of the mechanism and `rationale` is the agent's
 * argument for it; they are shown as different things, labelled differently,
 * because a proposer narrating what the system will do is precisely the
 * confusion this separation exists to avoid.
 */
function ActionPanel({
	action,
	approved,
	disabled,
	onApprovedChange,
	onExecute,
	pending,
}: {
	action: IncidentActionView;
	approved: boolean;
	disabled: boolean;
	onApprovedChange: (next: boolean) => void;
	onExecute: (approve: boolean) => void;
	pending: boolean;
}) {
	const { t } = useTranslation();
	if (!action.executable) {
		return (
			<Section title={t("incident.recommended")}>
				<p className="text-muted-foreground">{action.refusalReason || action.describe}</p>
			</Section>
		);
	}
	const blocked = disabled || pending || (action.needsApproval && !approved);
	return (
		<div className="space-y-2 rounded border p-3">
			<p className="font-medium">{t("incident.recommended")}</p>
			<p>{action.describe}</p>
			{action.rationale && (
				<p className="text-muted-foreground">{t("incident.agentRationale", { rationale: action.rationale })}</p>
			)}
			<p className="text-muted-foreground">{t("incident.risk")}: {action.risk}</p>

			{action.needsApproval && (
				<label className="flex items-start gap-2">
					<input checked={approved} onChange={(e: React.ChangeEvent<HTMLInputElement>) => onApprovedChange(e.target.checked)} type="checkbox" />
					<span>
						{action.writesCode ? t("incident.approveWritesCode") : null}
						{action.endsWork ? t("incident.approveEndsWork") : null}
						{!action.writesCode && !action.endsWork ? t("incident.approveGeneric") : null}
					</span>
				</label>
			)}

			<button
				className="rounded border border-primary bg-primary px-3 py-1.5 text-sm text-primary-foreground disabled:opacity-50"
				disabled={blocked}
				onClick={() => onExecute(action.needsApproval)}
				type="button"
			>
				{pending ? t("incident.working") : t("incident.doIt")}
			</button>
		</div>
	);
}

function ClassBadge({ classification }: { classification: string }) {
	const { t } = useTranslation();
	return (
		<p className="text-muted-foreground">
			{t("incident.classification")}: <span className="font-mono">{classification}</span>
		</p>
	);
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
	return (
		<div>
			<p className="font-medium">{title}</p>
			<div className="text-muted-foreground">{children}</div>
		</div>
	);
}
