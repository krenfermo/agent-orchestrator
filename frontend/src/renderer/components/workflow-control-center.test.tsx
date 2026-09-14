import { render, screen, within } from "@testing-library/react";
import type { ReactElement } from "react";
import { I18nextProvider } from "react-i18next";
import { describe, expect, it } from "vitest";
import { createAppI18n } from "../i18n/instance";
import type { WorkflowRunDetail } from "../lib/workflow-control-center";
import { WorkflowControlCenter, WorkflowRunAnatomy } from "./workflow-control-center";

/**
 * P8's UX acceptance stories, rendered:
 *
 *  A. (creation form, covered in -_shell.workflows.test.tsx)
 *  B. a running workflow: what it is doing, who works, last signal, step
 *  C. needs_attention: what happened, the evidence, what AO could not determine
 *  D. a completed workflow: worker → review → fix → review → verify → result
 *  E. workflow vs agent session
 *
 * Every fixture is shaped like the daemon's response; nothing here depends on a
 * field the API does not send.
 */

type Step = WorkflowRunDetail["steps"][number];
const NOW = Date.parse("2026-09-01T11:00:00Z");
const T = (minute: number) => new Date(Date.parse("2026-09-01T10:00:00Z") + minute * 60_000).toISOString();

function renderIn(ui: ReactElement, locale: "en" | "es" = "en") {
	return render(<I18nextProvider i18n={createAppI18n(locale)}>{ui}</I18nextProvider>);
}

const link = (sessionId: string) => <a href={`/sessions/${sessionId}`}>{sessionId}</a>;

function step(partial: Partial<Step> & Pick<Step, "id" | "kind" | "ordinal" | "state">): Step {
	return { createdAt: T(0), updatedAt: T(0), attempts: [], ...partial };
}

function baseRun(over: Partial<WorkflowRunDetail["run"]> = {}): WorkflowRunDetail["run"] {
	return {
		id: "wf-8a1c",
		projectId: "proj-1",
		objective: "Add rate limiting",
		state: "running",
		phase: "running",
		createdAt: T(0),
		updatedAt: T(30),
		lastActivityAt: T(30),
		executionMode: "autonomous",
		canContinue: false,
		executionStrategy: { effectiveStrategy: "task", requestedStrategy: "task", selectionSource: "explicit", policyVersion: "v1", reasonCode: "explicit_request", depth: 0 },
		maxFixCycles: 3,
		...over,
	} as WorkflowRunDetail["run"];
}

