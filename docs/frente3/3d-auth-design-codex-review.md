# Frente 3 / 3D — Revisión adversarial REAL de Codex de PRECONDITION_3D_AUTH_DESIGN (v1)

Copia literal de `~/.ao/scratch/frente3/reviews/3d-preflight/auth-design/codex/review.md` (ejecutada por el operador en la máquina del laboratorio, solo lectura).
sha256 del original: `ddaac46aca1c8a0a9bd86e269df8c1197762c375a942255e899c203b698c18f6`.

**Veredicto: PRECONDITION_3D_AUTH_DESIGN = NO-GO (P0 1 / P1 5 / P2 5 / P3 1).** Sustituye a la revisión provisional (no Codex) de [3d-auth-design.md](3d-auth-design.md) §9.2. Respuesta: [3d-auth-design-v2.md](3d-auth-design-v2.md).

---

No files were modified, no provider or AO process was launched, no credentials or `ao.db` were accessed, and no provider API was called.

## P0

### P0-1 — The stated blinding requirement is impossible under the treatment as defined

**Attack path.** The ASSISTED worker receives a memory pack in its prompt; OFF does not. A model can inspect its own prompt and infer the treatment. The reviewer can also read the worker transcript containing that pack. No filesystem, environment, API, or side channel is required.

**Evidence.**

- Threat model says the agent must not learn “OFF/ASSISTED”, `memoryMode`, or treatment: [3d-auth-design.md:63](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:63>).
- The preflight explicitly says the ASSISTED arm receives the pack in its prompt: [3d-preflight.md:359](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-preflight.md:359>).
- It also acknowledges that the reviewer reads the worker transcript containing the pack: [3d-preflight.md:524](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-preflight.md:524>).

**Fix.** Either:

1. redefine blinding honestly: conceal the assignment metadata and earlier repetitions, but accept that treatment recipients can infer treatment; or
2. use equal-shaped placebo context in OFF, with an independently justified placebo design.

Until that methodological contradiction is resolved, “the agent cannot learn its treatment” cannot be a precondition.

## P1

### P1-1 — The dedicated-user amendment does not isolate repetitions from macOS user-scoped state

**Attack path.** Repetition *k* writes a bit through pasteboard, `cfprefsd`/defaults, user notifications, shared memory, named semaphores, or another user-scoped broker. Repetition *k+1*, running as the same dedicated lab user, reads it. A filesystem write deny does not necessarily govern brokered Mach/XPC state.

**Evidence.**

- The design itself names defaults, pasteboard, and notifications as open channels, but its amendment only adds a dedicated user plus default-deny filesystem writes: [3d-auth-design.md:354](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:354>).
- All repetitions are assigned the same dedicated macOS user: [3d-auth-design.md:289](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:289>).
- The current profile is `allow default` and denies only one Mach service: `agent.sb:10,83`. Thus the claimed amendment is not present and its proposed form is insufficient.

**Fix.** Prefer a disposable OS identity or VM per repetition. If one user must be reused, define and test a deny-by-default Mach/XPC/IPC policy, including pasteboard, preferences, notifications, POSIX/System V shared memory and semaphores, and delete/reset all user state between repetitions. A filesystem canary alone is insufficient.

### P1-2 — The orphan-process amendment is internally inconsistent and does not close detached processes

**Attack path.** A child double-forks, changes its process group, survives tmux teardown, retains the capability token, and serves state over an allowed local mechanism or waits for the next repetition. Descendant-only or process-group enumeration loses it after reparenting.

**Evidence.**

- Current teardown kills tmux, the AO gateway, and the canary only: `run.py:499-509`.
- The proposed fix says to kill every process of the lab UID and require zero: [3d-auth-design.md:355](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:355>).
- But the same document says the harness and daemons run under that UID: [3d-auth-design.md:289](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:289>). The runner itself therefore prevents “zero processes”.

**Fix.** Run a trusted supervisor under a different UID and create a disposable UID/login session or VM per repetition, then kill that entire UID/session. Merely excluding “trusted” PIDs from a same-UID sweep is not robust against reparenting or executable-name impersonation.

### P1-3 — Shared organization rate limits remain a writable cross-repetition channel

**Attack path.** Repetition *k* deliberately consumes requests/tokens through allowed `/v1/messages` or `/v1/responses`. Repetition *k+1* observes 429s, capacity errors, latency, or reduced throughput. Removing rate-limit headers does not remove the observable service behavior. A 65-minute cache cooldown is not necessarily a quota reset.

**Evidence.**

