import type { components } from "../../api/schema";

/**
 * workflow-control-center.ts — P8's reading of a workflow run, in one place.
 *
 * The run page already carried every fact a person needs to answer "what is
 * this doing, who is working, is it stuck, why, and what now" — spread over a
 * dozen panels, with the raw run state printed next to the human stage. This
 * module folds those facts into the few answers the control-center header
 * shows, and it follows the rule every workflow surface in this renderer
 * already follows: **it decides nothing.**
 *
 *  - Every value is read from the daemon's response (`run`, `steps`,
 *    `presentation`, `advice`, `usage`). Nothing is re-derived from timestamps,
 *    nothing is inferred from prose, and nothing here can write.
 *  - A fact the daemon did not send is `undefined` / `null` / "unknown", never
 *    a default that reads like a measurement.
 *  - The durable run state is never replaced. The headline is a presentation
 *    OVER it, and the raw codes stay available for the technical line.
 */

export type WorkflowRunDetail = components["schemas"]["WorkflowRunDetailView"];
type Step = components["schemas"]["WorkflowStepView"];
type Usage = components["schemas"]["ControllersWorkflowUsageResponse"];
type TimelineEvent = components["schemas"]["WorkflowPresentationEvent"];

const TERMINAL_RUN_STATES: ReadonlySet<string> = new Set(["completed", "failed", "cancelled"]);

export function runIsTerminal(detail: Pick<WorkflowRunDetail, "run">): boolean {
	return TERMINAL_RUN_STATES.has(detail.run.state);
}

/**
 * When the worker-liveness surfaces start saying an agent has gone quiet. Shared
 * with WorkflowLivenessPanel so the header and the panel cannot disagree about
 * the same clock. Deliberately generous: an agent running a long test suite is
 * silent for minutes and healthy.
 */
export const LIVENESS_QUIET_THRESHOLD_SECONDS = 600;

// ---------------------------------------------------------------------------
// 1. Headline status
// ---------------------------------------------------------------------------

export type HeadlineStatus =
	| "preparing"
	| "planning"
	| "running"
	| "waiting_agent"
	| "reviewing"
	| "correcting"
	| "verifying"
	| "integrating"
	| "waiting"
	| "waiting_you"
	| "needs_attention"
	| "completed"
	| "cancelled"
	| "failed";

export type HeadlineTone = "working" | "review" | "attention" | "failed" | "done" | "idle";

/**
 * The one status a person reads first.
 *
 * Precedence, and why:
 *  1. A terminal durable state wins outright. Nothing the projection says can
 *     make a completed run read as running.
 *  2. `needs_attention` is itself a durable state and outranks a pending
 *     question — the stop is the bigger fact.
 *  3. A question AO could not answer for itself is "waiting for you".
 *  4. A `pending` run has not started: "preparing".
 *  5. Otherwise the daemon's own stage. The only refinement is `working`, split
 *     by the durable state of the step that is supposed to be running: a step
 *     still `ready`/`waiting` means AO handed work to an agent that has not
 *     started it yet.
 */
export function headlineStatus(detail: WorkflowRunDetail): HeadlineStatus {
	const state = detail.run.state;
	if (state === "completed" || state === "cancelled" || state === "failed") return state;
	if (state === "needs_attention") return "needs_attention";
	if ((detail.questions ?? []).some((question) => question.state === "human_required")) return "waiting_you";
	if (state === "pending") return "preparing";

	const stage = detail.presentation?.stage ?? detail.run.stage;
	switch (stage) {
		case "preparing":
		case "planning":
		case "reviewing":
		case "correcting":
		case "verifying":
		case "integrating":
		case "waiting":
		case "needs_attention":
			return stage;
		case "completed":
		case "cancelled":
		case "failed":
			// The run row is not terminal but the projection is: trust the row,
			// which is the durable fact, and say AO is still on it.
			return "running";
		case "working": {
			const step = currentStep(detail);
			// A live signal from the agent on this very step outranks the step row:
			// an agent the daemon can hear is working, whatever the row says.
			const heard = detail.run.workerLiveness?.observed && detail.run.workerLiveness.stepId === step?.id;
			if (step && !heard && step.state !== "running" && (step.kind === "work" || step.kind === "fix")) return "waiting_agent";
			return "running";
		}
		default:
			return state === "waiting" ? "waiting" : "running";
	}
}

