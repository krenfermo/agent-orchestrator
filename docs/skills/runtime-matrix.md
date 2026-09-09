# Skill runner: capability matrix and operating requirements

What AO can contain, on which platform, and what an operator has to install for
any of it to work. The decision behind this is
[`docs/adr/0004-skill-runner-isolation.md`](../adr/0004-skill-runner-isolation.md);
this page is the operational half.

**One mode executes: `static-code`, via `ao.static-scan/v1`.** Everything else
is refused with the missing control named. See "What can actually run" below.

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

## What can actually run

`static-code`. It stages a scope-limited copy of the checkout, runs AO's own
`ao.static-scan/v1` pattern scanner inside a container with no network, and
returns a report whose coverage lists the files scanned, the files skipped and
why, the rules that ran, and the tool's own limitations.

**Reading is not exempt from confinement.** Phase 3 left `repo.read` requiring
no control, on the reasoning that AO already had the checkout. That does not
survive the question "restricted by what?": the only thing that would have
confined an agent reading a repository outside a container is the agent CLI's
tool allowlist, which AGENTS.md records as void under `bypassPermissions`. A
prompt, a manifest and a CLI permission are none of them a boundary, so reading
now requires the same five controls as everything else — it simply needs
nothing beyond them, which is why it is the mode that runs.

`ao.static-scan/v1` is a **pattern scanner, not a static analyzer**. It matches
text; it does not parse, and it cannot follow a value to a sink. Its report says
so in `coverage.limitations`, and an empty findings list means "these eight
rules matched nothing in the files listed as scanned" — never "no
vulnerabilities".

### The tool contract

A manifest names a **tool** from a closed vocabulary. It cannot name an image,
a binary, or one argument. AO resolves the tool to a base image present on this
host, pins it by digest, and authors the command in Go. Two validated integers
(file budget, per-file size cap) are the only variable part.

That is why `arbitrary_process_execution` remains a separate control: the
contract is deliberately not general.

## Platform matrix

| Control | Linux + container | macOS + Linux VM (colima / Docker Desktop / Lima / Rancher) | macOS, no VM (`sandbox-exec`) | Any host, no runtime |
| --- | --- | --- | --- | --- |
| `filesystem_isolation` | **Yes** | **Yes** | **No** — see below | No |
| `process_isolation` | **Yes** | **Yes** | Partial (no pid cap) | No |
| `no_credential_inheritance` | **Yes** | **Yes** | Yes (AO clears the env) | No |
| `resource_limits` | **Yes** (cgroup v2) | **Yes** (cgroup v2 in the VM) | **No** — macOS has no cgroups | No |
| `egress_deny_all` | **Yes** | **Yes** | **Yes** (`-n no-network`) | No |
| `egress_allowlist` | **Yes**, with the packaged proxy sidecar | **Yes**, with the packaged proxy sidecar | **Not possible** | No |
| `writable_workspace` | **Yes**, with a usable workspace root | **Yes**, with a usable workspace root | **No** — no confinement to write inside | No |
| `scoped_secret_delivery` | **Yes**, with a secret authority | **Yes**, with a secret authority | **No** — no confinement to deliver into | No |
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

### Where inputs are staged

AO copies **only the in-scope files** to a staging directory and mounts that
read-only, rather than mounting the checkout. Scope is then enforced by what
exists on the mount instead of by an instruction a skill may ignore, and `.git`
(every version of every file, including deleted secrets), `node_modules`,
`vendor` and friends never cross the boundary at all.

The staging root defaults to `<project-parent>/.ao-skill-staging`, because the
project's parent is the one path AO can reason about being inside the VM's
shared tree. It is never `~/.ao` (outside the mount set on a normal colima
install) and never the home directory itself. An operator whose layout differs
can override it.

Per-run directories are removed on every exit path, success or failure. The
root itself is kept: removing and recreating it churns a path virtiofs caches,
and the next run's mount then fails to resolve a directory that demonstrably
exists on the host.

Files AO cannot stage are **recorded, not dropped**: a filename the tool cannot
address unambiguously through a shell, or a file it could not read, appears in
`coverage.skipped` with a reason. A file counted as staged but never scanned
would be a coverage lie, which is the failure this whole path exists to prevent.

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

## Scoped secret delivery

