# Skill runner: capability matrix and operating requirements

What AO can contain, on which platform, and what an operator has to install for
any of it to work. The decision behind this is
[`docs/adr/0004-skill-runner-isolation.md`](../adr/0004-skill-runner-isolation.md);
this page is the operational half.

**Nothing here is wired into AO's production path.** The runner exists,
its boundary is tested, and no capability is unblocked by it.

## Controls

A *control* is one named guarantee an execution environment provides. A
capability names the controls it depends on, and is refused — naming the
missing one — until all of them are attested.

| Control | Means |
| --- | --- |
| `filesystem_isolation` | The run sees only the inputs AO mounted, and the root filesystem is immutable |
| `process_isolation` | Its own PID namespace, non-root, no orphan surviving teardown |
| `no_credential_inheritance` | None of AO's environment reaches the run |
| `resource_limits` | CPU, memory, pid count, wall clock and output size are bounded and kernel-enforced |
| `egress_deny_all` | No outbound connection is possible |
| `egress_allowlist` | Outbound traffic is limited to declared destinations |
| `writable_workspace` | The run may modify a workspace and AO can return the changes under review |
| `scoped_secret_delivery` | AO can hand the run one named secret, scoped to it, without an env var |
| `arbitrary_process_execution` | A skill may run commands it authored (needs the skill-image contract) |

## Platform matrix

| Control | Linux + container | macOS + Linux VM (colima / Docker Desktop / Lima / Rancher) | macOS, no VM (`sandbox-exec`) | Any host, no runtime |
| --- | --- | --- | --- | --- |
| `filesystem_isolation` | **Yes** | **Yes** | **No** — see below | No |
| `process_isolation` | **Yes** | **Yes** | Partial (no pid cap) | No |
| `no_credential_inheritance` | **Yes** | **Yes** | Yes (AO clears the env) | No |
| `resource_limits` | **Yes** (cgroup v2) | **Yes** (cgroup v2 in the VM) | **No** — macOS has no cgroups | No |
| `egress_deny_all` | **Yes** | **Yes** | **Yes** (`-n no-network`) | No |
| `egress_allowlist` | Not built | Not built | **Not possible** | No |
| `writable_workspace` | Not built | Not built | Not built | No |
| `scoped_secret_delivery` | Not built | Not built | Not built | No |
| `arbitrary_process_execution` | Not built | Not built | Not built | No |

"Not built" is AO's gap. "No"/"Not possible" is the platform's.

### Why `sandbox-exec` does not qualify

Measured on macOS 26.6.2, not inferred:

- Apple's named profile `-n no-network` **does** block an outbound connect
  (`PermissionError`), so a binary network on/off switch is real.
- That same profile leaves the filesystem wide open — a "secret" file outside
  any allowed path read fine.
- A custom `(deny default)` profile killed `/bin/cat` with **SIGABRT and no
  diagnostic on stderr**. A modern macOS process needs far more than any
  allowlist AO could author against an interface Apple marked deprecated and
  never documented.
- macOS has no cgroups, so there is no CPU, memory or pid limit to set.

A network switch with no filesystem boundary and no resource limits is not a
boundary AO will run a security-audit skill inside.

## Requirements

### Supported runtimes

Docker Engine 20.10+ (CLI reachable as `docker`), reporting:

- `OSType: linux` — Windows containers are not the boundary AO relies on.
- `CgroupVersion: 2` — v1 splits memory and pid accounting in ways that make
  "the limit was enforced" unreadable from inside, so AO refuses rather than
  attest a limit it cannot read back.

Podman and nerdctl are CLI-compatible enough that adding them is a constant
rather than a redesign, but neither is claimed until it is probed and tested.

### On macOS: the VM's shared paths are the trap

macOS reaches Linux containers through a VM, and **that VM shares only the host
paths it is configured to share**. A bind mount from any other path arrives as
an *empty directory, silently, with exit 0*.

For a security audit that is the worst failure available: the skill reads
nothing, finds nothing, and reports a clean audit of a project it never opened.

On the machine this was developed on, colima's VM mounts exactly one host path
(`/Users/joaquinmora/Downloads/proyectos_resp`), and a mount of `~/.ao` — AO's
own data dir — produced an empty `/work`.

Two consequences:

1. **AO cannot stage a run under `~/.ao` and assume the container sees it.**
   The staging directory has to be inside a shared path, and AO does not
   control that setting.
2. **AO verifies rather than assumes.** The runner refuses a request whose
   input directory is empty on the host, and every run reports how many files
   the container could actually see. A run whose inputs did not arrive is
   failed, so the false-clean report is unreachable.

Operators: add the project checkout's parent to the VM's mounts —
`colima start --mount <path>:w`, or Docker Desktop → Settings → Resources →
File sharing.

### What a run gets

```
--network none          --user 65534:65534      --read-only
--cap-drop ALL          --security-opt no-new-privileges
--memory / --memory-swap (equal, so swap cannot make the limit advisory)
--cpus  --pids-limit    --tmpfs /tmp (64m, noexec, nosuid)
image pinned by digest  env: an explicit allowlist, never the daemon's
```

Defaults: 2 minutes wall clock, 512 MiB, 1 CPU, 128 pids, 1 MiB of captured
output. Output is capped by AO, not the kernel — an unbounded stdout is a
memory limit on the *daemon*, which no container flag protects.

### What a run never gets

- Any AO credential. Not the operator's CLI credential, not an agent
  credential, not the daemon's environment. Inputs arrive on a mount and the
  report leaves on a mount; that is the whole interface.
- The Docker socket. Mounting it would hand the container the host, and the
  runner has no option to do so.
- A writable host path. Every mount is read-only.

## Failure behavior

Every one of these refuses **without executing anything on the host**:

| Condition | Result |
| --- | --- |
| No `docker` binary, or the daemon is unreachable | `ErrRuntimeUnavailable`, with the daemon's own error |
| Windows containers | Refused — the boundary AO relies on is Linux namespaces |
| cgroup v1 | Refused — AO cannot read back the limits it set |
| Image given by tag | Refused — a tag is a mutable pointer |
| Input directory empty on the host | Refused — a run over no inputs reports a clean audit of nothing |
| Credential-shaped env name | Refused — a backstop; the real control is that AO never reads its own env |

There is no host fallback and no degraded mode. A capability that needed
containment and did not get it is refused, never downgraded.

## Evidence

Every run returns what AO observed, collected from inside the boundary rather
than from what the workload says about itself: effective uid, the cgroup limits
**as the kernel reports them**, the result of an actual outbound attempt, the
image digest that ran, whether the root filesystem refused a write, how many
credential-shaped variables leaked in, and how many input files were visible.

Controls are then *derived* from those numbers and compared against what AO
asked for — so a flag the runtime silently ignored yields a missing control
rather than a false one. `BoundaryEvidence.Verify` fails a run that did not
demonstrate what the runtime claims.

## Residual risks

- **Container escape.** Namespaces + seccomp + apparmor is the substrate. On
  macOS the VM is a second layer — a fact, not a claim AO makes.
- **Image supply chain.** Digest pinning proves you got the bytes you named,
  not that they are trustworthy. AO has no signature verification.
- **The VM's mount configuration is the operator's.** AO can detect that inputs
  did not arrive; it cannot fix the setting.
- **A confined audit can still be wrong.** The boundary stops exfiltration and
  escalation. It does not make a report accurate — that is what the report
  schema's coverage and false-positive fields are for.
