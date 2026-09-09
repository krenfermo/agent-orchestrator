import type { components } from "../../api/schema";

type WorkflowRunDetailView = components["schemas"]["WorkflowRunDetailView"];

/**
 * run-diagnostics.ts — the answer to "what do I paste when I ask for help".
 *
 * Before this, every fact needed to explain a stuck run existed — on screen, in
 * the technical disclosure, in the advice block, in each step's attempts — and
 * none of it could leave the window. So the actual recovery workflow was: read
 * the run id off the page, open a terminal, and query SQLite for the rest. That
 * is the specific thing this file ends.
 *
 * # It is an allowlist, and that is the security property
 *
 * Every field below is named explicitly. Nothing is spread, copied wholesale or
 * serialized by walking the response, so a field added to the API later cannot
 * silently start appearing in something a user pastes into a chat window. A
 * denylist would have the opposite default and would be wrong the first time
 * the daemon grew a field nobody thought about.
 *
 * Two kinds of value are additionally treated as unsafe even though the daemon
 * hands them over quite legitimately:
 *
 *  - **Absolute filesystem paths** (`worktreePath`, `repoPath`) carry the
 *    user's home directory and therefore their account name. Reduced to a
 *    basename, which is the part that actually identifies the worktree.
 *  - **The objective's body.** Its first line is the run's name and is needed
 *    to know which run this is; the rest is free prose a person may have pasted
 *    anything into. Only the first line travels.
 *
 * # It derives nothing
 *
 * Every value is copied from the daemon's own projection. This module states
 * facts and redacts; it never computes a verdict, and a field the daemon did
 * not send is simply absent rather than guessed.
 */

export type RunDiagnostics = {
	runId: string;
	projectId: string;
	/** The objective's first line only. See the redaction note above. */
	title: string;
	state: string;
	phase: string;
	stage?: string;
	strategy?: string;
	executionMode: string;
	createdAt: string;
	lastActivityAt: string;
	/** The stop, in the daemon's closed vocabulary. */
	attention?: string;
	attentionReason?: string;
	attentionDetail?: string;
	summaryCode?: string;
	errorClass?: string;
	/** The advice block: category, what AO will do, and every refusal. */
	adviceCategory?: string;
	adviceVersion?: string;
	requiresHuman?: boolean;
	automaticAction?: string;
	automaticActionActive?: boolean;
	automaticActionBlockedReason?: string;
	expectedNextStage?: string;
	recommendedAction?: string;
	availableActions: string[];
	blockedActions: { action: string; reason: string }[];
	repairEligibility?: string;
	repairSpent?: number;
	repairBudget?: number;
	waitReason?: string;
	waitUntil?: string;
	/** Which execution this reading is about. */
	attemptId?: string;
	attemptNumber?: number;
	provider?: string;
	sessionId?: string;
	authority?: string;
	placementGeneration?: number;
	lifecycleGeneration?: number;
	repairRunId?: string;
	/** Where the work happens. Paths reduced to a basename. */
	placementType?: string;
	placementChosenBy?: string;
	executionBranch?: string;
	baseBranch?: string;
	worktreeName?: string;
	integration?: string;
	steps: RunDiagnosticsStep[];
};

export type RunDiagnosticsStep = {
	ordinal: number;
	kind: string;
	state: string;
	branch?: string;
	headSha?: string;
	verdict?: string;
	reviewDecision?: string;
	reviewReasons?: string[];
	/** The evidence reference a reader needs to re-derive the change set. */
	changedFileCount?: number;
	committedChangedFileCount?: number;
	changedFilesSource?: string;
	changedFilesUnprovable?: boolean;
	unprovableChangeSetReason?: string;
	changeSetBaseSha?: string;
	changeSetHeadSha?: string;
	attempts: RunDiagnosticsAttempt[];
};

export type RunDiagnosticsAttempt = {
	attemptNumber: number;
	outcome?: string;
	errorClass?: string;
	harness?: string;
	model?: string;
	startedAt: string;
	finishedAt?: string;
};

