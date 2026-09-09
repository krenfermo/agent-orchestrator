import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nextProvider } from "react-i18next";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { components } from "../../api/schema";
import { createAppI18n } from "../i18n/instance";
import { aoBridge } from "../lib/bridge";
import { WorkflowDiagnosticsButton } from "./workflow-diagnostics-button";

type WorkflowRunDetailView = components["schemas"]["WorkflowRunDetailView"];

const detail = {
	run: {
		id: "wf-1234abcd",
		projectId: "proj-7",
		objective: "Fix the flaky checkout test",
		state: "needs_attention",
		phase: "blocked",
		executionMode: "autonomous",
		canContinue: false,
		createdAt: "2026-09-01T10:00:00.000Z",
		lastActivityAt: "2026-09-01T10:30:00.000Z",
		attentionReason: "verify_ambiguous",
	},
	steps: [],
} as unknown as WorkflowRunDetailView;

function renderButton(locale: "en" | "es" = "en") {
	return render(
		<I18nextProvider i18n={createAppI18n(locale)}>
			<WorkflowDiagnosticsButton detail={detail} />
		</I18nextProvider>,
	);
}

afterEach(() => {
	vi.restoreAllMocks();
});

describe("WorkflowDiagnosticsButton", () => {
	it("copies the run's diagnostics in one action, so no terminal is needed", async () => {
		const write = vi.spyOn(aoBridge.clipboard, "writeText").mockResolvedValue(undefined);
		renderButton();

		await userEvent.click(screen.getByRole("button", { name: "Copy diagnostics" }));

		expect(write).toHaveBeenCalledTimes(1);
		const copied = write.mock.calls[0][0];
		expect(copied).toContain("run: wf-1234abcd");
		expect(copied).toContain("attentionReason: verify_ambiguous");
		await waitFor(() => expect(screen.getByRole("button", { name: "Copied" })).toBeInTheDocument());
	});

	// A clipboard the platform refused has to say so. The silent version leaves
	// somebody pasting whatever was there before.
	it("reports a refused clipboard instead of falsely confirming", async () => {
		vi.spyOn(aoBridge.clipboard, "writeText").mockRejectedValue(new Error("denied"));
		renderButton();

		await userEvent.click(screen.getByRole("button", { name: "Copy diagnostics" }));

		await waitFor(() =>
			expect(screen.getByText("Could not copy to the clipboard.")).toBeInTheDocument(),
		);
		expect(screen.queryByRole("button", { name: "Copied" })).not.toBeInTheDocument();
	});

	it("is localized", () => {
		renderButton("es");
		expect(screen.getByRole("button", { name: "Copiar diagnóstico" })).toBeInTheDocument();
	});
});
