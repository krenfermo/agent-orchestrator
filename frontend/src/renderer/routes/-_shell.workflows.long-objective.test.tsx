import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { MAX_OBJECTIVE_BYTES, objectiveByteLength, WorkflowsList } from "./_shell.workflows";

// -_shell.workflows.long-objective.test.tsx — P2-E B6, at the widget.
//
// The bug this covers was never a validation bug: nothing rejected long text.
// The objective was a single-line <input>, and a browser strips the newlines
// when multi-line text is pasted into one — so a structured brief silently
// arrived as a run-on paragraph and no error was ever shown. These tests are
// therefore mostly about STRUCTURE SURVIVING, not about length.

const { useProjectsListMock, useWorkflowRunsMock, useExecutionPolicyMock, openGlobalSettingsMock } = vi.hoisted(() => ({
	useProjectsListMock: vi.fn(),
	useWorkflowRunsMock: vi.fn(),
	useExecutionPolicyMock: vi.fn(),
	openGlobalSettingsMock: vi.fn(),
}));

vi.mock("@tanstack/react-router", async (importOriginal) => {
	const actual = await importOriginal<typeof import("@tanstack/react-router")>();
	return {
		...actual,
		Link: ({ children, to }: { children: React.ReactNode; to: string }) => <a href={to}>{children}</a>,
	};
});
vi.mock("../hooks/useProjectsList", () => ({ useProjectsList: useProjectsListMock }));
vi.mock("../hooks/useWorkflowRuns", () => ({
	useWorkflowRuns: useWorkflowRunsMock,
	EXECUTION_STRATEGIES: ["task", "autonomous", "master"] as const,
	APPROVAL_POLICIES: ["automatic", "manual"] as const,
	REPAIR_POLICIES: ["disabled", "suggest", "automatic"] as const,
}));
vi.mock("../hooks/useExecutionPolicy", () => ({ useExecutionPolicy: useExecutionPolicyMock }));
vi.mock("../stores/ui-store", () => ({
	useUiStore: (selector: (state: { openGlobalSettings: typeof openGlobalSettingsMock }) => unknown) =>
		selector({ openGlobalSettings: openGlobalSettingsMock }),
}));

const PROJECTS = [
	{ id: "proj-a", name: "Project A", path: "/repos/a", kind: "single_repo" as const, sessionPrefix: "a", valid: true },
];

/** The shape a real Task brief has: sections, blank lines, markdown, non-ASCII. */
const SPECIFICATION = [
	"OBJETIVO",
	"",
	"Añadir un índice de documentación.",
	"",
	"ALCANCE",
	"",
	"- Sólo `docs/`",
	"- No modificar código",
	"",
	"CRITERIOS DE ACEPTACIÓN",
	"",
	"1. El archivo existe",
	"2. Enlaza documentos reales",
	"",
	"NO HACER",
	"",
	"No inventar documentos.",
].join("\n");

let createRun: ReturnType<typeof vi.fn>;

beforeEach(() => {
	vi.clearAllMocks();
	createRun = vi.fn().mockResolvedValue({});
	useWorkflowRunsMock.mockReturnValue({
		runs: [], isLoading: false, error: undefined, createRun, creating: false, createError: undefined,
	});
	useExecutionPolicyMock.mockReturnValue({ policy: { autonomousMode: false }, isLoading: false, error: undefined });
	useProjectsListMock.mockReturnValue({ projects: PROJECTS, isLoading: false, error: undefined });
});

async function selectProject() {
	await userEvent.click(screen.getByRole("combobox", { name: "Project" }));
	await userEvent.click(await screen.findByText("Project A"));
}

async function pasteObjective(text: string) {
	const field = screen.getByLabelText(/objective/i);
	field.focus();
	await userEvent.paste(text);
	return field;
}