describe("B. a running workflow reads in one screen", () => {
	const running: WorkflowRunDetail = {
		run: baseRun({
			workerLiveness: { observed: true, sessionId: "sess-w1", stepId: "w", state: "active", lastSignalAt: T(59), lastTransitionAt: T(40), silentForSeconds: 12 },
		}),
		presentation: {
			stage: "working",
			requiresHuman: false,
			automaticActionActive: false,
			summaryCode: "working",
			technical: { lastEventAt: T(40), lastEventPhase: "worker_dispatched" },
			timeline: [
				{ at: T(0), kind: "started" },
				{ at: T(1), kind: "worker_launched", detail: "claude-code" },
			],
		},
		steps: [
			step({
				id: "w",
				kind: "work",
				ordinal: 1,
				state: "running",
				sessionId: "sess-w1",
				attempts: [{ id: "a1", attemptNumber: 1, startedAt: T(1), harness: "claude-code", model: "opus" }],
			}),
		],
	};

	it("states status, mode, step, agent, session and the last signal", () => {
		renderIn(<WorkflowControlCenter detail={running} now={NOW} renderSessionLink={link} />);
		expect(screen.getByTestId("workflow-headline-status")).toHaveTextContent("Working");
		expect(screen.getByTestId("workflow-mode-chip")).toHaveTextContent("Mode: Task");
		expect(screen.getByTestId("workflow-approval-chip")).toHaveTextContent("Automatic approval");
		expect(screen.getByTestId("workflow-fact-step")).toHaveTextContent("#1 · Worker");
		const agent = screen.getByTestId("workflow-fact-agent");
		expect(agent).toHaveTextContent("Worker · claude-code / opus");
		expect(within(agent).getByRole("link", { name: "sess-w1" })).toBeInTheDocument();
		expect(screen.getByTestId("workflow-fact-signal")).toHaveTextContent("12");
		expect(screen.getByTestId("workflow-fact-duration")).toHaveTextContent("1h");
		// A healthy run raises no attention block and no incident.
		expect(screen.queryByTestId("workflow-attention-report")).toBeNull();
		expect(screen.queryByTestId("workflow-incident-advisor")).toBeNull();
		// The durable codes are still on the page, ranked below the human status.
		expect(screen.getByTestId("workflow-technical-line")).toHaveTextContent("state running · phase running · stage working");
	});

	it("names a quiet worker as a possible problem, with the evidence and no verdict", () => {
		const quiet = { ...running, run: { ...running.run, workerLiveness: { ...running.run.workerLiveness!, silentForSeconds: 1500 } } };
		renderIn(<WorkflowControlCenter detail={quiet} now={NOW} renderSessionLink={link} />, "es");
		const advisor = screen.getByTestId("workflow-incident-advisor");
		expect(advisor).toHaveTextContent("Agente sin señal reciente");
		expect(advisor).toHaveTextContent("última señal hace 25 min.");
		expect(advisor).toHaveTextContent("AO todavía no puede probar desde aquí que el proceso siga vivo");
		// Not a stop: the durable state is still running, and the page says so.
		expect(screen.getByTestId("workflow-headline-status")).toHaveTextContent("Ejecutando");
	});
});

describe("C. needs_attention says what happened, what AO knows and what it does not", () => {
	const stopped = (reason: string, over: Partial<WorkflowRunDetail> = {}): WorkflowRunDetail => ({
		run: baseRun({ state: "needs_attention", phase: "needs_attention", attentionReason: reason }),
		presentation: {
			stage: "needs_attention",
			requiresHuman: true,
			automaticActionActive: false,
			summaryCode: reason,
			recommendedAction: "open_session",
			placement: { type: "isolated_worktree", chosenBy: "automatic", executionBranch: "ao/wf-8a1c", worktreePath: "/tmp/ao/wt-8a1c" } as NonNullable<WorkflowRunDetail["presentation"]>["placement"],
			technical: { sessionId: "sess-f1", attemptNumber: 2, provider: "anthropic", lastEventPhase: "fix_dispatched", lastEventAt: T(50), authority: "legacy_unproven" },
		},
		steps: [
			step({ id: "w", kind: "work", ordinal: 1, state: "completed", sessionId: "sess-w1" }),
			step({ id: "f", kind: "fix", ordinal: 2, state: "failed", sessionId: "sess-f1" }),
		],
		advice: { category: "human_action", requiresHuman: true, automaticActionActive: false, repairable: false, retryable: false, repairBudget: 0, repairSpent: 0, recommendedAction: "open_session", authority: {} } as unknown as WorkflowRunDetail["advice"],
		...over,
	});

	it("renders the four answers in Spanish, and says plainly when the cause is unknown", () => {
		renderIn(<WorkflowControlCenter detail={stopped("unclassified_stop")} now={NOW} renderSessionLink={link} />, "es");
		const report = screen.getByTestId("workflow-attention-report");
		expect(report).toHaveTextContent("Qué pasó");
		expect(report).toHaveTextContent("Qué sabe AO");
		expect(report).toHaveTextContent("Qué no pudo determinar AO");
		expect(report).toHaveTextContent("Acción recomendada");
		expect(screen.getByTestId("workflow-attention-undetermined")).toHaveTextContent("AO no puede determinar la causa.");
		const known = screen.getByTestId("workflow-attention-known");
		expect(known).toHaveTextContent("unclassified_stop");
		expect(known).toHaveTextContent("sess-f1");
		expect(known).toHaveTextContent("ao/wf-8a1c");
		expect(known).toHaveTextContent("fix_dispatched");
		expect(within(known).getByText("Fix")).toBeInTheDocument();
		// The ownership gap is stated in plain words, not as a lease code.
		expect(screen.getByTestId("workflow-attention-unknowns")).toHaveTextContent(
			"No se pudo confirmar que la sesión activa siga perteneciendo a este workflow.",
		);
		expect(screen.getByTestId("workflow-headline-status")).toHaveTextContent("Necesita atención");
	});

	it("does not claim the cause is unknown when AO's own reason names it", () => {
		renderIn(<WorkflowControlCenter detail={stopped("fix_budget_exhausted")} now={NOW} renderSessionLink={link} />);
		expect(screen.queryByTestId("workflow-attention-undetermined")).toBeNull();
		expect(screen.getByTestId("workflow-attention-action")).toHaveTextContent("Open the session");
		expect(screen.getByTestId("workflow-incident-advisor")).toHaveTextContent("Fix budget used up");
	});
});