export function headlineTone(status: HeadlineStatus): HeadlineTone {
	switch (status) {
		case "reviewing":
			return "review";
		case "needs_attention":
		case "waiting_you":
			return "attention";
		case "failed":
			return "failed";
		case "completed":
			return "done";
		case "cancelled":
		case "waiting":
			return "idle";
		default:
			return "working";
	}
}

// ---------------------------------------------------------------------------
// 2. Steps, roles and the session tree
// ---------------------------------------------------------------------------

/**
 * What a step IS, in the vocabulary of "workflow vs session". Planner, worker,
 * reviewer and fix steps are carried out by an agent session; verify is AO
 * running the configured checks itself, with no agent session behind it.
 */
export type StepRole = "planner" | "worker" | "reviewer" | "fix" | "verify" | "advance";

export function stepRole(kind: Step["kind"]): StepRole {
	switch (kind) {
		case "plan":
			return "planner";
		case "work":
			return "worker";
		case "review":
			return "reviewer";
		case "fix":
			return "fix";
		case "verify":
			return "verify";
		default:
			return "advance";
	}
}

function byOrdinal(steps: readonly Step[]): Step[] {
	return [...steps].sort((a, b) => a.ordinal - b.ordinal);
}

function latestAttempt(step: Step) {
	return step.attempts.length > 0 ? step.attempts[step.attempts.length - 1] : undefined;
}

/**
 * The step AO is on right now, from durable step states only: a running step
 * first, then one waiting on something, then one ready to go. A terminal run —
 * or a run with nothing in flight — has no current step, and says so.
 */
export function currentStep(detail: WorkflowRunDetail): Step | undefined {
	if (runIsTerminal(detail)) return undefined;
	const steps = byOrdinal(detail.steps).filter((step) => step.kind !== "advance");
	return (
		steps.find((step) => step.state === "running") ??
		steps.find((step) => step.state === "waiting") ??
		steps.find((step) => step.state === "ready")
	);
}

export type SessionNode = {
	stepId: string;
	ordinal: number;
	role: StepRole;
	state: Step["state"];
	/** True when an agent session carries this step; false for verify. */
	agentSession: boolean;
	harness?: string;
	model?: string;
	sessionId?: string;
	attempts: number;
	current: boolean;
};

/**
 * The run's structure as it actually exists: one node per durable step, in
 * order. `advance` is AO's own bookkeeping step and is left out for the same
 * reason the daemon's timeline leaves it out. No node is ever synthesised — a
 * run that never reached review has no reviewer node.
 */
export function sessionTree(detail: WorkflowRunDetail): SessionNode[] {
	const current = currentStep(detail);
	const liveness = detail.run.workerLiveness;
	return byOrdinal(detail.steps)
		.filter((step) => step.kind !== "advance")
		.map((step) => {
			const role = stepRole(step.kind);
			const attempt = latestAttempt(step);
			const liveSession =
				liveness?.observed && liveness.stepId === step.id ? liveness.sessionId : undefined;
			return {
				stepId: step.id,
				ordinal: step.ordinal,
				role,
				state: step.state,
				agentSession: role !== "verify",
				harness: (role === "reviewer" ? step.reviewer : undefined) || attempt?.harness || step.assignedHarness || undefined,
				model: attempt?.model || undefined,
				sessionId: step.sessionId || step.fixDelivery?.sessionId || liveSession || undefined,
				attempts: step.attempts.length,
				current: current?.id === step.id,
			};
		});
}