describe("Task specification input", () => {
	it("is a real multiline textarea, not a single-line input", () => {
		render(<WorkflowsList />);
		const field = screen.getByLabelText(/objective/i);
		expect(field.tagName).toBe("TEXTAREA");
	});

	it("preserves newlines, blank lines and markdown when a specification is pasted", async () => {
		render(<WorkflowsList />);
		const field = (await pasteObjective(SPECIFICATION)) as HTMLTextAreaElement;

		// The regression in one assertion: a single-line input would have
		// collapsed every one of these.
		expect(field.value).toBe(SPECIFICATION);
		expect(field.value).toContain("\n\n");
		expect(field.value).toContain("- Sólo `docs/`");
		expect(field.value.split("\n").length).toBe(SPECIFICATION.split("\n").length);
	});

	it("does not truncate a large specification", async () => {
		const big = `${SPECIFICATION}\n`.repeat(200);
		render(<WorkflowsList />);
		const field = (await pasteObjective(big)) as HTMLTextAreaElement;
		expect(field.value.length).toBe(big.length);
		expect(objectiveByteLength(field.value)).toBeGreaterThan(10_000);
	});

	it("submits the specification unchanged", async () => {
		render(<WorkflowsList />);
		await selectProject();
		// Task is chosen explicitly: the point of this change is that a long
		// specification does NOT require Autonomous or Master, so the test has
		// to pick the strategy the feature is about rather than the form's
		// default.
		await userEvent.click(screen.getByRole("radio", { name: /task/i }));
		await pasteObjective(SPECIFICATION);
		await userEvent.click(screen.getByRole("button", { name: /create|crear/i }));

		expect(createRun).toHaveBeenCalledTimes(1);
		const sent = createRun.mock.calls[0][0] as { objective: string; strategy: string };
		expect(sent.objective).toBe(SPECIFICATION);
		// It is still a Task. Nothing about pasting a long brief changes the
		// strategy or reaches for a planner.
		expect(sent.strategy).toBe("task");
	});

	it("shows the size only once a specification is long, and refuses over the maximum", async () => {
		render(<WorkflowsList />);
		await selectProject();

		// Short: no counter, because a limit nobody is near is noise.
		await pasteObjective("Ship the thing");
		expect(screen.queryByText(/\/\s*131,?072 bytes/)).not.toBeInTheDocument();

		// Long but legal: the counter appears and the form still submits.
		const field = screen.getByLabelText(/objective/i) as HTMLTextAreaElement;
		await userEvent.clear(field);
		field.focus();
		await userEvent.paste("x".repeat(3000));
		expect(screen.getByText(/3,000 \/ 131,072 bytes/)).toBeInTheDocument();
		expect(screen.getByRole("button", { name: /create|crear/i })).toBeEnabled();

		// Over the maximum: an explicit refusal, and the form will not submit.
		await userEvent.clear(field);
		field.focus();
		await userEvent.paste("x".repeat(MAX_OBJECTIVE_BYTES + 1));
		expect(screen.getByText(/over the .* maximum|supera el máximo/i)).toBeInTheDocument();
		expect(screen.getByRole("button", { name: /create|crear/i })).toBeDisabled();
		expect(createRun).not.toHaveBeenCalled();
	});

	it("counts UTF-8 bytes rather than characters", () => {
		// Three bytes per rune: a limit counted in characters would let this
		// through at three times the bytes the daemon allows.
		expect(objectiveByteLength("日本語")).toBe(9);
		expect(objectiveByteLength("abc")).toBe(3);
	});

	it("agrees with the daemon's published limit", () => {
		// The UI check is a courtesy and the daemon is the authority, so the
		// two must not drift: a UI that accepted more would produce a refusal
		// the user could not have predicted.
		// Resolved from the repository root rather than from import.meta.url:
		// vitest does not give this module a file: URL.
		const spec = readFileSync(
			resolve(process.cwd(), "../backend/internal/httpd/apispec/openapi.yaml"),
			"utf8",
		);
		expect(spec).toContain(`maxLength: ${MAX_OBJECTIVE_BYTES}`);
	});
});
