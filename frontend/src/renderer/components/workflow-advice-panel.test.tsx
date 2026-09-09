import { render, screen } from "@testing-library/react";
import { I18nextProvider } from "react-i18next";
import { describe, expect, it } from "vitest";
import { createAppI18n } from "../i18n/instance";
import { WorkflowAdvicePanel, type WorkflowAdvice } from "./workflow-advice-panel";

/**
 * The regression these tests exist for is not a crash: it is a *silence*. The
 * daemon has always sent `advice` on every run read and the renderer discarded
 * it, so "why can't I press Repair" had no answer anywhere in the product and
 * the user went to SQLite to find one.
 *
 * So each test below asserts that a specific durable fact reaches the screen —
 * never that the component decided something, because it decides nothing.
 */

function renderPanel(value: WorkflowAdvice | undefined, locale: "en" | "es" = "en") {
	return render(
		<I18nextProvider i18n={createAppI18n(locale)}>
			<WorkflowAdvicePanel advice={value} />
		</I18nextProvider>,
	);
}

function advice(overrides: Partial<WorkflowAdvice> = {}): WorkflowAdvice {
	return {
		category: "human_action",
		requiresHuman: true,
		automaticActionActive: false,
		repairSpent: 0,
		repairBudget: 0,
		repairable: false,
		retryable: false,
		authority: {},
		...overrides,
	};
}

describe("WorkflowAdvicePanel", () => {
	it("renders nothing when the daemon sent no advice", () => {
		const { container } = renderPanel(undefined);
		expect(container).toBeEmptyDOMElement();
	});

	it("states the category so a run that needs nobody says so", () => {
		renderPanel(advice({ category: "no_action_required", requiresHuman: false }));
		expect(screen.getByText("Nothing for you to do")).toBeInTheDocument();
	});

	// The core of P3-C §2: a refused action is REPORTED with its reason. Before
	// this panel the action simply was not rendered, which is why "where did the
	// button go" was unanswerable without reading the run's durable state.
	it("names every refused action together with the daemon's reason", () => {
		renderPanel(
			advice({
				blockedActions: [
					{ action: "repair", reason: "automatic_repair_pending" },
					{ action: "continue", reason: "not_recoverable" },
				],
			}),
		);
		expect(screen.getByText("Repair automatically — AO is about to repair this itself")).toBeInTheDocument();
		expect(screen.getByText("Continue — this stop cannot be resumed")).toBeInTheDocument();
	});

	it("says what AO will do by itself, and marks it when it is already running", () => {
		renderPanel(
			advice({ category: "auto_recoverable", automaticAction: "repair_in_flight", automaticActionActive: true }),
		);
		expect(screen.getByText(/finish the repair it started/)).toBeInTheDocument();
		expect(screen.getByText(/already under way/)).toBeInTheDocument();
	});

	it("says why AO will NOT act when it is holding back", () => {
		renderPanel(advice({ automaticActionBlockedReason: "repair_requires_authorization" }));
		expect(screen.getByText("this repair needs your authorization")).toBeInTheDocument();
	});

	it("reports the repair budget rather than only whether repair is possible", () => {
		renderPanel(advice({ repairable: true, repairEligibility: "eligible", repairSpent: 1, repairBudget: 3 }));
		expect(screen.getByText("Available (1 of 3 used)")).toBeInTheDocument();
	});

	// A code the daemon adds before the renderer has copy for it must appear as
	// itself. Rendering an empty cell would hide a fact AO went to the trouble
	// of computing.
	it("falls back to the raw code for a vocabulary the renderer does not know yet", () => {
		renderPanel(advice({ blockedActions: [{ action: "teleport", reason: "no_flux" }] }));
		expect(screen.getByText("teleport — no_flux")).toBeInTheDocument();
	});

	it("localizes the whole panel, so a Spanish user is not sent back to English codes", () => {
		renderPanel(
			advice({
				category: "human_action",
				blockedActions: [{ action: "repair", reason: "repair_exhausted" }],
			}),
			"es",
		);
		expect(screen.getByText("Necesita una decisión tuya")).toBeInTheDocument();
		expect(
			screen.getByText("Reparar automáticamente — se agotó el presupuesto de reparación"),
		).toBeInTheDocument();
	});
});
