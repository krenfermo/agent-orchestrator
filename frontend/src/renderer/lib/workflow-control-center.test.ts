import { describe, expect, it } from "vitest";
import {
	activeAgent,
	attentionReport,
	compactTimeline,
	fixCycleSummary,
	headlineStatus,
	headlineTone,
	incidentSignals,
	LIVENESS_QUIET_THRESHOLD_SECONDS,
	reviewSummary,
	runDurationSeconds,
	sessionTree,
	tokenCertainty,
	usageDigest,
	verifySummary,
	type WorkflowRunDetail,
} from "./workflow-control-center";

type Step = WorkflowRunDetail["steps"][number];

const T0 = "2026-09-01T10:00:00Z";

function step(partial: Partial<Step> & Pick<Step, "id" | "kind" | "ordinal" | "state">): Step {
	return { createdAt: T0, updatedAt: T0, attempts: [], ...partial };
}

function detail(partial: {
	run?: Partial<WorkflowRunDetail["run"]>;
	steps?: Step[];
	presentation?: Partial<NonNullable<WorkflowRunDetail["presentation"]>>;
	advice?: WorkflowRunDetail["advice"];
	questions?: WorkflowRunDetail["questions"];
	usage?: WorkflowRunDetail["usage"];
}): WorkflowRunDetail {
	return {
		run: {
			id: "wf-1",
			projectId: "proj-1",
			objective: "Do the thing",
			state: "running",
			phase: "running",
			createdAt: T0,
			updatedAt: T0,
			lastActivityAt: T0,
			executionMode: "autonomous",
			canContinue: false,
			...partial.run,
		} as WorkflowRunDetail["run"],
		steps: partial.steps ?? [],
		presentation: partial.presentation
			? ({
					stage: "working",
					requiresHuman: false,
					automaticActionActive: false,
					summaryCode: "working",
					technical: {},
					...partial.presentation,
				} as WorkflowRunDetail["presentation"])
			: undefined,
		advice: partial.advice,
		questions: partial.questions,
		usage: partial.usage,
	};
}

describe("headlineStatus: a presentation over the durable state, never a replacement", () => {
	it("lets a terminal run state win over any projection", () => {
		for (const state of ["completed", "cancelled", "failed"] as const) {
			expect(headlineStatus(detail({ run: { state }, presentation: { stage: "working" } }))).toBe(state);
		}
	});

	it("reads needs_attention from the durable state even with a question pending", () => {
		const d = detail({
			run: { state: "needs_attention" },
			questions: [{ state: "human_required" } as NonNullable<WorkflowRunDetail["questions"]>[number]],
		});
		expect(headlineStatus(d)).toBe("needs_attention");
	});

	it("says 'waiting for you' only for a question AO could not answer itself", () => {
		const d = detail({
			run: { state: "waiting" },
			presentation: { stage: "waiting" },
			questions: [{ state: "human_required" } as NonNullable<WorkflowRunDetail["questions"]>[number]],
		});
		expect(headlineStatus(d)).toBe("waiting_you");
	});

	it("maps a pending run to preparing", () => {
		expect(headlineStatus(detail({ run: { state: "pending" } }))).toBe("preparing");
	});

	it("maps the daemon's stages one to one", () => {
		for (const stage of ["planning", "reviewing", "correcting", "verifying", "integrating", "waiting"] as const) {
			expect(headlineStatus(detail({ presentation: { stage } }))).toBe(stage);
		}
	});

	it("splits working by the durable state of the work step, not by a clock", () => {
		const running = detail({
			presentation: { stage: "working" },
			steps: [step({ id: "s1", kind: "work", ordinal: 1, state: "running" })],
		});
		expect(headlineStatus(running)).toBe("running");
		const handedOff = detail({
			presentation: { stage: "working" },
			steps: [step({ id: "s1", kind: "work", ordinal: 1, state: "ready" })],
		});
		expect(headlineStatus(handedOff)).toBe("waiting_agent");
		// No false "waiting for the agent" while the daemon hears that agent on this step.
		const heard = detail({
			run: { workerLiveness: { observed: true, stepId: "s1", silentForSeconds: 5 } },
			presentation: { stage: "working" },
			steps: [step({ id: "s1", kind: "work", ordinal: 1, state: "waiting" })],
		});
		expect(headlineStatus(heard)).toBe("running");
	});

	it("gives attention states the attention tone", () => {
		expect(headlineTone("needs_attention")).toBe("attention");
		expect(headlineTone("waiting_you")).toBe("attention");
		expect(headlineTone("completed")).toBe("done");
		expect(headlineTone("failed")).toBe("failed");
		expect(headlineTone("reviewing")).toBe("review");
	});
});

