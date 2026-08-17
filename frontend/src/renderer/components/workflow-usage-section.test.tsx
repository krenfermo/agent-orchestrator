import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { WorkflowUsageSection } from "./workflow-usage-section";
import type { components } from "../../api/schema";

type WorkflowUsageResponse = components["schemas"]["ControllersWorkflowUsageResponse"];

describe("WorkflowUsageSection", () => {
	it("renders 'Unknown' for token fields, never '0', when nothing was observed", () => {
		const usage: WorkflowUsageResponse = {
			roles: [
				{
					role: "worker",
					stepKind: "work",
					harness: "codex",
					provider: "openai",
					model: "gpt-5.6-sol",
					durationMs: 4200,
					usage: { sessionId: "sess-1", incomplete: false, totals: { inputTokens: null, uncachedInputTokens: null, cacheReadTokens: null, cacheWriteTokens: null, outputTokens: null, reasoningTokens: null }, harnesses: [] },
				},
			],
			metrics: {
				attempts: 1,
				fixCycles: 0,
				reviewRuns: 0,
				reviewsSkipped: false,
				inputTokens: null,
				outputTokens: null,
				cachedTokens: null,
				tokensCertainty: "unknown",
			},
			advisory: { recommendation: "REUSE", reason: "fresh session, no fix cycles", signals: ["session created for this task's own work step"] },
			checkpoint: { objective: "ship the thing" },
			decisions: {
				questionsAsked: 0,
				policyResolved: 0,
				technicalResolved: 0,
				humanRequired: 0,
				resolverFailed: 0,
				waitingForCapacity: 0,
			},
		};

		render(<WorkflowUsageSection usage={usage} />);

		// "Unknown" must appear for every token field (role-level and
		// task-level) — real zero counts elsewhere (fixCycles: 0, reviewRuns:
		// 0) are legitimate and unrelated to the token-fabrication rule.
		expect(screen.getAllByText("Unknown").length).toBeGreaterThanOrEqual(6);
		expect(screen.getByText("Reuse session")).toBeInTheDocument();
	});

	it("renders real numbers when usage telemetry is actually known", () => {
		const usage: WorkflowUsageResponse = {
			roles: [
				{
					role: "worker",
					stepKind: "work",
					harness: "codex",
					provider: "openai",
					durationMs: 1500,
					usage: { sessionId: "sess-1", incomplete: false, totals: { inputTokens: 1000, uncachedInputTokens: 1000, cacheReadTokens: 50, cacheWriteTokens: 0, outputTokens: 200, reasoningTokens: null }, harnesses: [] },
				},
			],
			metrics: {
				attempts: 2,
				fixCycles: 1,
				reviewRuns: 1,
				reviewsSkipped: false,
				inputTokens: 1000,
				outputTokens: 200,
				cachedTokens: 50,
				tokensCertainty: "actual",
			},
			advisory: { recommendation: "CONSIDER_COMPACTION", reason: "one fix cycle has run", signals: [] },
			checkpoint: { objective: "ship the thing" },
			decisions: {
				questionsAsked: 0,
				policyResolved: 0,
				technicalResolved: 0,
				humanRequired: 0,
				resolverFailed: 0,
				waitingForCapacity: 0,
			},
		};

		render(<WorkflowUsageSection usage={usage} />);
		expect(screen.getAllByText("1,000").length).toBeGreaterThan(0);
		expect(screen.getByText("Consider compaction")).toBeInTheDocument();
	});
});