export type ActiveAgent = SessionNode;

/** Who is working right now, or undefined when nobody is. */
export function activeAgent(detail: WorkflowRunDetail): ActiveAgent | undefined {
	return sessionTree(detail).find((node) => node.current);
}

// ---------------------------------------------------------------------------
// 3. Review and fix cycles
// ---------------------------------------------------------------------------

export type FixCycleSummary = {
	/**
	 * Fix cycles actually dispatched: the highest cycle number on the fix
	 * step's delivery record, which is folded from the same fix_dispatched
	 * ledger the budget is enforced against. `null` when a fix step has run but
	 * carries no delivery record, so the number cannot be stated.
	 *
	 * NOT usage.metrics.fixCycles: that metric counts fix-step ATTEMPTS, which
	 * is a different number (a re-delivered cycle is one cycle, two attempts).
	 */
	spent: number | null;
	/** The run's frozen budget, or null when the daemon did not project one. */
	max: number | null;
};

export function fixCycleSummary(detail: WorkflowRunDetail): FixCycleSummary {
	const fixSteps = detail.steps.filter((step) => step.kind === "fix");
	const max = detail.run.maxFixCycles ?? null;
	if (fixSteps.length === 0) return { spent: 0, max };
	const delivered = fixSteps
		.map((step) => step.fixDelivery?.cycleNumber)
		.filter((n): n is number => typeof n === "number" && n > 0);
	if (delivered.length > 0) return { spent: Math.max(...delivered), max };
	const neverStarted = fixSteps.every((step) => step.attempts.length === 0 && (step.state === "pending" || step.state === "ready"));
	return { spent: neverStarted ? 0 : null, max };
}

export type ReviewOutcome = "approved" | "changes_requested" | "in_progress" | "pending" | "skipped" | "unknown";

export type ReviewSummary = {
	outcome: ReviewOutcome;
	reviewer?: string;
	findingsSummary?: string;
	target?: string;
	/** How many review steps this run has — not how many review RUNS. */
	reviewSteps: number;
	fixCycles: FixCycleSummary;
};

/** The latest review, or undefined for a run that has no review step. */
export function reviewSummary(detail: WorkflowRunDetail): ReviewSummary | undefined {
	const reviews = byOrdinal(detail.steps).filter((step) => step.kind === "review");
	const step = reviews[reviews.length - 1];
	if (!step) return undefined;
	let outcome: ReviewOutcome;
	if (step.reviewPolicy?.decision === "skipped") outcome = "skipped";
	else if (step.verdict === "approved" || step.verdict === "changes_requested") outcome = step.verdict;
	else if (step.state === "running" || step.state === "waiting") outcome = "in_progress";
	else if (step.state === "pending" || step.state === "ready") outcome = "pending";
	// Completed without a verdict, failed or cancelled: AO has no verdict to show.
	else outcome = "unknown";
	return {
		outcome,
		// No fallback harness name: a reviewer the daemon did not name is unknown,
		// not "claude-code".
		reviewer: step.reviewer || latestAttempt(step)?.harness || undefined,
		findingsSummary: step.findingsSummary || undefined,
		target: step.target || undefined,
		reviewSteps: reviews.length,
		fixCycles: fixCycleSummary(detail),
	};
}

// ---------------------------------------------------------------------------
// 4. Verify
// ---------------------------------------------------------------------------

export type VerifyOutcome = "pending" | "running" | "passed" | "failed" | "unknown";

export type VerifyCheckLine = {
	label: string;
	kind: string;
	passed: boolean;
	exitCode?: number | null;
	durationMs?: number;
	/** First line of the failure reason, bounded. The tails stay in the detail. */
	reason?: string;
};