describe("sessionTree: workflow vs agent session", () => {
	const steps = [
		step({ id: "p", kind: "plan", ordinal: 1, state: "completed" }),
		step({
			id: "w",
			kind: "work",
			ordinal: 2,
			state: "completed",
			sessionId: "sess-worker",
			attempts: [{ id: "a1", attemptNumber: 1, startedAt: T0, harness: "claude-code", model: "opus" }],
		}),
		step({ id: "r", kind: "review", ordinal: 3, state: "completed", reviewer: "codex", verdict: "changes_requested" }),
		step({
			id: "f",
			kind: "fix",
			ordinal: 4,
			state: "running",
			fixDelivery: { sessionId: "sess-worker", cycleNumber: 1 } as Step["fixDelivery"],
		}),
		step({ id: "v", kind: "verify", ordinal: 5, state: "pending" }),
		step({ id: "adv", kind: "advance", ordinal: 6, state: "pending" }),
	];

	it("renders one node per real step, in order, without the advance bookkeeping step", () => {
		const tree = sessionTree(detail({ steps }));
		expect(tree.map((node) => node.role)).toEqual(["planner", "worker", "reviewer", "fix", "verify"]);
	});

	it("marks verify as AO's own checks, with no agent session behind it", () => {
		const tree = sessionTree(detail({ steps }));
		expect(tree.find((node) => node.role === "verify")?.agentSession).toBe(false);
		expect(tree.filter((node) => node.agentSession).map((node) => node.role)).toEqual(["planner", "worker", "reviewer", "fix"]);
	});

	it("names the session and agent a step actually recorded, and nothing it did not", () => {
		const tree = sessionTree(detail({ steps }));
		const worker = tree.find((node) => node.role === "worker");
		expect(worker).toMatchObject({ sessionId: "sess-worker", harness: "claude-code", model: "opus" });
		expect(tree.find((node) => node.role === "reviewer")?.harness).toBe("codex");
		expect(tree.find((node) => node.role === "fix")?.sessionId).toBe("sess-worker");
		expect(tree.find((node) => node.role === "planner")?.harness).toBeUndefined();
	});

	it("reports the running step as the active agent, and nobody on a terminal run", () => {
		expect(activeAgent(detail({ steps }))?.role).toBe("fix");
		expect(activeAgent(detail({ steps, run: { state: "completed" } }))).toBeUndefined();
	});

	it("invents no nodes for a run that never got past work", () => {
		const tree = sessionTree(detail({ steps: [step({ id: "w", kind: "work", ordinal: 1, state: "running" })] }));
		expect(tree.map((node) => node.role)).toEqual(["worker"]);
	});
});