- The design marks the channel conditionally closed solely through header removal and cooldown: [3d-auth-design.md:95](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:95>).
- It later admits rate limits remain indirectly observable: [3d-auth-design.md:314](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:314>).
- Both providers use one organization across all repetitions: [3d-auth-design.md:280](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:280>).

**Fix.** Use independently quota-scoped provider identities per repetition, or demonstrate a provider-enforced per-workspace/project quota partition. Otherwise impose gateway request/token budgets below reserved capacity and normalize delay/error behavior. The latter still requires an adversarial saturation experiment and cannot prove isolation from unrelated provider-wide throttling.

### P1-4 — The OpenAI cache cooldown is not a sound isolation control

**Attack path.** Repetition *k* creates a cache entry whose actual retention exceeds the assumed bound. Repetition *k+1* probes it through response timing even after `cached_tokens` is removed. The gateway’s prefix ledger cannot identify a foreign cache hit if the provider does not expose a reliable cache identity.

**Evidence.**

- Project isolation and the one-hour upper bound are explicitly unverified: [3d-auth-design.md:187](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:187>).
- The proposed 65-minute delay rests entirely on that assumption: [3d-auth-design.md:188](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:188>).
- The amendment simultaneously says the gateway will “fix” retention while the normative allowlist says those retention fields are rejected: [3d-auth-design.md:156](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:156>), [3d-auth-design.md:358](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:358>).

**Fix.** Do not rely on cooldown until a documented hard maximum and an adversarial measured upper bound agree. Prefer a provider-enforced per-repetition cache boundary. A possible alternative is for the gateway to prepend a large, immutable, random per-repetition cache namespace to every request, but this must be validated against the exact cache-key algorithm and model version.

**UNKNOWN.** Static evidence cannot establish the OpenAI retention ceiling or scope. Settling experiment: positive cache creation, repeated active timing/usage probes beyond 65 minutes, and cross-project/cross-organization controls, using isolated canary identities.

### P1-5 — The design remains normatively contradictory about adversarial rejected requests

**Attack path.** An agent deliberately submits a forbidden hosted tool or field. Under §4.2 this aborts the repetition, allowing outcome-dependent self-exclusion. Under §9.2 it merely counts as a failed request and the sample remains. An implementation can conform to either text.

**Evidence.**

- “Any rejected request aborts”: [3d-auth-design.md:161](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:161>).
- Amendment says rejected requests must not abort: [3d-auth-design.md:357](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:357>).
- §4.5 still demands a gateway log “without violations”: [3d-auth-design.md:233](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:233>).

**Fix.** Rewrite the normative sections. Policy denials must be ordinary agent-visible failures and the sample must count. Abort only on evidence that isolation already failed, never on an attempted violation.

## P2

### P2-1 — The credential-store design conflicts with the dedicated-user design

**Attack path.** The gateway runs under the lab user, but its dedicated keychain password resides in the operator’s login keychain. Making it accessible requires cross-user keychain access, transferring the password, or running part of the gateway as the operator—all of which reintroduce the boundary the design intends to remove.

**Evidence.**

- Password in operator login keychain: [3d-auth-design.md:127](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:127>).
- Harness and gateway are to run as the dedicated lab user: [3d-auth-design.md:289](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:289>).

**Fix.** Put the credential store wholly under a trusted supervisor identity distinct from both the agent UID and operator session, or manually unlock a lab-user keychain before the batch. Specify the exact code-signing requirement and access-control behavior.

**UNKNOWN.** Whether the proposed ACL works with the intended signing arrangement. Settle with a non-secret canary item and verify that only the final signed gateway—not Python, `security`, child processes, or replaced binaries—can access it.

### P2-2 — `op run` is not equivalent to a non-environment secret store

**Attack path.** `op run` normally injects secrets into a process environment. A same-UID sandboxed process can attempt `KERN_PROCARGS2` against the `op` parent or gateway. The amendment later says keys must never enter argv or environment, contradicting the offered alternative.

**Evidence.**

- `op run` is offered as equivalent: [3d-auth-design.md:130](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:130>).
- Keys outside argv and environment are required later: [3d-auth-design.md:362](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:362>).
- The existing probe demonstrates same-user `KERN_PROCARGS2` enumeration: `probe.py:65-87`.

**Fix.** Remove `op run` unless it can supply the secret over a protected IPC channel without an environment-bearing parent. Verify every process in the launch chain is `CS_RESTRICT` before secrets exist.

### P2-3 — The gateway allowlist lacks a complete protocol-security specification

**Attack path.** A hostile local client uses absolute-form targets, duplicate headers, `Transfer-Encoding`/`Content-Length` ambiguity, redirects, compressed bodies, duplicate JSON keys, unexpected content types, nested tool definitions, or attacker-supplied authentication/host headers to bypass semantic validation or reach a blocked upstream behavior.