export type VerifySummary = {
	outcome: VerifyOutcome;
	checks: VerifyCheckLine[];
	failedCount: number;
	reusedCheckCount?: number;
	errorClass?: string;
	stopReason?: string;
	infraFailure?: { kind: string; detail: string; command: string };
	/**
	 * True only when a durable record says so: a fix delivery whose findings
	 * came from verification, or a stop reason that names the verify re-entry.
	 */
	triggeredFix: boolean;
};

const REASON_MAX_CHARS = 200;

export function shortReason(text: string | undefined): string | undefined {
	if (!text) return undefined;
	const first = text.split("\n", 1)[0]?.trim() ?? "";
	if (!first) return undefined;
	return first.length > REASON_MAX_CHARS ? `${first.slice(0, REASON_MAX_CHARS - 1)}…` : first;
}

export function verifySummary(detail: WorkflowRunDetail): VerifySummary | undefined {
	const verifies = byOrdinal(detail.steps).filter((step) => step.kind === "verify");
	const step = verifies[verifies.length - 1];
	if (!step) return undefined;
	const result = step.verification;
	let outcome: VerifyOutcome;
	if (result) outcome = result.passed ? "passed" : "failed";
	else if (step.state === "pending" || step.state === "ready") outcome = "pending";
	else if (step.state === "running" || step.state === "waiting") outcome = "running";
	else if (step.state === "failed") outcome = "failed";
	else outcome = "unknown";
	const checks: VerifyCheckLine[] = (result?.checks ?? []).map((check) => ({
		label: check.label,
		kind: check.kind,
		passed: check.passed,
		exitCode: check.exitCode,
		durationMs: check.durationMs,
		reason: check.passed ? undefined : shortReason(check.failureReason),
	}));
	const triggeredFix =
		detail.steps.some((s) => s.kind === "fix" && s.fixDelivery?.findingsSource === "verification") ||
		detail.run.attentionReason === "verify_fix_reentry";
	return {
		outcome,
		checks,
		failedCount: checks.filter((check) => !check.passed).length,
		reusedCheckCount: result?.reusedCheckCount,
		errorClass: result?.errorClass || undefined,
		stopReason: result?.stopReason || undefined,
		infraFailure: result?.infraFailure
			? { kind: result.infraFailure.kind, detail: shortReason(result.infraFailure.detail) ?? "", command: result.infraFailure.command }
			: undefined,
		triggeredFix,
	};
}

// ---------------------------------------------------------------------------
// 5. needs_attention
// ---------------------------------------------------------------------------

/**
 * Stop reasons that are, by the daemon's own naming, a statement that AO could
 * NOT establish what happened: ambiguous, unprovable, unreadable, unreconcilable
 * or unclassified. For these the page says "AO cannot determine the cause"
 * instead of dressing the code up as a diagnosis.
 */
const UNDETERMINED_REASONS: ReadonlySet<string> = new Set([
	"unclassified_stop",
	"review_state_ambiguous",
	"fix_dispatch_ambiguous",
	"fix_generation_unprovable",
	"planner_ambiguous",
	"planner_result_inconsistent",
	"recovery_unreconcilable",
	"worker_dispatch_ambiguous",
	"worker_workspace_unreadable",
	"provider_dialog_unreadable",
	"verify_workspace_unattributable",
	"verify_approved_head_unprovable",
]);

const UNDETERMINED_ERROR_CLASSES: ReadonlySet<string> = new Set(["ambiguous_worker_state", "verify_ambiguous"]);

/** The repair-eligibility copy the recovery and advice panels already own; reused, not restated. */
const REPAIR_ELIGIBILITY_KEY: Record<string, string> = {
	eligible: "recovery.repairEligible",
	ineligible: "recovery.repairIneligible",
	budget_exhausted: "recovery.repairBudgetExhausted",
	policy_disabled: "recovery.repairPolicyDisabled",
	unknown_condition: "recovery.repairUnknownCondition",
	artifact_unprovable: "recovery.repairArtifactUnprovable",
};

