# Skills catalog — integration roadmap

**Status: subfases 1-2 mostly done; subfase 3 partly done. Phase 4
(2026-09-09) made ONE mode execute: `static-code`, inside the container, over a
staged scope-limited copy.** Four controls remain unbuilt and each blocks one
capability — see `docs/adr/0005-pending-capability-controls.md` for the design
of all four and the implementation of none.

Phase 1 delivered the catalog core and the `security-audit` package. Phase 2
made it administrable: SQLite persistence, an audit trail, HTTP routes, `ao
skills`, a project settings panel, and the dry run. Each remaining subfase below
is independently shippable and lists what it touches, so it can be scheduled
against the stabilization work rather than colliding with it.

Ordering rule: **subfase 3 gates everything that runs.** Subfases 1, 2 and 5 can
proceed in parallel with it; subfases 4 and 6 cannot start before it lands.

---

## Subfase 1 — Install and per-project activation, end to end — **DONE**

Make the catalog reachable. Nothing new can run as a result; a user can install
a package, see what it asks for, and grant a subset per project.

- **Storage.** ✅ Migration 0161 adds `skill_installs`, `skill_activations` and
  `skill_audit`. No merged migration was modified. `skillcatalog.Registry`
  remains as the file-backed implementation its own tests exercise; both paths
  share one rule set via the exported `ValidateGrant` / `Authorize`.
- **Service.** ✅ `internal/service/skills`. Installing and uninstalling gated on
  `settings.manage`, activation on `project.manage`, the audit trail on
  `audit.read`.
- **HTTP.** ✅ `httpd/controllers/skills.go` plus nine `specgen` operations;
  `openapi.yaml` and `schema.ts` regenerated and committed with the Go change.
- **CLI.** ✅ `ao skills list|show|install|uninstall|enable|disable|dry-run|audit`.
- **UI.** ✅ Project Settings → Skills, built from `components/ui/*`, localized
  into all eight locales. The grant editor shows per capability what it allows,
  its risk, the permission required to grant it, and — for anything needing
  containment — that it stays blocked until a runner exists. All of that comes
  from the daemon's response, never from a table in React.
- **Touched:** migration 0161, `sqlc.yaml`, `queries/skills.sql`, new store/
  service/controller/CLI files, `api.go`, `authz_routes.go`, `specgen/build.go`,
  `telemetrymeta/cli.go`, the migration ledger, and the frontend settings
  surface. **Conflict risk with lifecycle work: low** — no lifecycle, planner,
  placement or recovery file was touched.

## Subfase 2 — Capability resolution and approvals in the product — **mostly done**

- ✅ Persist approvals: who granted what, when, against which package digest —
  `skill_audit`, with a distinct `grant_changed` action so authority widening
  over time is legible, and `install_rejected` so a package that FAILED
  verification leaves a trace.
- ✅ Surface the dry run as a route, a CLI verb and a panel. It needs no runner
  and is the highest-value surface in the roadmap.
- ⬜ Per-run and per-target approval prompts, with the approval recorded against
  the run rather than the activation. The dry run already reports
  `requiredApproval` and refuses a per-target capability with no named target;
  what is missing is the prompt and the durable per-run approval record.
- ⬜ Mirror the catalog trail into the installation-wide auth audit
  (`service/rbac`, `controllers.AuthAudit`) so one reader covers both.
- **Touches:** service + HTTP + UI only. **Conflict risk: low.**

## Subfase 3 — Isolated runner and egress control (**the gate — partly done**)

ADR 0004 decides the boundary and `internal/skillrunner` proves it. Containment
is no longer the open question; four specific controls are.

**Done (phase 3):**

- ✅ A Linux container boundary: no network, non-root, no capabilities,
  immutable rootfs, read-only input mount, cgroup v2 limits, digest-pinned
  image, an explicit env allowlist.
- ✅ Fail-closed on every unusable runtime — missing binary, unreachable
  daemon, Windows containers, cgroup v1. No host fallback exists to fall into.
- ✅ Attestation as a set of named `Control` values that AO derives from
  probes, replacing two coarse booleans. A caller cannot supply one; the
  dry-run API has no attestation field.
- ✅ Boundary evidence collected from inside the container — uid, the cgroup
  values the kernel reports, an actual outbound attempt, the image digest, and
  **how many input files were visible**, because on macOS an unshared bind
  mount arrives empty and silent and would otherwise produce a clean audit of
  nothing.
- ✅ Wall clock, memory, CPU, pid and output-size limits, with teardown that
  leaves no orphan.
- ✅ Seven isolation tests against a real runtime, plus fail-closed tests that
  run everywhere.

**Still missing — each blocks one capability:**

