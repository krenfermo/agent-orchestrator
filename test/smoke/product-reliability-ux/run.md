# Smoke — Product Reliability UX

**Status: PREPARED. NOT RUN. Awaiting explicit authorization.**

Code under test: `54cba2cdd69258ccc844978a3bb49b51fa74d0b8`
(`origin/feat/engineering-control-center`, the Product Reliability UX integration merge)

## What only this can prove

The test suite proves the renderer sends the right body to the right path against a
mocked `apiClient`. It cannot prove the **daemon accepts it**, and it cannot exercise
**hash history**, which only exists in the real Electron build. Both gaps are why this
smoke exists.

Five assertions, one run, one throwaway project:

1. Creation starts **from the project surface**, not the global Workflows nav.
2. The project arrives **already selected** — the hash-history fix, observable nowhere else.
3. **Structured checks** are accepted by the form *and* frozen into the run by the daemon.
4. A real **`workflow_run` ID** comes back from the API.
5. The advice panel, change-set block and diagnostics export render on that real run.

## Rules this smoke obeys

- **It never restarts AO** and never touches an existing session, project or run.
- **It deletes nothing** — not the fixture repo, not the project, not worktrees, not
  branches, not the run, not the evidence. Teardown is a human decision (`TEARDOWN.md`).
  A cleanup step that fires on failure destroys the only state that explains the failure.
- **One new temporary repo, one run.** Never MEDUSA, POSEIDÓN, CRM or any real project.
- No merge, push, release or deploy.

## Step 0 — preflight (read-only, run it first)

```bash
cd <this worktree>
./test/smoke/product-reliability-ux/preflight.sh
```

It resolves the run file the way the supervisor does — `$AO_RUN_FILE`, else
`~/.ao/dev/running.json` in dev, else the platform default — and reads the **owner** and
**port** out of the daemon's own record rather than assuming either. It also reports
whether any running supervisor predates the code under test.

**If it prints BLOCKED, stop and report.** Do not start or restart AO to unblock it; that
is the user's call.

### On what "the daemon contains the expected HEAD" means here

In dev the daemon is `go run ./cmd/ao daemon` from `<worktree>/backend`, and the renderer
is Vite-served from the same worktree. Both follow whatever was checked out **when the app
was started**.

This integration changed **0 files under `backend/`**, so the daemon's own code is
unaffected by it. What must be current is the **renderer**. A supervisor started before
`54cba2cdd` is serving pre-merge UI even though the files on disk are new — and assertions
2 and 5 would then be testing the old build. Preflight prints supervisor start times for
exactly this reason.

## Step 1 — the throwaway fixture

```bash
./test/smoke/product-reliability-ux/setup-fixture.sh
```

Creates `~/.ao/smoke/prux-<stamp>/` — a real git repo with one commit and a hermetic
`check.sh` that asserts `smoke.txt` exists. Nothing installs, nothing reaches the network,
so a failure here can only be AO's.

It prints the `ao project add …` line. **Run that line yourself**; it is the only mutating
command in the setup.

## Step 2 — create the workflow from the project surface (manual, by hand)

This half is manual on purpose: it is the part that only a human at the real app can
observe.

1. Open the **AO smoke `<stamp>`** project in the running desktop app.
2. Sidebar project menu (or the Board topbar) → **"New workflow run"**.
   - **Assertion 1** — the entry exists on the project surface, next to and distinct from
     "New session".
   - **Assertion 2** — the form opens with **AO smoke already selected**. If the project
     picker is empty, the hash-history fix has regressed. Record it and stop.
3. Strategy: **Task**. Objective: `Create a file named smoke.txt at the repository root`.
4. Verify command: `./check.sh`. Leave args empty; keep retry-safe checked.
   - **Assertion 3a** — the form accepts it and **Create workflow run** becomes enabled.
     (With the command field empty it must stay disabled — worth a two-second check.)
5. Click **Create workflow run**.

Then, **without creating anything**, confirm the naming fix: open the same project menu →
**"New session"**. The field must read **Instructions**, and the dialog must say it does
not create a workflow run. Close it without starting a worker.

## Step 3 — read back what was really created

```bash
./test/smoke/product-reliability-ux/verify-run.sh <project-id>
```

All GETs. Prints the **`workflow_run` ID** (assertion 4), the frozen strategy, the advice
block, the steps, and whether `check.sh` is bound into the run's plan — the half of
assertion 3 that only the daemon can confirm.

## Step 4 — the three new surfaces, on a real run

Open the run's detail page.

- **5a** — the advice panel states a category, and names any refused action *with its
  reason*.
- **5b** — on the review step, the change-set block states either a proven count or "AO
  could not establish…" plus the daemon's sentence. Either is a pass; **blank is a fail**.
- **5c** — **Copy diagnostics**, paste into an editor, confirm:
  - the run ID and reason code are present,
  - there is **no `/Users/…` path**,
  - the objective's body is absent (first line only).

Switch the app to Spanish, reload the same run: panel, change-set headline and button must
all be Spanish. The daemon-composed `unprovableChangeSetReason` staying English is
**expected** — that is the tracked debt in
`docs/contracts/unprovable-change-set-code.md`, not a failure.

## Stop conditions

Abort and report rather than improvising if:

- the form opens with **no project selected** (hash-history fix regressed);
- the create is refused with the checks present;
- no `wf-` ID appears, or the UI reported success and `verify-run.sh` finds nothing —
  that is the original defect returning;
- the diagnostics export contains a filesystem path;
- more than one run exists for the fixture project (the smoke was run twice; it is
  specified as a single run).

## What to hand back

The `workflow_run` ID, the `verify-run.sh` output, the pasted diagnostics bundle, and a
yes/no per assertion. Leave everything in place.
