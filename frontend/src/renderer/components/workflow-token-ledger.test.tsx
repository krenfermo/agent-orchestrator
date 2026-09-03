import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { WorkflowTokenLedger } from "./workflow-token-ledger";
import type { components } from "../../api/schema";

type Ledger = components["schemas"]["ControllersWorkflowUsageLedgerResponse"];
type Tokens = components["schemas"]["ControllersUsageTokenTotalsResponse"];
type Cost = components["schemas"]["ControllersUsageCostResponse"];

function tokens(total: number, overrides: Partial<Tokens> = {}): Tokens {
	return {
		input: total, uncachedInput: total, cacheRead: 0, cacheWrite: 0,
		output: 0, reasoning: null, total, events: 1, ...overrides,
	};
}

const unknownCost: Cost = { known: false, basis: "unknown", currency: "", amount: 0 };
const knownCost: Cost = {
	known: true, basis: "calculated", currency: "USD", amount: 0.42,
	pricingSource: "anthropic-list-price", pricingVersion: "2026-06-24", effectiveDate: "2026-06-24",
};

function ledger(overrides: Partial<Ledger> = {}): Ledger {
	return {
		workflowRunId: "wf-1", recorded: true, source: "provider_reported",
		// Every provider-backed role reported: the default fixture is a complete
		// bill, and the incomplete case is asserted explicitly below.
		complete: true,
		totals: tokens(34_000), cost: knownCost,
		baseTokens: tokens(34_000), repairTokens: tokens(0), baseCost: knownCost, repairCost: unknownCost,
		roles: [], models: [], providers: [],
		familyTotals: tokens(34_000), familyCost: knownCost,
		approximateEvents: 0, totalEvents: 12,
		budget: { state: "unset", scope: "run", warnPercent: 80, tokensUsed: 0, tokenPercent: null, costPercent: null, parentScoped: true },
		...overrides,
	};
}

