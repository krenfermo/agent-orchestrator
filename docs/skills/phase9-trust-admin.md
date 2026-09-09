# Skills phase 9 — trust administration and the first static run

Branch `feat/skills-phase9-trust-admin`, worktree `ao-skills-phase9`, based on ECC
`0874a67988e70783f5e081f369b7dfe7897b7bf2`.

## What phase 8 left, and what was actually missing

Phase 8 built the whole authorized path and then left it unreachable. The inventory:

| Already built — do not duplicate | Missing |
| --- | --- |
| Catalog, persistence, activation, audit | — |
| `ImageAuthority` (approve/revoke/list/resolve) with RBAC, confirmation, validation | Any UI or CLI for it |
| `GET/POST/DELETE /api/v1/skills/images` | — |
| `Service.RunSkill` — the complete authorized execution path | **No HTTP route, no CLI, no UI** |
| `skillrunner` — sandbox, image trust, boundary attestation | — |
| Daemon wiring of executor + trust root | — |
| Project skills UI: list, enable, disable, dry-run | Run and report |

So phase 9 is wiring and administration, not new machinery. No second runner, no second
catalog, no re-derived authorization.

## What changed

### 1. An approval is now about bytes, not about a string somebody typed

`Approve` recorded whatever digest it was given. That digest is **client-supplied** — it
is what the form sent — and until something asks the host it is a claim about an artifact,
not the artifact. The mismatch surfaced much later as a refused run naming no cause.

`ImageAuthority` now takes an `ImageInspector` (implemented by `*skillrunner.Runner`) and
refuses in four cases: the host does not have it, the host holds **different** bytes under
that name (the error names what is actually there), the runtime cannot answer, and no
inspector is wired.

That last one is a refusal, not a skip. "Record it now, find out at run time" is exactly
the trust-what-you-were-told this check exists to prevent, and an installation that cannot
see the bytes is not one that can vouch for them.

It is a **read**: `image inspect` neither pulls nor starts anything, so approving cannot
become a way to make AO fetch or run an image. A test counts the calls and requires
exactly one.

### 2. `ao skills images` — list, approve, revoke

The routes existed; composing the HTTP request by hand was the only way in, which made the
decision that authorizes AO to execute container bytes the least approachable thing in the
product.

- `list` includes revoked and expired entries, and prints the daemon's own trust model.
- `approve` requires the **whole** scope and `--confirm`; a missing flag is a usage error
  that never reaches the daemon. Without `--expires-in` the field is **omitted**, not sent
  as zero, so "no expiry" and "already expired" cannot be confused on the wire.
- `revoke` states the non-promises where the operator reads the result: it does not kill a
  running container and does not recall a delivered secret.

### 3. `POST /api/v1/projects/{id}/skills/{skillId}/run`, and `ao skills run`

Gated on **project.manage**, not project.read — the dry run reports what *would* happen,
this makes it happen. They are two interface methods deliberately: one method with a
boolean would let a caller turn a read into an execution by editing a field.

The body carries no image, no command, no argv, and an undeclared field is **refused**
rather than ignored, so nobody can start smuggling one in and discover later that AO was
silently dropping it. There is a test that tries exactly that with `image`, `argv` and
`attestation`.

### 4. A misleading refusal, fixed

`RunSkill` checked `s.executor == nil` **first**, so a skill that was never enabled on the
project was answered with *"this installation has no skill runner"* — sending somebody to
install a container runtime to fix an activation. The check now sits immediately before
the one line that needs an executor (taking AO's own attestation), which is the order the
function's own docstring already promised. Nothing above it stages or starts anything, so
a refusal still leaves no copy of anybody's source on disk.

### 5. Coverage before findings, everywhere

Both the CLI and the UI render **coverage first**, then findings, then the tool's stated
limitations. This is not layout preference: `0 findings` is not a result until you know
what was read. When nothing was scanned, both surfaces say so in words — *"nothing was
scanned, so nothing was found; this is not a clean result"* — instead of showing an empty
list that reads like a clean bill of health.

Both also show which bytes ran and who approved them, and both report an approval revoked
mid-run: AO does not stop a running container, so that fact has to reach the reader.

## Results

| Gate | Exit | Result |
| --- | --- | --- |
| `go build ./...` | 0 | — |
| `go vet ./...` | 0 | — |
| `go test ./... -short` | 0 | 202 packages, 0 failures |
| frontend `typecheck` | 0 | — |
| frontend `test` | 0 | 247 files, 2888 passed, 1 skipped |
| frontend `build` | 0 | — |

`npm run api` exited **127** (`openapi-typescript` not on PATH) and left `schema.ts` stale
while `openapi.yaml` had already moved — the known trap. Regenerated with the pinned
`openapi-typescript@7.4.4`; both artifacts are committed together.

## What a user can do from the UI now

**Settings → Skill container images** (needs `settings.manage`):

- See every approval — active, expired and revoked — with scope, digest, who approved it,
  when, and the stated reason.
- Approve a digest, with the whole scope required before the button enables.
- Revoke an active approval.

**Project settings → Skills** (needs `project.manage` to run):

- Install/enable/disable as before, and **Check what a run needs** (starts nothing).
- **Run** appears only once a dry run says the mode is executable.
- The report: coverage, skipped files, findings, the tool's limitations, which image ran
  and who approved it, plus a warning if the approval was revoked mid-run.

## What is still blocked

- `Runner.Execute` — refused, and nothing calls it. Running a skill-authored command needs
  the `arbitrary_process_execution` control, which does not exist.
- `process.exec`, `repo.write`, `secrets.read`, `net.active_scan` — no surface gained.
- `net.egress` — refused on every build that does not package the proxy
  (`-tags ao_embed_egress_proxy`) **and** measure the boundary on the host.
- `net.active_scan` — additionally needs named targets, a window, rate limits and its own
  approval. No pentest is reachable from anywhere in this branch.
- Only `static-code` / `ao.static-scan/v1` is executable. Every other mode is refused with
  `SKILL_MODE_NOT_EXECUTABLE`, which is deliberately distinct from an authorization
  failure.

## What a full on-demand security audit still needs

1. **`arbitrary_process_execution`** — a contract for what a skill may ship as a command,
   with a negative test proving it cannot exceed its scope. Until then only AO-authored
   tools run.
2. **The packaged egress proxy in a release build**, plus a measured boundary, before
   `net.egress` (dependency and advisory modes) can run at all.
3. **`net.active_scan`'s own control**: named targets, a time window, rate limits, and an
   approval separate from the image approval.
4. **Report persistence.** Runs are audited (`run_executed` / `run_refused`, with digest,
   mode and a summary) and readable via `GET /skills/{id}/audit`, but the **full report is
   returned to the caller and not stored**. Re-reading yesterday's findings is not
   possible yet; that needs a report store and a retrieval route.
5. **Approval ergonomics.** The digest still has to be obtained out of band
   (`docker image inspect`). A "these are the images on this host" picker would remove the
   copy-paste step without weakening anything, since the daemon verifies presence anyway.

## Residual risks

- The images screen is translated for **English and Spanish only**; the other six locales
  carry English copy. Machine-translating a security screen is worse than leaving it.
- **No live container run was performed.** Every gate above is unit/e2e with the live
  container tests skipped, per the instruction not to launch Skills. The static path is
  proven by construction and by the existing `*_live_test.go` suite, not by an execution
  in this session.
- The egress probe container does not pass `--pull=never` (the main runner does). Not
  exploitable — `resolveProbeContract` fails closed on an absent image and pins to the
  locally-resolved digest — but it is defense-in-depth worth adding.