| Control | Blocks | What has to be built |
| --- | --- | --- |
| ~~`egress_allowlist`~~ | ~~`net.egress`~~ | **Built (phase 7).** An `--internal` network with no route out, a dual-homed proxy sidecar enforcing an exact scheme/host/port allowlist, resolve-once-and-dial-the-address against rebinding. Packaging the binary is the remaining gap. |
| ~~`writable_workspace`~~ | ~~`repo.write`~~ | **Built (phase 6).** A capped tmpfs the run writes to, transferred into quarantine and validated on the host; artifacts and a diff, never applied. The patch-review surface is still missing. |
| ~~`scoped_secret_delivery`~~ | ~~`secrets.read`~~ | **Built (phase 5).** Scope-bound grants with expiry and revocation, single-use per-attempt leases, 0600 files at /run/secrets, never an env var. No HTTP/CLI/UI surface yet, deliberately. |
| `arbitrary_process_execution` | `process.exec` | The skill-image contract: what a skill may ship, how it is built, how its command is authored and pinned |

**ADR 0005 designs all four.** Secret delivery (phase 5), the writable
workspace (phase 6) and the egress allowlist (phase 7) are built with their
negative tests. Only `arbitrary_process_execution` remains design only — and it
is the one gated on a trust-root decision nobody has made.

`net.active_scan` stays blocked even with egress: it additionally requires
`arbitrary_process_execution`, and active testing needs named targets, a window,
rate limits and a specific approval that no network control provides.

Note the shape of the remaining work: none of it is "make the sandbox
stronger". The sandbox holds. What is missing is four narrower mechanisms, each
of which can be built and tested on its own.

`skillrunner.Runner.Execute` is deliberately still refused: proving the boundary
is not the same as having a contract for what may run inside it. That contract
is `arbitrary_process_execution`, above.

- **Touched:** `internal/skillrunner` (new), the attestation model in
  `internal/skillcatalog`, and the dry-run projection. **Conflict risk: low** —
  no lifecycle, planner, placement or migration file.
- **Reuse note:** `internal/reviewgateway` already prepares private
  config/state/cache/temp roots per reviewer, and ADR 0002 records it waiting on
  exactly this platform isolation. When the runner is wired for real, generalize
  that package rather than writing a second one.

## Subfase 4 — Workflow and reviewer integration — **blocked on subfase 3**

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

## Subfase 5 — Reports and durable evidence — **not started**

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

## Subfase 6 — First authorized pilot — **blocked on subfase 3**

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

1. ~~**Container runtime.**~~ **Settled in phase 3 (ADR 0004):** a Linux
   container is required, and a desktop without one gets a refusal, not a
   degraded run. What is NOT settled is the product decision below.
2. ~~**Egress enforcement on macOS.**~~ **Settled:** it exists only inside a
   container. `sandbox-exec` was measured and gives a binary network switch with
   no filesystem boundary and no resource limits.
3. **Secret scoping.** `secrets.read` has no store to read from. Which secrets
   can a skill request, and who approves each name?
4. **Signature verification.** `provenance.signature` is rejected today. Adding
   verification means deciding a trust root before the catalog accepts anything
   from outside this repository.
5. ~~**Multi-tenancy.**~~ **Settled in phase 2:** installs are
   installation-wide, activations are per project, and tenant isolation rides on
   project access. There is deliberately no tenant column — a second scope would
   be a second answer to a question projects already answer.

6. **Install sources.** Only a local absolute directory is supported. Adding a
   git or registry source means deciding a trust root first, which is the same
   decision `provenance.signature` is blocked on (question 4).

7. **THE DECISION THIS PHASE NEEDS.** Docker (or an equivalent Linux VM) is now
   a hard requirement for any skill that needs containment. Three options, and
   the answer changes what gets built next:

   a. **Require it.** Skills are unavailable on a desktop without a container
      runtime; the UI says so and points at the install. Simplest and most
      honest, and it makes AO's first-run story heavier.

   b. **Require it only for what needs it.** The read-only audit modes
      (`static-code`, `secret-scan`, `authz-review`) need no control at all —
      they are AO's own agent reading a checkout AO already has. Those could run
      with no runner, and everything else refuses. Best user experience;
      requires being very careful that "needs no control" stays true as the
      modes evolve, since it is the one path with no boundary.

   c. **Ship a runtime.** AO bundles or manages a VM. Largest scope by far, and
      it puts AO in the business of maintaining a Linux distribution.

   ~~Option (b) is the recommendation~~ **Decided: option B, conditioned.** The
   condition — a read mode may skip the runtime only if AO itself effectively
   restricts it — turned out to exclude every read mode, because AO has no such
   restriction outside the container. So reading requires confinement too, and
   the practical effect of option B is: with a container runtime, the read modes
   run; without one, nothing does. Recorded in ADR 0004's phase-4 note.
