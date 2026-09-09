# 4. Skill runner: a Linux container is the only boundary AO will accept

Date: 2026-09-09
Status: Accepted (boundary decision + prototype). **No capability is unblocked
by this ADR.** The prototype attests a strict subset of the controls the
blocked capabilities require; see "What is still missing" below.

## Context

ADR 0003 built the skill catalog and stated the thing it could not do: a
manifest is not a security boundary, and `Authorize` trusts exactly one value
for containment — a runner's own attestation. AO shipped `NoRunner()`, which
attests nothing, so every capability needing containment is refused.

This ADR decides what a real runner is, and proves the decision against this
machine rather than against documentation.

### What is being defended against

The threat is not a hostile author writing a malicious manifest. It is the
ordinary case: **a skill is an LLM agent plus tools, and a security-audit skill
in particular reads attacker-controlled content** — source, dependency
metadata, HTTP responses from a target. Prompt injection through that content
is the expected condition, not the exception. So the boundary must hold when
the thing inside it is doing something nobody intended.

Concretely, a skill run must not be able to:

1. **Read files outside its inputs** — other projects' source, `~/.ssh`,
   browser cookie stores, AO's own database.
2. **Inherit AO's credentials** — the daemon's environment holds provider
   tokens, SMTP credentials and the agent-credential file path
   (`AO_AGENT_CREDENTIAL_FILE`). An audit skill that can read the daemon's env
   has escalated to every project AO can reach.
3. **Reach the network** — exfiltration is one HTTP request, and a security
   report is exactly the payload worth exfiltrating.
4. **Write anywhere that outlives the run** unless that was granted.
5. **Exhaust the host** — a runaway or deliberate fork bomb / memory balloon
   must not take the desktop down.
6. **Survive its own teardown** — an orphaned child that keeps running after
   AO believes the run ended is an unaccounted process holding whatever it had.
7. **Lie about any of the above.** A run that self-reports "I stayed in scope"
   is worth nothing; the evidence has to come from the boundary.

### Explicitly not the threat model

AO does **not** claim to defend against a container escape by a kernel exploit,
a malicious image from a registry AO chose to trust, or a compromised Docker
daemon. Those are the substrate; if they fall, so does everything.

## Evidence gathered on this machine

Measured on macOS 26.6.2 (arm64), Docker CLI 29.2.1 against a **colima** VM
(Ubuntu 24.04, cgroups v2, seccomp + apparmor, Virtualization.framework,
virtiofs). Every line below was run, not assumed.

### macOS without a VM: `sandbox-exec` (Seatbelt)

| Probe | Result |
| --- | --- |
| Apple's named profile `-n no-network` blocks a socket connect | **Yes** — `PermissionError` |
| That same profile restricts filesystem reads | **No** — read a "secret" file outside any allowed path |
| A custom `(deny default)` profile running `/bin/cat` | **SIGABRT (exit 134), no diagnostic on stderr** |
| CPU / memory / pid limits | **None** — macOS has no cgroups |

The custom-profile result is the decisive one. A deny-by-default Seatbelt
profile aborts a plain system binary with no explanation, because a modern
macOS process needs far more (dyld shared cache, Mach services) than any
allowlist AO could author against an interface Apple has marked deprecated and
never documented. AO cannot write, test, or maintain that profile.

**Verdict: `sandbox-exec` gives a binary network on/off switch and nothing
else. It is not a filesystem boundary, has no resource limits, and cannot
express an egress allowlist. It is not sufficient for any blocked capability.**

### Linux container in a VM (what a Mac actually has)

| Probe | Result |
| --- | --- |
| Host paths outside the mount (`/Users`, `/private`) | **Not present** in the container |
| Daemon env var (`AOSK_SECRET`) visible to the container | **No** — 5 env vars total, none inherited |
| Write to a `:ro` bind mount | **Denied** — read-only file system |
| Write to the container root with `--read-only` | **Denied** — read-only file system |
| `--network none`, outbound connect | **Denied** — "Network unreachable" |
| `--network <internal>`, outbound connect | **Denied**, while the container network still routes internally |
| `--cpus 0.5` | **Enforced** — `cpu.max` = `50000 100000` |
| `--memory 64m`, over-allocate | **Enforced** — OOM-killed, exit 137 |
| `--pids-limit 16`, fork loop | **Enforced** — stopped at 14 forks, `pids.max` = 16 |
| `docker kill` on a container with 3 children | **All reaped with the PID namespace**, no orphans |
| Image identity | `alpine@sha256:6baf…` — pinnable and verifiable by digest |

### The finding that changed the design

**A bind mount from a host path the VM does not share mounts as an empty
directory, silently, with exit 0.**

This machine's colima VM mounts exactly one host path
(`/Users/joaquinmora/Downloads/proyectos_resp`). A mount of `~/.ao/...` —
AO's own data dir — produced an empty `/work` with no error at all.

For a security audit that is the worst possible failure: the skill sees zero
files, finds zero problems, and reports a clean audit of a project it never
read. Nothing in the skill, the manifest or the exit code distinguishes that
from a genuinely clean result.

It also rules out the obvious staging location: **AO cannot stage a run under
`~/.ao` and expect the container to see it**, because `~/.ao` is outside the
VM's mount set on this machine and AO does not control that setting.

## Decision

### 1. The boundary is a Linux container, and nothing else qualifies

A skill run executes in a container with, at minimum:

```
--network none                    (or an AO-owned internal network; never the default bridge)
--user <non-root>                 no root inside the namespace
--read-only                       immutable root filesystem
--cap-drop ALL                    no capabilities
--security-opt no-new-privileges  no setuid escalation
--memory / --cpus / --pids-limit  cgroup v2 enforcement
--tmpfs /tmp                      bounded, non-persistent scratch
image pinned by digest            not by tag
env: an explicit allowlist        never the daemon's environment
```