export type EvidenceRow = {
	/** Translation key for the row's label. */
	labelKey: string;
	/** A literal value (a code, an id, a branch). */
	text?: string;
	/** Or a translation key for the value. */
	textKey?: string;
	/** Or an instant the component renders as "N ago". */
	at?: string;
	mono?: boolean;
};

export type AttentionReport = {
	summaryCode?: string;
	/** The daemon's own sentence about the stop, used only as a fallback. */
	attentionDetail?: string;
	attentionReason?: string;
	/** True when AO's own vocabulary says it could not establish the cause. */
	causeUndetermined: boolean;
	known: EvidenceRow[];
	/** Translation keys, one per thing AO does not know. */
	unknowns: string[];
	recommendedAction?: string;
	automaticAction?: string;
	automaticActionActive: boolean;
	automaticActionBlockedReason?: string;
	requiresHuman: boolean;
};

/** The step a stop is about: the one owning the stopped session, else the last in flight or failed. */
function stoppedStep(detail: WorkflowRunDetail): Step | undefined {
	const sessionId = detail.presentation?.technical.sessionId;
	const steps = byOrdinal(detail.steps).filter((step) => step.kind !== "advance");
	if (sessionId) {
		const owner = steps.find((step) => step.sessionId === sessionId || step.fixDelivery?.sessionId === sessionId);
		if (owner) return owner;
	}
	return [...steps].reverse().find((step) => step.state !== "pending" && step.state !== "completed");
}

export function attentionReport(detail: WorkflowRunDetail): AttentionReport | undefined {
	const presentation = detail.presentation;
	if (detail.run.state !== "needs_attention" && presentation?.stage !== "needs_attention") return undefined;
	const tech = presentation?.technical;
	const advice = detail.advice;
	const reason = detail.run.attentionReason || tech?.attentionReason || undefined;
	const errorClass = tech?.errorClass || undefined;
	const summaryCode = presentation?.summaryCode || detail.run.summaryCode || undefined;
	const causeUndetermined =
		!reason ||
		UNDETERMINED_REASONS.has(reason) ||
		(errorClass !== undefined && UNDETERMINED_ERROR_CLASSES.has(errorClass));

	const known: EvidenceRow[] = [];
	if (reason) known.push({ labelKey: "cc.evidence.reason", text: reason, mono: true });
	if (errorClass) known.push({ labelKey: "cc.evidence.errorClass", text: errorClass, mono: true });
	const step = stoppedStep(detail);
	if (step) {
		known.push({ labelKey: "cc.evidence.step", textKey: `cc.role.${stepRole(step.kind)}` });
	}
	const sessionId = tech?.sessionId || step?.sessionId || undefined;
	if (sessionId) known.push({ labelKey: "cc.evidence.session", text: sessionId, mono: true });
	if (tech?.attemptNumber) {
		known.push({
			labelKey: "cc.evidence.attempt",
			text: `#${tech.attemptNumber}${tech.provider ? ` · ${tech.provider}` : ""}`,
		});
	}
	const liveness = detail.run.workerLiveness;
	if (liveness?.observed && liveness.lastSignalAt) {
		known.push({ labelKey: "cc.evidence.lastSignal", at: liveness.lastSignalAt });
	}
	if (tech?.lastEventPhase) {
		known.push({ labelKey: "cc.evidence.lastCheckpoint", text: tech.lastEventPhase, mono: true });
		if (tech.lastEventAt) known.push({ labelKey: "cc.evidence.lastCheckpointAt", at: tech.lastEventAt });
	}
	const placement = presentation?.placement;
	if (placement?.executionBranch) known.push({ labelKey: "cc.evidence.branch", text: placement.executionBranch, mono: true });
	if (placement?.worktreePath) known.push({ labelKey: "cc.evidence.worktree", text: placement.worktreePath, mono: true });
	if (tech?.authority) known.push({ labelKey: "cc.evidence.authority", textKey: `cc.authority.${tech.authority}` });
	if (advice?.repairEligibility) {
		known.push({
			labelKey: "cc.evidence.repair",
			textKey: REPAIR_ELIGIBILITY_KEY[advice.repairEligibility],
			text: advice.repairEligibility,
		});
	}

	const unknowns: string[] = [];
	if (causeUndetermined) unknowns.push("cc.unknown.cause");
	if (!sessionId) unknowns.push("cc.unknown.noSession");
	if (!liveness?.observed) unknowns.push("cc.unknown.noSignal");
	if (tech?.authority === "legacy_unproven") unknowns.push("cc.unknown.authorityUnproven");
	if (advice?.repairEligibility === "unknown_condition") unknowns.push("cc.unknown.repairCondition");

	return {
		summaryCode,
		attentionDetail: tech?.attentionDetail || undefined,
		attentionReason: reason,
		causeUndetermined,
		known,
		unknowns,
		recommendedAction: advice?.recommendedAction || presentation?.recommendedAction || detail.run.recommendedAction || undefined,
		automaticAction: advice?.automaticAction,
		automaticActionActive: Boolean(advice?.automaticActionActive),
		automaticActionBlockedReason: advice?.automaticActionBlockedReason || undefined,
		requiresHuman: Boolean(advice?.requiresHuman ?? presentation?.requiresHuman),
	};
}