describe("reviewSummary and fix cycles", () => {
	it("reads the verdict of the latest review step", () => {
		const d = detail({
			run: { maxFixCycles: 3 },
			steps: [
				step({ id: "r1", kind: "review", ordinal: 2, state: "completed", verdict: "changes_requested", reviewer: "codex" }),
				step({ id: "f", kind: "fix", ordinal: 3, state: "completed", fixDelivery: { cycleNumber: 2 } as Step["fixDelivery"] }),
				step({ id: "r2", kind: "review", ordinal: 4, state: "completed", verdict: "approved", reviewer: "codex" }),
			],
		});
		expect(reviewSummary(d)).toMatchObject({ outcome: "approved", reviewer: "codex", reviewSteps: 2, fixCycles: { spent: 2, max: 3 } });
	});

	it("distinguishes skipped, in progress, pending and unknown", () => {
		const one = (s: Partial<Step>) =>
			reviewSummary(detail({ steps: [step({ id: "r", kind: "review", ordinal: 1, state: "completed", ...s })] }))?.outcome;
		expect(one({ reviewPolicy: { decision: "skipped" } as Step["reviewPolicy"] })).toBe("skipped");
		expect(one({ state: "running" })).toBe("in_progress");
		expect(one({ state: "ready" })).toBe("pending");
		expect(one({ state: "failed" })).toBe("unknown");
		expect(one({ state: "completed" })).toBe("unknown");
	});

	it("never names a reviewer the daemon did not", () => {
		const d = detail({ steps: [step({ id: "r", kind: "review", ordinal: 1, state: "running" })] });
		expect(reviewSummary(d)?.reviewer).toBeUndefined();
	});

	it("has no review summary for a run without a review step", () => {
		expect(reviewSummary(detail({ steps: [step({ id: "w", kind: "work", ordinal: 1, state: "running" })] }))).toBeUndefined();
	});

	it("counts fix cycles from the delivery ledger, not from attempts, and says unknown when it cannot", () => {
		expect(fixCycleSummary(detail({ steps: [] }))).toEqual({ spent: 0, max: null });
		const redelivered = detail({
			run: { maxFixCycles: 3 },
			steps: [
				step({
					id: "f",
					kind: "fix",
					ordinal: 1,
					state: "running",
					// Three attempts, one cycle: a re-delivery is not a new cycle.
					attempts: [
						{ id: "a1", attemptNumber: 1, startedAt: T0 },
						{ id: "a2", attemptNumber: 2, startedAt: T0 },
						{ id: "a3", attemptNumber: 3, startedAt: T0 },
					],
					fixDelivery: { cycleNumber: 1 } as Step["fixDelivery"],
				}),
			],
		});
		expect(fixCycleSummary(redelivered)).toEqual({ spent: 1, max: 3 });
		const legacy = detail({
			steps: [step({ id: "f", kind: "fix", ordinal: 1, state: "completed", attempts: [{ id: "a", attemptNumber: 1, startedAt: T0 }] })],
		});
		expect(fixCycleSummary(legacy).spent).toBeNull();
	});
});

describe("verifySummary", () => {
	const failed = step({
		id: "v",
		kind: "verify",
		ordinal: 5,
		state: "completed",
		verification: {
			passed: false,
			checks: [
				{ kind: "command", label: "npm test", passed: false, exitCode: 1, failureReason: "exit 1\nlong stderr…" },
				{ kind: "file", label: "README.md", passed: true },
			],
			preFingerprint: "a",
			postFingerprint: "a",
			reviewedFingerprint: "a",
			targetKey: "k",
			version: "v1",
		},
	});

	it("reports passed/failed from the recorded result, with a bounded first-line reason", () => {
		const summary = verifySummary(detail({ steps: [failed] }));
		expect(summary?.outcome).toBe("failed");
		expect(summary?.failedCount).toBe(1);
		expect(summary?.checks[0]?.reason).toBe("exit 1");
		expect(summary?.checks[1]?.reason).toBeUndefined();
	});

	it("reports pending and running from the step state when there is no result yet", () => {
		expect(verifySummary(detail({ steps: [step({ id: "v", kind: "verify", ordinal: 1, state: "pending" })] }))?.outcome).toBe("pending");
		expect(verifySummary(detail({ steps: [step({ id: "v", kind: "verify", ordinal: 1, state: "running" })] }))?.outcome).toBe("running");
		expect(verifySummary(detail({ steps: [step({ id: "v", kind: "verify", ordinal: 1, state: "completed" })] }))?.outcome).toBe("unknown");
	});

	it("says a verify failure caused a fix cycle only when a durable record says so", () => {
		expect(verifySummary(detail({ steps: [failed] }))?.triggeredFix).toBe(false);
		const withFix = detail({
			steps: [
				failed,
				step({ id: "f", kind: "fix", ordinal: 6, state: "running", fixDelivery: { findingsSource: "verification", cycleNumber: 1 } as Step["fixDelivery"] }),
			],
		});
		expect(verifySummary(withFix)?.triggeredFix).toBe(true);
	});
});

