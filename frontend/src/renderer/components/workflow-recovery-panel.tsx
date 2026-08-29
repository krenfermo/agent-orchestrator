import { Link } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import {
	RECOVERY_OPERATIONS,
	useWorkflowRecovery,
	type RecoveryAction,
	type RecoveryOperation,
	type WorkflowRecoveryView,
	type WorkflowRepairPlanView,
} from "../hooks/useWorkflowRecovery";
import type { components } from "../../api/schema";
import type { MessageKey } from "../i18n/messages";

type WorkflowRunView = components["schemas"]["WorkflowRunView"];

/**
 * WorkflowRecoveryPanel is P1-B §L: the operator's answer to "what do I do
 * about this run", rendered from the backend's assessment and nothing else.
 *
 * The one rule that governs this file: **it decides nothing.** Which action is
 * recommended, whether AO may take it automatically, whether the plan can be
 * reused, whether a repair is available — every one of those is computed by
 * `workflow.AssessRecovery` from durable facts and arrives on the wire. This
 * component chooses layout and wording; it never chooses safety.
 *
 * Concretely, an operation is offered only when the assessment says it is
 * available (see `offeredOperations`). An action AO has refused is not rendered
 * as a disabled hint or a "try anyway" — it is absent, because offering a
 * button whose backend will refuse it is how a UI teaches people to ignore it.
 */
export function WorkflowRecoveryPanel({ run }: { run: WorkflowRunView }) {
	const { t } = useTranslation();
	// The panel is for a run that is stopped or that the backend says has
	// something to recover. A healthy running workflow gets nothing: the
	// assessment probes the project's planner context, and asking for it on
	// every poll of every run would be a real cost for an answer nobody needs.
	const relevant = run.state === "needs_attention" || run.state === "waiting" || run.phase === "blocked";
	const { recovery, repairPlan, isLoading, error, run: runOperation, pending, pendingOperation, actionError } =
		useWorkflowRecovery(run.id, relevant);

	if (!relevant) return null;
	if (isLoading && !recovery) {
		return (
			<section className="rounded-lg border border-border p-4">
				<p className="text-sm text-muted-foreground">{t("recovery.loading")}</p>
			</section>
		);
	}
	if (error && !recovery) {
		return (
			<section className="rounded-lg border border-border p-4">
				<p className="text-sm text-destructive">{error}</p>
			</section>
		);
	}
	if (!recovery) return null;

	const offered = offeredOperations(recovery);

	return (
		<section className="flex flex-col gap-3 rounded-lg border border-border p-4">
			<div className="flex flex-wrap items-baseline justify-between gap-2">
				<h2 className="text-sm font-semibold">{t("recovery.title")}</h2>
				<span className="text-xs text-muted-foreground">{recoveryActionLabel(t, recovery.recommendedAction)}</span>
			</div>

			{/* What happened, in AO's own words. This is the same sentence the
			    Board shows for the stop, never a second wording of it. */}
			{recovery.explanation && <p className="text-sm text-foreground">{recovery.explanation}</p>}

			<dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
				<RecoveryFact label={t("recovery.recommendedAction")} value={recoveryActionLabel(t, recovery.recommendedAction)} />
				<RecoveryFact
					label={t("recovery.automation")}
					value={recovery.automaticAllowed ? t("recovery.automationAuto") : t("recovery.automationManual")}
				/>
				{recovery.reasonCode && <RecoveryFact label={t("recovery.reason")} value={recovery.reasonCode} />}
				{recovery.blockingCondition && (
					<RecoveryFact label={t("recovery.blockedOn")} value={recovery.blockingCondition} />
				)}
				<RecoveryFact label={t("recovery.planStatus")} value={planReuseLabel(t, recovery.planReusable)} />
				<RecoveryFact label={t("recovery.repairAvailability")} value={repairLabel(t, recovery, repairPlan)} />
				{recovery.obligation && recovery.obligation !== "none" && (
					<RecoveryFact label={t("recovery.obligation")} value={obligationLabel(t, recovery.obligation)} />
				)}
			</dl>

			{/* When this run's stop is a mirror of a child's, the operator is
			    sent to the run that actually owns the problem. */}
			{recovery.targetRunId && recovery.targetRunId !== run.id && (
				<Link className="text-sm underline" params={{ workflowId: recovery.targetRunId }} to="/workflows/$workflowId">
					{t("recovery.openAffectedRun")}
				</Link>
			)}

			{offered.length > 0 ? (
				<div className="flex flex-wrap gap-2">
					{offered.map((op) => (
						<button
							className={`rounded px-3 py-1.5 text-sm disabled:opacity-50 ${
								op === recommendedOperation(recovery.recommendedAction)
									? "border border-primary bg-primary text-primary-foreground"
									: "border border-border"
							}`}
							disabled={pending}
							key={op}
							onClick={() => runOperation(op)}
							type="button"
						>
							{pendingOperation === op ? t("recovery.working") : recoveryOperationLabel(t, op)}
						</button>
					))}
				</div>
			) : (
				<p className="text-xs text-muted-foreground">{t("recovery.noAutomaticActions")}</p>
			)}

			{/* A backend refusal is shown as a refusal. It is never swallowed and
			    never rendered as a success. */}
			{actionError && <p className="text-sm text-destructive">{actionError}</p>}
		</section>
	);
}

function RecoveryFact({ label, value }: { label: string; value: string }) {
	return (
		<>
			<dt className="text-muted-foreground">{label}</dt>
			<dd className="text-foreground">{value}</dd>
		</>
	);
}

