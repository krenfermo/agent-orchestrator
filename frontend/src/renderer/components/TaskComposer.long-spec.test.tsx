import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// TaskComposer.long-spec.test.tsx — the composer half of the long-specification
// work.
//
// The bug was a limit, not a widget: the task field has always been a
// textarea, but the daemon refused a brief over 4096 bytes with TASK_TOO_LONG
// and the composer said nothing about the ceiling until the request came back.
// So these tests are about a real specification GOING THROUGH UNCHANGED, about
// the ceiling being visible before you submit, and about the file route out
// when a specification is longer than any form should carry.

const h = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), capture: vi.fn() }));

vi.mock("../hooks/useAgentsQuery", () => ({
	agentsQueryKey: ["agents"],
	agentsQueryOptions: { queryKey: ["agents"], queryFn: async () => ({}) },
	refreshAgents: vi.fn(),
	refreshAgentsIfStale: vi.fn(async () => undefined),
}));

vi.mock("./CreateProjectAgentSheet", () => ({
	RequiredAgentField: ({ value }: { value: string }) => (
		<button type="button" aria-label="Agent" data-value={value} />
	),
}));

vi.mock("../lib/api-client", () => ({
	apiClient: { GET: h.get, POST: h.post },
	apiErrorCode: (error: { code?: string }) => error?.code,
	apiErrorMessage: (_e: unknown, fallback = "err") => fallback,
}));

vi.mock("../lib/telemetry", () => ({ captureRendererEvent: h.capture }));

import { TaskComposer } from "./TaskComposer";
import { MAX_TASK_SPECIFICATION_BYTES, specificationByteLength } from "../../shared/task-specification";