describe("attentionReport: what happened, what AO knows, what it could not determine", () => {
	it("is absent for a run that is not stopped", () => {
		expect(attentionReport(detail({ presentation: { stage: "working" } }))).toBeUndefined();
	});

	it("states the evidence it holds and the action AO recommends", () => {
		const d = detail({
			run: { state: "needs_attention", attentionReason: "fix_budget_exhausted" },
			presentation: {
				stage: "needs_attention",
				summaryCode: "fix_budget_exhausted",
				recommendedAction: "continue",
				placement: { type: "isolated_worktree", executionBranch: "ao/wf-1", worktreePath: "/tmp/wt" } as NonNullable<WorkflowRunDetail["presentation"]>["placement"],
				technical: { sessionId: "sess-1", attemptNumber: 2, provider: "anthropic", lastEventPhase: "review_verdict", lastEventAt: T0, authority: "active" },
			},
			steps: [step({ id: "f", kind: "fix", ordinal: 3, state: "failed", sessionId: "sess-1" })],
			advice: { recommendedAction: "repair", requiresHuman: true } as WorkflowRunDetail["advice"],
		});
		const report = attentionReport(d);
		expect(report?.causeUndetermined).toBe(false);
		expect(report?.recommendedAction).toBe("repair");
		const labels = report?.known.map((row) => row.labelKey);
		expect(labels).toEqual(
			expect.arrayContaining([
				"cc.evidence.reason",
				"cc.evidence.step",
				"cc.evidence.session",
				"cc.evidence.attempt",
				"cc.evidence.lastCheckpoint",
				"cc.evidence.branch",
				"cc.evidence.worktree",
				"cc.evidence.authority",
			]),
		);
		expect(report?.known.find((row) => row.labelKey === "cc.evidence.step")?.textKey).toBe("cc.role.fix");
		expect(report?.unknowns).not.toContain("cc.unknown.cause");
	});

	it("says plainly that AO cannot determine the cause when its own code says so", () => {
		for (const reason of ["unclassified_stop", "review_state_ambiguous", "fix_generation_unprovable"]) {
			const report = attentionReport(detail({ run: { state: "needs_attention", attentionReason: reason } }));
			expect(report?.causeUndetermined, reason).toBe(true);
			expect(report?.unknowns).toContain("cc.unknown.cause");
		}
		const noReason = attentionReport(detail({ run: { state: "needs_attention" } }));
		expect(noReason?.causeUndetermined).toBe(true);
	});

	it("lists what is missing instead of guessing it", () => {
		const report = attentionReport(
			detail({
				run: { state: "needs_attention", attentionReason: "worker_blocked" },
				presentation: { stage: "needs_attention", technical: { authority: "legacy_unproven" } },
			}),
		);
		expect(report?.unknowns).toEqual(
			expect.arrayContaining(["cc.unknown.noSession", "cc.unknown.noSignal", "cc.unknown.authorityUnproven"]),
		);
	});
});

describe("incidentSignals: deterministic, evidence-only", () => {
	it("raises nothing for a terminal run", () => {
		expect(
			incidentSignals(detail({ run: { state: "failed", attentionReason: "fix_budget_exhausted" } })),
		).toEqual([]);
	});

	it("flags a quiet worker only past the shared threshold", () => {
		const at = (silentForSeconds: number) =>
			incidentSignals(
				detail({ run: { workerLiveness: { observed: true, silentForSeconds, sessionId: "sess-1" } } }),
			).map((signal) => signal.id);
		expect(at(LIVENESS_QUIET_THRESHOLD_SECONDS - 1)).not.toContain("worker_quiet");
		expect(at(LIVENESS_QUIET_THRESHOLD_SECONDS)).toContain("worker_quiet");
	});

	it("does not call an unobserved worker quiet", () => {
		expect(incidentSignals(detail({ run: { workerLiveness: { observed: false, silentForSeconds: 99_999 } } }))).toEqual([]);
	});

	it("names provider failures only for provider-shaped error classes", () => {
		const withError = (errorClass: string) =>
			incidentSignals(
				detail({
					steps: [
						step({
							id: "w",
							kind: "work",
							ordinal: 1,
							state: "running",
							attempts: [{ id: "a", attemptNumber: 1, startedAt: T0, outcome: "failed", harness: "codex", errorClass: errorClass as never }],
						}),
					],
				}),
			).map((signal) => signal.id);
		expect(withError("rate_limited")).toContain("provider_failed");
		expect(withError("test_failed")).not.toContain("provider_failed");
	});

	it("flags the ownership gap only when the daemon reports the authority as unproven", () => {
		const ids = (authority: "active" | "legacy_unproven") =>
			incidentSignals(detail({ presentation: { stage: "working", technical: { authority } } })).map((signal) => signal.id);
		expect(ids("active")).not.toContain("authority_unproven");
		expect(ids("legacy_unproven")).toContain("authority_unproven");
	});
});