A run receives a secret only when **both** halves exist: a container to deliver
into, and a working authority (a store plus a sealing key) to produce the value.
A container alone proves nothing about where a secret would come from, and a
value handed to an unconfined process is a value handed to the host — so
`secrets.read` needs the five confinement controls *and* the delivery control.

### What authorizes one delivery

A grant is bound to a full scope with no wildcard: **tenant + project + skill +
version + mode**. A lease on top of it is bound to **one run and one attempt**
and is redeemable exactly once. So a secret granted for medusa's `dependencies`
mode at version 0.1.0 is unusable from poseidon, from another organization, from
version 0.2.0, from `active-pentest`, from a previous attempt, and from a second
redemption of the same lease.

Grants expire — a grant with no expiry is refused, because it is one nobody
removes — and revocation is re-checked **at redemption**, not only when the
lease was minted. A lease is a right to ask, not a right to receive.

Granting requires `settings.manage`. Handing a credential to a package is an
installation-level act however narrow the scope, so a project administrator who
can activate a skill still cannot give it a secret.

Partial authorization delivers **nothing**. A run written against two secrets
and handed one behaves in ways nobody designed.

### How it arrives

A per-run directory, mode 0700, one 0600 file per reference, bind-mounted
read-only at `/run/secrets`. **Never an environment variable** — the reasons
`internal/agentcred` records have not changed: an env var is readable from the
process table by anything running as this user, is inherited by every child, and
lands in crash dumps.

Never mounted: the home directory, the Keychain, a credential file, AO's data
dir, or the container socket. The secrets mount is the only mount a run gets
beyond its read-only inputs.

Cleanup overwrites and removes on **every** exit path — success, failure,
timeout, cancellation — and refuses any path outside AO's own namespace, so a
bug in root selection cannot turn teardown into a delete of somebody's files.

### What never carries a value

`SecretValue.String`, `GoString` and `MarshalJSON` all redact, and
`UnmarshalJSON` refuses. `%v`, `%+v`, `%#v`, `fmt.Errorf`, `slog` and
`json.Marshal` are the six routes a leak takes, and all six are closed by
construction. Evidence and reports carry **names only** — not values, and not
prefixes or hashes of them, because a prefix is enough to confirm a guess.

### Residual risk

The value transits the host filesystem. A container runtime cannot pre-fill a
tmpfs, so a bind mount is the mechanism available. The file is 0600 for the
length of the run and is overwritten before unlinking, which removes it from
everything that reads by path — but that is not a guarantee about the medium on
a copy-on-write or journaling filesystem. A real vault delivers over a channel
the value never leaves; AO is not one, and this is where somebody deciding
whether that is good enough finds out.

There is also **no surface**: no HTTP route, no CLI verb, no UI. Registering a
secret and granting one are Go APIs only, so nothing here is reachable from the
app. That is deliberate — which secrets a skill may request, and who approves
each name, is a policy decision that should precede the surface.

## Writable workspace

A run may write, and it may write only to a filesystem that exists for the
length of the container. **The operator's checkout is never mounted — writable
or otherwise.** What the run sees is the staged copy at `/work`, read-only.

```
/work        read-only   the staged, scope-limited copy
/workspace   read-write  a tmpfs, size-capped, where the run writes
/out         read-write  a bind mount AO's own wrapper transfers into
```

### Why a tmpfs and not a writable bind mount

A bind mount has no size limit AO can enforce, and `--memory` does not help
because pages written to one are page cache rather than the cgroup's. A tmpfs is
charged to the memory cgroup and its `size=` is enforced by the kernel —
measured: a 4 MiB write into a 1 MiB tmpfs stops at exactly 1 MiB.

The cost, also measured: a tmpfs dies with the container and `docker cp` cannot
reach it afterwards. So AO's wrapper transfers `/workspace` into `/out` as its
last act, and what reaches the host is bounded by the cap by construction.

### Everything collected is quarantined, not trusted

The transfer preserves file types on purpose, so the host-side validator faces
the real thing: a symlink the run created arrives pointing at the **host's**
filesystem, and a FIFO arrives as a FIFO. Collection uses `Lstat` and never
follows a link. It refuses, and **reports**:

| Refusal | Why |
| --- | --- |
| `symlink` | Arrives pointing at the host; deciding one is harmless is not AO's judgement to make |
| `hardlink` | An artifact whose bytes another name can change is not an artifact |
| `special_file` | A device, socket or FIFO is not a change to a repository |
| `path_escapes_workspace` | Lexical traversal out of the quarantine |
| `path_too_long` | Refuses the directory and skips the subtree |
| `file_too_large`, `total_size_exceeded`, `file_budget_exhausted` | The caps, re-checked on the host so a runtime that ignored the tmpfs flag produces a refusal rather than a surprise |

A refused file is left in quarantine rather than deleted, so a person can look
at it. A caller who sees three artifacts and expected four is told which one was
refused and why.

### What comes out, and what does not happen to it

`WorkspaceResult` carries the artifacts by digest, each marked added, modified
or unchanged against the staged inputs; the deletions; the refusals; and the
inputs actually used, by digest — evidence of what the run read, not only of
what it wrote.

`Applied` is a field on that struct and it is always `false`. **Nothing is ever
written to the operator's checkout.** Applying is a separate, human-approved act
with its own base/HEAD check, and there is no route, CLI verb or UI for it yet.

### Ownership and cleanup

Each workspace is one run AND one attempt — the directory name carries both, so
two attempts never share one. A marker records the owner, the ids and a token
minted at preparation.

Cleanup **fails closed**: a missing, unreadable or mismatched marker refuses the
removal and keeps the directory. An orphan somebody deletes by hand is
recoverable; deleting a directory that turned out to be somebody else's is not.
A leftover from a crashed attempt is kept, never reused.

### What writing does NOT unblock

`process.exec`, `secrets.read`, `net.egress` and `net.active_scan` are untouched
by this control, and a test asserts it. Writing is not a reason to reach the
network, run a chosen command, or read a credential.

## Egress allowlist

Outbound traffic is limited to destinations granted for this run. Not "the
network is off" — that is `egress_deny_all`, which a plain container already
gives — and not a promise the tool is asked to keep.

### The topology is the control

```
ao-egress-int-<run>   --internal   skill + proxy
ao-egress-ext-<run>   bridge       proxy + the outside
```

The skill joins **only** the internal network. Measured from inside it: an
internet IP is "Network unreachable", the cloud-metadata address is "Network
unreachable", and Docker's embedded resolver answers **SERVFAIL** for external
names — so DNS exfiltration through the resolver is closed too.

A live test unsets `HTTP_PROXY`, `http_proxy` and `HTTPS_PROXY` inside the
container and confirms that restores nothing. The variables are a convenience
for well-behaved clients; **there is no route to bypass**.

### What a destination may be

`scheme://host:port`, matched exactly on all three. A grant for
`https://api.example.test:443` does not authorize `http://` to the same host, a
different port, a subdomain, or `api.example.test.evil.test`.

Refused outright: **wildcards** (a grant covering hosts that do not exist yet is
one nobody can enumerate), **IP literals** ("may reach 34.117.x.y" is not a
statement anybody can evaluate, and it is the shape an exfiltration destination
takes), unqualified names, paths, and embedded credentials.

### DNS rebinding

The proxy resolves the host **itself**, checks **every** returned address
against the blocked ranges, and then dials the checked address rather than the
name — so the second lookup that would have answered differently never happens.
A name that resolves to both a usable address and a blocked one is refused
outright: allowing it would leave which one gets used to chance.

Blocked whatever a name resolves to: link-local (including 169.254.169.254),
loopback, RFC1918, carrier-grade NAT, unique-local v6, multicast and reserved.

### Redirects

The proxy does not follow them. A 30x reaches the client, and the client's next
request comes back through the proxy and is checked like any other. For a
CONNECT tunnel there is nothing to follow — one authority is authorized and
that is what the tunnel carries.

### The private-range exception

`permittedPrivateCidrs` lets an installation whose artifact registry genuinely
lives at 10.x use this control instead of turning it off. Each entry is
validated, appears in the proxy's startup line and in the audit summary, and
**can never re-open link-local** — 169.254.0.0/16 is where cloud metadata hands
out the host's own credentials.

It widens which **addresses a granted name may resolve to**, never which names
may be reached.

### Evidence

Every authorization is recorded: the method, scheme, host, port, the address it
resolved to, whether it was allowed and, if not, why. Destinations and outcomes
only — never payloads, never credentials. `Proxy-Authorization` and every other
hop-by-hop header are stripped before a request reaches an upstream.

