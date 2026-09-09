import { useTranslation } from "react-i18next";
import { Ban, Bot, CircleCheck, Clock, UserRoundCheck } from "lucide-react";
import type { components } from "../../api/schema";
import { actionLabelKey, stageLabelKey } from "../lib/workflow-presentation";
import { translateDynamic } from "./workflow-activity";
import { cn } from "../lib/utils";

export type WorkflowAdvice = components["schemas"]["ControllersWorkflowAdviceView"];

/**
 * workflow-advice-panel.tsx — the renderer half of P3-C.
 *
 * `GET /workflows/{id}` has always carried an `advice` block: the daemon's
 * single deterministic answer to "what do I do now", derived once on the server
 * from the same durable facts the board card and the status panel read. Until
 * now the renderer parsed it off the wire and threw all of it away, which meant
 * the three questions it answers had no surface at all:
 *
 *	is anyone needed / what is AO doing about it by itself / why can't I press
 *	that button
 *
 * The third one is the reason this file exists. `presentation.actions` already
 * drives the buttons, and the recovery panel deliberately HIDES an operation the
 * backend has refused — which is right for the button row and leaves "where did
 * it go" unanswerable. Advice carries the closed refusal set with a stable
 * reason code for each, so the answer is available without guessing.
 *
 * The rule this file inherits from every other workflow surface: **it decides
 * nothing.** Category, automatic action, refusals, budget and next stage are all
 * computed by `workflow.DeriveAdvice`, a pure function that cannot write. This
 * component chooses wording and order, never safety, and it renders no button —
 * the authorised offers stay with `WorkflowActions`, so a person is never shown
 * two controls that make the same POST.
 */

const CATEGORY_ICON = {
	no_action_required: CircleCheck,
	auto_recoverable: Bot,
	wait_only: Clock,
	human_action: UserRoundCheck,
	terminal: CircleCheck,
} as const;

const CATEGORY_CLASS = {
	no_action_required: "border-border",
	auto_recoverable: "border-status-working/40 bg-status-working/5",
	wait_only: "border-border",
	human_action: "border-status-needs-you/40 bg-status-needs-you/5",
	terminal: "border-border",
} as const;

/**
 * Every code below comes from a closed Go vocabulary — AdviceCategory,
 * AutomaticActionID, the DisabledReason set in presentation.go, and
 * automaticBlockedReason. `translateDynamic` still falls back to the raw code
 * rather than blank, so a value added on the daemon before a translation exists
 * is shown as itself instead of vanishing.
 */
function categoryKey(category: string): string {
	return `wf.advice.category.${category}`;
}

function automaticActionKey(action: string): string {
	return `wf.advice.auto.${action}`;
}

function automaticBlockedKey(reason: string): string {
	return `wf.advice.autoBlocked.${reason}`;
}

function blockedReasonKey(reason: string): string {
	return `wf.advice.blocked.${reason}`;
}

/** The repair-eligibility labels the recovery panel already owns. Reused rather
 *  than restated, so the two surfaces cannot disagree about the same field. */
const REPAIR_ELIGIBILITY_KEY: Record<string, string> = {
	eligible: "recovery.repairEligible",
	ineligible: "recovery.repairIneligible",
	budget_exhausted: "recovery.repairBudgetExhausted",
	policy_disabled: "recovery.repairPolicyDisabled",
	unknown_condition: "recovery.repairUnknownCondition",
	artifact_unprovable: "recovery.repairArtifactUnprovable",
};