// ---------------------------------------------------------------------------
// 6. Incident advisor (deterministic)
// ---------------------------------------------------------------------------

export type IncidentSignalId =
	| "worker_quiet"
	| "authority_unproven"
	| "fix_budget_exhausted"
	| "verify_failed"
	| "provider_failed"
	| "usage_budget"
	| "question_waiting";

export type IncidentSignal = {
	id: IncidentSignalId;
	severity: "warn" | "info";
	/** Interpolation values for the signal's title/evidence/next copy. */
	params: Record<string, string | number>;
	/** A session the "next" step can link to, when the evidence names one. */
	sessionId?: string;
};

const PROVIDER_ERROR_CLASSES: ReadonlySet<string> = new Set([
	"auth",
	"rate_limited",
	"capacity_exhausted",
	"binary_missing",
	"provider_auth_required",
	"provider_workspace_trust_required",
	"provider_preflight_failed",
]);

/**
 * Problems the page can name from evidence it already holds. Each rule is a
 * direct reading of one or two durable fields — no rule combines weak signals
 * into a guess — and a terminal run raises none, because nothing about a
 * finished run is still a problem to act on.
 */
export function incidentSignals(detail: WorkflowRunDetail): IncidentSignal[] {
	if (runIsTerminal(detail)) return [];
	const signals: IncidentSignal[] = [];
	const liveness = detail.run.workerLiveness;
	const silent = liveness?.silentForSeconds;
	if (liveness?.observed && typeof silent === "number" && silent >= LIVENESS_QUIET_THRESHOLD_SECONDS) {
		signals.push({
			id: "worker_quiet",
			severity: "warn",
			params: { minutes: Math.round(silent / 60) },
			sessionId: liveness.sessionId || undefined,
		});
	}
	const tech = detail.presentation?.technical;
	if (tech?.authority === "legacy_unproven") {
		signals.push({ id: "authority_unproven", severity: "warn", params: {}, sessionId: tech.sessionId || undefined });
	}
	const reason = detail.run.attentionReason || tech?.attentionReason;
	if (reason === "fix_budget_exhausted") {
		const cycles = fixCycleSummary(detail);
		signals.push({
			id: "fix_budget_exhausted",
			severity: "warn",
			params: { spent: cycles.spent ?? "?", max: cycles.max ?? "?" },
		});
	}
	const verify = verifySummary(detail);
	if (verify?.outcome === "failed") {
		signals.push({ id: "verify_failed", severity: "warn", params: { failed: verify.failedCount } });
	}
	const step = currentStep(detail) ?? stoppedStep(detail);
	const attempt = step ? latestAttempt(step) : undefined;
	if (attempt?.outcome === "failed" && attempt.errorClass && PROVIDER_ERROR_CLASSES.has(attempt.errorClass)) {
		signals.push({
			id: "provider_failed",
			severity: "warn",
			params: { harness: attempt.harness || "?", errorClass: attempt.errorClass },
		});
	}
	const budget = detail.usage?.tokens?.budget;
	if (budget && (budget.state === "warning" || budget.state === "exhausted")) {
		signals.push({
			id: "usage_budget",
			severity: budget.state === "exhausted" ? "warn" : "info",
			params: { percent: Math.round(budget.tokenPercent ?? budget.costPercent ?? 0), state: budget.state },
		});
	}
	const waiting = (detail.questions ?? []).filter((question) => question.state === "human_required").length;
	if (waiting > 0) signals.push({ id: "question_waiting", severity: "info", params: { n: waiting } });
	return signals;
}