describe("D. a completed workflow tells worker → review → fix → verify → result", () => {
	const completed: WorkflowRunDetail = {
		run: baseRun({ state: "completed", phase: "completed", completedAt: T(45) }),
		presentation: {
			stage: "completed",
			requiresHuman: false,
			automaticActionActive: false,
			summaryCode: "completed",
			technical: {},
			timeline: [
				{ at: T(0), kind: "started" },
				{ at: T(1), kind: "worker_launched", detail: "claude-code" },
				{ at: T(10), kind: "work_completed" },
				{ at: T(11), kind: "review_started", detail: "codex" },
				{ at: T(15), kind: "review_verdict", detail: "changes_requested" },
				{ at: T(16), kind: "fix_started", detail: "claude-code" },
				{ at: T(25), kind: "review_started", detail: "codex" },
				{ at: T(30), kind: "review_verdict", detail: "approved" },
				{ at: T(40), kind: "verified" },
				{ at: T(45), kind: "completed" },
			],
		},
		steps: [
			step({ id: "w", kind: "work", ordinal: 1, state: "completed", sessionId: "sess-w1" }),
			step({ id: "r", kind: "review", ordinal: 2, state: "completed", reviewer: "codex", verdict: "approved" }),
			step({ id: "f", kind: "fix", ordinal: 3, state: "completed", fixDelivery: { cycleNumber: 1, sessionId: "sess-w1" } as Step["fixDelivery"] }),
			step({
				id: "v",
				kind: "verify",
				ordinal: 4,
				state: "completed",
				verification: {
					passed: true,
					checks: [{ kind: "command", label: "go test ./...", passed: true, exitCode: 0 }],
					preFingerprint: "a",
					postFingerprint: "a",
					reviewedFingerprint: "a",
					targetKey: "k",
					version: "v1",
				},
			}),
		],
	};

	it("folds the durable timeline into the phases that actually happened", () => {
		renderIn(<WorkflowControlCenter detail={completed} now={NOW} />);
		const phases = within(screen.getByTestId("workflow-compact-timeline"))
			.getAllByRole("listitem")
			.map((item) => item.getAttribute("data-phase"));
		expect(phases).toEqual(["created", "worker", "review", "fix", "review", "verify", "completed"]);
		expect(screen.getByTestId("workflow-headline-status")).toHaveTextContent("Completed");
		expect(screen.queryByRole("status")).toBeNull();
		expect(screen.getByTestId("workflow-fact-duration")).toHaveTextContent("45m");
	});

	it("keeps review and Verify as two separate answers, with the fix budget", () => {
		renderIn(<WorkflowRunAnatomy detail={completed} />);
		expect(screen.getByTestId("workflow-review-outcome")).toHaveTextContent("Approved");
		expect(screen.getByTestId("workflow-review-fix-cycle")).toHaveTextContent("1 / 3");
		expect(screen.getByTestId("workflow-verify-outcome")).toHaveTextContent("Passed");
		expect(screen.getByTestId("workflow-verify-card")).toHaveTextContent("Checks: 1 passed, 0 failed");
		expect(screen.queryByTestId("workflow-verify-triggered-fix")).toBeNull();
	});
});