export function WorkflowAdvicePanel({ advice }: { advice: WorkflowAdvice | undefined }) {
	const { t } = useTranslation();
	if (!advice) return null;

	const category = advice.category;
	const Icon = CATEGORY_ICON[category] ?? CircleCheck;
	const blocked = advice.blockedActions ?? [];

	// What AO intends to do by itself, or the named reason it will not. Both are
	// the daemon's; an automatic action already running says so rather than
	// reading as something about to start.
	const automatic = advice.automaticAction
		? translateDynamic(t, automaticActionKey(advice.automaticAction), advice.automaticAction)
		: "";
	const automaticBlocked = advice.automaticActionBlockedReason
		? translateDynamic(
				t,
				automaticBlockedKey(advice.automaticActionBlockedReason),
				advice.automaticActionBlockedReason,
			)
		: "";

	// The budget line is worth a row only while repair is actually part of this
	// run's story. A budget of zero on a run nobody would repair is noise.
	const showRepair = advice.repairable || advice.repairBudget > 0 || advice.repairSpent > 0;
	const repairEligibility = advice.repairEligibility
		? translateDynamic(
				t,
				REPAIR_ELIGIBILITY_KEY[advice.repairEligibility] ?? advice.repairEligibility,
				advice.repairEligibility,
			)
		: "";

	return (
		<section
			className={cn("flex flex-col gap-2 rounded-lg border p-3", CATEGORY_CLASS[category] ?? "border-border")}
			data-testid="workflow-advice"
		>
			<div className="flex items-start gap-2">
				<Icon aria-hidden="true" className="mt-0.5 size-icon-sm shrink-0 text-muted-foreground" />
				<div className="flex min-w-0 flex-col gap-0.5">
					<p className="text-sm font-medium">{translateDynamic(t, categoryKey(category), category)}</p>
					{/* AO's own sentence, shown only when it adds something the
					    category label does not already say. */}
					{advice.explanation ? (
						<p className="text-xs text-muted-foreground">{advice.explanation}</p>
					) : null}
				</div>
			</div>

			<dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs text-muted-foreground">
				{automatic ? (
					<>
						<dt>{t("wf.advice.aoWillDo")}</dt>
						<dd>
							{automatic}
							{advice.automaticActionActive ? ` · ${t("wf.advice.inProgress")}` : ""}
						</dd>
					</>
				) : null}
				{automaticBlocked ? (
					<>
						<dt>{t("wf.advice.aoWillNot")}</dt>
						<dd>{automaticBlocked}</dd>
					</>
				) : null}
				{advice.expectedNextStage ? (
					<>
						<dt>{t("wf.advice.nextStage")}</dt>
						<dd>
							{translateDynamic(
								t,
								stageLabelKey(advice.expectedNextStage),
								advice.expectedNextStage,
							)}
						</dd>
					</>
				) : null}
				{advice.waitReason || advice.waitUntil ? (
					<>
						<dt>{t("wf.advice.waiting")}</dt>
						<dd>
							{advice.waitReason
								? translateDynamic(
											t,
											`shell.workflowsCapacityWaitReason.${advice.waitReason}`,
											advice.waitReason,
										)
								: ""}
							{advice.waitUntil
								? `${advice.waitReason ? " · " : ""}${new Date(advice.waitUntil).toLocaleString()}`
								: ""}
						</dd>
					</>
				) : null}
				{showRepair && repairEligibility ? (
					<>
						<dt>{t("wf.advice.repair")}</dt>
						<dd>
							{advice.repairBudget > 0
								? t("recovery.repairWithBudget", {
										eligibility: repairEligibility,
										spent: advice.repairSpent,
										budget: advice.repairBudget,
									})
								: repairEligibility}
						</dd>
					</>
				) : null}
			</dl>

			{/* §2: a refused action is REPORTED, never silently missing. This is
			    the only place in the renderer that answers "why can't I press
			    that", and it renders text rather than a disabled control on
			    purpose — a greyed-out button invites the click again. */}
			{blocked.length > 0 ? (
				<div className="flex flex-col gap-1 border-t border-border pt-2">
					<p className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
						<Ban aria-hidden="true" className="size-3 shrink-0" />
						{t("wf.advice.blockedTitle")}
					</p>
					<ul className="flex flex-col gap-0.5">
						{blocked.map((item) => (
							<li className="text-xs text-muted-foreground" key={`${item.action}:${item.reason}`}>
								{t("wf.advice.blockedRow", {
									action: translateDynamic(t, actionLabelKey(item.action), item.action),
									reason: translateDynamic(t, blockedReasonKey(item.reason), item.reason),
								})}
							</li>
						))}
					</ul>
				</div>
			) : null}
		</section>
	);
}
