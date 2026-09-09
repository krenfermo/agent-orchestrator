# Skills catalog — integration roadmap

Phase 1 (this branch) delivered the catalog core and the `security-audit`
package. Nothing is wired into the daemon and nothing can execute. Each subfase
below is independently shippable and lists what it touches, so it can be
scheduled against the stabilization work rather than colliding with it.

Ordering rule: **subfase 3 gates everything that runs.** Subfases 1, 2 and 5 can
proceed in parallel with it; subfases 4 and 6 cannot start before it lands.

---

## Subfase 1 — Install and per-project activation, end to end

Make the catalog reachable. Nothing new can run as a result; a user can install
a package, see what it asks for, and grant a subset per project.

- **Storage.** Move the registry from `registry.json` into two SQLite tables
  (`skill_installs`, `skill_activations`) in one new migration. Do not modify
  any merged migration. Keep the file-backed registry behind the same interface
  during the move so the tests carry over unchanged.
- **Service.** `internal/service/skills`, following the read/write boundary of
  the existing services. Every route gated on `domain.Permission` — installing
  and uninstalling on `settings.manage`, activation on `project.manage`.
- **HTTP.** New DTOs in `httpd/controllers/dto.go` plus `apispec/specgen`
  entries, then `npm run api`. Commit `openapi.yaml` and `schema.ts` with the Go
  change.
- **CLI.** `ao skills list|install|show|enable|disable|uninstall`, thin over the
  daemon routes.
- **UI.** A Skills section in project settings, built from `components/ui/*`.
  The activation dialog must show, per capability: what it allows, its risk, the
  permission required to grant it, and — for anything needing containment — that
  it will be refused until a runner exists. An approval dialog that hides the
  refusal is worse than no dialog.
- **Touches:** new migration, new service, `dto.go`, `specgen/build.go`, new CLI
  commands, new frontend routes. **Conflict risk with lifecycle work: low.**

## Subfase 2 — Capability resolution and approvals in the product

- Persist approvals: who granted what, when, against which package digest.
- Per-run and per-target approval prompts, with the approval recorded against
  the run rather than the activation.
- Surface `PlanRun` as a dry-run route: "what would this need, and what is
  missing". This is the highest-value UI in the whole roadmap and needs no
  runner.
- Emit every grant, denial and approval into the existing auth audit trail
  (`service/rbac`, `controllers.AuthAudit`).
- **Touches:** service + HTTP + UI only. **Conflict risk: low.**

## Subfase 3 — Isolated runner and egress control (**the gate**)

Nothing that needs containment runs until this exists. Requirements, not
suggestions:

- A container or equivalent boundary with its own filesystem view. The project
  checkout is mounted read-only unless `repo.write` was granted.
- **Default-deny egress**, opened only to `scope.network.allow` plus the run's
  explicitly authorized targets. This is the single hardest requirement and the
  reason the roadmap has this shape.
- No AO credentials in the runner's environment. Follow `agentcred`'s pattern:
  the secret in a `0600` file, the filename in the env, never the value.
- Wall-clock and resource limits, with a hard kill.
- An honest `RunnerAttestation`. Returning `Isolated: true` without the boundary
  defeats every check in `skillcatalog`.
- `Execute` re-runs `Authorize` against its own attestation. A `Plan` is an
  input to re-check, never a permission slip.

Reuse rather than rebuild: `internal/reviewgateway` already prepares private
config/state/cache/temp roots and an empty git-hooks dir per reviewer, and ADR
0002 records that it is waiting on exactly this platform isolation. Generalize
it; do not write a second one.

- **Touches:** new runtime package, likely `internal/process` and
  `internal/runtimehome`. **Conflict risk: medium** — schedule after
  stabilization settles.

## Subfase 4 — Workflow and reviewer integration

Only once the core is stable and subfase 3 has landed.

- A workflow step that runs a skill mode, with its denial surfacing as a normal
  step failure carrying the reason.
- Optionally offer a security-audit mode as a reviewer, feeding
  `domain.ReviewRun`.
- **Still on demand.** No automatic audit on every change; the user picks the
  project, the mode and the scope every time. If a scheduled audit is ever
  wanted, it is a separate decision with its own approval record.
- **Touches:** `internal/workflow`, `internal/review`. **Conflict risk: high**
  — do not start while lifecycle work is in flight.

## Subfase 5 — Reports and durable evidence

- A `skill_runs` table: run id, project, skill, version, mode, decision, granted
  capabilities, approver, target, timings, exit reason.
- Validate every report against the manifest's declared output schema before
  storing it. An unvalidated report is not evidence.
- Model findings on `domain.PreReviewEvidence`, which is already versioned and
  typed, rather than inventing a third evidence shape.
- **Redaction is a storage-layer control, not a prompt.** The skill is told
  never to put a secret in a report; the store must also reject or redact one
  that arrives anyway.
- History, diffing between runs, and export.
- **Touches:** new migration, new service, UI. **Conflict risk: low** (can be
  built against fixtures before subfase 3).

## Subfase 6 — First authorized pilot

- Target: a **non-production** environment, with written authorization recorded
  as `authorizationRef`.
- Run the read-only modes first and review the findings by hand — including the
  false positives, which are the real measure of whether the report format works.
- Only then, and only with a named target inside the runner's egress allowlist,
  the `active-pentest` mode.
- Success is not "findings were produced". It is: the report is accurate, the
  refusals were correct, no secret reached the report, and the runner's
  boundaries held.

---

## Open questions to settle before subfase 3

1. **Container runtime.** Docker is already a dependency for container reaping
   (`ContainerReapConfig`). Is it a hard requirement for skills, and what
   happens on a desktop without it — refuse, or degrade to read-only modes?
2. **Egress enforcement on macOS.** The most likely answer is that egress
   control only exists inside a container, which makes the previous question
   load-bearing.
3. **Secret scoping.** `secrets.read` has no store to read from. Which secrets
   can a skill request, and who approves each name?
4. **Signature verification.** `provenance.signature` is rejected today. Adding
   verification means deciding a trust root before the catalog accepts anything
   from outside this repository.
5. **Multi-tenancy.** Is an installed skill installation-wide or per tenant?
   `domain.TenantID` exists; the registry currently has no tenant column.
