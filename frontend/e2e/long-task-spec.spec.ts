import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent, installFakeIdentity } from "./support/fake-bridge";

// LONG-SPEC RENDERER SMOKE — the composer half of raising a Task's ceiling to
// domain.MaxWorkflowObjectiveBytes (128 KiB).
//
// TaskComposer.long-spec.test.tsx proves the logic in jsdom. What it cannot
// show is the thing this change is actually about: that a real browser, laying
// out a real dialog, holds a six-figure-byte specification in the textarea
// without clipping it, and puts the counter, the refusal and the way out where
// a person will actually see them.
//
// Same scope caveat as every spec in this directory: dev:web + fake bridge, so
// no daemon, no preload, no PTY. Green here means "the renderer renders it",
// not "the boundary works".

const MAX_BYTES = 131072;

/**
 * Sets the task field the way a specification actually arrives: pasted whole,
 * in one input event, rather than typed.
 *
 * Playwright's fill() is not usable at this size -- it spends ~90s on a
 * 10,500-line value, which measures fill() and not the composer. The same
 * value through a real input event costs the page ~66ms.
 */
async function paste(page: Page, value: string) {
	await page.getByRole("textbox", { name: "Task" }).focus();
	await page.evaluate((text) => {
		const el = document.activeElement as HTMLTextAreaElement;
		const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
		setter.call(el, text);
		el.dispatchEvent(new Event("input", { bubbles: true }));
	}, value);
}

/** The shape of the brief that provoked this work: a full RBAC specification. */
const RBAC_BLOCK = [
	"OBJETIVO",
	"",
	"Implementar control de acceso basado en roles (RBAC) en MEDUSA.",
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
	"",
].join("\n");

/** Grows the specification to at least `bytes` UTF-8 bytes. */
function rbacSpecification(bytes: number): string {
	const encoder = new TextEncoder();
	let out = "";
	while (encoder.encode(out).length < bytes) out += RBAC_BLOCK;
	return out;
}

test.beforeEach(async ({ page }) => {
	await installFakeIdentity(page);
});

async function openComposer(page: Page) {
	await installFakeAgent(page, { workers: [{ id: "w1", title: "A worker", status: "working" }] });
	// The New task control lives on the project board, not the orchestrator
	// root; fake-proj is the project the fake agent's snapshot provides.
	await page.goto("/#/projects/fake-proj");
	await page.getByRole("button", { name: "New task" }).first().click();
	await expect(page.getByRole("dialog")).toBeVisible();
	return page.getByRole("textbox", { name: "Task" });
}

test("renderer: a long specification is held whole and the counter appears @SPEC", async ({ page }) => {
	const task = await openComposer(page);

	// A short brief shows no counter: the limit is not something you should
	// have to think about while typing one line.
	await paste(page, "Fix the flaky checkout test");
	await expect(page.getByText(/\/ 131,072 bytes/)).toHaveCount(0);

	// A real specification: the counter appears and the field holds every
	// byte. inputValue() reads what the DOM actually has, so a clipped or
	// truncated textarea fails here.
	const spec = rbacSpecification(60_000);
	await paste(page, spec);
	expect(await task.inputValue()).toBe(spec);
	await expect(page.getByText(/\/ 131,072 bytes/)).toBeVisible();
	await expect(page.getByRole("button", { name: "Start task" })).toBeEnabled();
});

test("renderer: over the ceiling the refusal is visible and Start is disabled @SPEC", async ({ page }) => {
	const task = await openComposer(page);

	await paste(page, "x".repeat(MAX_BYTES + 500));

	// The refusal names the overage and promises no truncation, and it is on
	// screen rather than merely in the DOM.
	const alert = page.getByRole("alert");
	await expect(alert).toBeVisible();
	await expect(alert).toContainText("500 bytes over the 131,072-byte maximum");
	await expect(alert).toContainText("Nothing is truncated");
	await expect(task).toHaveAttribute("aria-invalid", "true");
	await expect(page.getByRole("button", { name: "Start task" })).toBeDisabled();

	// And the text is all still there: the form refused, it did not edit.
	expect((await task.inputValue()).length).toBe(MAX_BYTES + 500);
});

test("renderer: the file route out moves the specification verbatim @SPEC", async ({ page }) => {
	const task = await openComposer(page);
	const spec = rbacSpecification(MAX_BYTES + 2000);
	await paste(page, spec);
	await expect(page.getByRole("alert")).toBeVisible();

	await page.getByRole("button", { name: "Attach as file" }).click();

	// The brief becomes the pointer line, the file lands in the attachment
	// list, and Start comes back. The brief names no path: the daemon owns the
	// on-disk name and appends the real worktree-relative reference itself.
	await expect(page.getByText("task-specification.md")).toBeVisible();
	await expect(task).toHaveValue(
		"The full specification is attached as a Markdown file. Read the attached file before starting.",
	);
	expect(await task.inputValue()).not.toContain("task-specification.md");
	await expect(page.getByRole("button", { name: "Start task" })).toBeEnabled();
	await expect(page.getByRole("alert")).toHaveCount(0);
});
