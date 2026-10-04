import { expect, test } from "@playwright/test";
import { installFakeAgent, installFakeIdentity } from "./support/fake-bridge";

// SES-* RENDERER SMOKE (issue #2483, renderer slice).
//
// Scope: this runs under `dev:web` (VITE_NO_ELECTRON=1) with an injected
// `window.ao` + a fake CDC/SSE stream + an in-page workspace snapshot. It
// exercises the renderer's SSE → invalidate → refetch path only — NOT the real
// daemon, storage, API, preload, PTY, or filesystem. Those boundaries are
// exercised only in the packaged-app pod gate (#2697), which today runs a
// boot-level smoke (app launches, daemon ready), NOT these cases — per-case pod
// coverage is future work. The case IDs cross-reference the #2483 catalog; they
// are not a claim of full-boundary coverage, and this suite is not the canonical
// T0/P0 gate.

const card = (id: string) => `[data-testid="board-session-card"][data-session-id="${id}"]`;
const columnCard = (column: string, id: string) =>
	`[data-testid="board-column"][data-column="${column}"] [data-session-id="${id}"]`;

// #2483 SES-002.

// Every renderer spec boots past identity resolution first.
//
// The renderer resolves the current user on mount and renders the sign-in
// screen IN PLACE OF the shell for any answer it cannot read as a user — and
// these specs run with no daemon, so an unstubbed identity turns every
// assertion in this file into "element not found" against a login form. The
// hook is here rather than inside each test because a spec that forgets it does
// not fail loudly; it fails describing the wrong thing.
test.beforeEach(async ({ page }) => {
	await installFakeIdentity(page);
});

test("renderer: new session card appears in the spawning/working state @T0 @SES", async ({ page }) => {
	// Renderer note: there is no distinct "spawning" badge — a freshly spawned
	// session enters the Working column (badge "Working"); the daemon's
	// spawning→working transition lands here. The card must not exist until the
	// fake agent creates it.
	await installFakeAgent(page);
	await page.goto("/#/");
	await expect(page.getByTestId("board")).toBeVisible();
	await expect(page.locator(card("fake-spawn"))).toHaveCount(0);

	await page.evaluate(() =>
		window.__aoFakeAgent!.createWorker({ id: "fake-spawn", title: "Spawning worker", activity: "exited" }),
	);

	await expect(page.locator(columnCard("working", "fake-spawn"))).toBeVisible();
	await expect(page.locator(card("fake-spawn"))).toContainText("Working");
});