/** The trailing path segment, for a path of either separator. */
function basename(path: string | undefined): string | undefined {
	if (!path) return undefined;
	const parts = path.split(/[\\/]/).filter((part) => part.length > 0);
	return parts.length > 0 ? parts[parts.length - 1] : undefined;
}

/** The objective's first line: the run's name, without its body. */
function firstLine(objective: string): string {
	const line = objective.split("\n", 1)[0]?.trim();
	return line && line.length > 0 ? line : objective.trim();
}

function nullableIso(value: string | null | undefined): string | undefined {
	return value ?? undefined;
}

export function buildRunDiagnostics(detail: WorkflowRunDetailView): RunDiagnostics {
	const run = detail.run;
	const tech = detail.presentation?.technical;
	const advice = detail.advice;
	const placement = detail.presentation?.placement;

	return {
		runId: run.id,
		projectId: run.projectId,
		title: firstLine(run.objective),
		state: run.state,
		phase: run.phase,
		stage: detail.presentation?.stage,
		strategy: run.executionStrategy?.effectiveStrategy,
		executionMode: run.executionMode,
		createdAt: run.createdAt,
		lastActivityAt: run.lastActivityAt,

		attention: run.attention ?? tech?.attention,
		attentionReason: run.attentionReason ?? tech?.attentionReason,
		attentionDetail: tech?.attentionDetail,
		summaryCode: detail.presentation?.summaryCode,
		errorClass: tech?.errorClass,

		adviceCategory: advice?.category,
		adviceVersion: advice?.version,
		requiresHuman: advice?.requiresHuman,
		automaticAction: advice?.automaticAction,
		automaticActionActive: advice?.automaticActionActive,
		automaticActionBlockedReason: advice?.automaticActionBlockedReason,
		expectedNextStage: advice?.expectedNextStage,
		recommendedAction: advice?.recommendedAction ?? detail.presentation?.recommendedAction,
		availableActions: advice?.availableActions ?? [],
		blockedActions: (advice?.blockedActions ?? []).map((item) => ({
			action: item.action,
			reason: item.reason,
		})),
		repairEligibility: advice?.repairEligibility,
		repairSpent: advice?.repairSpent,
		repairBudget: advice?.repairBudget,
		waitReason: advice?.waitReason ?? tech?.waitReason,
		waitUntil: nullableIso(advice?.waitUntil) ?? nullableIso(tech?.nextWakeAt),

		attemptId: tech?.attemptId,
		attemptNumber: tech?.attemptNumber,
		provider: tech?.provider,
		sessionId: tech?.sessionId,
		authority: tech?.authority,
		placementGeneration: tech?.placementGeneration,
		lifecycleGeneration: tech?.lifecycleGeneration,
		repairRunId: tech?.repairRunId,

		placementType: placement?.type,
		placementChosenBy: placement?.chosenBy,
		executionBranch: placement?.executionBranch,
		baseBranch: placement?.baseBranch,
		worktreeName: basename(placement?.worktreePath),
		integration: placement?.integration,

		steps: (detail.steps ?? []).map((step) => {
			const facts = step.reviewPolicy?.facts;
			return {
				ordinal: step.ordinal,
				kind: step.kind,
				state: step.state,
				branch: step.branch,
				headSha: step.headSha,
				verdict: step.verdict || undefined,
				reviewDecision: step.reviewPolicy?.decision,
				reviewReasons: step.reviewPolicy?.reasons,
				changedFileCount: facts?.changedFileCount,
				committedChangedFileCount: facts?.committedChangedFileCount,
				changedFilesSource: facts?.changedFilesSource,
				changedFilesUnprovable: facts?.changedFilesUnprovable,
				unprovableChangeSetReason: facts?.unprovableChangeSetReason,
				changeSetBaseSha: facts?.changeSetBaseSha,
				changeSetHeadSha: facts?.changeSetHeadSha,
				attempts: (step.attempts ?? []).map((attempt) => ({
					attemptNumber: attempt.attemptNumber,
					outcome: attempt.outcome,
					errorClass: attempt.errorClass,
					harness: attempt.harness,
					model: attempt.model,
					startedAt: attempt.startedAt,
					finishedAt: nullableIso(attempt.finishedAt),
				})),
			};
		}),
	};
}