### What egress does NOT unblock

**`net.active_scan` stays blocked.** A forward proxy can express "may open a
connection to this host"; it cannot express "may probe this host for
weaknesses", because it does not inspect payloads and would not know the
difference. The scan additionally requires `arbitrary_process_execution`, which
nothing provides — and active testing needs what no network control can give:
named targets, a time window, rate limits, and an approval that says which
system may be attacked.

`process.exec`, `secrets.read` and `repo.write` are untouched too.

### How the proxy reaches the container (phase 8)

The proxy is a Go binary that must run inside a Linux container. It is **not**
compiled while a skill runs. It is built ahead of time, pinned by digest, and
embedded into the daemon.

```
scripts/build-egress-proxy.sh          build linux/amd64 + linux/arm64, verify digests
  --check                              build twice, compare bytes  (reproducibility)
  --record                             rewrite provenance.json     (a reviewable act)

backend/internal/skillegress/proxybin/
  provenance.json                      committed: version, toolchain, flags, sha256 per arch
  artifacts/ao-egress-proxy-linux-{amd64,arm64}   built, gitignored, 6 MiB each
```

The build is reproducible by construction — `CGO_ENABLED=0`, `-trimpath`,
`-buildvcs=false`, `-ldflags="-s -w -buildid="` — and measured so: `--check`
rebuilds both architectures byte-identical. A Go version change moves every
digest, and the verify path says exactly that instead of failing opaquely.

**Release builds carry the proxy; nothing else does.**

| Build | Carries the proxy | Attests `egress_allowlist` |
| --- | --- | --- |
| `go build ./...` (any developer, any test) | No | **No** — refuses, naming the build tag |
| `go build -tags ao_embed_egress_proxy ./...` | Both architectures | Only after the boundary is measured |

The embed directive names both artifacts, so a release missing one **fails to
compile** rather than shipping and refusing on exactly the machines that needed
it.

### What the runner checks before attesting

`VerifyEgressBoundary` measures two things. Neither is a flag a caller passes —
the boolean that phase 7 accepted is gone, because a promise is not an
attestation.

1. **The right proxy is present.** Packaged in this build; for the architecture
   the **container runtime** reports (on macOS the VM's, not the daemon's);
   hashing to the digest `provenance.json` records. Checked again after the file
   is written to staging, because "AO carried the right binary" and "the right
   binary is what the container will mount" are different claims.
2. **The boundary holds.** AO creates an `--internal` network, confirms the
   runtime reports it as internal, and watches a container on it **fail** to
   reach `192.0.2.1` (RFC 5737) and `169.254.169.254`. Nothing external is
   contacted: with no default route the kernel refuses locally.

The artifact check runs first and short-circuits — a build with no proxy cannot
enforce anything however good the host's topology is.

### The staged mount

```
<project parent>/.ao-egress-proxy/<run id>/     0711   traversable, not listable
  ao-egress-proxy                               0555   no write bit for anybody
  policy.json                                   0444   destinations and a lease, never a credential
                                                       mounted read-only at /aoproxy
```

Two files and nothing else: a read-only mount still exposes everything under it.
The permissions look loose and are not — the container runs as uid 65534 and
owns nothing, so it needs other-execute and other-read; what is absent is any
write bit. **Neither the home directory nor AO's data dir may be the mount
source**: AO's data dir holds the database and every project's state, and
mounting it to deliver one binary would put all of that inside a container.

### Two findings from building this

**A caller must not be able to name the architecture.** `StageRequest` takes what
the runtime *reported* (`"aarch64"`, `"x86_64"`) and maps it itself. A field
called `GOARCH` is one somebody fills in with the daemon's own architecture,
which on macOS is the wrong one.

**The kernel is not the safeguard.** A `linux/amd64` proxy on a `linux/arm64`
runtime does *not* fail with an exec-format error on Docker Desktop: binfmt
emulation is registered inside the VM, and the foreign binary starts and serves
normally (measured — it ran for five minutes until the test was killed). The
architecture check is AO's control, not the kernel's, and the live test asserts
it by hashing the mounted binary from inside the container.

### Still open