describe("compactTimeline", () => {
	it("folds the durable timeline into worker → review → fix → review → verify → completed", () => {
		const chips = compactTimeline([
			{ at: T0, kind: "started" },
			{ at: T0, kind: "worker_launched", detail: "claude-code" },
			{ at: T0, kind: "provider_failed", detail: "claude-code rate_limited" },
			{ at: T0, kind: "worker_launched", detail: "codex" },
			{ at: T0, kind: "work_completed" },
			{ at: T0, kind: "review_started" },
			{ at: T0, kind: "review_verdict", detail: "changes_requested" },
			{ at: T0, kind: "fix_started" },
			{ at: T0, kind: "review_started" },
			{ at: T0, kind: "review_verdict", detail: "approved" },
			{ at: T0, kind: "verified" },
			{ at: T0, kind: "completed" },
		]);
		expect(chips.map((chip) => chip.phase)).toEqual(["created", "worker", "review", "fix", "review", "verify", "completed"]);
		expect(chips[1]).toMatchObject({ count: 2, failures: 1 });
		expect(chips[2]?.verdict).toBe("changes_requested");
		expect(chips[4]?.verdict).toBe("approved");
	});

	it("adds nothing the daemon did not send", () => {
		expect(compactTimeline(undefined)).toEqual([]);
		expect(compactTimeline([{ at: T0, kind: "started" }]).map((chip) => chip.phase)).toEqual(["created"]);
	});
});

describe("usageDigest: measured vs modelled vs unknown", () => {
	const ledger = (over: Record<string, unknown>) =>
		({
			tokens: {
				recorded: true,
				source: "provider_reported",
				totals: { input: 1000, output: 200, total: 1200, cacheRead: 0, cacheWrite: 0, events: 3, uncachedInput: 1000, reasoning: null },
				cost: { known: true, basis: "calculated", amount: 1.5, currency: "USD", unpricedModels: [] },
				budget: { state: "unset", tokenPercent: null, costPercent: null },
				...over,
			},
		}) as unknown as WorkflowRunDetail["usage"];

	it("labels provider-reported tokens and costs priced from them as measured", () => {
		const digest = usageDigest(ledger({}));
		expect(digest?.tokens).toBe("measured");
		expect(digest?.cost.certainty).toBe("measured");
		expect(digest?.inputTokens).toBe(1000);
	});

	it("labels estimated tokens, and costs priced from them, as modelled", () => {
		const digest = usageDigest(ledger({ source: "estimated" }));
		expect(digest?.tokens).toBe("modelled");
		expect(digest?.cost.certainty).toBe("modelled");
	});

	it("never turns an unknown cost or unrecorded tokens into a number", () => {
		const digest = usageDigest(ledger({ recorded: false, cost: { known: false, basis: "unknown", amount: 0, unpricedModels: ["sonnet"] } }));
		expect(digest?.tokens).toBe("unknown");
		expect(digest?.inputTokens).toBeUndefined();
		expect(digest?.cost.certainty).toBe("unknown");
		expect(digest?.cost.amount).toBeUndefined();
		expect(digest?.cost.unpricedModels).toEqual(["sonnet"]);
	});

	it("reports context only from an observable trajectory", () => {
		const unobservable = usageDigest(ledger({ dynamics: { recorded: true, trajectory: { observable: false, peakContextTokens: 5 } } }));
		expect(unobservable?.context).toBe("unknown");
		expect(unobservable?.contextPeak).toBeUndefined();
		const observable = usageDigest(
			ledger({
				dynamics: {
					recorded: true,
					trajectory: { observable: true, providerCalls: 12, lastContextTokens: 90_000, peakContextTokens: 120_000, growthTokens: 60_000 },
				},
			}),
		);
		expect(observable).toMatchObject({ context: "measured", calls: 12, contextCurrent: 90_000, contextPeak: 120_000, contextGrowth: 60_000 });
	});

	it("has a certainty for every token source", () => {
		expect(tokenCertainty("provider_reported", true)).toBe("measured");
		expect(tokenCertainty("estimated", true)).toBe("modelled");
		expect(tokenCertainty("unknown", true)).toBe("unknown");
		expect(tokenCertainty("provider_reported", false)).toBe("unknown");
	});
});

describe("runDurationSeconds", () => {
	it("stops the clock at completion and runs it while the run is live", () => {
		const done = detail({ run: { state: "completed", completedAt: "2026-09-01T10:10:00Z" } });
		expect(runDurationSeconds(done, Date.parse("2026-09-01T12:00:00Z"))).toBe(600);
		const live = detail({ run: { state: "running" } });
		expect(runDurationSeconds(live, Date.parse("2026-09-01T10:05:00Z"))).toBe(300);
	});
});