/**
 * The bundle as text, for pasting into an issue or a chat.
 *
 * Deliberately plain and stable rather than pretty: it is read by people and
 * diffed by them, and the field names are the daemon's own so a reader can grep
 * the codebase for any line of it.
 */
export function formatRunDiagnostics(d: RunDiagnostics): string {
	const lines: string[] = [];
	const put = (label: string, value: unknown) => {
		if (value === undefined || value === null || value === "" || (Array.isArray(value) && value.length === 0)) {
			return;
		}
		lines.push(`${label}: ${Array.isArray(value) ? value.join(", ") : String(value)}`);
	};

	lines.push(`# AO run diagnostics`);
	put("run", d.runId);
	put("project", d.projectId);
	put("title", d.title);
	put("state", d.state);
	put("phase", d.phase);
	put("stage", d.stage);
	put("strategy", d.strategy);
	put("executionMode", d.executionMode);
	put("createdAt", d.createdAt);
	put("lastActivityAt", d.lastActivityAt);

	lines.push("", "## Stop");
	put("attention", d.attention);
	put("attentionReason", d.attentionReason);
	put("attentionDetail", d.attentionDetail);
	put("summaryCode", d.summaryCode);
	put("errorClass", d.errorClass);

	lines.push("", "## Advice");
	put("category", d.adviceCategory);
	put("requiresHuman", d.requiresHuman);
	put("automaticAction", d.automaticAction);
	put("automaticActionActive", d.automaticActionActive);
	put("automaticActionBlockedReason", d.automaticActionBlockedReason);
	put("expectedNextStage", d.expectedNextStage);
	put("recommendedAction", d.recommendedAction);
	put("availableActions", d.availableActions);
	for (const blocked of d.blockedActions) {
		put(`blocked:${blocked.action}`, blocked.reason);
	}
	put("repairEligibility", d.repairEligibility);
	if (d.repairBudget) put("repairBudget", `${d.repairSpent ?? 0} of ${d.repairBudget} used`);
	put("waitReason", d.waitReason);
	put("waitUntil", d.waitUntil);
	put("adviceVersion", d.adviceVersion);

	lines.push("", "## Execution");
	put("attempt", d.attemptId ? `${d.attemptId}${d.attemptNumber ? ` (#${d.attemptNumber})` : ""}` : undefined);
	put("provider", d.provider);
	put("session", d.sessionId);
	put("authority", d.authority);
	put("placementGeneration", d.placementGeneration);
	put("lifecycleGeneration", d.lifecycleGeneration);
	put("repairRun", d.repairRunId);
	put("placement", d.placementType);
	put("placementChosenBy", d.placementChosenBy);
	put("executionBranch", d.executionBranch);
	put("baseBranch", d.baseBranch);
	put("worktree", d.worktreeName);
	put("integration", d.integration);

	for (const step of d.steps) {
		lines.push("", `## Step ${step.ordinal} — ${step.kind} (${step.state})`);
		put("branch", step.branch);
		put("headSha", step.headSha);
		put("verdict", step.verdict);
		put("reviewDecision", step.reviewDecision);
		put("reviewReasons", step.reviewReasons);
		if (step.changedFilesUnprovable) {
			put("changeSet", "unprovable");
			put("unprovableReason", step.unprovableChangeSetReason);
		} else if (step.changedFileCount !== undefined) {
			put("changedFiles", step.changedFileCount);
			put("committedChangedFiles", step.committedChangedFileCount);
		}
		put("changedFilesSource", step.changedFilesSource);
		if (step.changeSetBaseSha && step.changeSetHeadSha) {
			put("changeSetRange", `${step.changeSetBaseSha}..${step.changeSetHeadSha}`);
		}
		for (const attempt of step.attempts) {
			const detail = [
				attempt.outcome,
				attempt.errorClass,
				[attempt.harness, attempt.model].filter(Boolean).join("/") || undefined,
				attempt.startedAt,
			]
				.filter(Boolean)
				.join(" · ");
			put(`attempt ${attempt.attemptNumber}`, detail);
		}
	}

	return lines.join("\n");
}