**Evidence.**

- The design specifies route and field names but not request framing, canonicalization, header allowlisting, redirects, duplicate JSON semantics, size limits, or response schema: [3d-auth-design.md:139](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:139>).
- The suggested model is the current small Python gateway, which forwards the original query and most headers: `gateway.py:65-80`.

**Fix.** Parse and reconstruct requests from a strict typed schema; allowlist headers; reject duplicate JSON keys, compression, transfer encoding, absolute-form targets and unexpected query parameters; cap body/response sizes; disable redirects; construct upstream authorization internally; and mutation-test every forbidden field at every nesting position.

### P2-4 — Before/after “empty account” checks are incomplete and may require a stronger credential

**Attack path.** State survives in an unenumerated or non-listable surface—stored responses, conversations, containers, eval artifacts, fine-tuning/uploads, managed-agent memory, deleted-but-recoverable objects—or listing silently fails because the workspace service key lacks permission. The runner records “empty” without proving full coverage.

**Evidence.**

- The proposed inventory is a short fixed list: [3d-auth-design.md:226](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:226>).
- Several scope claims remain marked `VERIFICAR`: [3d-auth-design.md:88](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:88>), [3d-auth-design.md:91](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:91>).

**Fix.** Produce a versioned endpoint/state inventory from authoritative provider schemas. Every check must fail closed on authorization or pagination errors. Keep any admin credential in a separate control-plane process unavailable during samples.

### P2-5 — The “providerAuth” canary can itself contaminate the sample

**Attack path.** A deliberately unrestricted relaunch before or during a repetition writes local provider state, consumes rate limits, warms caches, or changes server feature state. Those effects then alter the measured worker/reviewer.

**Evidence.**

- The canary is described as an unrestricted session in the lab HOME: [3d-auth-design.md:222](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:222>).
- Only the later cache amendment explicitly moves cache canaries to separate scopes: [3d-auth-design.md:356](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-auth-design.md:356>).

**Fix.** Run authentication/relaunch canaries only in dedicated canary homes and provider scopes before preregistration, never inside a measured repetition.

## P3

### P3-1 — The document claims amendments are incorporated when operative sections still contradict them

Examples include abort versus no-abort, reject versus force cache retention, one OpenAI project versus archiving “the project” after each repetition, and optional versus mandatory dedicated user.

**Fix.** Replace the provisional-review table with one authoritative, internally consistent specification before implementation.

## Requested judgments

- **Necessary?** A dedicated provider organization is necessary for isolation from personal accounts unless an equally strong provider-enforced boundary is demonstrated. A per-repetition capability token, strict credential gateway, and outbound-network deny are also necessary.
- **Sufficient?** No. They do not close treatment disclosure, user-scoped macOS state, orphan processes, shared quotas/rate limits, or the unverified OpenAI cache channel.
- **Per-repetition scope:** Necessary for Anthropic under the stated workspace-scoped cache model unless a hard TTL cooldown is accepted and proven. It is useful defense in depth for server-state mistakes. A per-repetition OpenAI project is not shown to isolate cache or quota and adds little when all stateful routes are strictly blocked; the final single-project decision is reasonable only after cache and rate-limit channels are independently closed.
- **OpenAI cooldown:** Unsound as currently justified. The ceiling, scope, reset origin, extended-retention defaults, and timing oracle remain unverified.
- **Public-internet egress finding:** Real and open in cycle 6. The current profile is `allow default` and only denies loopback IP traffic (`agent.sb:10,67-68`); the preflight explicitly treated WebFetch/WebSearch as harmless “reading”: [3d-preflight.md:880](</Users/joaquinmora/Downloads/proyectos_resp/dev-orchestrator/ao-3d-prereq/docs/frente3/3d-preflight.md:880>).
- **Personal non-LLM credentials finding:** Real and open in cycle 6. The profile does not deny `~/.ssh`, `~/.config/gh`, `.netrc`, git credentials, or general keychain services, while the provider HOME links the real keychain (`labcore.py:183-190`).
- **Regression of §4.6 controls:** Unix-socket and container denies can be preserved, but preservation has not been demonstrated. The second loopback gateway broadens the network allowlist; disabling WebFetch/WebSearch and `AO_BROWSER_RUNTIME_ADDRESS` changes agent capability; rebuilding provider homes/configuration changes the frozen provider baseline; and copied skills/plugins differ operationally from the previous live symlinks. These require a fresh preregistration and full probe/mutation coverage. The statement “No cambia ninguno” is currently unsupported.

PRECONDITION_3D_AUTH_DESIGN = NO-GO