// ---------------------------------------------------------------------------
// 7. Compact timeline
// ---------------------------------------------------------------------------

export type TimelinePhase =
	| "created"
	| "planned"
	| "worker"
	| "review"
	| "fix"
	| "verify"
	| "repair"
	| "integrated"
	| "stopped"
	| "completed"
	| "cancelled"
	| "failed";

export type TimelineChip = {
	phase: TimelinePhase;
	/** How many consecutive durable events folded into this chip (retries). */
	count: number;
	/** Provider failures recorded while this phase was the latest one. */
	failures: number;
	/** The review verdict, on a review chip whose verdict was recorded. */
	verdict?: string;
	at: string;
};

const TIMELINE_PHASE: Partial<Record<TimelineEvent["kind"], TimelinePhase>> = {
	started: "created",
	planned: "planned",
	worker_launched: "worker",
	review_started: "review",
	fix_started: "fix",
	verified: "verify",
	repair_started: "repair",
	integrated: "integrated",
	stopped: "stopped",
	completed: "completed",
	cancelled: "cancelled",
	failed: "failed",
};

/**
 * The daemon's durable timeline folded into "Created → Worker → Review → Fix →
 * Review → Verify → Completed". It only ever shortens what the daemon sent:
 * consecutive launches of the same phase (retries, failovers) become one chip
 * with a count, a completion or verdict attaches to the phase it closes, and a
 * provider failure is counted on the phase it interrupted. No phase is added
 * that the daemon did not record.
 */
export function compactTimeline(events: readonly TimelineEvent[] | undefined): TimelineChip[] {
	const chips: TimelineChip[] = [];
	for (const event of events ?? []) {
		const last = chips[chips.length - 1];
		if (event.kind === "work_completed") continue;
		if (event.kind === "provider_failed") {
			if (last) last.failures += 1;
			continue;
		}
		if (event.kind === "review_verdict") {
			if (last?.phase === "review") {
				last.verdict = event.detail || undefined;
			} else {
				chips.push({ phase: "review", count: 1, failures: 0, verdict: event.detail || undefined, at: event.at });
			}
			continue;
		}
		const phase = TIMELINE_PHASE[event.kind];
		if (!phase) continue;
		if (last && last.phase === phase && last.verdict === undefined) {
			last.count += 1;
			continue;
		}
		chips.push({ phase, count: 1, failures: 0, at: event.at });
	}
	return chips;
}

// ---------------------------------------------------------------------------
// 8. Cost / context
// ---------------------------------------------------------------------------

/**
 * How a number was obtained. `measured` is reported by the provider (or priced
 * from provider-reported tokens); `modelled` is AO's own estimate; `unknown` is
 * everything else. A modelled number is never shown under a measured label.
 */
export type Certainty = "measured" | "modelled" | "unknown";