describe("E. workflow vs agent session", () => {
	it("draws the workflow as the parent and marks which nodes are agent sessions", () => {
		const detail: WorkflowRunDetail = {
			run: baseRun(),
			steps: [
				step({ id: "w", kind: "work", ordinal: 1, state: "completed", sessionId: "sess-w1", attempts: [{ id: "a", attemptNumber: 1, startedAt: T(1), harness: "claude-code" }] }),
				step({ id: "r", kind: "review", ordinal: 2, state: "running", reviewer: "codex" }),
				step({ id: "v", kind: "verify", ordinal: 3, state: "pending" }),
			],
		};
		renderIn(<WorkflowRunAnatomy detail={detail} renderSessionLink={link} />);
		const structure = screen.getByTestId("workflow-structure");
		expect(structure).toHaveTextContent("Workflow wf-8a1c");
		const nodes = within(structure).getAllByTestId("workflow-structure-node");
		expect(nodes.map((node) => [node.getAttribute("data-role"), node.getAttribute("data-agent-session")])).toEqual([
			["worker", "true"],
			["reviewer", "true"],
			["verify", "false"],
		]);
		expect(within(nodes[0]!).getByRole("link", { name: "sess-w1" })).toBeInTheDocument();
		expect(nodes[1]).toHaveTextContent("No session recorded");
		expect(nodes[2]).toHaveTextContent("No agent session: AO runs the checks");
	});

	it("labels the header as a workflow, with its definition one hover away", () => {
		renderIn(<WorkflowControlCenter detail={{ run: baseRun(), steps: [] }} now={NOW} />);
		const label = screen.getByTestId("workflow-kind-label");
		expect(label).toHaveTextContent("Workflow");
		expect(label.getAttribute("title")).toContain("durable unit of work");
	});
});

describe("usage digest: measured, modelled and unknown are never confused", () => {
	it("labels each figure by how it was obtained and never prints an unknown cost", () => {
		const detail = {
			run: baseRun(),
			steps: [],
			usage: {
				tokens: {
					recorded: true,
					source: "provider_reported",
					totals: { input: 12_000, output: 3_000, total: 15_000, cacheRead: 0, cacheWrite: 0, events: 4, uncachedInput: 12_000, reasoning: null },
					cost: { known: false, basis: "unknown", amount: 0, unpricedModels: ["sonnet"] },
					budget: { state: "unset", tokenPercent: null, costPercent: null },
					context: { recorded: true, estimatedAssembledTokens: 8_000 },
				},
			},
		} as unknown as WorkflowRunDetail;
		renderIn(<WorkflowRunAnatomy detail={detail} />);
		const tokens = screen.getByTestId("workflow-usage-tokens");
		expect(tokens).toHaveTextContent("12,000 / 3,000");
		expect(within(tokens).getByText("measured")).toHaveAttribute("data-certainty", "measured");
		const cost = screen.getByTestId("workflow-usage-cost");
		expect(cost).toHaveTextContent("Unknown");
		expect(cost).not.toHaveTextContent("0.00");
		expect(screen.getByTestId("workflow-usage-digest")).toHaveTextContent("Pricing unknown for: sonnet");
		// AO's own estimate of assembled context is always labelled modelled.
		expect(screen.getByTestId("workflow-usage-digest").querySelectorAll('[data-certainty="modelled"]')).toHaveLength(1);
	});
});