function Wrap({ children }: { children: ReactNode }) {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

const task = () => screen.getByRole("textbox", { name: "Task" });
const start = () => screen.getByRole("button", { name: "Start task" });

/** The shape of the brief that provoked this work: a full RBAC specification. */
const RBAC_BLOCK = [
	"OBJETIVO",
	"",
	"Implementar control de acceso basado en roles (RBAC) en MEDUSA.",
	"",
	"ALCANCE",
	"",
	"- Modelo de roles, permisos y asignaciones",
	"- Middleware de autorización en cada ruta protegida",
	"",
	"RESTRICCIONES",
	"",
	"```sql",
	"CREATE TABLE role_assignments (...);",
	"```",
	"",
	"CRITERIOS DE ACEPTACIÓN",
	"",
	"1. Un usuario sin rol no accede a ninguna ruta protegida",
	"2. La migración es reversible",
	"",
	"NO HACER",
	"",
	"No introducir un proveedor de identidad externo.",
	"",
].join("\n");

/** Grows the specification to roughly the requested number of bytes. */
function rbacSpecification(approxBytes: number): string {
	let out = "";
	while (specificationByteLength(out) < approxBytes) out += RBAC_BLOCK;
	return out;
}

function renderComposer(onCreated = vi.fn()) {
	render(
		<Wrap>
			<TaskComposer projectId="proj-1" onCreated={onCreated} />
		</Wrap>,
	);
}

beforeEach(() => {
	h.get.mockImplementation(async (path: string) => {
		if (path.includes("/models")) {
			return {
				data: { agent: "codex", selectionMode: "text", models: [], allowCustom: true, refreshRecommended: false },
			};
		}
		return { data: { status: "ok", project: { config: {} } } };
	});
});

afterEach(() => {
	h.get.mockReset();
	h.post.mockReset();
	h.capture.mockReset();
});

describe("Task specification size", () => {
	it("sends a multi-thousand-word RBAC specification unchanged", async () => {
		const spec = rbacSpecification(30_000);
		h.post.mockResolvedValueOnce({ data: { workerId: "sess-rbac" } });
		renderComposer();

		fireEvent.change(task(), { target: { value: spec } });
		// The textarea itself must not clip it — the value round-trips whole.
		expect(task()).toHaveValue(spec);
		fireEvent.click(start());

		await waitFor(() => expect(h.post).toHaveBeenCalledTimes(1));
		const body = h.post.mock.calls[0][1].body as { brief: string };
		expect(body.brief).toBe(spec);
		// Structure, multibyte content and fences all survive: the failure this
		// replaces was a refusal, and the failure that must not replace it is
		// silent alteration.
		expect(body.brief).toContain("\n\n");
		expect(body.brief).toContain("CRITERIOS DE ACEPTACIÓN");
		expect(body.brief).toContain("```sql");
	});

	it("hides the counter for a short task and shows it once a specification is long", () => {
		renderComposer();

		fireEvent.change(task(), { target: { value: "Fix the flaky checkout test" } });
		expect(screen.queryByText(/\/ 131,072 bytes/)).not.toBeInTheDocument();

		fireEvent.change(task(), { target: { value: "x".repeat(3000) } });
		expect(screen.getByText("3,000 / 131,072 bytes")).toBeInTheDocument();
		expect(start()).toBeEnabled();
	});

	it("refuses over the maximum, says how much is over, and does not submit", () => {
		renderComposer();

		fireEvent.change(task(), { target: { value: "x".repeat(MAX_TASK_SPECIFICATION_BYTES + 500) } });

		// The refusal names the overage rather than a generic "too long".
		expect(screen.getByText(/500 bytes over the 131,072-byte maximum/)).toBeInTheDocument();
		expect(screen.getByText(/Nothing is truncated/)).toBeInTheDocument();
		expect(start()).toBeDisabled();
		expect(task()).toHaveAttribute("aria-invalid", "true");

		fireEvent.click(start());
		expect(h.post).not.toHaveBeenCalled();
	});

	it("offers the file route out, and moves the specification into it verbatim", async () => {
		const spec = rbacSpecification(MAX_TASK_SPECIFICATION_BYTES + 1000);
		h.post.mockResolvedValueOnce({ data: { workerId: "sess-attached" } });
		renderComposer();

		fireEvent.change(task(), { target: { value: spec } });
		fireEvent.click(screen.getByRole("button", { name: "Attach as file" }));

		// The brief becomes the line that points the agent at the file, and the
		// file appears in the attachment list. Nothing was truncated: the text
		// changed container, it did not lose content.
		await waitFor(() => expect(screen.getByText("task-specification.md")).toBeInTheDocument());
		expect(task()).toHaveValue(
			"The full specification is attached as a Markdown file. Read the attached file before starting.",
		);
		expect(start()).toBeEnabled();

		fireEvent.click(start());
		await waitFor(() => expect(h.post).toHaveBeenCalledTimes(1));
		const body = h.post.mock.calls[0][1].body as {
			brief: string;
			attachments: { mimeType: string; data: string }[];
		};
		expect(body.attachments).toHaveLength(1);
		expect(body.attachments[0].mimeType).toBe("text/markdown");
		// Byte-for-byte: what the daemon writes into the worktree is the
		// specification the author wrote.
		const decoded = new TextDecoder().decode(
			Uint8Array.from(atob(body.attachments[0].data), (c) => c.charCodeAt(0)),
		);
		expect(decoded).toBe(spec);
		// The brief must NOT name a path. The upload carries only a mime type
		// and bytes, so the daemon names the file itself under .ao/attachments
		// and appends that real reference; a brief that quoted
		// "task-specification.md" would point the agent at a file that does
		// not exist. The name survives only as the composer's display label.
		expect(body.brief).not.toContain("task-specification.md");
		expect(body.brief).toContain("attached");
	});

	it("counts UTF-8 bytes rather than characters", () => {
		// Three bytes per rune: a limit counted in characters would let a
		// Spanish or Japanese specification through at up to three times the
		// bytes the daemon actually allows.
		expect(specificationByteLength("日本語")).toBe(9);
		expect(specificationByteLength("abc")).toBe(3);
		expect(specificationByteLength("ñ")).toBe(2);
	});

	it("agrees with the daemon's published limit for both task entry points", () => {
		// The UI check is a courtesy and the daemon is the authority, so the
		// two must not drift: a composer that accepted more than the daemon
		// does would produce a refusal the author could not have predicted.
		const spec = readFileSync(
			resolve(process.cwd(), "../backend/internal/httpd/apispec/openapi.yaml"),
			"utf8",
		);
		// objective (workflow), brief (delegate) and prompt (spawn) — the three
		// places a task specification enters AO — all carry the same ceiling.
		const occurrences = spec.split(`maxLength: ${MAX_TASK_SPECIFICATION_BYTES}`).length - 1;
		expect(occurrences).toBeGreaterThanOrEqual(3);
	});
});