Wiring `-tags ao_embed_egress_proxy` into the desktop release pipeline. Until
that lands, every shipped daemon is an unpackaged one that refuses egress —
which is the correct failure, and still a failure.

## Which image AO may execute (phase 8)

The trust root is **explicit administrative approval, per immutable digest and
per full scope**. It replaces what the runner did before, which was to ask the
runtime what `alpine:3.19` was and use the answer -- trusting whoever last ran
`docker pull` on the host.

```
approval = (tenant, project, skill, version, mode) + tool + sha256 digest
           + approver + stated reason + optional expiry + revocation
```

| Property | Answer |
| --- | --- |
| Who may approve | `settings.manage`. A **project** administrator is not enough |
| What is approved | One immutable `sha256:` digest. A tag is refused |
| For what | One exact scope AND one AO tool. Nothing neighbouring |
| Resolved how | By digest, against what the runtime reports it holds. Never by name |
| Pulling | Never. `--pull=never` on every container; an absent image is a refusal |
| Re-approving | REPLACES. One scope has one answer, never an accumulating list |
| Checked when | At resolution, and **again immediately before the container starts** |

**What an approval is not:** a publisher signature. AO verifies none, contacts
no registry and pulls nothing. An administrator who approves a malicious digest
has approved a malicious image; what AO guarantees is that the decision was made
by somebody with administrative permission, recorded with their name and reason,
scoped to one thing, and that the bytes which run are the bytes approved.

### Revocation

| | |
| --- | --- |
| Stops new executions | **Yes**, immediately -- re-checked at the last point before launch |
| Stops a container already running | **No.** A kill mid-run produces a truncated report a reader could mistake for a complete one, and the blast radius is bounded: no network, read-only mount, wall clock in minutes. The report records that the approval was withdrawn during the run |
| Recalls a delivered secret | **No.** A value in a container's memory does not come back. Rotate it if it matters |

The exact wording lives in `skillrunner.RevocationPolicy` and is served in the
API response, so a client shows the same promise the runner enforces.

## What can actually execute

One mode: `static-code`. Only when the runtime is usable AND an approval covers
the exact scope. The caller supplies a project, a skill, a mode, who they are,
and the manifest's declared inputs -- and cannot supply an attestation, a
command, an image, an isolation flag, a scope, or the files to read.

`process.exec`, `repo.write`, `net.active_scan` and `secrets.read` gain no
surface from this. They are refused by the capability table with the missing
control named, and a test with the real runner wired asserts exactly that.

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
| No secret store or no sealing key | Every secret operation refused; the control is not attested |
| A secret grant that never expires | Refused — one nobody removes |
| A lease redeemed twice, or by another attempt | Refused |
| A grant revoked between minting and launch | Refused at redemption |
| A workspace whose ownership cannot be proven | Kept for recovery, not deleted |
| A leftover workspace from a crashed attempt | Kept, never reused |
| A run that produced a symlink, hardlink or special file | Refused and reported; the file stays in quarantine |
| A destination not in this run's allowlist | Refused by the proxy, recorded with the reason |
| A granted name that resolves into a blocked range | Refused at connection time, recorded with the address |
| An expired egress lease | Refused per request, so a run loses the network mid-run |
| The proxy down, or refusing its policy | No egress at all; there is no fallback route |
| A daemon built without `-tags ao_embed_egress_proxy` | `egress_allowlist` not attested; `net.egress` refused, naming the tag |
| A proxy artifact for another architecture | Refused at selection; the foreign binary is never staged |
| Proxy bytes that do not hash to `provenance.json` | Refused before staging, and again after the file is written |
| An `--internal` network the runtime does not report as internal | The boundary is not attested |
| A container on AO's internal network that DOES reach out | The boundary is not attested; the evidence records what was reached |
| A leftover proxy staging directory | Kept, never reused |
| No image approved for this scope | Refused before staging; no copy of the source is made |
| An approval for another tenant, project, version, mode or tool | Refused; there is no neighbouring match |
| An approval expired or revoked | Refused, and the two stay distinguishable from "never approved" |
| An approval revoked between resolution and launch | Refused at the pre-launch re-check |
| The runtime holds different bytes under the approved digest | Refused, naming both digests |
| The approved image is absent from the host | Refused. AO does not pull, build or import |
| A manifest read scope AO cannot express as files | Refused, rather than widened to the whole checkout |

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