macOS reaches this through a Linux VM (colima, Lima, Docker Desktop, Rancher).
The VM is not the boundary — the container inside it is; the VM is what makes a
Linux container available on a Mac at all.

### 2. No runtime means no run. Ever.

If the runtime is absent, the daemon is unreachable, or a required feature is
missing, the runner **refuses**. There is no host fallback, no degraded mode,
no "run it unconfined this once". A capability that needed containment and did
not get it is refused, not downgraded.

This is the same rule ADR 0003 applied to `NoRunner()`, restated where it is
now enforceable.

### 3. Attestation is a set of named controls, produced by AO, from probes

ADR 0003 modelled containment as two booleans (`Isolated`, `EgressControlled`).
That is too coarse: "the process is confined" and "this skill may write to your
repository" are different claims, and one boolean cannot carry both.

`RunnerAttestation` now carries a set of named `Control` values, and each
capability names the controls it actually requires. A runner attests only what
it has **probed**, and a capability whose controls are not all present is
refused with the missing control named.

The caller cannot supply an attestation. The dry-run API takes no attestation
parameter, and the service passes the runner's own — a self-declared
`isolated: true` is exactly the claim this design exists to refuse.

### 4. Evidence comes from the boundary, not from the skill

Every run returns `BoundaryEvidence` collected by AO: the effective uid, the
cgroup limits as the kernel reports them, whether outbound network was
reachable, the image digest that actually ran, and — because of the silent-empty
mount — **the number of input files the container could see**.

That last one is a control, not a diagnostic. A run whose inputs did not arrive
is failed by the runner, so a false clean report is not reachable.

### 5. Egress: deny-all now, allowlist later, and they are different claims

Default-deny is `--network none` and is verified. An allowlist is not the same
control and is not built. The evidence above shows the substrate for it — an
`--internal` network reaches containers but not the internet — so the design is
an AO-owned forward proxy on that network, with the skill container holding no
other route and the proxy enforcing the manifest's `scope.network.allow`.

Until that exists, `net.egress` and `net.active_scan` stay refused. "The
network is off" is not "the network is limited to these hosts", and conflating
them would be the exact mistake this ADR is written to avoid.

### 6. Runner identity, not human credentials

The runner is a component of the daemon and acts with the daemon's own
authority to write results. The skill container gets **no** AO credential: not
the operator's CLI credential, not an agent credential, not the daemon's
environment. It receives inputs on a mount and returns a report on a mount, and
that is the entire interface.

This follows `internal/agentcred`'s existing rule for the same reason recorded
there: a credential in an environment variable is readable from the process
table, inherited by every child, and lands in crash dumps.

## Acceptance criteria

A runner may attest a control only when a test demonstrates it. The prototype
in `internal/skillrunner` demonstrates:

| # | Criterion | Control attested |
| --- | --- | --- |
| 1 | Cannot read a file outside the input mount | `filesystem_isolation` |
| 2 | Does not inherit the daemon's environment or credentials | `no_credential_inheritance` |
| 3 | Outbound network denied by default | `egress_deny_all` |
| 4 | CPU, memory, pid and wall-clock limits enforced; output truncated at a cap | `resource_limits` |
| 5 | Timeout kills the run and leaves no orphaned child | `process_isolation` |
| 6 | The result carries boundary evidence AO collected, including proof the inputs arrived | (all of the above) |
| 7 | An unavailable runtime refuses without executing on the host | (fail-closed) |

## What is still missing, and what stays blocked

The prototype attests `filesystem_isolation`, `process_isolation`,
`no_credential_inheritance`, `resource_limits` and `egress_deny_all`. It does
**not** attest, and therefore does not unblock:

| Capability | Missing control | What has to be built |
| --- | --- | --- |
| `net.egress` | `egress_allowlist` | The forward proxy on an internal network, enforcing `scope.network.allow` |
| `net.active_scan` | `egress_allowlist` | The above, plus per-target authorization plumbed to the proxy |
| `repo.write` | `writable_workspace` | A writable overlay and a reviewed way to return changes to the host |
| `secrets.read` | `scoped_secret_delivery` | Per-run secret injection by name from AO's store, without an env var |
| `process.exec` | `arbitrary_process_execution` | The skill-image contract: what a skill may ship, how it is built, how its command is authored and pinned |

So after this ADR, `security-audit`'s read-only modes still report *blocked* in
production, because AO does not yet wire this runner into the dry-run path as
its default — see the integration plan in `docs/skills/roadmap.md`. Nothing
this ADR adds can be reached by a person clicking through the app.

## Residual risks accepted

- **Container escape.** Namespaces plus seccomp plus apparmor is the substrate;
  a kernel escape defeats it. On macOS the VM is a second layer, which is a
  fact, not a claim AO makes.
- **The image supply chain.** Digest pinning proves you got the bytes you
  named. It does not prove those bytes are trustworthy. AO has no signature
  verification (ADR 0003 rejects `provenance.signature` for the same reason).
- **The VM's mount configuration is not AO's to control.** AO can detect that
  inputs did not arrive and fail; it cannot fix the user's colima or Docker
  Desktop settings.
- **A read-only audit still reads the source.** Confinement stops exfiltration
  and escalation; it does not stop the skill from producing a wrong or
  misleading report. That is what the report schema's coverage and
  false-positive fields are for, not what the boundary is for.
- **Docker socket exposure is never granted.** Mounting `/var/run/docker.sock`
  into a skill container would hand it the host; the runner has no option to do
  so, and adding one would void every claim here.