/**
 * offeredOperations is the whole of the UI's authorization logic, and it is
 * deliberately a projection rather than a decision:
 *
 *   - resume is offered when the assessment recommends it, or when a durable
 *     obligation AO can discharge is outstanding;
 *   - reuse/regenerate are offered from the backend's own plan classification;
 *   - repair is offered only when the backend says a repair is available.
 *
 * Every input is a field the daemon computed. Nothing here inspects run state,
 * step state or a reason code to make its own ruling.
 */
export function offeredOperations(recovery: WorkflowRecoveryView): RecoveryOperation[] {
	const offered = new Set<RecoveryOperation>();
	if (recovery.recommendedAction === "resume") offered.add("resume");
	if (recovery.planReusable === "exact") offered.add("reuse_plan");
	if (recovery.planReusable === "stale_but_revalidatable" || recovery.recommendedAction === "regenerate_plan") {
		offered.add("regenerate_plan");
	}
	if (recovery.recommendedAction === "reuse_plan") offered.add("reuse_plan");
	if (recovery.repairAvailable) offered.add("repair");
	return RECOVERY_OPERATIONS.filter((op) => offered.has(op));
}

/** recommendedOperation maps the recommendation onto an offered button, when
 * one corresponds, so the panel can emphasise it. An operator action has no
 * button by design. */
function recommendedOperation(action: RecoveryAction): RecoveryOperation | undefined {
	switch (action) {
		case "resume":
			return "resume";
		case "reuse_plan":
			return "reuse_plan";
		case "regenerate_plan":
			return "regenerate_plan";
		case "repair":
			return "repair";
		default:
			return undefined;
	}
}

// Exhaustive maps: an action, plan state or obligation the UI has no name for
// must be a compile-time problem, not a raw enum leaking onto the page.
const recoveryActionKeys: Record<RecoveryAction, MessageKey> = {
	resume: "recovery.actionResume",
	reuse_plan: "recovery.actionReusePlan",
	regenerate_plan: "recovery.actionRegeneratePlan",
	repair: "recovery.actionRepair",
	authenticate: "recovery.actionAuthenticate",
	inspect_repository: "recovery.actionInspectRepository",
	operator_action: "recovery.actionOperatorAction",
	restart_required: "recovery.actionRestartRequired",
	abandon: "recovery.actionAbandon",
	terminal: "recovery.actionTerminal",
	unrecoverable: "recovery.actionUnrecoverable",
};

const recoveryOperationKeys: Record<RecoveryOperation, MessageKey> = {
	resume: "recovery.actionResume",
	reuse_plan: "recovery.actionReusePlan",
	regenerate_plan: "recovery.actionRegeneratePlan",
	repair: "recovery.actionRepair",
};

const planReuseKeys: Record<NonNullable<WorkflowRecoveryView["planReusable"]>, MessageKey> = {
	not_applicable: "recovery.planNotApplicable",
	exact: "recovery.planExact",
	stale_but_revalidatable: "recovery.planStale",
	not_reusable: "recovery.planNotReusable",
};

const repairEligibilityKeys: Record<NonNullable<WorkflowRecoveryView["repairEligibility"]>, MessageKey> = {
	eligible: "recovery.repairEligible",
	ineligible: "recovery.repairIneligible",
	budget_exhausted: "recovery.repairBudgetExhausted",
	policy_disabled: "recovery.repairPolicyDisabled",
	unknown_condition: "recovery.repairUnknownCondition",
};

const obligationKeys: Record<NonNullable<WorkflowRecoveryView["obligation"]>, MessageKey> = {
	none: "recovery.obligationNone",
	plan_generation: "recovery.obligationPlanGeneration",
	plan_approval: "recovery.obligationPlanApproval",
	plan_dispatch: "recovery.obligationPlanDispatch",
	work_dispatch: "recovery.obligationWorkDispatch",
	work_observation: "recovery.obligationWorkObservation",
	review_dispatch: "recovery.obligationReviewDispatch",
	review_observation: "recovery.obligationReviewObservation",
	fix_delivery: "recovery.obligationFixDelivery",
	fix_observation: "recovery.obligationFixObservation",
	verify: "recovery.obligationVerify",
	convergence: "recovery.obligationConvergence",
	terminal: "recovery.obligationTerminal",
};

function recoveryActionLabel(t: TFunction, action: RecoveryAction): string {
	return t(recoveryActionKeys[action]);
}

function recoveryOperationLabel(t: TFunction, op: RecoveryOperation): string {
	return t(recoveryOperationKeys[op]);
}

function planReuseLabel(t: TFunction, plan: WorkflowRecoveryView["planReusable"]): string {
	return t(planReuseKeys[plan]);
}

function obligationLabel(t: TFunction, obligation: NonNullable<WorkflowRecoveryView["obligation"]>): string {
	return t(obligationKeys[obligation]);
}

function repairLabel(t: TFunction, recovery: WorkflowRecoveryView, plan: WorkflowRepairPlanView | undefined): string {
	const eligibility = t(repairEligibilityKeys[recovery.repairEligibility]);
	// The budget is only shown where it is part of the answer. Appending
	// "0 of 2 used" to "not a repairable condition" would suggest the budget is
	// why AO refused, when the condition is.
	const budgetIsTheAnswer = recovery.repairEligibility === "eligible" || recovery.repairEligibility === "budget_exhausted";
	if (!plan || !budgetIsTheAnswer) return eligibility;
	return t("recovery.repairWithBudget", { eligibility, spent: plan.spent, budget: plan.budget });
}