describe("WorkflowTokenLedger", () => {
	it("says 'no usage data recorded' rather than rendering zeroes", () => {
		// A legacy run, or one whose provider AO cannot meter. Zero would be a
		// claim that the run was free; it is an absence, not a measurement.
		render(<WorkflowTokenLedger ledger={ledger({ recorded: false, totals: tokens(0), cost: unknownCost })} />);
		expect(screen.getByText(/No usage data recorded/i)).toBeTruthy();
		expect(screen.queryByTestId("usage-ledger-total")).toBeNull();
	});

	it("prints a provider-reported total bare and marks an estimated one", () => {
		const { rerender } = render(<WorkflowTokenLedger ledger={ledger()} />);
		expect(screen.getByTestId("usage-ledger-total").textContent).toContain("34.0k");
		expect(screen.getByTestId("usage-ledger-total").textContent).not.toContain("~");

		rerender(<WorkflowTokenLedger ledger={ledger({ source: "estimated" })} />);
		expect(screen.getByTestId("usage-ledger-total").textContent).toContain("~34.0k");
	});

	it("never renders an unknown cost as $0.00", () => {
		render(<WorkflowTokenLedger ledger={ledger({ cost: unknownCost })} />);
		const total = screen.getByTestId("usage-ledger-total").textContent ?? "";
		expect(total).toContain("cost unknown");
		expect(total).not.toContain("0.00");
	});

	it("shows repair spend beside base execution rather than folded into it", () => {
		render(
			<WorkflowTokenLedger
				ledger={ledger({ totals: tokens(58_000), baseTokens: tokens(40_000), repairTokens: tokens(18_000) })}
			/>,
		);
		const split = screen.getByTestId("usage-repair-split").textContent ?? "";
		expect(split).toContain("40.0k");
		expect(split).toContain("18.0k");
	});

	it("names roles that have not reported yet instead of omitting them", () => {
		// Omitting the reviewer would let the total read as the whole bill. The
		// reviewer CAN be metered now, so this is a pending report rather than a
		// permanent blind spot — and either way it is never rendered as zero.
		render(
			<WorkflowTokenLedger
				ledger={ledger({
					complete: false,
					incompleteReason: "awaiting provider report from: reviewer",
					unobservable: [
						{
							role: "reviewer", cycle: 0, repair: false, attemptOrdinal: 0,
							tokens: tokens(0), cost: unknownCost, source: "unknown",
							observable: false, unobservableReason: "awaiting_provider_report",
						},
					],
				})}
			/>,
		);
		const notice = screen.getByText(/Awaiting a provider report/i).textContent ?? "";
		expect(notice).toContain("Reviewer");
		expect(notice).toContain("lower bound");
		// And never a zero for the role that has not reported.
		expect(notice).not.toContain("0 tokens");
	});

	it("shows no pending-report notice once every role has reported", () => {
		render(<WorkflowTokenLedger ledger={ledger({ complete: true })} />);
		expect(screen.queryByText(/Awaiting a provider report/i)).toBeNull();
	});

	it("draws no budget meter when no ceiling is configured", () => {
		render(<WorkflowTokenLedger ledger={ledger()} />);
		expect(screen.queryByText(/Within budget|Budget exhausted|Approaching the budget/)).toBeNull();
	});

	it("warns, then blocks, as the budget is crossed", () => {
		const { rerender } = render(
			<WorkflowTokenLedger
				ledger={ledger({
					budget: { state: "warning", scope: "run", warnPercent: 80, tokensUsed: 82_000, tokenPercent: 82, costPercent: null, parentScoped: true },
				})}
			/>,
		);
		expect(screen.getByText(/Approaching the budget/)).toBeTruthy();
		expect(screen.getByText(/82% used/)).toBeTruthy();

		rerender(
			<WorkflowTokenLedger
				ledger={ledger({
					budget: { state: "exhausted", scope: "family", warnPercent: 80, tokensUsed: 210_000, tokenPercent: 105, costPercent: null, parentScoped: true },
				})}
			/>,
		);
		expect(screen.getByText(/Budget exhausted/)).toBeTruthy();
		expect(screen.getByText(/will not start new work/i)).toBeTruthy();
	});

	it("labels assembled context as AO's own estimate and never as provider tokens", () => {
		render(
			<WorkflowTokenLedger
				ledger={ledger({
					context: {
						recorded: true, dispatches: 3, unmeasured: 0, skippedRecords: 0, complete: true,
						assembledBytes: 40_000, estimatedAssembledTokens: 10_000,
						avoidedAssembledBytes: 44_000, estimatedAvoidedTokens: 11_000, avoidedComparable: true,
						memory: {
							generation: 7, provider: "LocalGraph", mode: "preferred",
							packItems: 12, packBytes: 6_000, estimatedPackTokens: 1_500,
							candidates: 30, rejectedByBudget: 4, staleExcluded: 1,
							cacheHits: 2, cacheMisses: 1, syncs: 1, fullSyncs: 0, incrementalSyncs: 1,
							noOpSyncs: 2, syncFilesRead: 3,
							sharedCandidates: 9, sharedSelected: 4, sharedExcluded: 5,
							taskLocalItems: 2, workflowLocalItems: 1, canonicalItems: 1,
						},
						estimateMethod: "utf8 bytes / 4 heuristic (no provider tokenizer)",
						basis: "ao_assembled",
					},
				})}
			/>,
		);
		const block = screen.getByTestId("usage-context-block").textContent ?? "";
		expect(block).toContain("Context AO assembled");
		expect(block).toContain("Not the provider's input token count");
		// The saving claim, in the only words it is entitled to.
		expect(block).toContain("Estimated AO context avoided");
		expect(block).toContain("~11.0k tokens");
		// And the graph backend is named honestly.
		expect(block).toContain("LocalGraph");
		expect(block).not.toContain("Graphify");
	});

	it("claims no saving at all when there is no comparable baseline", () => {
		render(
			<WorkflowTokenLedger
				ledger={ledger({
					context: {
						recorded: true, dispatches: 1, unmeasured: 0, skippedRecords: 0, complete: true,
						assembledBytes: 1_000, estimatedAssembledTokens: 250,
						avoidedAssembledBytes: 0, estimatedAvoidedTokens: 0, avoidedComparable: false,
						memory: {
							generation: 0, provider: "LocalGraph", packItems: 0, packBytes: 0,
							estimatedPackTokens: 0, candidates: 0, rejectedByBudget: 0, staleExcluded: 0,
							cacheHits: 0, cacheMisses: 0, syncs: 0, fullSyncs: 0, incrementalSyncs: 0,
							noOpSyncs: 0, syncFilesRead: 0, sharedCandidates: 0, sharedSelected: 0,
							sharedExcluded: 0, taskLocalItems: 0, workflowLocalItems: 0, canonicalItems: 0,
						},
						estimateMethod: "utf8 bytes / 4 heuristic (no provider tokenizer)",
						basis: "ao_assembled",
					},
				})}
			/>,
		);
		const block = screen.getByTestId("usage-context-block").textContent ?? "";
		expect(block).toContain("No comparable baseline");
		expect(block).not.toContain("avoided: 0");
	});

	it("says how much of the role breakdown rests on a fallback attribution", () => {
		render(<WorkflowTokenLedger ledger={ledger({ approximateEvents: 3, totalEvents: 12 })} />);
		expect(screen.getByText(/3 of 12 events carried no provider timestamp/)).toBeTruthy();
	});
});