export type UsageDigest = {
	tokens: Certainty;
	inputTokens?: number;
	outputTokens?: number;
	context: Certainty;
	calls?: number;
	contextCurrent?: number;
	contextPeak?: number;
	contextGrowth?: number;
	/** AO's own estimate of the context it assembled; always modelled. */
	assembledContextEstimate?: number;
	cost: {
		certainty: Certainty;
		amount?: number;
		currency?: string;
		basis?: string;
		unpricedModels: string[];
	};
	budgetState?: string;
	budgetPercent?: number;
	advisories: string[];
};

export function tokenCertainty(source: string | undefined, recorded: boolean): Certainty {
	if (!recorded) return "unknown";
	if (source === "provider_reported") return "measured";
	if (source === "estimated") return "modelled";
	return "unknown";
}

export function usageDigest(usage: Usage | undefined): UsageDigest | undefined {
	const ledger = usage?.tokens;
	if (!ledger) return undefined;
	const tokens = tokenCertainty(ledger.source, ledger.recorded);
	const trajectory = ledger.dynamics?.recorded ? ledger.dynamics.trajectory : undefined;
	const observable = Boolean(trajectory?.observable);
	const cost = ledger.cost;
	let costCertainty: Certainty = "unknown";
	if (cost?.known) {
		if (cost.basis === "provider_reported") costCertainty = "measured";
		else if (cost.basis === "calculated") costCertainty = tokens === "measured" ? "measured" : "modelled";
	}
	return {
		tokens,
		inputTokens: tokens === "unknown" ? undefined : ledger.totals.input,
		outputTokens: tokens === "unknown" ? undefined : ledger.totals.output,
		context: observable ? "measured" : "unknown",
		calls: observable ? trajectory?.providerCalls : undefined,
		contextCurrent: observable ? trajectory?.lastContextTokens : undefined,
		contextPeak: observable ? trajectory?.peakContextTokens : undefined,
		contextGrowth: observable ? trajectory?.growthTokens : undefined,
		assembledContextEstimate:
			ledger.context?.recorded && ledger.context.estimatedAssembledTokens > 0 ? ledger.context.estimatedAssembledTokens : undefined,
		cost: {
			certainty: costCertainty,
			amount: costCertainty === "unknown" ? undefined : cost.amount,
			currency: cost?.currency || undefined,
			basis: cost?.basis || undefined,
			unpricedModels: cost?.unpricedModels ?? [],
		},
		budgetState: ledger.budget && ledger.budget.state !== "unset" ? ledger.budget.state : undefined,
		budgetPercent:
			ledger.budget && ledger.budget.state !== "unset"
				? Math.round(ledger.budget.tokenPercent ?? ledger.budget.costPercent ?? 0)
				: undefined,
		advisories: (ledger.dynamics?.warnings ?? []).map((warning) => warning.code),
	};
}

// ---------------------------------------------------------------------------
// 9. Clocks
// ---------------------------------------------------------------------------

/** Seconds since an ISO instant, or null when absent/unparseable. */
export function secondsSince(iso: string | null | undefined, now: number): number | null {
	if (!iso) return null;
	const at = Date.parse(iso);
	return Number.isNaN(at) ? null : Math.max(0, (now - at) / 1000);
}

/**
 * How long the run has existed, in seconds: to its completion or cancellation
 * when it has one, to its last update for a failed run (the only terminal
 * instant a failed row carries), and to now while it is still going.
 */
export function runDurationSeconds(detail: WorkflowRunDetail, now: number): number | null {
	const run = detail.run;
	const start = Date.parse(run.createdAt);
	if (Number.isNaN(start)) return null;
	const endIso =
		run.completedAt ?? run.cancelledAt ?? (run.state === "failed" ? run.updatedAt : undefined);
	const end = endIso ? Date.parse(endIso) : now;
	if (Number.isNaN(end)) return null;
	return Math.max(0, (end - start) / 1000);
}
